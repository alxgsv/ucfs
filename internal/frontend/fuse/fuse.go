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
)

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
	a, err := n.v.Getattr(ctx, n.n)
	if err != nil {
		return errno(err)
	}
	fillAttr(&out.Attr, n.n, a)
	out.SetTimeout(a.TTL)
	return 0
}

func (n *node) Lookup(ctx context.Context, name string, out *gofuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	e, err := n.v.Lookup(ctx, n.n, name)
	if err != nil {
		return nil, errno(err)
	}
	fillAttr(&out.Attr, e.Node, e.Attr)
	out.SetEntryTimeout(e.Attr.TTL)
	out.SetAttrTimeout(e.Attr.TTL)
	child := &node{v: n.v, n: e.Node}
	return n.NewInode(ctx, child, gofs.StableAttr{Mode: typeBits(e.Node), Ino: e.Node.ID()}), 0
}

func (n *node) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	entries, err := n.v.ReadDir(ctx, n.n)
	if err != nil {
		return nil, errno(err)
	}
	out := make([]gofuse.DirEntry, len(entries))
	for i, e := range entries {
		out[i] = gofuse.DirEntry{Name: e.Name, Ino: e.Node.ID(), Mode: typeBits(e.Node)}
	}
	return gofs.NewListDirStream(out), 0
}

// Open keeps the page cache across opens of originals, whose content never
// changes. A variant's content can change when its HEAD is renewed.
func (n *node) Open(_ context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	if n.n.IsDir() {
		return nil, 0, syscall.EISDIR
	}
	if flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	if n.n.Kind == vfs.KindVariant {
		return nil, 0, 0
	}
	return nil, gofuse.FOPEN_KEEP_CACHE, 0
}

func (n *node) Read(ctx context.Context, _ gofs.FileHandle, dest []byte, off int64) (gofuse.ReadResult, syscall.Errno) {
	k, err := n.v.ReadAt(ctx, n.n, dest, off)
	if err != nil && k == 0 && !errors.Is(err, io.EOF) {
		return nil, errno(err)
	}
	return gofuse.ReadResultData(dest[:k]), 0
}

const maxIO = 1 << 20

// Mount mounts v read-only at dir. DirectMount mounts with mount(2) when the
// process may, as root in a container does, and falls back to fusermount3.
func Mount(dir string, v *vfs.FS, debug bool) (*gofuse.Server, error) {
	negative := time.Second
	root := &node{v: v, n: v.Root()}
	return gofs.Mount(dir, root, &gofs.Options{
		MountOptions: gofuse.MountOptions{
			FsName:       "ucfs",
			Name:         "ucfs",
			Options:      []string{"ro", "noatime"},
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
