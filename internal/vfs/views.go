package vfs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/index"
)

// Views a mount can show at its root. Each is a directory of that name.
const (
	ViewFiles  = "files"
	ViewTree   = "tree"
	ViewByDate = "by-date"
	ViewTags   = "tags"
	ViewGroups = "groups"
)

// AllViews is every view in root listing order; DefaultViews are mounted
// unless --views says otherwise. tags/ and groups/ are opt-in: groups/
// costs a listing every sweep and one request per group opened.
var (
	AllViews     = []string{ViewFiles, ViewTree, ViewByDate, ViewTags, ViewGroups}
	DefaultViews = []string{ViewFiles, ViewTree, ViewByDate}
)

// Remote is the part of api.Client the filesystem reads through outside
// writes, also on a read-only mount.
type Remote interface {
	Info(ctx context.Context, uuid string) (api.File, error)
	GroupInfo(ctx context.Context, id string) (api.Group, error)
}

// Options configure what a mount shows beyond the index.
type Options struct {
	// Views are the views the root shows; nil means DefaultViews.
	Views []string
	// MetaTTL is how old a file's info may be before reading its extended
	// attributes or resolving it fetches the info again.
	MetaTTL time.Duration
	// DateZone is the time zone by-date/ places upload times in.
	DateZone *time.Location
	// CDNBase is the CDN base URL of user.uc.cdn_url and `ucfs resolve`.
	CDNBase string
	// Remote refreshes file info and reads groups; without it the
	// filesystem answers from the index alone.
	Remote Remote
}

// DefaultMetaTTL is how long file info read from the API counts as current.
const DefaultMetaTTL = 5 * time.Minute

// Configure applies options; a filesystem never configured shows the
// default views, places dates in the local zone, and never asks the API.
func (f *FS) Configure(o Options) {
	if o.Views == nil {
		o.Views = DefaultViews
	}
	if o.MetaTTL == 0 {
		o.MetaTTL = DefaultMetaTTL
	}
	if o.DateZone == nil {
		o.DateZone = time.Local
	}
	f.views = map[string]bool{}
	for _, v := range o.Views {
		f.views[v] = true
	}
	f.opts = o
}

// Views returns the views the root shows, in listing order.
func (f *FS) Views() []string {
	var out []string
	for _, v := range AllViews {
		if f.views[v] {
			out = append(out, v)
		}
	}
	return out
}

// viewEntry is the entry of a view directory at a mount path.
func (f *FS) viewEntry(p string) Entry {
	return Entry{Name: p[strings.LastIndexByte(p, '/')+1:], Node: Node{Kind: KindViewDir, Ops: p}, Attr: f.dirAttr(dirTTL)}
}

// rootView is the entry of a view at the root, if it is mounted.
func (f *FS) rootView(name string) (Entry, bool) {
	if !f.views[name] {
		return Entry{}, false
	}
	switch name {
	case ViewFiles:
		return Entry{Name: name, Node: Node{Kind: KindFilesDir}, Attr: f.writableDirAttr(dirTTL)}, true
	case ViewTree:
		return Entry{Name: name, Node: Node{Kind: KindTreeDir, Ops: "/"}, Attr: f.writableDirAttr(dirTTL)}, true
	case ViewByDate, ViewTags, ViewGroups:
		return f.viewEntry(name), true
	}
	return Entry{}, false
}

func (f *FS) lookupView(ctx context.Context, dir, name string) (Entry, error) {
	parts := strings.Split(dir, "/")
	switch parts[0] {
	case ViewByDate:
		return f.lookupDate(ctx, parts[1:], name)
	case ViewTags:
		return f.lookupTag(ctx, parts[1:], name)
	case ViewGroups:
		return f.lookupGroup(ctx, parts[1:], name)
	}
	return Entry{}, syscall.ENOENT
}

func (f *FS) readView(ctx context.Context, dir string) ([]Entry, error) {
	parts := strings.Split(dir, "/")
	switch parts[0] {
	case ViewByDate:
		return f.readDate(ctx, parts[1:])
	case ViewTags:
		return f.readTag(ctx, parts[1:])
	case ViewGroups:
		return f.readGroup(ctx, parts[1:])
	}
	return nil, syscall.ENOENT
}

