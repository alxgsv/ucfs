package vfs

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"syscall"
	"time"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/index"
)

const (
	// VariantsDir is the directory in files/<uuid>/ that holds the variants
	// view, named after the URL API's delimiter.
	VariantsDir = "-"
	opsRoot     = VariantsDir + "/"
	// variantTTL is how long a HEAD answer for a variant is trusted.
	// Originals never change, but a variant's ETag is weak: the CDN may
	// serve other bytes for the same URL later.
	variantTTL = 24 * time.Hour
	// badTTL keeps a 400 as long as the CDN lets its error page be cached.
	badTTL = time.Hour
	// missingTTL only spans the several walks one NFS request makes; the
	// API, not the CDN, decides that a file is gone.
	missingTTL  = 10 * time.Second
	maxFailures = 4096
	maxNodeOps  = 1 << 16
)

type failure struct {
	errno syscall.Errno
	until time.Time
}

// atOpRoot reports whether ops ends in "-/", where an operation name follows.
func atOpRoot(ops string) bool { return ops == opsRoot || strings.HasSuffix(ops, "/"+opsRoot) }

func (f *FS) lookupFileDir(ctx context.Context, uuid, name string) (Entry, error) {
	file, err := f.file(ctx, uuid)
	if err != nil {
		return Entry{}, err
	}
	switch {
	case name == VariantsDir:
		return Entry{Name: name, Node: Node{Kind: KindVariantDir, UUID: uuid, Ops: opsRoot}, Attr: f.dirAttr(fileTTL)}, nil
	case sameName(name, baseName(file)):
		return Entry{Name: name, Node: Node{Kind: KindFile, UUID: uuid}, Attr: fileAttr(file)}, nil
	}
	return Entry{}, syscall.ENOENT
}

// lookupVariant resolves a name in the variants view. After "-" only an
// operation from the table may follow. Elsewhere "-" starts the next
// operation, the default entry is the variant, and any other name is an
// argument directory, except hidden names: probes for .DS_Store or ._*
// files must not turn into directories and then into CDN requests.
func (f *FS) lookupVariant(ctx context.Context, dir Node, name string) (Entry, error) {
	file, err := f.file(ctx, dir.UUID)
	if err != nil {
		return Entry{}, err
	}
	sub := func(ops string) (Entry, error) {
		return Entry{Name: name, Node: Node{Kind: KindVariantDir, UUID: dir.UUID, Ops: ops}, Attr: f.dirAttr(fileTTL)}, nil
	}
	switch {
	case atOpRoot(dir.Ops):
		if !cdn.IsOperation(name) {
			return Entry{}, syscall.ENOENT
		}
		return sub(dir.Ops + name + "/")
	case name == VariantsDir:
		return sub(dir.Ops + opsRoot)
	case sameName(name, leafName(file, dir.Ops)):
		e, err := f.variantEntry(ctx, file, dir.Ops)
		e.Name = name
		return e, err
	case name == "" || name[0] == '.':
		return Entry{}, syscall.ENOENT
	}
	return sub(dir.Ops + name + "/")
}

// readVariantDir lists nothing after "-", so a recursive walk of a file's
// directory costs no CDN request, and the variant elsewhere, which costs the
// one HEAD a stat of it would.
func (f *FS) readVariantDir(ctx context.Context, dir Node) ([]Entry, error) {
	file, err := f.file(ctx, dir.UUID)
	if err != nil || atOpRoot(dir.Ops) {
		return nil, err
	}
	e, err := f.variantEntry(ctx, file, dir.Ops)
	if err != nil {
		return nil, err
	}
	return []Entry{e}, nil
}

func (f *FS) variantEntry(ctx context.Context, file index.File, ops string) (Entry, error) {
	v, err := f.variant(ctx, file.UUID, ops)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Name: leafName(file, ops), Node: Node{Kind: KindVariant, UUID: file.UUID, Ops: ops}, Attr: f.variantAttr(v)}, nil
}

// variantAttr dates a variant by when its current content appeared, so NFS
// clients drop cached pages when it changes, and lets kernels cache the
// attributes no longer than the HEAD answer stays trusted.
func (f *FS) variantAttr(v index.Variant) Attr {
	ttl := min(fileTTL, v.CheckedAt.Add(variantTTL).Sub(f.now()))
	return Attr{Mode: 0o444, Size: v.Size, Mtime: v.ChangedAt, TTL: max(ttl, 0)}
}

