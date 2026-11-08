// Package vfs is the platform-neutral filesystem model that the NFS and FUSE
// front-ends adapt. Every metadata operation is answered from the index;
// only ReadAt reaches the CDN, through the block cache. Writes go to spools
// and become uploads (write.go).
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

	"github.com/alxgsv/ucfs/internal/api"
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
	// KindTreeDir is tree/ or a directory below it, by path.
	KindTreeDir
	// KindInbox is tree/_inbox, the files without a tree path.
	KindInbox
	// KindTreeFile is a file in tree/ or tree/_inbox, or a name written in
	// files/: an object, whose identity survives its upload, overwrites and
	// renames (index.Object).
	KindTreeFile
	// KindShadow is a local-only file such as .DS_Store, also an object.
	KindShadow
	// KindShadowDir is a local-only directory such as .TemporaryItems.
	KindShadowDir
	// KindUcfsDir is .ucfs at the root; KindJournal is .ucfs/uploads.log.
	KindUcfsDir
	KindJournal
	// KindNeverIndex is .metadata_never_index at the root, which keeps
	// Spotlight away.
	KindNeverIndex
	// KindViewDir is a directory of the by-date, tags or groups view, known
	// by its mount path: "by-date/2026/11", "tags/approved", "groups".
	KindViewDir
	// KindDateFile is a file in by-date/YYYY/MM/DD/, whose place follows
	// from its upload time.
	KindDateFile
	// KindViewFile is a file in tags/<tag>/ or groups/<id>/, known by its
	// mount path, which says which tag or group it was found through.
	KindViewFile
)

// HasUUID reports whether NFS handles of this kind carry a uuid. A tree
// file's is the key of its object, which may be empty.
func (k Kind) HasUUID() bool {
	switch k {
	case KindFile, KindFileDir, KindVariantDir, KindVariant, KindTreeFile, KindDateFile, KindViewFile:
		return true
	}
	return false
}

// ByPath reports whether nodes of this kind are known by their path, which
// the index keeps by node id for NFS handles (FS.Remember).
func (k Kind) ByPath() bool {
	switch k {
	case KindVariantDir, KindVariant, KindTreeDir, KindShadowDir, KindViewDir, KindViewFile:
		return true
	}
	return false
}

type Node struct {
	Kind Kind
	// UUID is the file of a files/ or variants view node, and the key of a
	// tree file's object.
	UUID string
	// Ops is the operations path of a variants view node in the form the CDN
	// URL takes: "-/" for files/<uuid>/-, and "-/resize/1200x/" for that
	// argument directory and for the variant inside it. For a tree
	// directory it is the tree path ("/" for tree/ itself, "/photos" below
	// it), and for a shadow directory the path relative to the mount root.
	Ops string
	// Ino is an object's inode number: tree files and shadow files. A tree
	// or shadow directory rebuilt from a handle handed out before it was
	// renamed keeps the inode number the handle carries.
	Ino uint64
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
	case KindTreeFile, KindShadow:
		return n.Ino
	case KindTreeDir:
		if n.Ino != 0 {
			return n.Ino
		}
		_, _ = h.Write([]byte("tree\x00" + n.Ops))
	case KindInbox:
		_, _ = h.Write([]byte("tree\x00" + InboxDir))
	case KindShadowDir:
		if n.Ino != 0 {
			return n.Ino
		}
		_, _ = h.Write([]byte("shadow\x00" + n.Ops))
	case KindUcfsDir:
		_, _ = h.Write([]byte(UcfsDir))
	case KindJournal:
		_, _ = h.Write([]byte(UcfsDir + "/" + JournalName))
	case KindNeverIndex:
		_, _ = h.Write([]byte(NeverIndex))
	case KindViewDir:
		_, _ = h.Write([]byte("view\x00" + n.Ops))
	case KindDateFile:
		_, _ = h.Write([]byte(ViewByDate + "\x00" + n.UUID))
	case KindViewFile:
		_, _ = h.Write([]byte("view\x00" + n.Ops + "\x00" + n.UUID))
	}
	return h.Sum64()
}

func (n Node) IsDir() bool {
	switch n.Kind {
	case KindFile, KindVariant, KindTreeFile, KindShadow, KindJournal, KindNeverIndex, KindDateFile, KindViewFile:
		return false
	}
	return true
}

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
	FilesDir    = "files"
	TreeDir     = "tree"
	InboxDir    = "_inbox"
	UcfsDir     = ".ucfs"
	JournalName = "uploads.log"
	NeverIndex  = ".metadata_never_index"
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
	// w is nil on a read-only mount.
	w       *Writes
	wstats  writeStats
	journal string
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

	views map[string]bool
	opts  Options
	stats metaStats
	// refreshes, md5s and groupFetch share one request among concurrent
	// readers of the same file or group.
	refreshes, md5s, groupFetch singleflight.Group
}

