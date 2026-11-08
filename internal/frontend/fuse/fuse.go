//go:build linux

// Package fuse serves a vfs.FS through the Linux kernel FUSE driver with go-fuse.
package fuse

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	gofs "github.com/hanwen/go-fuse/v2/fs"
	gofuse "github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alxgsv/ucfs/internal/vfs"
)

type node struct {
	gofs.Inode
	v *vfs.FS
	n vfs.Node
}

var (
	_ gofs.NodeLookuper  = (*node)(nil)
	_ gofs.NodeReaddirer = (*node)(nil)
	_ gofs.NodeGetattrer = (*node)(nil)
	_ gofs.NodeOpener    = (*node)(nil)
	_ gofs.NodeReader    = (*node)(nil)
	_ gofs.NodeCreater   = (*node)(nil)
	_ gofs.NodeWriter    = (*node)(nil)
	_ gofs.NodeFlusher   = (*node)(nil)
	_ gofs.NodeFsyncer   = (*node)(nil)
	_ gofs.NodeSetattrer = (*node)(nil)
	_ gofs.NodeUnlinker  = (*node)(nil)
	_ gofs.NodeRmdirer   = (*node)(nil)
	_ gofs.NodeMkdirer   = (*node)(nil)
	_ gofs.NodeRenamer   = (*node)(nil)
)

// cur is the node as the vfs knows it now. Directories in tree/ and shadow
// directories are known by path, which changes when they or a parent are
// renamed; go-fuse moves the inode, so its path is the current one. Every
// other node is known by an identity that does not change.
func (n *node) cur() vfs.Node {
	if n.n.Kind != vfs.KindTreeDir && n.n.Kind != vfs.KindShadowDir {
		return n.n
	}
	return vfs.PathNode(n.n.Kind, n.Path(nil))
}

func (n *node) child(ctx context.Context, e vfs.Entry, out *gofuse.EntryOut) *gofs.Inode {
	fillAttr(&out.Attr, e.Node, e.Attr)
	out.SetEntryTimeout(e.Attr.TTL)
	out.SetAttrTimeout(e.Attr.TTL)
	return n.NewInode(ctx, &node{v: n.v, n: e.Node}, gofs.StableAttr{Mode: typeBits(e.Node), Ino: e.Node.ID()})
}

func errno(err error) syscall.Errno {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	return syscall.EIO
}

func typeBits(n vfs.Node) uint32 {
	if n.IsDir() {
		return syscall.S_IFDIR
	}
	return syscall.S_IFREG
}

func fillAttr(out *gofuse.Attr, n vfs.Node, a vfs.Attr) {
	out.Ino = n.ID()
	out.Mode = typeBits(n) | uint32(a.Mode.Perm())
	out.Size = uint64(a.Size)
	out.Blocks = (out.Size + 511) / 512
	out.Nlink = 1
	if n.IsDir() {
		out.Nlink = 2
	}
	out.SetTimes(nil, &a.Mtime, &a.Mtime)
	out.Owner = gofuse.Owner{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid())}
}

func (n *node) Getattr(ctx context.Context, _ gofs.FileHandle, out *gofuse.AttrOut) syscall.Errno {
	cur := n.cur()
	a, err := n.v.Getattr(ctx, cur)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, cur, a)
	out.SetTimeout(a.TTL)
	return 0
}

func (n *node) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	e, err := n.v.Lookup(ctx, n.cur(), name)
	if err != nil {
		return nil, errno(err)
	}
	return n.child(ctx, e, out), 0
}

func (n *node) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	entries, err := n.v.ReadDir(ctx, n.cur())
	if err != nil {
		return nil, errno(err)
	}
	out := make([]gofuse.DirEntry, len(entries))
	for i, e := range entries {
		out[i] = gofuse.DirEntry{Name: e.Name, Ino: e.Node.ID(), Mode: typeBits(e.Node)}
	}
	return gofs.NewListDirStream(out), 0
}

// Open keeps the page cache across opens of originals in files/, whose
// content never changes. A variant's content can change when its HEAD is
// renewed, and a tree file's when it is overwritten.
func (n *node) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	if n.n.IsDir() {
		return nil, 0, syscall.EISDIR
	}
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		if err := n.v.OpenWrite(ctx, n.n, flags&syscall.O_TRUNC != 0); err != nil {
			return nil, 0, errno(err)
		}
		return nil, 0, 0
	}
	if n.n.Kind != vfs.KindFile {
		return nil, 0, 0
	}
	return nil, gofuse.FOPEN_KEEP_CACHE, 0
}