// viewDirAttr checks that a view directory still exists by walking to it.
func (f *FS) viewDirAttr(ctx context.Context, p string) (Attr, error) {
	e, err := f.Walk(ctx, p)
	if err != nil {
		return Attr{}, err
	}
	return e.Attr, nil
}

// dateSpan is the period a by-date directory stands for: a year, a month or
// a day in the configured zone. Components must be zero-padded and valid.
func (f *FS) dateSpan(parts []string) (from, to time.Time, ok bool) {
	widths := []int{4, 2, 2}
	nums := []int{0, 1, 1}
	if len(parts) == 0 || len(parts) > 3 {
		return from, to, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || len(p) != widths[i] || n < 1 {
			return from, to, false
		}
		nums[i] = n
	}
	from = time.Date(nums[0], time.Month(nums[1]), nums[2], 0, 0, 0, 0, f.opts.DateZone)
	if from.Year() != nums[0] || int(from.Month()) != nums[1] || from.Day() != nums[2] {
		return from, to, false
	}
	switch len(parts) {
	case 1:
		to = from.AddDate(1, 0, 0)
	case 2:
		to = from.AddDate(0, 1, 0)
	default:
		to = from.AddDate(0, 0, 1)
	}
	return from, to, true
}

// datePath is where a file shows in by-date/.
func (f *FS) datePath(file index.File) []string {
	t := file.UploadedAt.In(f.opts.DateZone)
	return []string{ViewByDate, fmt.Sprintf("%04d", t.Year()), fmt.Sprintf("%02d", int(t.Month())),
		fmt.Sprintf("%02d", t.Day()), DisplayName(file)}
}

func (f *FS) lookupDate(ctx context.Context, parts []string, name string) (Entry, error) {
	dir := strings.Join(append([]string{ViewByDate}, parts...), "/")
	if len(parts) < 3 {
		from, to, ok := f.dateSpan(append(parts, name))
		if !ok {
			return Entry{}, syscall.ENOENT
		}
		has, err := f.ix.UploadedBetween(ctx, from, to)
		switch {
		case err != nil:
			return Entry{}, f.ioErr("by-date lookup", err)
		case !has:
			return Entry{}, syscall.ENOENT
		}
		return f.viewEntry(dir + "/" + name), nil
	}
	from, to, ok := f.dateSpan(parts)
	uuid, hasName, isUUID := parseName(name)
	if !ok || !isUUID || !hasName {
		return Entry{}, syscall.ENOENT
	}
	file, err := f.file(ctx, uuid)
	if err != nil {
		return Entry{}, err
	}
	if file.UploadedAt.Before(from) || !file.UploadedAt.Before(to) || !sameName(name, DisplayName(file)) {
		return Entry{}, syscall.ENOENT
	}
	return Entry{Name: name, Node: Node{Kind: KindDateFile, UUID: uuid}, Attr: fileAttr(file)}, nil
}