// variant returns what the CDN says about a variant: from the index while the
// last HEAD is younger than variantTTL, otherwise from a new HEAD that
// concurrent callers share.
func (f *FS) variant(ctx context.Context, uuid, ops string) (index.Variant, error) {
	v, err := f.ix.Variant(ctx, uuid, ops)
	found := err == nil
	switch {
	case found && f.now().Sub(v.CheckedAt) < variantTTL:
		return v, nil
	case err != nil && !errors.Is(err, index.ErrNotFound):
		slog.Error("variant lookup", "uuid", uuid, "ops", ops, "err", err)
		return v, syscall.EIO
	}
	key := uuid + "/" + ops
	if err := f.recentFailure(key); err != nil {
		return v, err
	}
	res, err, _ := f.heads.Do(key, func() (any, error) {
		return f.head(context.WithoutCancel(ctx), v, found)
	})
	if err != nil {
		return v, err
	}
	return res.(index.Variant), nil
}

// head asks the CDN about a variant and records the answer. When the size or
// ETag changed since the last HEAD, the blocks cached for the old content
// are dropped; new blocks go under the new ETag either way.
func (f *FS) head(ctx context.Context, old index.Variant, found bool) (index.Variant, error) {
	key := old.UUID + "/" + old.Ops
	info, err := f.cdn.Head(ctx, old.UUID, old.Ops)
	var bad *cdn.BadRequestError
	switch {
	case errors.As(err, &bad):
		slog.Info("CDN rejected a variant", "uuid", old.UUID, "ops", old.Ops, "message", bad.Message)
		return old, f.fail(key, syscall.EINVAL, badTTL)
	case errors.Is(err, cdn.ErrNotFound):
		f.notFound(old.UUID)
		return old, f.fail(key, syscall.ENOENT, missingTTL)
	case err != nil:
		slog.Warn("variant HEAD", "uuid", old.UUID, "ops", old.Ops, "err", err)
		return old, syscall.EIO
	}
	now := f.now()
	v := index.Variant{UUID: old.UUID, Ops: old.Ops, Size: info.Size, MIME: info.MIME, ETag: info.ETag,
		CheckedAt: now, ChangedAt: now}
	switch {
	case found && v.Size == old.Size && v.ETag == old.ETag:
		v.ChangedAt = old.ChangedAt
	case found:
		slog.Info("variant changed", "uuid", v.UUID, "ops", v.Ops, "size", v.Size, "etag", v.ETag)
		f.cache.Drop(cache.Key{UUID: old.UUID, Ops: old.Ops, Version: old.ETag})
	}
	if err := f.ix.PutVariant(ctx, v); err != nil {
		slog.Warn("store variant", "uuid", v.UUID, "ops", v.Ops, "err", err)
	}
	return v, nil
}

func (f *FS) fail(key string, errno syscall.Errno, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.failed) >= maxFailures {
		clear(f.failed)
	}
	f.failed[key] = failure{errno: errno, until: f.now().Add(ttl)}
	return errno
}

func (f *FS) recentFailure(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fl, ok := f.failed[key]
	switch {
	case !ok:
		return nil
	case f.now().After(fl.until):
		delete(f.failed, key)
		return nil
	}
	return fl.errno
}

// Remember records the operations path of a variants view node in the index,
// so NodeByID still resolves the node's id after a restart.
func (f *FS) Remember(ctx context.Context, n Node) {
	if n.Ops == "" {
		return
	}
	id := n.ID()
	f.mu.Lock()
	_, known := f.nodeOps[id]
	if !known {
		if len(f.nodeOps) >= maxNodeOps {
			clear(f.nodeOps)
		}
		f.nodeOps[id] = n.Ops
	}
	f.mu.Unlock()
	if known {
		return
	}
	if err := f.ix.PutNodeOps(ctx, id, n.UUID, n.Ops); err != nil {
		slog.Warn("record node", "uuid", n.UUID, "ops", n.Ops, "err", err)
	}
}

// NodeByID rebuilds a node from what a file handle carries: its kind, uuid
// and id. Variants view nodes also need the operations path that Remember
// stored. A node that cannot be rebuilt is ESTALE.
func (f *FS) NodeByID(ctx context.Context, kind Kind, uuid string, id uint64) (Node, error) {
	n := Node{Kind: kind, UUID: uuid}
	if kind == KindVariantDir || kind == KindVariant {
		f.mu.Lock()
		ops, ok := f.nodeOps[id]
		f.mu.Unlock()
		if !ok {
			var err error
			if _, ops, err = f.ix.NodeOps(ctx, id); err != nil {
				return Node{}, syscall.ESTALE
			}
		}
		n.Ops = ops
	}
	if n.ID() != id {
		return Node{}, syscall.ESTALE
	}
	return n, nil
}