// New builds the filesystem. missing is called, in its own goroutine, when
// the CDN answers 404 for a file the index has.
func New(ix *index.Index, c *cache.Cache, head Header, missing func(uuid string)) *FS {
	f := &FS{ix: ix, cache: c, cdn: head, now: time.Now, missing: missing,
		failed: map[string]failure{}, nodeOps: map[uint64]string{}}
	f.Configure(Options{})
	f.Touch()
	return f
}

// Journal sets the upload journal file shown as .ucfs/uploads.log.
func (f *FS) Journal(p string) { f.journal = p }

// Touch records that the index changed.
func (f *FS) Touch() { f.dirMtime.Store(time.Now().UnixNano()) }

func (f *FS) Root() Node { return Node{Kind: KindRoot} }

func (f *FS) dirAttr(ttl time.Duration) Attr {
	return Attr{Mode: os.ModeDir | 0o555, Mtime: time.Unix(0, f.dirMtime.Load()), TTL: ttl}
}

// writableDirAttr is the attribute of a directory that takes new entries.
func (f *FS) writableDirAttr(ttl time.Duration) Attr {
	a := f.dirAttr(ttl)
	if f.w != nil {
		a.Mode |= 0o200
	}
	return a
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
	case KindTreeDir:
		ok, err := f.ix.TreeDirExists(ctx, n.Ops)
		switch {
		case err != nil:
			return Attr{}, f.ioErr("tree dir", err)
		case !ok:
			return Attr{}, syscall.ENOENT
		}
		return f.writableDirAttr(dirTTL), nil
	case KindInbox:
		return f.dirAttr(dirTTL), nil
	case KindTreeFile:
		o, err := f.occupant(ctx, n)
		if err != nil {
			return Attr{}, err
		}
		return f.occupantAttr(o), nil
	case KindShadow, KindShadowDir:
		return f.shadowAttr(ctx, n)
	case KindUcfsDir:
		return f.dirAttr(dirTTL), nil
	case KindJournal:
		return f.journalAttr(), nil
	case KindNeverIndex:
		return Attr{Mode: 0o444, Mtime: time.Unix(0, 0), TTL: fileTTL}, nil
	case KindViewDir:
		return f.viewDirAttr(ctx, n.Ops)
	case KindDateFile, KindViewFile:
		file, err := f.file(ctx, n.UUID)
		if err != nil {
			return Attr{}, err
		}
		a := fileAttr(file)
		if n.Kind == KindViewFile {
			a.TTL = dirTTL
		}
		return a, nil
	}
	return Attr{}, syscall.ENOENT
}

func (f *FS) ioErr(what string, err error) error {
	slog.Error(what, "err", err)
	return syscall.EIO
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
		if e, ok := f.rootView(name); ok {
			return e, nil
		}
		switch {
		case name == UcfsDir:
			return Entry{Name: name, Node: Node{Kind: KindUcfsDir}, Attr: f.dirAttr(dirTTL)}, nil
		case name == NeverIndex:
			n := Node{Kind: KindNeverIndex}
			a, _ := f.Getattr(ctx, n)
			return Entry{Name: name, Node: n, Attr: a}, nil
		case IsShadowName(name):
			return f.lookupShadow(ctx, name)
		}
	case KindFilesDir:
		uuid, hasName, ok := parseName(name)
		if !ok {
			return f.lookupWritten(ctx, name)
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
	case KindTreeDir:
		return f.lookupTree(ctx, dir.Ops, name)
	case KindInbox:
		return f.lookupInbox(ctx, name)
	case KindShadowDir:
		return f.lookupShadow(ctx, dir.Ops+"/"+name)
	case KindUcfsDir:
		if name == JournalName {
			n := Node{Kind: KindJournal}
			return Entry{Name: name, Node: n, Attr: f.journalAttr()}, nil
		}
	case KindViewDir:
		return f.lookupView(ctx, dir.Ops, name)
	case KindFile, KindVariant, KindTreeFile, KindShadow, KindJournal, KindNeverIndex, KindDateFile, KindViewFile:
		return Entry{}, syscall.ENOTDIR
	}
	return Entry{}, syscall.ENOENT
}