// readDate lists years, months or days that have uploads, each found with
// one index probe, or the files of a day.
func (f *FS) readDate(ctx context.Context, parts []string) ([]Entry, error) {
	dir := strings.Join(append([]string{ViewByDate}, parts...), "/")
	if len(parts) == 3 {
		from, to, ok := f.dateSpan(parts)
		if !ok {
			return nil, syscall.ENOENT
		}
		files, err := f.ix.FilesBetween(ctx, from, to)
		if err != nil {
			return nil, f.ioErr("by-date listing", err)
		}
		out := make([]Entry, len(files))
		for i, file := range files {
			out[i] = Entry{Name: DisplayName(file), Node: Node{Kind: KindDateFile, UUID: file.UUID}, Attr: fileAttr(file)}
		}
		return out, nil
	}
	var names []string
	if len(parts) == 0 {
		first, last, ok, err := f.ix.UploadedSpan(ctx)
		if err != nil {
			return nil, f.ioErr("by-date listing", err)
		}
		if !ok {
			return nil, nil
		}
		for y := first.In(f.opts.DateZone).Year(); y <= last.In(f.opts.DateZone).Year(); y++ {
			names = append(names, fmt.Sprintf("%04d", y))
		}
	} else {
		n := 12
		if len(parts) == 2 {
			from, _, ok := f.dateSpan(parts)
			if !ok {
				return nil, syscall.ENOENT
			}
			n = from.AddDate(0, 1, -1).Day()
		}
		for i := 1; i <= n; i++ {
			names = append(names, fmt.Sprintf("%02d", i))
		}
	}
	var out []Entry
	for _, name := range names {
		from, to, ok := f.dateSpan(append(parts, name))
		if !ok {
			continue
		}
		has, err := f.ix.UploadedBetween(ctx, from, to)
		if err != nil {
			return nil, f.ioErr("by-date listing", err)
		}
		if has {
			out = append(out, f.viewEntry(dir+"/"+name))
		}
	}
	return out, nil
}

// tagName reports whether a tag can be a directory name: the API allows
// letters, digits, "-", "_" and ".", so only "." and ".." cannot.
func tagName(tag string) bool { return tag != "" && tag != "." && tag != ".." }

func (f *FS) lookupTag(ctx context.Context, parts []string, name string) (Entry, error) {
	switch len(parts) {
	case 0:
		if !tagName(name) {
			return Entry{}, syscall.ENOENT
		}
		has, err := f.ix.HasTag(ctx, name)
		switch {
		case err != nil:
			return Entry{}, f.ioErr("tag lookup", err)
		case !has:
			return Entry{}, syscall.ENOENT
		}
		return f.viewEntry(ViewTags + "/" + name), nil
	case 1:
		uuid, hasName, ok := parseName(name)
		if !ok || !hasName {
			return Entry{}, syscall.ENOENT
		}
		d, err := f.details(ctx, uuid, false)
		if err != nil {
			return Entry{}, err
		}
		if !slices.Contains(d.Tags, parts[0]) || !sameName(name, DisplayName(d.File)) {
			return Entry{}, syscall.ENOENT
		}
		return f.taggedEntry(parts[0], d.File), nil
	}
	return Entry{}, syscall.ENOENT
}

// taggedEntry is a file in tags/<tag>/. Its attributes are kept briefly: a
// tag can be removed at any time.
func (f *FS) taggedEntry(tag string, file index.File) Entry {
	name := DisplayName(file)
	a := fileAttr(file)
	a.TTL = dirTTL
	return Entry{Name: name, Node: Node{Kind: KindViewFile, UUID: file.UUID, Ops: ViewTags + "/" + tag + "/" + name}, Attr: a}
}

func (f *FS) readTag(ctx context.Context, parts []string) ([]Entry, error) {
	if len(parts) == 0 {
		tags, err := f.ix.Tags(ctx)
		if err != nil {
			return nil, f.ioErr("tags listing", err)
		}
		var out []Entry
		for _, t := range tags {
			if tagName(t) {
				out = append(out, f.viewEntry(ViewTags+"/"+t))
			}
		}
		return out, nil
	}
	files, err := f.ix.TaggedFiles(ctx, parts[0])
	if err != nil {
		return nil, f.ioErr("tag listing", err)
	}
	out := make([]Entry, len(files))
	for i, file := range files {
		out[i] = f.taggedEntry(parts[0], file)
	}
	return out, nil
}

// isGroupID matches <uuid>~<count>, the form of a group id.
func isGroupID(s string) bool {
	uuid, n, ok := strings.Cut(s, "~")
	if !ok || len(uuid) != uuidLen || !isUUID(uuid) {
		return false
	}
	c, err := strconv.Atoi(n)
	return err == nil && c > 0 && strconv.Itoa(c) == n
}

