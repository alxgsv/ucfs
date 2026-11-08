package vfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
)

// shadowNames are written by macOS for its own bookkeeping: Finder view
// settings, AppleDouble sidecars for xattrs (macOS attaches
// com.apple.provenance to every copied file, so cp writes one too, Spike
// C), Spotlight and FSEvents stores, the Trash, and safe-save scratch space.
var shadowNames = map[string]bool{
	".DS_Store": true, ".Spotlight-V100": true, ".fseventsd": true, ".Trashes": true, ".TemporaryItems": true,
}

// IsShadowName reports whether a name is kept local and never uploaded:
// the macOS names above, AppleDouble "._" files, and vim swap files
// (".name.swp", and ".swx" and the like it probes with), which vim rewrites
// while editing and would otherwise upload over and over.
func IsShadowName(name string) bool {
	if shadowNames[name] || strings.HasPrefix(name, "._") {
		return true
	}
	n := len(name)
	return n > 5 && name[0] == '.' && name[n-4:n-1] == ".sw" && name[n-1] >= 'a' && name[n-1] <= 'z'
}

func (f *FS) blobPath(sh index.Shadow) string { return filepath.Join(f.w.ShadowDir, sh.Blob) }

func shadowNode(sh index.Shadow) Node {
	if sh.Dir {
		return Node{Kind: KindShadowDir, Ops: sh.Path}
	}
	return Node{Kind: KindShadow, Ino: sh.Ino}
}

func (f *FS) shadowEntry(sh index.Shadow) Entry {
	return Entry{Name: path.Base(sh.Path), Node: shadowNode(sh), Attr: f.shadowRowAttr(sh)}
}

func (f *FS) shadowRowAttr(sh index.Shadow) Attr {
	if sh.Dir {
		return f.writableDirAttr(dirTTL)
	}
	a := Attr{Mode: 0o644}
	if f.w != nil {
		if info, err := os.Stat(f.blobPath(sh)); err == nil {
			a.Size, a.Mtime = info.Size(), info.ModTime()
		}
	}
	return a
}

func (f *FS) shadowAttr(ctx context.Context, n Node) (Attr, error) {
	sh, err := f.shadowOf(ctx, n)
	if err != nil {
		return Attr{}, err
	}
	return f.shadowRowAttr(sh), nil
}

func (f *FS) shadowOf(ctx context.Context, n Node) (index.Shadow, error) {
	var sh index.Shadow
	var err error
	if n.Kind == KindShadowDir {
		sh, err = f.ix.Shadow(ctx, n.Ops)
	} else {
		sh, err = f.ix.ShadowByIno(ctx, n.Ino)
	}
	switch {
	case errors.Is(err, index.ErrNotFound):
		return sh, syscall.ENOENT
	case err != nil:
		return sh, f.ioErr("shadow lookup", err)
	}
	return sh, nil
}

// lookupShadow resolves a local-only entry by its mount path.
func (f *FS) lookupShadow(ctx context.Context, p string) (Entry, error) {
	sh, err := f.ix.Shadow(ctx, p)
	switch {
	case errors.Is(err, index.ErrNotFound):
		return Entry{}, syscall.ENOENT
	case err != nil:
		return Entry{}, f.ioErr("shadow lookup", err)
	}
	return f.shadowEntry(sh), nil
}

// withShadows appends the shadow entries directly in dir, a mount path.
func (f *FS) withShadows(ctx context.Context, dir string, out []Entry) ([]Entry, error) {
	shadows, err := f.ix.ShadowChildren(ctx, dir)
	if err != nil {
		return nil, f.ioErr("shadow listing", err)
	}
	for _, sh := range shadows {
		out = append(out, f.shadowEntry(sh))
	}
	return out, nil
}

// createShadow makes or truncates a local-only file.
func (f *FS) createShadow(ctx context.Context, p string) (Entry, error) {
	sh, err := f.ix.Shadow(ctx, p)
	switch {
	case err == nil && sh.Dir:
		return Entry{}, syscall.EISDIR
	case err == nil:
		if err := os.Truncate(f.blobPath(sh), 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Entry{}, err
		}
	case errors.Is(err, index.ErrNotFound):
		ino := spool.NewIno()
		sh = index.Shadow{Path: p, Ino: ino, Blob: fmt.Sprintf("%016x", ino)}
		if err := os.WriteFile(f.blobPath(sh), nil, 0o600); err != nil {
			return Entry{}, err
		}
		if err := f.ix.PutShadow(ctx, sh); err != nil {
			return Entry{}, f.ioErr("shadow create", err)
		}
	default:
		return Entry{}, f.ioErr("shadow lookup", err)
	}
	f.wstats.shadowCreates.Add(1)
	f.Touch()
	return f.shadowEntry(sh), nil
}

func (f *FS) mkdirShadow(ctx context.Context, p string) (Entry, error) {
	if _, err := f.ix.Shadow(ctx, p); err == nil {
		return Entry{}, syscall.EEXIST
	}
	sh := index.Shadow{Path: p, Ino: spool.NewIno(), Dir: true}
	if err := f.ix.PutShadow(ctx, sh); err != nil {
		return Entry{}, f.ioErr("shadow mkdir", err)
	}
	f.Touch()
	return f.shadowEntry(sh), nil
}

func (f *FS) readShadow(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	if f.w == nil {
		return 0, io.EOF
	}
	sh, err := f.shadowOf(ctx, n)
	if err != nil {
		return 0, err
	}
	file, err := os.Open(f.blobPath(sh))
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	return file.ReadAt(p, off)
}

func (f *FS) writeShadow(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	sh, err := f.shadowOf(ctx, n)
	if err != nil {
		return 0, err
	}
	file, err := os.OpenFile(f.blobPath(sh), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0, err
	}
	got, err := file.WriteAt(p, off)
	f.wstats.shadowWrites.Add(1)
	f.wstats.shadowBytes.Add(int64(got))
	return got, errors.Join(err, file.Close())
}

func (f *FS) truncateShadow(ctx context.Context, n Node, size int64) error {
	sh, err := f.shadowOf(ctx, n)
	if err != nil {
		return err
	}
	return os.Truncate(f.blobPath(sh), size)
}

func (f *FS) removeShadow(ctx context.Context, sh index.Shadow) error {
	if !sh.Dir {
		if err := os.Remove(f.blobPath(sh)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := f.ix.DeleteShadow(ctx, sh.Path); err != nil {
		return f.ioErr("shadow remove", err)
	}
	f.Touch()
	return nil
}

func (f *FS) journalAttr() Attr {
	a := Attr{Mode: 0o444}
	if info, err := os.Stat(f.journal); err == nil {
		a.Size, a.Mtime = info.Size(), info.ModTime()
	}
	return a
}

func (f *FS) readJournal(p []byte, off int64) (int, error) {
	file, err := os.Open(f.journal)
	if errors.Is(err, os.ErrNotExist) {
		return 0, io.EOF
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	return file.ReadAt(p, off)
}
