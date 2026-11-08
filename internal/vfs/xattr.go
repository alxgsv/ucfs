package vfs

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
)

// Extended attributes of files. The user.uc.* attributes describe the
// Uploadcare file behind a name; user.uc.meta.<key> is its metadata and
// user.uc.tags its tags, both writable.
const (
	XattrUUID   = "user.uc.uuid"
	XattrCDNURL = "user.uc.cdn_url"
	XattrMIME   = "user.uc.mime"
	XattrMD5    = "user.uc.md5"
	XattrWidth  = "user.uc.width"
	XattrHeight = "user.uc.height"
	XattrStored = "user.uc.stored"
	XattrTags   = "user.uc.tags"
	XattrMeta   = "user.uc.meta."
	xattrNS     = "user.uc."
)

// setxattr(2) flags, the same on Linux and macOS.
const (
	XattrCreate  = 1
	XattrReplace = 2
)

// The API's limits (RESEARCH §2, probed 2026-11-08): 50 metadata keys, a key
// of up to 64 characters from [\w\-\.\:], a value of 1 to 512 characters;
// 50 tags of up to 100 characters from [a-z0-9._-] after lowercasing.
const (
	maxKeyLen   = 64
	maxValueLen = 512
	maxTags     = 50
	maxTagLen   = 100
)

var (
	keyRe = regexp.MustCompile(`^[\w\-.:]+$`)
	tagRe = regexp.MustCompile(`^[a-z0-9._-]+$`)
	md5Re = regexp.MustCompile(`^"[0-9a-f]{32}"$`)
)

type metaStats struct {
	refreshes, refreshErrors, lists, gets, sets, removes, md5Heads, groupFetches atomic.Int64
}

// MetaStats counts metadata work: info refreshes, extended attribute
// calls, CDN HEADs for md5 and group reads.
type MetaStats struct {
	Refreshes, RefreshErrors, Lists, Gets, Sets, Removes, MD5Heads, GroupFetches int64
}

func (f *FS) MetaStats() MetaStats {
	s := &f.stats
	return MetaStats{Refreshes: s.refreshes.Load(), RefreshErrors: s.refreshErrors.Load(), Lists: s.lists.Load(),
		Gets: s.gets.Load(), Sets: s.sets.Load(), Removes: s.removes.Load(), MD5Heads: s.md5Heads.Load(),
		GroupFetches: s.groupFetches.Load()}
}

// cdnURL is the CDN URL of an original or a variant.
func (f *FS) cdnURL(uuid, ops string) string {
	return strings.TrimSuffix(f.opts.CDNBase, "/") + "/" + uuid + "/" + ops
}

// details returns a present file with its metadata and tags. With refresh,
// info older than MetaTTL is fetched again first, since listings never
// show metadata or tags that other clients changed later; concurrent
// callers share one request, and a failed refresh serves the index's copy.
func (f *FS) details(ctx context.Context, uuid string, refresh bool) (index.Details, error) {
	d, err := f.ix.Details(ctx, uuid)
	switch {
	case errors.Is(err, index.ErrNotFound) || (err == nil && d.Removed):
		return d, syscall.ENOENT
	case err != nil:
		return d, f.ioErr("index lookup", err)
	case !refresh || f.opts.Remote == nil || f.now().Sub(d.RefreshedAt) < f.opts.MetaTTL:
		return d, nil
	}
	if _, err, _ := f.refreshes.Do(uuid, func() (any, error) { return nil, f.refresh(ctx, uuid) }); err != nil {
		slog.Warn("refresh file info; serving the index", "uuid", uuid, "err", err)
		return d, nil
	}
	return f.details(ctx, uuid, false)
}

func (f *FS) refresh(ctx context.Context, uuid string) error {
	file, err := f.opts.Remote.Info(context.WithoutCancel(ctx), uuid)
	switch {
	case api.IsNotFound(err):
		err = f.ix.MarkRemoved(ctx, uuid)
	case err == nil:
		err = f.ix.PutInfo(ctx, f.now(), file)
	}
	if err != nil {
		f.stats.refreshErrors.Add(1)
		return err
	}
	f.stats.refreshes.Add(1)
	f.Touch()
	return nil
}