// group returns a group with its members, reading them from the API the
// first time. A group not synced yet is read too, so a group created a
// moment ago can be opened by id before the next group sync lists it.
func (f *FS) group(ctx context.Context, id string) (index.Group, error) {
	g, err := f.ix.Group(ctx, id)
	switch {
	case err == nil && g.Fetched:
		return g, nil
	case err != nil && !errors.Is(err, index.ErrNotFound):
		return g, f.ioErr("group lookup", err)
	case f.opts.Remote == nil:
		return g, syscall.ENOENT
	}
	if _, err, _ := f.groupFetch.Do(id, func() (any, error) { return nil, f.fetchGroup(ctx, id) }); err != nil {
		return g, err
	}
	g, err = f.ix.Group(ctx, id)
	if err != nil {
		return g, syscall.ENOENT
	}
	return g, nil
}

func (f *FS) fetchGroup(ctx context.Context, id string) error {
	f.stats.groupFetches.Add(1)
	g, err := f.opts.Remote.GroupInfo(context.WithoutCancel(ctx), id)
	switch {
	case api.IsNotFound(err):
		if err := f.ix.DeleteGroup(ctx, id); err != nil {
			slog.Warn("forget group", "id", id, "err", err)
		}
		return syscall.ENOENT
	case err != nil:
		slog.Warn("group info", "id", id, "err", err)
		return api.Errno(err)
	}
	if err := f.ix.PutGroup(ctx, g, f.now()); err != nil {
		return f.ioErr("record group", err)
	}
	f.Touch()
	return nil
}

// memberName is a group member's name: its index in the group, zero-padded
// to the width of the last index so names sort in group order, and its
// original name. Indexes count from 0, as the CDN's /nth/ does.
func memberName(i, count int, file index.File) string {
	return fmt.Sprintf("%0*d_%s", len(strconv.Itoa(max(count-1, 0))), i, baseName(file))
}

func (f *FS) memberEntry(id string, i int, g index.Group, file index.File) Entry {
	name := memberName(i, len(g.Files), file)
	return Entry{Name: name, Node: Node{Kind: KindViewFile, UUID: file.UUID, Ops: ViewGroups + "/" + id + "/" + name}, Attr: fileAttr(file)}
}

func (f *FS) lookupGroup(ctx context.Context, parts []string, name string) (Entry, error) {
	switch len(parts) {
	case 0:
		if !isGroupID(name) {
			return Entry{}, syscall.ENOENT
		}
		if _, err := f.ix.Group(ctx, name); err != nil {
			if _, err := f.group(ctx, name); err != nil {
				return Entry{}, err
			}
		}
		return f.viewEntry(ViewGroups + "/" + name), nil
	case 1:
		idx, _, ok := strings.Cut(name, "_")
		i, err := strconv.Atoi(idx)
		if !ok || err != nil || i < 0 {
			return Entry{}, syscall.ENOENT
		}
		g, err := f.group(ctx, parts[0])
		if err != nil {
			return Entry{}, err
		}
		if i >= len(g.Files) || g.Files[i] == "" {
			return Entry{}, syscall.ENOENT
		}
		file, err := f.file(ctx, g.Files[i])
		if err != nil {
			return Entry{}, err
		}
		e := f.memberEntry(parts[0], i, g, file)
		if !sameName(name, e.Name) {
			return Entry{}, syscall.ENOENT
		}
		return e, nil
	}
	return Entry{}, syscall.ENOENT
}

func (f *FS) readGroup(ctx context.Context, parts []string) ([]Entry, error) {
	if len(parts) == 0 {
		groups, err := f.ix.Groups(ctx)
		if err != nil {
			return nil, f.ioErr("groups listing", err)
		}
		out := make([]Entry, len(groups))
		for i, g := range groups {
			out[i] = f.viewEntry(ViewGroups + "/" + g.ID)
		}
		return out, nil
	}
	g, err := f.group(ctx, parts[0])
	if err != nil {
		return nil, err
	}
	var out []Entry
	for i, uuid := range g.Files {
		if uuid == "" {
			continue
		}
		file, err := f.file(ctx, uuid)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, f.memberEntry(parts[0], i, g, file))
	}
	return out, nil
}
