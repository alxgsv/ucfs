// Package vfs is the platform-neutral filesystem model that the NFS and FUSE
// front-ends adapt. Every metadata operation is answered from the index;
// only ReadAt reaches the CDN, through the block cache.
package vfs

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/index"
)

type Kind uint8

const (
	KindRoot Kind = iota + 1
	KindFilesDir
	KindFile
	// KindFileDir is files/<uuid>/, resolvable but not listed in files/. It
	// holds the original under its own name and the variants directory "-".
	KindFileDir
	// KindVariantDir is a directory of the variants view: "-" itself, an
	// operation or an argument.
	KindVariantDir
	// KindVariant is the file in an operation or argument directory.
	KindVariant
)

// HasUUID reports whether nodes of this kind belong to one file.
func (k Kind) HasUUID() bool { return k != KindRoot && k != KindFilesDir }

type Node struct {
	Kind Kind
	UUID string
	// Ops is the operations path of a variants view node in the form the CDN
	// URL takes: "-/" for files/<uuid>/-, and "-/resize/1200x/" for that
	// argument directory and for the variant inside it.
	Ops string
}

// ID is the node's stable 64-bit identity, used as inode and NFS file id.
func (n Node) ID() uint64 {
	h := fnv.New64a()
	switch n.Kind {
	case KindRoot:
		_, _ = h.Write([]byte("root"))
	case KindFilesDir:
		_, _ = h.Write([]byte("files"))
	case KindFile:
		_, _ = h.Write([]byte("files\x00" + n.UUID + "\x00"))
	case KindFileDir:
		_, _ = h.Write([]byte("files\x00" + n.UUID + "\x00/"))
	case KindVariantDir:
		_, _ = h.Write([]byte("files\x00" + n.UUID + "\x00" + n.Ops))
	case KindVariant:
		_, _ = h.Write([]byte("files\x00" + n.UUID + "\x00" + n.Ops + "\x00"))
	}
	return h.Sum64()
}

func (n Node) IsDir() bool { return n.Kind != KindFile && n.Kind != KindVariant }

// Attr carries attributes plus how long a kernel may cache them.
type Attr struct {
	Mode  os.FileMode
	Size  int64
	Mtime time.Time
	TTL   time.Duration
}

type Entry struct {
	Name string
	Node Node
	Attr Attr
}

const (
	FilesDir = "files"
	// File content never changes, so attributes can be cached for long.
	fileTTL = time.Hour
	// Directory listings change with every sync.
	dirTTL = 5 * time.Second
)

// Header asks the CDN about a variant; cdn.Client implements it.
type Header interface {
	Head(ctx context.Context, uuid, ops string) (cdn.Info, error)
}

type FS struct {
	ix    *index.Index
	cache *cache.Cache
	cdn   Header
	now   func() time.Time
	// dirMtime moves whenever the index changes, so NFS clients that compare
	// directory mtimes re-read the listing.
	dirMtime atomic.Int64
	missing  func(uuid string)

	// heads shares one HEAD among concurrent lookups of a variant.
	heads singleflight.Group
	mu    sync.Mutex
	// failed holds recent HEAD failures by uuid and ops.
	failed map[string]failure
	// nodeOps holds the operations paths of node ids already in the index.
	nodeOps map[uint64]string
}

// New builds the filesystem. missing is called, in its own goroutine, when
// the CDN answers 404 for a file the index has.
func New(ix *index.Index, c *cache.Cache, head Header, missing func(uuid string)) *FS {
	f := &FS{ix: ix, cache: c, cdn: head, now: time.Now, missing: missing,
		failed: map[string]failure{}, nodeOps: map[uint64]string{}}
	f.Touch()
	return f
}

// Touch records that the index changed.
func (f *FS) Touch() { f.dirMtime.Store(time.Now().UnixNano()) }

func (f *FS) Root() Node { return Node{Kind: KindRoot} }

func (f *FS) dirAttr(ttl time.Duration) Attr {
	return Attr{Mode: os.ModeDir | 0o555, Mtime: time.Unix(0, f.dirMtime.Load()), TTL: ttl}
}

func fileAttr(file index.File) Attr {
	return Attr{Mode: 0o444, Size: file.Size, Mtime: file.UploadedAt, TTL: fileTTL}
}

func (f *FS) Getattr(ctx context.Context, n Node) (Attr, error) {
	switch n.Kind {
	case KindRoot:
		return f.dirAttr(fileTTL), nil
	case KindFilesDir:
		return f.dirAttr(dirTTL), nil
	case KindFile:
		file, err := f.file(ctx, n.UUID)
		if err != nil {
			return Attr{}, err
		}
		return fileAttr(file), nil
	case KindFileDir, KindVariantDir:
		if _, err := f.file(ctx, n.UUID); err != nil {
			return Attr{}, err
		}
		return f.dirAttr(fileTTL), nil
	case KindVariant:
		if _, err := f.file(ctx, n.UUID); err != nil {
			return Attr{}, err
		}
		v, err := f.variant(ctx, n.UUID, n.Ops)
		if err != nil {
			return Attr{}, err
		}
		return f.variantAttr(v), nil
	}
	return Attr{}, syscall.ENOENT
}

// file returns a present file or ENOENT.
func (f *FS) file(ctx context.Context, uuid string) (index.File, error) {
	file, err := f.ix.Get(ctx, uuid)
	switch {
	case errors.Is(err, index.ErrNotFound) || (err == nil && file.Removed):
		return file, syscall.ENOENT
	case err != nil:
		slog.Error("index lookup", "uuid", uuid, "err", err)
		return file, syscall.EIO
	}
	return file, nil
}