// md5 is a file's content hash: the CDN's strong ETag of an original is
// its MD5, except for multipart uploads, whose ETag is S3's hash of the
// parts' hashes with a "-<parts>" suffix. It costs one HEAD the first time
// and is kept, since content never changes; files the mount uploaded have
// it from their upload. Empty when the ETag is not an MD5.
func (f *FS) md5(ctx context.Context, d index.Details) (string, error) {
	if d.MD5 != "" {
		return d.MD5, nil
	}
	v, err, _ := f.md5s.Do(d.UUID, func() (any, error) {
		f.stats.md5Heads.Add(1)
		info, err := f.cdn.Head(context.WithoutCancel(ctx), d.UUID, "")
		if err != nil {
			return "", err
		}
		if !md5Re.MatchString(info.ETag) {
			return "", nil
		}
		sum := strings.Trim(info.ETag, `"`)
		if err := f.ix.SetMD5(ctx, d.UUID, sum); err != nil {
			slog.Warn("record md5", "uuid", d.UUID, "err", err)
		}
		return sum, nil
	})
	if err != nil {
		slog.Warn("md5 from the CDN", "uuid", d.UUID, "err", err)
		return "", syscall.EIO
	}
	return v.(string), nil
}

// xattrTarget is what a node's extended attributes describe: an uploaded
// file, a variant of one, or a file still being written.
type xattrTarget struct {
	uuid  string
	ops   string
	spool *spool.Spool
}

// target returns ok false for nodes without extended attributes:
// directories, local-only files and the journal.
func (f *FS) target(ctx context.Context, n Node) (xattrTarget, bool, error) {
	switch n.Kind {
	case KindFile, KindDateFile, KindViewFile:
		_, err := f.file(ctx, n.UUID)
		return xattrTarget{uuid: n.UUID}, true, err
	case KindVariant:
		_, err := f.file(ctx, n.UUID)
		return xattrTarget{uuid: n.UUID, ops: n.Ops}, true, err
	case KindTreeFile:
		o, err := f.occupant(ctx, n)
		if err != nil {
			return xattrTarget{}, true, err
		}
		if o.spool != nil {
			return xattrTarget{spool: o.spool}, true, nil
		}
		return xattrTarget{uuid: o.file.UUID}, true, nil
	}
	return xattrTarget{}, false, nil
}

// fileXattrs are the attributes of an uploaded file, as name-value pairs.
// md5 is listed with an empty value that Getxattr fills, when it is known
// or the file is too small to have been uploaded in parts.
func (f *FS) fileXattrs(d index.Details) [][2]string {
	out := [][2]string{{XattrUUID, d.UUID}, {XattrCDNURL, f.cdnURL(d.UUID, "")}}
	if d.MIME != "" {
		out = append(out, [2]string{XattrMIME, d.MIME})
	}
	if d.MD5 != "" || (d.IsReady && d.Size < api.MinMultipart) {
		out = append(out, [2]string{XattrMD5, ""})
	}
	if d.Width > 0 && d.Height > 0 {
		out = append(out, [2]string{XattrWidth, strconv.Itoa(d.Width)}, [2]string{XattrHeight, strconv.Itoa(d.Height)})
	}
	out = append(out, [2]string{XattrStored, strconv.FormatBool(d.Stored)})
	if len(d.Tags) > 0 {
		out = append(out, [2]string{XattrTags, strings.Join(d.Tags, ",")})
	}
	return append(out, metaXattrs(d.Metadata)...)
}

func metaXattrs(meta map[string]string) [][2]string {
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, len(keys))
	for i, k := range keys {
		out[i] = [2]string{XattrMeta + k, meta[k]}
	}
	return out
}

// pendingXattrs are the attributes of a file not uploaded yet: an empty
// uuid, and what was set on it, which its upload form will carry.
func pendingXattrs(q index.Queued) [][2]string {
	out := [][2]string{{XattrUUID, ""}}
	if q.Tags != nil && len(*q.Tags) > 0 {
		out = append(out, [2]string{XattrTags, strings.Join(*q.Tags, ",")})
	}
	return append(out, metaXattrs(q.Meta)...)
}

// xattrs returns a node's attributes; refresh is false for listings, which
// ls -l asks for on every file and must stay off the network.
func (f *FS) xattrs(ctx context.Context, n Node, refresh bool) ([][2]string, *index.Details, error) {
	t, ok, err := f.target(ctx, n)
	switch {
	case err != nil:
		return nil, nil, err
	case !ok:
		return nil, nil, nil
	case t.spool != nil:
		return pendingXattrs(t.spool.Queued()), nil, nil
	}
	d, err := f.details(ctx, t.uuid, refresh)
	if err != nil {
		return nil, nil, err
	}
	if t.ops == "" {
		return f.fileXattrs(d), &d, nil
	}
	out := [][2]string{{XattrUUID, d.UUID}, {XattrCDNURL, f.cdnURL(d.UUID, t.ops)}}
	if v, err := f.variant(ctx, t.uuid, t.ops); err == nil && v.MIME != "" {
		out = append(out, [2]string{XattrMIME, v.MIME})
	}
	return out, nil, nil
}

