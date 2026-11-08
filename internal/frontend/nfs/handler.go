// Package nfs serves a vfs.FS over loopback NFSv3 with go-nfs.
package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	gonfs "github.com/willscott/go-nfs"
	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/alxgsv/ucfs/internal/vfs"
)

// handler implements gonfs.Handler with self-describing file handles, so a
// restarted daemon serves old handles without a path cache.
type handler struct {
	fs *billyFS

	mu        sync.Mutex
	listings  map[uint64]listing
	order     []uint64
	nextVerif uint64
}

type listing struct {
	path    string
	entries []fs.FileInfo
}

// maxListings bounds the READDIR continuation cache; a listing is needed only
// while a client pages through one directory.
const maxListings = 64

func newHandler(v *vfs.FS) *handler {
	return &handler{fs: &billyFS{v: v, uid: uint32(os.Getuid()), gid: uint32(os.Getgid())}, listings: map[uint64]listing{}}
}

// Mount offers AUTH_UNIX only: offered AUTH_NULL, the macOS client sends no
// uid, and FilterAccess could not tell its users apart.
func (h *handler) Mount(context.Context, net.Conn, gonfs.MountRequest) (gonfs.MountStatus, billy.Filesystem, []gonfs.AuthFlavor) {
	return gonfs.MountStatusOk, h.fs, []gonfs.AuthFlavor{gonfs.AuthFlavorUnix}
}

// Change accepts mode, owner and time changes and ignores them: files are
// immutable once uploaded, and cp and Finder set these on every copy
// (Spike C). Without a changer every CREATE that carries a mode would fail.
func (h *handler) Change(billy.Filesystem) billy.Change { return h.fs }

func (h *handler) FSStat(context.Context, billy.Filesystem, *gonfs.FSStat) error { return nil }

// Handle layout: version (1), kind (1), reserved (2), uuid (16), node id (8).
// The length must be a multiple of 4: go-nfs reads handles with
// xdr.ReadOpaque, which skips no XDR padding, so in ACCESS, COMMIT and
// SETATTR the arguments after an unaligned handle would be misread. A
// variants view node does not fit its operations path in the handle; the
// vfs keeps it by node id.
const (
	handleVersion = 1
	handleLen     = 28
)

func (h *handler) ToHandle(_ billy.Filesystem, p []string) []byte {
	ctx := context.Background()
	e, err := h.fs.v.Walk(ctx, path.Join(p...))
	if err != nil {
		return nil
	}
	h.fs.v.Remember(ctx, e.Node)
	return encodeHandle(h.fs.v, e.Node)
}

func encodeHandle(v *vfs.FS, n vfs.Node) []byte {
	b := make([]byte, handleLen)
	b[0], b[1] = handleVersion, byte(n.Kind)
	if n.UUID != "" {
		_, _ = hex.Decode(b[4:20], []byte(strings.ReplaceAll(n.UUID, "-", "")))
	}
	binary.BigEndian.PutUint64(b[20:], v.ID(n))
	return b
}

var errStale = errors.New("stale file handle")

// zeroUUID stands for an object whose inode number was drawn at random
// and so has no key.
var zeroUUID [16]byte

func (h *handler) decodeHandle(ctx context.Context, b []byte) (vfs.Node, error) {
	if len(b) != handleLen || b[0] != handleVersion {
		return vfs.Node{}, errStale
	}
	kind := vfs.Kind(b[1])
	var uuid string
	if kind.HasUUID() && !bytes.Equal(b[4:20], zeroUUID[:]) {
		x := hex.EncodeToString(b[4:20])
		uuid = x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:32]
	}
	n, err := h.fs.v.NodeByID(ctx, kind, uuid, binary.BigEndian.Uint64(b[20:]))
	if err != nil {
		return vfs.Node{}, errStale
	}
	return n, nil
}

// FromHandle must return a freshly allocated path: go-nfs appends to it, and
// with concurrent handlers a shared backing array would be a data race.
func (h *handler) FromHandle(b []byte) (billy.Filesystem, []string, error) {
	ctx := context.Background()
	n, err := h.decodeHandle(ctx, b)
	if err != nil {
		return nil, nil, err
	}
	p, err := h.fs.v.Path(ctx, n)
	if err != nil {
		return nil, nil, errStale
	}
	return h.fs, p[:len(p):len(p)], nil
}

