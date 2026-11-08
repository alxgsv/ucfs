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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/index"
)

type Kind uint8

const (
	KindRoot Kind = iota + 1
	KindFilesDir
	KindFile
	// KindFileDir is files/<uuid>/, resolvable but not listed; Phase 2 puts
	// variants under it.
	KindFileDir
)

type Node struct {
	Kind Kind
	UUID string
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
	}
	return h.Sum64()
}

func (n Node) IsDir() bool { return n.Kind != KindFile }

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

type FS struct {
	ix    *index.Index
	cache *cache.Cache
	// dirMtime moves whenever the index changes, so NFS clients that compare
	// directory mtimes re-read the listing.
	dirMtime atomic.Int64
	missing  func(uuid string)
}

// New builds the filesystem. missing is called, in its own goroutine, when
// the CDN answers 404 for a file the index has.
func New(ix *index.Index, c *cache.Cache, missing func(uuid string)) *FS {
	f := &FS{ix: ix, cache: c, missing: missing}
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
	case KindFileDir:
		if _, err := f.file(ctx, n.UUID); err != nil {
			return Attr{}, err
		}
		return f.dirAttr(fileTTL), nil
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
	case KindFile:
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
		if _, err := f.file(ctx, dir.UUID); err != nil {
			return nil, err
		}
		return nil, nil
	case KindFile:
		return nil, syscall.ENOTDIR
	}
	return nil, syscall.ENOENT
}

// ReadAt reads file content. A CDN 404 hands the uuid to the missing hook,
// which decides from the API whether the file is really gone.
func (f *FS) ReadAt(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	if n.Kind != KindFile {
		return 0, syscall.EISDIR
	}
	file, err := f.file(ctx, n.UUID)
	if err != nil {
		return 0, err
	}
	got, err := f.cache.ReadAt(ctx, cache.Key{UUID: n.UUID}, file.Size, p, off)
	switch {
	case err == nil || errors.Is(err, io.EOF):
		return got, err
	case errors.Is(err, cdn.ErrNotFound):
		f.cache.Forget(cache.Key{UUID: n.UUID})
		if f.missing != nil {
			go f.missing(n.UUID)
		}
		return got, syscall.ENOENT
	default:
		slog.Error("read", "uuid", n.UUID, "off", off, "err", err)
		return got, syscall.EIO
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
	}
	return nil, syscall.ENOENT
}