// Listxattr names a node's extended attributes, from the index alone.
func (f *FS) Listxattr(ctx context.Context, n Node) ([]string, error) {
	f.stats.lists.Add(1)
	attrs, _, err := f.xattrs(ctx, n, false)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(attrs))
	for i, a := range attrs {
		names[i] = a[0]
	}
	return names, nil
}

// Getxattr reads one attribute. Names outside user.uc.*, such as the
// security.capability the kernel asks for on writes, are ENODATA without
// any lookup.
func (f *FS) Getxattr(ctx context.Context, n Node, name string) ([]byte, error) {
	if !strings.HasPrefix(name, xattrNS) {
		return nil, syscall.ENODATA
	}
	f.stats.gets.Add(1)
	attrs, d, err := f.xattrs(ctx, n, true)
	if err != nil {
		return nil, err
	}
	if name == XattrMD5 && d != nil {
		sum, err := f.md5(ctx, *d)
		switch {
		case err != nil:
			return nil, err
		case sum == "":
			return nil, syscall.ENODATA
		}
		return []byte(sum), nil
	}
	for _, a := range attrs {
		if a[0] == name {
			return []byte(a[1]), nil
		}
	}
	return nil, syscall.ENODATA
}

// checkKey validates a metadata key the way the API does; keys with other
// characters do not even route there (the API answers 404).
func checkKey(key string) error {
	switch {
	case utf8.RuneCountInString(key) > maxKeyLen:
		return syscall.ENAMETOOLONG
	case !keyRe.MatchString(key):
		return syscall.EINVAL
	case key == index.TreePathKey:
		// The tree path moves the file in tree/; mv does that, and keeps
		// files other clients uploaded out of reach without --allow-foreign.
		return syscall.EPERM
	}
	return nil
}

func checkValue(v []byte) error {
	switch {
	case len(v) == 0 || !utf8.Valid(v):
		return syscall.EINVAL
	case utf8.RuneCount(v) > maxValueLen:
		return syscall.E2BIG
	}
	return nil
}

// parseTags reads user.uc.tags: tags separated by commas, which a tag
// cannot contain, normalised as the API does. An empty value clears them.
func parseTags(v []byte) ([]string, error) {
	tags := []string{}
	if !utf8.Valid(v) {
		return nil, syscall.EINVAL
	}
	for _, t := range strings.Split(string(v), ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		switch {
		case t == "" || slices.Contains(tags, t):
			continue
		case utf8.RuneCountInString(t) > maxTagLen:
			return nil, syscall.ENAMETOOLONG
		case !tagRe.MatchString(t):
			return nil, syscall.EINVAL
		}
		tags = append(tags, t)
	}
	if len(tags) > maxTags {
		return nil, syscall.ENOSPC
	}
	return tags, nil
}

// checkFlags applies XATTR_CREATE and XATTR_REPLACE.
func checkFlags(flags int, exists bool) error {
	switch {
	case flags&XattrCreate != 0 && exists:
		return syscall.EEXIST
	case flags&XattrReplace != 0 && !exists:
		return syscall.ENODATA
	}
	return nil
}