func (h *handler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

// FilterAccess grants nothing to callers other than the user the daemon
// runs as and root, so other users of the machine cannot read the project
// through the mount, as FUSE without allow_other keeps them out.
func (h *handler) FilterAccess(uid uint32, known bool, mask uint32) uint32 {
	if known && (uid == h.fs.uid || uid == 0) {
		return mask
	}
	return 0
}

// HandleLimit is large because handles cost nothing to keep, and go-nfs caps
// a READDIR reply at HandleLimit()/2 entries.
func (h *handler) HandleLimit() int { return 1 << 30 }

// VerifierFor and DataForVerifier let a client page through a large directory
// without the directory being listed again for every READDIR call.
func (h *handler) VerifierFor(p string, entries []fs.FileInfo) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextVerif++
	v := h.nextVerif
	h.listings[v] = listing{path: p, entries: entries}
	h.order = append(h.order, v)
	if len(h.order) > maxListings {
		delete(h.listings, h.order[0])
		h.order = h.order[1:]
	}
	return v
}

func (h *handler) DataForVerifier(p string, v uint64) []fs.FileInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.listings[v]; ok && l.path == p {
		return l.entries
	}
	return nil
}

// billyFS adapts vfs.FS to the path-based billy interface go-nfs uses. Every
// method except file reads and writes is answered from the index.
type billyFS struct {
	v        *vfs.FS
	uid, gid uint32
}

var (
	_ billy.Change    = (*billyFS)(nil)
	_ gonfs.Committer = (*billyFS)(nil)
)

func pathErr(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// Capabilities include writing when the mount is writable; go-nfs answers
// every write procedure with ROFS otherwise.
func (b *billyFS) Capabilities() billy.Capability {
	c := billy.ReadCapability | billy.SeekCapability
	if b.v.Writable() {
		c |= billy.WriteCapability | billy.TruncateCapability
	}
	return c
}

func (b *billyFS) info(e vfs.Entry) *fileInfo {
	nlink := uint32(1)
	if e.Node.IsDir() {
		nlink = 2
	}
	return &fileInfo{e: e, sys: &nfsfile.FileInfo{Nlink: nlink, UID: b.uid, GID: b.gid, Fileid: b.v.ID(e.Node)}}
}

func (b *billyFS) Stat(name string) (os.FileInfo, error) {
	e, err := b.v.Walk(context.Background(), name)
	if err != nil {
		return nil, pathErr("stat", name, err)
	}
	return b.info(e), nil
}

func (b *billyFS) Lstat(name string) (os.FileInfo, error) { return b.Stat(name) }

func (b *billyFS) ReadDir(name string) ([]os.FileInfo, error) {
	ctx := context.Background()
	e, err := b.v.Walk(ctx, name)
	if err != nil {
		return nil, pathErr("readdir", name, err)
	}
	entries, err := b.v.ReadDir(ctx, e.Node)
	if err != nil {
		return nil, pathErr("readdir", name, err)
	}
	out := make([]os.FileInfo, len(entries))
	for i, c := range entries {
		out[i] = b.info(c)
	}
	return out, nil
}

func (b *billyFS) Open(name string) (billy.File, error) {
	e, err := b.v.Walk(context.Background(), name)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if e.Node.IsDir() {
		return nil, pathErr("open", name, syscall.EISDIR)
	}
	return &file{v: b.v, name: name, node: e.Node}, nil
}

// OpenFile opens for writing when asked: go-nfs opens with O_RDWR for every
// WRITE and with O_WRONLY|O_EXCL to apply a SETATTR of the size.
func (b *billyFS) OpenFile(name string, flag int, _ os.FileMode) (billy.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) == 0 {
		return b.Open(name)
	}
	ctx := context.Background()
	e, err := b.v.Walk(ctx, name)
	if errors.Is(err, syscall.ENOENT) && flag&os.O_CREATE != 0 {
		return b.Create(name)
	}
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if err := b.v.OpenWrite(ctx, e.Node, flag&os.O_TRUNC != 0); err != nil {
		return nil, pathErr("open", name, err)
	}
	return &file{v: b.v, name: name, node: e.Node}, nil
}

// parent resolves the directory of a path and the last component.
func (b *billyFS) parent(ctx context.Context, name string) (vfs.Node, string, error) {
	dir, base := path.Split(path.Clean("/" + name))
	e, err := b.v.Walk(ctx, dir)
	return e.Node, base, err
}

func (b *billyFS) Create(name string) (billy.File, error) {
	ctx := context.Background()
	dir, base, err := b.parent(ctx, name)
	if err != nil {
		return nil, pathErr("create", name, err)
	}
	e, err := b.v.Create(ctx, dir, base)
	if err != nil {
		return nil, pathErr("create", name, err)
	}
	return &file{v: b.v, name: name, node: e.Node}, nil
}