func (f *FS) Lookup(ctx context.Context, dir Node, name string) (Entry, error) {
	switch dir.Kind {
	case KindRoot:
		if name == FilesDir {
			n := Node{Kind: KindFilesDir}
			return Entry{Name: name, Node: n, Attr: f.dirAttr(dirTTL)}, nil
		}
	case KindFilesDir:
		uuid, hasName, ok := parseName(name)
		if !ok {
			return Entry{}, syscall.ENOENT
		}
		file, err := f.file(ctx, uuid)
		if err != nil {
			return Entry{}, err
		}
		if !hasName {
			return Entry{Name: name, Node: Node{Kind: KindFileDir, UUID: uuid}, Attr: f.dirAttr(fileTTL)}, nil
		}
		if !sameName(name, DisplayName(file)) {
			return Entry{}, syscall.ENOENT
		}
		return Entry{Name: name, Node: Node{Kind: KindFile, UUID: uuid}, Attr: fileAttr(file)}, nil
	case KindFileDir:
		return f.lookupFileDir(ctx, dir.UUID, name)
	case KindVariantDir:
		return f.lookupVariant(ctx, dir, name)
	case KindFile, KindVariant:
		return Entry{}, syscall.ENOTDIR
	}
	return Entry{}, syscall.ENOENT
}

func (f *FS) ReadDir(ctx context.Context, dir Node) ([]Entry, error) {
	switch dir.Kind {
	case KindRoot:
		return []Entry{{Name: FilesDir, Node: Node{Kind: KindFilesDir}, Attr: f.dirAttr(dirTTL)}}, nil
	case KindFilesDir:
		files, err := f.ix.Files(ctx)
		if err != nil {
			slog.Error("index listing", "err", err)
			return nil, syscall.EIO
		}
		out := make([]Entry, len(files))
		for i, file := range files {
			out[i] = Entry{Name: DisplayName(file), Node: Node{Kind: KindFile, UUID: file.UUID}, Attr: fileAttr(file)}
		}
		return out, nil
	case KindFileDir:
		file, err := f.file(ctx, dir.UUID)
		if err != nil {
			return nil, err
		}
		return []Entry{
			{Name: baseName(file), Node: Node{Kind: KindFile, UUID: file.UUID}, Attr: fileAttr(file)},
			{Name: VariantsDir, Node: Node{Kind: KindVariantDir, UUID: file.UUID, Ops: opsRoot}, Attr: f.dirAttr(fileTTL)},
		}, nil
	case KindVariantDir:
		return f.readVariantDir(ctx, dir)
	case KindFile, KindVariant:
		return nil, syscall.ENOTDIR
	}
	return nil, syscall.ENOENT
}

// ReadAt reads file content. A CDN 404 hands the uuid to the missing hook,
// which decides from the API whether the file is really gone; a CDN 400
// means the operations do not apply to the file.
func (f *FS) ReadAt(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	if n.IsDir() {
		return 0, syscall.EISDIR
	}
	file, err := f.file(ctx, n.UUID)
	if err != nil {
		return 0, err
	}
	k, size := cache.Key{UUID: n.UUID}, file.Size
	if n.Kind == KindVariant {
		v, err := f.variant(ctx, n.UUID, n.Ops)
		if err != nil {
			return 0, err
		}
		k, size = cache.Key{UUID: n.UUID, Ops: n.Ops, Version: v.ETag}, v.Size
	}
	got, err := f.cache.ReadAt(ctx, k, size, p, off)
	var bad *cdn.BadRequestError
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return got, err
	case errors.Is(err, cdn.ErrNotFound):
		f.cache.Forget(k)
		f.notFound(n.UUID)
		return got, syscall.ENOENT
	case errors.As(err, &bad):
		slog.Info("CDN rejected a variant", "uuid", n.UUID, "ops", n.Ops, "message", bad.Message)
		return got, syscall.EINVAL
	default:
		slog.Error("read", "uuid", n.UUID, "ops", n.Ops, "off", off, "err", err)
		return got, syscall.EIO
	}
}

func (f *FS) notFound(uuid string) {
	if f.missing != nil {
		go f.missing(uuid)
	}
}

// Walk resolves a slash-separated path relative to the root.
func (f *FS) Walk(ctx context.Context, p string) (Entry, error) {
	n := f.Root()
	e := Entry{Node: n, Attr: f.dirAttr(fileTTL)}
	for _, name := range strings.Split(path.Clean("/" + p)[1:], "/") {
		if name == "" {
			continue
		}
		var err error
		if e, err = f.Lookup(ctx, e.Node, name); err != nil {
			return Entry{}, err
		}
	}
	return e, nil
}

// Path returns the path components that lead to a node, for front-ends
// that address nodes by path.
func (f *FS) Path(ctx context.Context, n Node) ([]string, error) {
	switch n.Kind {
	case KindRoot:
		return []string{}, nil
	case KindFilesDir:
		return []string{FilesDir}, nil
	case KindFile:
		file, err := f.file(ctx, n.UUID)
		if err != nil {
			return nil, err
		}
		return []string{FilesDir, DisplayName(file)}, nil
	case KindFileDir:
		if _, err := f.file(ctx, n.UUID); err != nil {
			return nil, err
		}
		return []string{FilesDir, n.UUID}, nil
	case KindVariantDir, KindVariant:
		file, err := f.file(ctx, n.UUID)
		if err != nil {
			return nil, err
		}
		p := append([]string{FilesDir, n.UUID}, strings.Split(strings.TrimSuffix(n.Ops, "/"), "/")...)
		if n.Kind == KindVariant {
			p = append(p, leafName(file, n.Ops))
		}
		return p, nil
	}
	return nil, syscall.ENOENT
}