// Create is only sent for a name the kernel did not find, but another
// process may create it first: an existing file is opened, truncated only
// with O_TRUNC, as open(2) does.
func (n *node) Create(ctx context.Context, name string, flags, _ uint32, out *gofuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	cur := n.cur()
	if e, err := n.v.Lookup(ctx, cur, name); err == nil {
		if flags&syscall.O_EXCL != 0 {
			return nil, nil, 0, syscall.EEXIST
		}
		if err := n.v.OpenWrite(ctx, e.Node, flags&syscall.O_TRUNC != 0); err != nil {
			return nil, nil, 0, errno(err)
		}
		if e, err = n.v.Lookup(ctx, cur, name); err != nil {
			return nil, nil, 0, errno(err)
		}
		return n.child(ctx, e, out), nil, 0, 0
	}
	e, err := n.v.Create(ctx, cur, name)
	if err != nil {
		return nil, nil, 0, errno(err)
	}
	return n.child(ctx, e, out), nil, 0, 0
}

func (n *node) Write(ctx context.Context, _ gofs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	k, err := n.v.WriteAt(ctx, n.n, data, off)
	if err != nil {
		return uint32(k), errno(err)
	}
	return uint32(k), 0
}

// Flush runs for every close(2) and blocks while a written file uploads,
// so the writer gets the upload's error.
func (n *node) Flush(ctx context.Context, _ gofs.FileHandle) syscall.Errno {
	return errno0(n.v.Flush(ctx, n.n))
}

func (n *node) Fsync(ctx context.Context, _ gofs.FileHandle, _ uint32) syscall.Errno {
	return errno0(n.v.Flush(ctx, n.n))
}

// Setattr applies a size and ignores mode, owner and times, which an
// immutable file cannot keep.
func (n *node) Setattr(ctx context.Context, _ gofs.FileHandle, in *gofuse.SetAttrIn, out *gofuse.AttrOut) syscall.Errno {
	cur := n.cur()
	if size, ok := in.GetSize(); ok {
		if err := n.v.Truncate(ctx, cur, int64(size)); err != nil {
			return errno(err)
		}
	}
	return n.Getattr(ctx, nil, out)
}

func (n *node) Unlink(ctx context.Context, name string) syscall.Errno {
	return errno0(n.v.Remove(ctx, n.cur(), name))
}

func (n *node) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errno0(n.v.Rmdir(ctx, n.cur(), name))
}

func (n *node) Mkdir(ctx context.Context, name string, _ uint32, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	e, err := n.v.Mkdir(ctx, n.cur(), name)
	if err != nil {
		return nil, errno(err)
	}
	return n.child(ctx, e, out), 0
}

const (
	renameNoReplace = 1
	renameExchange  = 2
)

func (n *node) Rename(ctx context.Context, name string, newParent gofs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	np, ok := newParent.(*node)
	if !ok || flags&renameExchange != 0 {
		return syscall.EINVAL
	}
	to := np.cur()
	if flags&renameNoReplace != 0 {
		if _, err := n.v.Lookup(ctx, to, newName); err == nil {
			return syscall.EEXIST
		}
	}
	return errno0(n.v.Rename(ctx, n.cur(), name, to, newName))
}

func errno0(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	return errno(err)
}

func (n *node) Read(ctx context.Context, _ gofs.FileHandle, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	k, err := n.v.ReadAt(ctx, n.n, dest, off)
	if err != nil && k == 0 && !errors.Is(err, io.EOF) {
		return nil, errno(err)
	}
	return gofuse.ReadResultData(dest[:k]), 0
}

const maxIO = 1 << 20

// Mount mounts v at dir, read-only unless v takes writes. DirectMount
// mounts with mount(2) when the process may, as root in a container does,
// and falls back to fusermount3.
func Mount(dir string, v *vfs.FS, debug bool) (*gofuse.Server, error) {
	negative := time.Second
	root := &node{v: v, n: v.Root()}
	opts := []string{"noatime"}
	if !v.Writable() {
		opts = append(opts, "ro")
	}
	return gofs.Mount(dir, root, &gofs.Options{
		MountOptions: gofuse.MountOptions{
			FsName:       "ucfs",
			Name:         "ucfs",
			Options:      opts,
			MaxReadAhead: maxIO,
			MaxWrite:     maxIO,
			DirectMount:  true,
			Debug:        debug,
		},
		NegativeTimeout: &negative,
		UID:             uint32(os.Getuid()),
		GID:             uint32(os.Getgid()),
	})
}