func (b *billyFS) Rename(from, to string) error {
	ctx := context.Background()
	fdir, fbase, err := b.parent(ctx, from)
	if err != nil {
		return pathErr("rename", from, err)
	}
	tdir, tbase, err := b.parent(ctx, to)
	if err != nil {
		return pathErr("rename", to, err)
	}
	if err := b.v.Rename(ctx, fdir, fbase, tdir, tbase); err != nil {
		return pathErr("rename", from, err)
	}
	return nil
}

// Remove serves both REMOVE and RMDIR, as go-nfs calls it for either.
func (b *billyFS) Remove(name string) error {
	ctx := context.Background()
	dir, base, err := b.parent(ctx, name)
	if err != nil {
		return pathErr("remove", name, err)
	}
	e, err := b.v.Lookup(ctx, dir, base)
	if err != nil {
		return pathErr("remove", name, err)
	}
	if e.Node.IsDir() {
		err = b.v.Rmdir(ctx, dir, base)
	} else {
		err = b.v.Remove(ctx, dir, base)
	}
	if err != nil {
		return pathErr("remove", name, err)
	}
	return nil
}

// MkdirAll makes the last component only: go-nfs checks that the parent
// exists before it calls.
func (b *billyFS) MkdirAll(name string, _ os.FileMode) error {
	ctx := context.Background()
	dir, base, err := b.parent(ctx, name)
	if err != nil {
		return pathErr("mkdir", name, err)
	}
	if _, err := b.v.Mkdir(ctx, dir, base); err != nil {
		return pathErr("mkdir", name, err)
	}
	return nil
}

// Commit is NFS COMMIT, which go-nfs passes on because billyFS implements
// gonfs.Committer: it starts the upload of a written file.
func (b *billyFS) Commit(name string) error {
	ctx := context.Background()
	slog.Debug("nfs commit", "path", name)
	e, err := b.v.Walk(ctx, name)
	if err != nil {
		return pathErr("commit", name, err)
	}
	if err := b.v.Commit(ctx, e.Node); err != nil {
		return pathErr("commit", name, err)
	}
	return nil
}

func (b *billyFS) Chmod(string, os.FileMode) error            { return nil }
func (b *billyFS) Lchown(string, int, int) error              { return nil }
func (b *billyFS) Chown(string, int, int) error               { return nil }
func (b *billyFS) Chtimes(string, time.Time, time.Time) error { return nil }

func (b *billyFS) Symlink(_, link string) error { return pathErr("symlink", link, syscall.EPERM) }
func (b *billyFS) Readlink(link string) (string, error) {
	return "", pathErr("readlink", link, syscall.EINVAL)
}
func (b *billyFS) TempFile(dir, _ string) (billy.File, error) {
	return nil, pathErr("tempfile", dir, syscall.EROFS)
}
func (b *billyFS) Chroot(p string) (billy.Filesystem, error) {
	return nil, pathErr("chroot", p, syscall.ENOTSUP)
}
func (b *billyFS) Root() string               { return "/" }
func (b *billyFS) Join(elem ...string) string { return path.Join(elem...) }

type fileInfo struct {
	e   vfs.Entry
	sys *nfsfile.FileInfo
}

func (f *fileInfo) Name() string       { return f.e.Name }
func (f *fileInfo) Size() int64        { return f.e.Attr.Size }
func (f *fileInfo) Mode() os.FileMode  { return f.e.Attr.Mode }
func (f *fileInfo) ModTime() time.Time { return f.e.Attr.Mtime }
func (f *fileInfo) IsDir() bool        { return f.e.Node.IsDir() }
func (f *fileInfo) Sys() any           { return f.sys }

// file is opened afresh for every READ and WRITE RPC, so it holds no state
// worth sharing; the cache tracks sequential access per object instead, and
// the spool what was written.
type file struct {
	v    *vfs.FS
	name string
	node vfs.Node
	off  int64
}

func (f *file) Name() string { return f.name }

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	return f.v.ReadAt(context.Background(), f.node, p, off)
}

func (f *file) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *file) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekCurrent:
		f.off += offset
	default:
		return 0, pathErr("seek", f.name, syscall.EINVAL)
	}
	return f.off, nil
}

func (f *file) Write(p []byte) (int, error) {
	n, err := f.v.WriteAt(context.Background(), f.node, p, f.off)
	f.off += int64(n)
	if err != nil {
		return n, pathErr("write", f.name, err)
	}
	return n, nil
}

func (f *file) Truncate(size int64) error {
	if err := f.v.Truncate(context.Background(), f.node, size); err != nil {
		return pathErr("truncate", f.name, err)
	}
	return nil
}

func (f *file) Close() error  { return nil }
func (f *file) Lock() error   { return nil }
func (f *file) Unlock() error { return nil }