// Setxattr sets user.uc.meta.<key> with one metadata PUT, or user.uc.tags
// with one tags PUT, and records the result in the index. On a file not
// uploaded yet the value goes into its upload form instead. Other user.uc.*
// attributes are read-only; on a file not uploaded yet they are ignored,
// as its upload decides them, so that cp --preserve=xattr and rsync -X of
// a file from the mount do not fail on them.
func (f *FS) Setxattr(ctx context.Context, n Node, name string, value []byte, flags int) error {
	if f.w == nil {
		return syscall.EROFS
	}
	if !strings.HasPrefix(name, xattrNS) {
		return syscall.ENOTSUP
	}
	f.stats.sets.Add(1)
	key, isMeta := strings.CutPrefix(name, XattrMeta)
	var tags []string
	var check error
	switch {
	case isMeta:
		if check = checkKey(key); check == nil {
			check = checkValue(value)
		}
	case name == XattrTags:
		tags, check = parseTags(value)
	default:
		check = syscall.EPERM
	}
	t, ok, err := f.target(ctx, n)
	switch {
	case errors.Is(check, syscall.EPERM) && err == nil && t.spool != nil:
		return nil
	case check != nil:
		return check
	case err != nil:
		return err
	case !ok:
		return syscall.ENOTSUP
	case t.ops != "":
		return syscall.EPERM
	case t.spool != nil:
		return t.spool.Queue(func(q *index.Queued) error {
			if !isMeta {
				if err := checkFlags(flags, q.Tags != nil && len(*q.Tags) > 0); err != nil {
					return err
				}
				q.Tags = &tags
				return nil
			}
			_, exists := q.Meta[key]
			if err := checkFlags(flags, exists); err != nil {
				return err
			}
			q.Meta[key] = string(value)
			return nil
		})
	}
	d, err := f.details(ctx, t.uuid, flags != 0)
	if err != nil {
		return err
	}
	if !isMeta {
		if err := checkFlags(flags, len(d.Tags) > 0); err != nil {
			return err
		}
		stored, err := f.w.API.SetTags(ctx, t.uuid, tags)
		if err != nil {
			slog.Warn("set tags", "uuid", t.uuid, "err", err)
			return api.Errno(err)
		}
		if err := f.ix.SetTags(ctx, t.uuid, stored); err != nil {
			return f.ioErr("record tags", err)
		}
		f.Touch()
		return nil
	}
	_, exists := d.Metadata[key]
	if err := checkFlags(flags, exists); err != nil {
		return err
	}
	if err := f.w.API.SetMetadata(ctx, t.uuid, key, string(value)); err != nil {
		slog.Warn("set metadata", "uuid", t.uuid, "key", key, "err", err)
		return api.Errno(err)
	}
	if err := f.ix.SetMeta(ctx, t.uuid, key, string(value)); err != nil {
		return f.ioErr("record metadata", err)
	}
	return nil
}

// Removexattr deletes a metadata key with one DELETE, or clears the tags.
// The API answers DELETE of a missing key with 204, so whether the key
// exists is decided from the file's current info.
func (f *FS) Removexattr(ctx context.Context, n Node, name string) error {
	if f.w == nil {
		return syscall.EROFS
	}
	if !strings.HasPrefix(name, xattrNS) {
		return syscall.ENODATA
	}
	f.stats.removes.Add(1)
	key, isMeta := strings.CutPrefix(name, XattrMeta)
	switch {
	case isMeta:
		if key == index.TreePathKey {
			return syscall.EPERM
		}
	case name != XattrTags:
		return syscall.EPERM
	}
	t, ok, err := f.target(ctx, n)
	switch {
	case err != nil:
		return err
	case !ok:
		return syscall.ENODATA
	case t.ops != "":
		return syscall.EPERM
	case t.spool != nil:
		return t.spool.Queue(func(q *index.Queued) error {
			if !isMeta {
				if q.Tags == nil || len(*q.Tags) == 0 {
					return syscall.ENODATA
				}
				q.Tags = &[]string{}
				return nil
			}
			if _, ok := q.Meta[key]; !ok {
				return syscall.ENODATA
			}
			delete(q.Meta, key)
			return nil
		})
	}
	d, err := f.details(ctx, t.uuid, true)
	if err != nil {
		return err
	}
	if !isMeta {
		if len(d.Tags) == 0 {
			return syscall.ENODATA
		}
		stored, err := f.w.API.SetTags(ctx, t.uuid, []string{})
		if err != nil {
			slog.Warn("clear tags", "uuid", t.uuid, "err", err)
			return api.Errno(err)
		}
		if err := f.ix.SetTags(ctx, t.uuid, stored); err != nil {
			return f.ioErr("record tags", err)
		}
		f.Touch()
		return nil
	}
	if _, ok := d.Metadata[key]; !ok {
		return syscall.ENODATA
	}
	if err := f.w.API.DeleteMetadata(ctx, t.uuid, key); err != nil {
		slog.Warn("delete metadata", "uuid", t.uuid, "key", key, "err", err)
		return api.Errno(err)
	}
	if err := f.ix.DeleteMeta(ctx, t.uuid, key); err != nil {
		return f.ioErr("record metadata", err)
	}
	return nil
}