func (f *FS) ReadDir(ctx context.Context, dir Node) ([]Entry, error) {
	switch dir.Kind {
	case KindRoot:
		var out []Entry
		for _, v := range f.Views() {
			e, _ := f.rootView(v)
			out = append(out, e)
		}
		never, _ := f.Lookup(ctx, dir, NeverIndex)
		out = append(out, Entry{Name: UcfsDir, Node: Node{Kind: KindUcfsDir}, Attr: f.dirAttr(dirTTL)}, never)
		return f.withShadows(ctx, "", out)
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
		out = append(out, f.spoolEntries(FilesDir)...)
		return f.withShadows(ctx, FilesDir, out)
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
	case KindTreeDir:
		return f.readTree(ctx, dir.Ops)
	case KindInbox:
		return f.readInbox(ctx)
	case KindShadowDir:
		return f.withShadows(ctx, dir.Ops, nil)
	case KindUcfsDir:
		return []Entry{{Name: JournalName, Node: Node{Kind: KindJournal}, Attr: f.journalAttr()}}, nil
	case KindViewDir:
		return f.readView(ctx, dir.Ops)
	case KindFile, KindVariant, KindTreeFile, KindShadow, KindJournal, KindNeverIndex, KindDateFile, KindViewFile:
		return nil, syscall.ENOTDIR
	}
	return nil, syscall.ENOENT
}

// ReadAt reads file content. A CDN 404 hands the uuid to the missing hook,
// which decides from the API whether the file is really gone; a CDN 400
// means the operations do not apply to the file.
func (f *FS) ReadAt(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	switch {
	case n.IsDir():
		return 0, syscall.EISDIR
	case n.Kind == KindTreeFile:
		return f.readObject(ctx, n, p, off)
	case n.Kind == KindShadow:
		return f.readShadow(ctx, n, p, off)
	case n.Kind == KindJournal:
		return f.readJournal(p, off)
	case n.Kind == KindNeverIndex:
		return 0, io.EOF
	}
	file, err := f.file(ctx, n.UUID)
	if err != nil {
		return 0, err
	}
	return f.readFile(ctx, n, file, p, off)
}

// readFile reads an original, or the variant n names, through the cache.
func (f *FS) readFile(ctx context.Context, n Node, file index.File, p []byte, off int64) (int, error) {
	k, size := cache.Key{UUID: file.UUID}, file.Size
	if n.Kind == KindVariant {
		v, err := f.variant(ctx, n.UUID, n.Ops)
		if err != nil {
			return 0, err
		}
		k, size = cache.Key{UUID: n.UUID, Ops: n.Ops, Version: v.ETag}, v.Size
	}
	got, err := f.cache.ReadAt(ctx, k, size, p, off)
	if err == nil || errors.Is(err, io.EOF) {
		return got, err
	}
	var bad *cdn.BadRequestError
	var changed *cache.ChangedError
	switch {
	case errors.Is(err, cdn.ErrNotFound):
		f.cache.Forget(k)
		f.notFound(n.UUID)
	case errors.As(err, &bad):
		slog.Info("CDN rejected a variant", "uuid", n.UUID, "ops", n.Ops, "message", bad.Message)
	case errors.As(err, &changed):
		slog.Warn("content changed while being read; its cached blocks are dropped and the read fails",
			"uuid", n.UUID, "ops", n.Ops, "etag", changed.Got, "was", changed.Want)
		if n.Kind == KindVariant {
			f.staleVariant(ctx, n.UUID, n.Ops)
		}
	case errors.Is(err, cache.ErrBudget):
	default:
		slog.Error("read", "uuid", n.UUID, "ops", n.Ops, "off", off, "err", err)
	}
	return got, api.Errno(err)
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
	case KindTreeDir:
		return split(treeMount(n.Ops)), nil
	case KindInbox:
		return []string{TreeDir, InboxDir}, nil
	case KindTreeFile:
		return f.objectPath(ctx, n)
	case KindShadow:
		sh, err := f.ix.ShadowByIno(ctx, n.Ino)
		if err != nil {
			return nil, syscall.ENOENT
		}
		return split(sh.Path), nil
	case KindShadowDir:
		return split(n.Ops), nil
	case KindUcfsDir:
		return []string{UcfsDir}, nil
	case KindJournal:
		return []string{UcfsDir, JournalName}, nil
	case KindNeverIndex:
		return []string{NeverIndex}, nil
	case KindViewDir:
		return split(n.Ops), nil
	case KindDateFile:
		file, err := f.file(ctx, n.UUID)
		if err != nil {
			return nil, err
		}
		return f.datePath(file), nil
	case KindViewFile:
		if _, err := f.file(ctx, n.UUID); err != nil {
			return nil, err
		}
		return split(n.Ops), nil
	}
	return nil, syscall.ENOENT
}

func split(p string) []string {
	if p == "" {
		return []string{}
	}
	return strings.Split(p, "/")
}
