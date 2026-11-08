package vfs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf8"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
)

// REST is the part of api.Client that writes need beyond uploads.
type REST interface {
	DeleteStorage(ctx context.Context, uuid string) error
	SetMetadata(ctx context.Context, uuid, key, value string) error
	DeleteMetadata(ctx context.Context, uuid, key string) error
	SetTags(ctx context.Context, uuid string, tags []string) ([]string, error)
}

// Writes makes a mount writable.
type Writes struct {
	Spools *spool.Manager
	API    REST
	// ShadowDir holds the bytes of local-only files.
	ShadowDir string
	// Foreign lets rm, overwrites and mv change files without a tree path
	// that the mount did not write: files/<uuid>_<name> and tree/_inbox/.
	// Without it a stray rm -r cannot empty a project.
	Foreign bool
	// Keep is the keep overwrite policy, which rename-over follows too.
	Keep bool
}

type writeStats struct {
	shadowCreates, shadowWrites, shadowBytes atomic.Int64
}

// WriteStats counts what the mount kept local.
type WriteStats struct {
	ShadowCreates, ShadowWrites, ShadowBytes int64
}

// renameWarn is the size of a directory rename worth a warning: each file
// below it costs one metadata request.
const renameWarn = 1000

// EnableWrites makes the filesystem writable.
func (f *FS) EnableWrites(w *Writes) {
	f.w = w
	w.Spools.OnChange = f.Touch
}

func (f *FS) Writable() bool { return f.w != nil }

func (f *FS) WriteStats() WriteStats {
	return WriteStats{ShadowCreates: f.wstats.shadowCreates.Load(), ShadowWrites: f.wstats.shadowWrites.Load(),
		ShadowBytes: f.wstats.shadowBytes.Load()}
}

// mountPath is the path of a directory node relative to the mount root,
// for the kinds that hold written entries.
func (f *FS) mountPath(dir Node) (string, bool) {
	switch dir.Kind {
	case KindRoot:
		return "", true
	case KindFilesDir:
		return FilesDir, true
	case KindTreeDir:
		return treeMount(dir.Ops), true
	case KindInbox:
		return TreeDir + "/" + InboxDir, true
	case KindShadowDir:
		return dir.Ops, true
	}
	return "", false
}

// PathNode is the node of a kind known by path at a mount path, for
// front-ends that track paths themselves.
func PathNode(kind Kind, mountPath string) Node {
	if kind == KindTreeDir {
		ops := strings.TrimPrefix(mountPath, TreeDir)
		if ops == "" {
			ops = "/"
		}
		return Node{Kind: kind, Ops: ops}
	}
	return Node{Kind: kind, Ops: mountPath}
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// checkTreeName rejects names a file in tree/ cannot be uploaded with or
// whose ucfs:path would not fit in a metadata value.
func checkTreeName(dir, name string) error {
	if err := spool.ValidName(name); err != nil {
		return err
	}
	if utf8.RuneCountInString(treeChild(dir, name)) > index.MaxTreePath {
		return syscall.ENAMETOOLONG
	}
	return nil
}

// foreign reports whether a file is outside what the mount may change by
// default: no tree path, and not written through the mount.
func (f *FS) foreign(file index.File) bool {
	_, _, inTree := index.ParseTreePath(file.TreePath)
	return !inTree && file.Object.Ino == 0
}

// Create makes a file, or truncates an existing one, which for an uploaded
// file starts an overwrite.
func (f *FS) Create(ctx context.Context, dir Node, name string) (Entry, error) {
	if f.w == nil {
		return Entry{}, syscall.EROFS
	}
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return Entry{}, syscall.EINVAL
	}
	base, ok := f.mountPath(dir)
	if !ok {
		return Entry{}, syscall.EROFS
	}
	if IsShadowName(name) || dir.Kind == KindShadowDir {
		return f.createShadow(ctx, join(base, name))
	}
	switch dir.Kind {
	case KindRoot, KindInbox:
		return Entry{}, syscall.EPERM
	case KindFilesDir:
		if _, _, isUUID := parseName(name); isUUID {
			return Entry{}, syscall.EROFS
		}
		if err := spool.ValidName(name); err != nil {
			return Entry{}, err
		}
	case KindTreeDir:
		if dir.Ops == "/" && name == InboxDir {
			return Entry{}, syscall.EISDIR
		}
		if err := checkTreeName(dir.Ops, name); err != nil {
			return Entry{}, err
		}
	}
	e, err := f.Lookup(ctx, dir, name)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return f.newSpool(ctx, join(base, name), index.Object{Ino: spool.NewIno()}, "", name)
	case err != nil:
		return Entry{}, err
	case e.Node.IsDir():
		return Entry{}, syscall.EISDIR
	}
	if err := f.truncateObject(ctx, e.Node, 0); err != nil {
		return Entry{}, err
	}
	return f.Lookup(ctx, dir, name)
}

func (f *FS) newSpool(ctx context.Context, p string, o index.Object, replaces, name string) (Entry, error) {
	s, err := f.w.Spools.Create(ctx, p, o, replaces)
	if err != nil {
		return Entry{}, err
	}
	f.Touch()
	e := f.spoolEntry(s)
	e.Name = name
	return e, nil
}

// overwrite starts writing a new version of an uploaded file in place of
// it: a spool that keeps the file's object, at the file's own path.
func (f *FS) overwrite(ctx context.Context, n Node, file index.File) (*spool.Spool, error) {
	if f.foreign(file) && !f.w.Foreign {
		return nil, syscall.EPERM
	}
	p, err := f.objectPath(ctx, n)
	if err != nil {
		return nil, err
	}
	mp := strings.Join(p, "/")
	if dir, name, ok := index.ParseTreePath(file.TreePath); ok {
		mp = treeMount(treeChild(dir, name))
	}
	if strings.HasPrefix(mp, TreeDir+"/"+InboxDir+"/") {
		mp = FilesDir + "/" + path.Base(mp)
	}
	return f.w.Spools.Create(ctx, mp, index.Object{Ino: n.Ino, Key: n.UUID}, file.UUID)
}

// reopen continues a write after its file was uploaded too early, as when
// a slow writer paused for longer than the idle timer: an overwrite that
// starts from the uploaded content.
func (f *FS) reopen(ctx context.Context, n Node, file index.File) (*spool.Spool, error) {
	s, err := f.overwrite(ctx, n, file)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1<<20)
	for off := int64(0); off < file.Size; {
		got, rerr := f.readFile(ctx, Node{Kind: KindFile, UUID: file.UUID}, file, buf, off)
		if got > 0 {
			if _, err := s.WriteAt(buf[:got], off); err != nil {
				return nil, err
			}
			off += int64(got)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			_ = f.w.Spools.Drop(ctx, s)
			return nil, rerr
		}
		if got == 0 {
			break
		}
	}
	slog.Info("write continues an uploaded file", "uuid", file.UUID, "size", file.Size)
	return s, nil
}

// writable returns the spool a write to an object goes to.
func (f *FS) writable(ctx context.Context, n Node, reopen bool) (*spool.Spool, index.File, error) {
	o, err := f.occupant(ctx, n)
	if err != nil {
		return nil, index.File{}, err
	}
	if o.spool != nil {
		return o.spool, index.File{}, nil
	}
	if reopen && f.w.Spools.Recent(o.file.UUID) {
		s, err := f.reopen(ctx, n, o.file)
		return s, o.file, err
	}
	return nil, o.file, nil
}

func (f *FS) WriteAt(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	if f.w == nil {
		return 0, syscall.EROFS
	}
	switch n.Kind {
	case KindTreeFile:
		s, _, err := f.writable(ctx, n, true)
		if err != nil {
			return 0, err
		}
		if s == nil {
			return 0, syscall.EOPNOTSUPP
		}
		return s.WriteAt(p, off)
	case KindShadow:
		return f.writeShadow(ctx, n, p, off)
	}
	if n.IsDir() {
		return 0, syscall.EISDIR
	}
	return 0, syscall.EROFS
}

// Truncate sets a file's size. An uploaded file can be truncated to its
// own size, which changes nothing, or to zero, which starts an overwrite;
// files are immutable otherwise.
func (f *FS) Truncate(ctx context.Context, n Node, size int64) error {
	switch n.Kind {
	case KindTreeFile:
		if f.w == nil {
			return syscall.EROFS
		}
		return f.truncateObject(ctx, n, size)
	case KindShadow:
		if f.w == nil {
			return syscall.EROFS
		}
		return f.truncateShadow(ctx, n, size)
	}
	if n.IsDir() {
		return syscall.EISDIR
	}
	a, err := f.Getattr(ctx, n)
	if err == nil && a.Size != size {
		err = syscall.EROFS
	}
	return err
}

func (f *FS) truncateObject(ctx context.Context, n Node, size int64) error {
	o, err := f.occupant(ctx, n)
	if err != nil {
		return err
	}
	s := o.spool
	switch {
	case s != nil:
	case size == o.file.Size:
		return nil
	case size == 0:
		if _, err := f.overwrite(ctx, n, o.file); err != nil {
			return err
		}
		f.Touch()
		return nil
	case f.w.Spools.Recent(o.file.UUID):
		if s, err = f.reopen(ctx, n, o.file); err != nil {
			return err
		}
	default:
		return syscall.EOPNOTSUPP
	}
	return s.Truncate(size)
}

// OpenWrite checks that a node takes writes and applies O_TRUNC.
func (f *FS) OpenWrite(ctx context.Context, n Node, trunc bool) error {
	switch {
	case n.IsDir():
		return syscall.EISDIR
	case f.w == nil || (n.Kind != KindTreeFile && n.Kind != KindShadow):
		return syscall.EROFS
	case trunc:
		return f.Truncate(ctx, n, 0)
	}
	return nil
}

// Commit is NFS COMMIT: the upload starts once no write follows shortly.
func (f *FS) Commit(ctx context.Context, n Node) error {
	if f.w == nil || n.Kind != KindTreeFile {
		return nil
	}
	if s := f.w.Spools.ByIno(n.Ino); s != nil {
		s.Commit()
	}
	return nil
}

// Flush is FUSE close: it uploads what was written and returns the
// upload's error.
func (f *FS) Flush(ctx context.Context, n Node) error {
	if f.w == nil || n.Kind != KindTreeFile {
		return nil
	}
	if s := f.w.Spools.ByIno(n.Ino); s != nil {
		return s.Flush(ctx)
	}
	return nil
}

// Remove soft-deletes an uploaded file, drops a spool, or removes a
// shadow file. Files outside tree/ that the mount did not write need
// Foreign.
func (f *FS) Remove(ctx context.Context, dir Node, name string) error {
	if f.w == nil || dir.Kind == KindViewDir {
		return syscall.EROFS
	}
	e, err := f.Lookup(ctx, dir, name)
	if err != nil {
		return err
	}
	switch e.Node.Kind {
	case KindTreeFile:
		o, err := f.occupant(ctx, e.Node)
		if err != nil {
			return err
		}
		if o.spool != nil {
			err = f.w.Spools.Drop(ctx, o.spool)
		} else {
			err = f.deleteFile(ctx, o.file)
		}
		if err != nil {
			return err
		}
		if dir.Kind == KindFilesDir {
			_ = f.ix.DeleteAlias(ctx, FilesDir, name)
		}
	case KindFile:
		file, err := f.file(ctx, e.Node.UUID)
		if err != nil {
			return err
		}
		if err := f.deleteFile(ctx, file); err != nil {
			return err
		}
	case KindShadow:
		sh, err := f.shadowOf(ctx, e.Node)
		if err != nil {
			return err
		}
		return f.removeShadow(ctx, sh)
	default:
		if e.Node.IsDir() {
			return syscall.EISDIR
		}
		return syscall.EPERM
	}
	f.keepDir(ctx, dir)
	f.Touch()
	return nil
}

// keepDir makes a tree directory outlast its last file, as directories do
// until rmdir.
func (f *FS) keepDir(ctx context.Context, dir Node) {
	if dir.Kind == KindTreeDir && dir.Ops != "/" {
		if err := f.ix.PutTreeDir(ctx, dir.Ops); err != nil {
			slog.Warn("keep tree dir", "dir", dir.Ops, "err", err)
		}
	}
}

func (f *FS) deleteFile(ctx context.Context, file index.File) error {
	if f.foreign(file) && !f.w.Foreign {
		return syscall.EPERM
	}
	if err := f.w.API.DeleteStorage(ctx, file.UUID); err != nil && api.Errno(err) != syscall.ENOENT {
		slog.Error("delete", "uuid", file.UUID, "err", err)
		return api.Errno(err)
	}
	if err := f.ix.MarkRemoved(ctx, file.UUID); err != nil {
		return f.ioErr("record removal", err)
	}
	slog.Info("deleted", "uuid", file.UUID, "name", file.Name)
	return nil
}

// Mkdir makes a tree directory, which lives in the local index until a file
// below it is uploaded, or a shadow directory.
func (f *FS) Mkdir(ctx context.Context, dir Node, name string) (Entry, error) {
	if f.w == nil {
		return Entry{}, syscall.EROFS
	}
	base, ok := f.mountPath(dir)
	if !ok {
		return Entry{}, syscall.EROFS
	}
	if name == "" || name == "." || name == ".." {
		return Entry{}, syscall.EEXIST
	}
	if IsShadowName(name) || dir.Kind == KindShadowDir {
		return f.mkdirShadow(ctx, join(base, name))
	}
	if dir.Kind != KindTreeDir {
		return Entry{}, syscall.EPERM
	}
	if _, err := f.Lookup(ctx, dir, name); err == nil {
		return Entry{}, syscall.EEXIST
	}
	if utf8.RuneCountInString(treeChild(dir.Ops, name)) >= index.MaxTreePath {
		return Entry{}, syscall.ENAMETOOLONG
	}
	child := treeChild(dir.Ops, name)
	if err := f.ix.PutTreeDir(ctx, child); err != nil {
		return Entry{}, f.ioErr("mkdir", err)
	}
	f.Touch()
	return f.Lookup(ctx, dir, name)
}

// Rmdir removes an empty tree or shadow directory.
func (f *FS) Rmdir(ctx context.Context, dir Node, name string) error {
	if f.w == nil {
		return syscall.EROFS
	}
	e, err := f.Lookup(ctx, dir, name)
	if err != nil {
		return err
	}
	if !e.Node.IsDir() {
		return syscall.ENOTDIR
	}
	if e.Node.Kind == KindViewDir {
		return syscall.EROFS
	}
	if e.Node.Kind != KindTreeDir && e.Node.Kind != KindShadowDir {
		return syscall.EPERM
	}
	entries, err := f.ReadDir(ctx, e.Node)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return syscall.ENOTEMPTY
	}
	if e.Node.Kind == KindShadowDir {
		return f.removeShadow(ctx, index.Shadow{Path: e.Node.Ops, Dir: true})
	}
	if err := f.ix.DeleteTreeDir(ctx, e.Node.Ops); err != nil {
		return f.ioErr("rmdir", err)
	}
	f.keepDir(ctx, dir)
	f.Touch()
	return nil
}

// Rename moves an entry. In tree/ an uploaded file moves with one metadata
// update, a directory with one per file below it, and a spool before its
// upload. A local-only file renamed to an ordinary name in tree/ or files/
// becomes a spool and is uploaded: that is how a safe save that wrote into
// a scratch directory lands.
func (f *FS) Rename(ctx context.Context, dir Node, name string, newDir Node, newName string) error {
	if f.w == nil || dir.Kind == KindViewDir || newDir.Kind == KindViewDir {
		return syscall.EROFS
	}
	src, err := f.Lookup(ctx, dir, name)
	if err != nil {
		return err
	}
	base, ok := f.mountPath(newDir)
	if !ok {
		return syscall.EXDEV
	}
	if newName == "" || newName == "." || newName == ".." || strings.ContainsRune(newName, '/') {
		return syscall.EINVAL
	}
	to := join(base, newName)
	toShadow := IsShadowName(newName) || newDir.Kind == KindShadowDir
	dst, derr := f.Lookup(ctx, newDir, newName)
	exists := derr == nil
	if derr != nil && !errors.Is(derr, syscall.ENOENT) {
		return derr
	}
	if exists && dst.Node == src.Node {
		return nil
	}
	if exists && dst.Node.IsDir() != src.Node.IsDir() {
		if dst.Node.IsDir() {
			return syscall.EISDIR
		}
		return syscall.ENOTDIR
	}
	defer f.Touch()

	switch src.Node.Kind {
	case KindShadow, KindShadowDir:
		from, _ := f.mountPath(dir)
		return f.renameShadow(ctx, join(from, name), src, to, toShadow, newDir, newName, dst, exists)
	case KindTreeFile:
		if toShadow || newDir.Kind == KindRoot {
			return syscall.EXDEV
		}
		return f.renameObject(ctx, dir, src.Node, newDir, newName, dst, exists)
	case KindTreeDir:
		if newDir.Kind != KindTreeDir || toShadow {
			return syscall.EXDEV
		}
		return f.renameTreeDir(ctx, src.Node.Ops, treeChild(newDir.Ops, newName), dst, exists)
	case KindFile:
		file, err := f.file(ctx, src.Node.UUID)
		if err != nil {
			return err
		}
		if newDir.Kind != KindTreeDir || toShadow {
			return syscall.EXDEV
		}
		return f.moveFile(ctx, objectNode(file), file, newDir, newName, dst, exists)
	}
	return syscall.EPERM
}

// replaceDst clears the destination of a rename: a spool is dropped, an
// uploaded file follows the overwrite policy, a shadow file is removed and
// an empty directory removed.
func (f *FS) replaceDst(ctx context.Context, dst Entry, newDir Node, newName string) error {
	switch dst.Node.Kind {
	case KindTreeFile:
		o, err := f.occupant(ctx, dst.Node)
		if err != nil {
			return err
		}
		if o.spool != nil {
			return f.w.Spools.Drop(ctx, o.spool)
		}
		if f.w.Keep {
			return nil
		}
		return f.deleteFile(ctx, o.file)
	case KindShadow:
		sh, err := f.shadowOf(ctx, dst.Node)
		if err != nil {
			return err
		}
		return f.removeShadow(ctx, sh)
	case KindTreeDir, KindShadowDir:
		return f.Rmdir(ctx, newDir, newName)
	}
	return syscall.EPERM
}

// dstReplaces is the uploaded file a spool moved onto dst replaces.
func (f *FS) dstReplaces(ctx context.Context, dst Entry, exists bool) (string, error) {
	if !exists || dst.Node.Kind != KindTreeFile {
		return "", nil
	}
	o, err := f.occupant(ctx, dst.Node)
	if err != nil || o.spool != nil {
		return "", err
	}
	if f.foreign(o.file) && !f.w.Foreign {
		return "", syscall.EPERM
	}
	return o.file.UUID, nil
}

func (f *FS) renameObject(ctx context.Context, dir, n, newDir Node, newName string, dst Entry, exists bool) error {
	base, _ := f.mountPath(newDir)
	to := join(base, newName)
	if newDir.Kind == KindTreeDir {
		if err := checkTreeName(newDir.Ops, newName); err != nil {
			return err
		}
	}
	for range 2 {
		o, err := f.occupant(ctx, n)
		if err != nil {
			return err
		}
		if o.spool == nil {
			return f.moveFile(ctx, n, o.file, newDir, newName, dst, exists)
		}
		if newDir.Kind != KindTreeDir && newDir.Kind != KindFilesDir {
			return syscall.EXDEV
		}
		replaces, err := f.dstReplaces(ctx, dst, exists)
		if err != nil {
			return err
		}
		in := o.spool.Info()
		if in.Replaces != "" && in.Path != to {
			// The overwrite moves away from the file it replaced, which
			// stays and needs an identity of its own again.
			if err := f.ix.PutObject(ctx, index.Object{Ino: spool.NewIno()}, in.Replaces); err != nil {
				return f.ioErr("rename", err)
			}
		}
		err = f.w.Spools.Move(ctx, o.spool, to, replaces)
		if errors.Is(err, syscall.ENOENT) {
			continue
		}
		if err == nil {
			f.keepDir(ctx, dir)
		}
		return err
	}
	return syscall.ESTALE
}

// moveFile moves an uploaded file: into or within tree/ by setting its
// ucfs:path, into tree/_inbox by deleting it.
func (f *FS) moveFile(ctx context.Context, n Node, file index.File, newDir Node, newName string, dst Entry, exists bool) error {
	if f.foreign(file) && !f.w.Foreign {
		return syscall.EPERM
	}
	var value string
	switch newDir.Kind {
	case KindTreeDir:
		if err := checkTreeName(newDir.Ops, newName); err != nil {
			return err
		}
		value = treeChild(newDir.Ops, newName)
	case KindInbox:
	default:
		return syscall.EXDEV
	}
	if exists {
		if err := f.replaceDst(ctx, dst, newDir, newName); err != nil {
			return err
		}
	}
	var err error
	if value == "" {
		err = f.w.API.DeleteMetadata(ctx, file.UUID, index.TreePathKey)
	} else {
		err = f.w.API.SetMetadata(ctx, file.UUID, index.TreePathKey, value)
	}
	if err != nil {
		slog.Error("rename", "uuid", file.UUID, "to", value, "err", err)
		return api.Errno(err)
	}
	if err := f.ix.SetTreePath(ctx, file.UUID, value); err != nil {
		return f.ioErr("rename", err)
	}
	if dir, _, ok := index.ParseTreePath(file.TreePath); ok && dir != "/" {
		if err := f.ix.PutTreeDir(ctx, dir); err != nil {
			slog.Warn("keep tree dir", "dir", dir, "err", err)
		}
	}
	slog.Info("moved", "uuid", file.UUID, "to", value)
	return nil
}

// renameTreeDir moves every file below a directory, one metadata update
// each, along with spools, shadow entries and directories made by mkdir.
func (f *FS) renameTreeDir(ctx context.Context, from, to string, dst Entry, exists bool) error {
	if to == from || strings.HasPrefix(to, from+"/") {
		return syscall.EINVAL
	}
	if exists {
		if err := f.Rmdir(ctx, Node{Kind: KindTreeDir, Ops: path.Dir(to)}, path.Base(to)); err != nil {
			return err
		}
	}
	files, err := f.ix.TreeBelow(ctx, from)
	if err != nil {
		return f.ioErr("rename dir", err)
	}
	if len(files) > renameWarn {
		slog.Warn("directory rename updates the metadata of every file below it", "from", from, "to", to, "files", len(files))
	}
	for _, file := range files {
		value := to + file.TreePath[len(from):]
		if utf8.RuneCountInString(value) > index.MaxTreePath {
			return syscall.ENAMETOOLONG
		}
	}
	for _, file := range files {
		value := to + file.TreePath[len(from):]
		if err := f.w.API.SetMetadata(ctx, file.UUID, index.TreePathKey, value); err != nil {
			slog.Error("rename dir: file left behind", "uuid", file.UUID, "err", err)
			return api.Errno(err)
		}
		if err := f.ix.SetTreePath(ctx, file.UUID, value); err != nil {
			return f.ioErr("rename dir", err)
		}
	}
	for _, s := range f.w.Spools.Below(treeMount(from)) {
		in := s.Info()
		if err := f.w.Spools.Move(ctx, s, treeMount(to)+in.Path[len(treeMount(from)):], in.Replaces); err != nil && !errors.Is(err, syscall.ENOENT) {
			return err
		}
	}
	if err := f.ix.MoveShadow(ctx, treeMount(from), treeMount(to)); err != nil {
		return f.ioErr("rename dir", err)
	}
	if err := f.ix.MoveTreeDirs(ctx, from, to); err != nil {
		return f.ioErr("rename dir", err)
	}
	if err := f.ix.DeleteTreeDir(ctx, from); err != nil {
		return f.ioErr("rename dir", err)
	}
	f.movedDirs(ctx, from, to)
	if parent := path.Dir(from); parent != "/" {
		_ = f.ix.PutTreeDir(ctx, parent)
	}
	slog.Info("moved directory", "from", from, "to", to, "files", len(files))
	return nil
}

// renameShadow moves a local-only entry; renamed to an ordinary name in
// tree/ or files/, a shadow file becomes a spool that uploads at once.
func (f *FS) renameShadow(ctx context.Context, from string, src Entry, to string, toShadow bool, newDir Node, newName string, dst Entry, exists bool) error {
	sh, err := f.shadowOf(ctx, src.Node)
	if err != nil {
		return err
	}
	if toShadow {
		if exists {
			if err := f.replaceDst(ctx, dst, newDir, newName); err != nil {
				return err
			}
		}
		if err := f.ix.MoveShadow(ctx, from, to); err != nil {
			return f.ioErr("rename shadow", err)
		}
		if sh.Dir {
			if err := f.ix.MoveNodePaths(ctx, from, to); err != nil {
				slog.Warn("move directory handles", "from", from, "to", to, "err", err)
			}
		}
		return nil
	}
	if sh.Dir {
		return syscall.EXDEV
	}
	switch newDir.Kind {
	case KindTreeDir:
		if err := checkTreeName(newDir.Ops, newName); err != nil {
			return err
		}
	case KindFilesDir:
		if err := spool.ValidName(newName); err != nil {
			return err
		}
	default:
		return syscall.EXDEV
	}
	replaces, err := f.dstReplaces(ctx, dst, exists)
	if err != nil {
		return err
	}
	o := index.Object{Ino: spool.NewIno()}
	if exists && dst.Node.Kind == KindTreeFile {
		o = index.Object{Ino: dst.Node.Ino, Key: dst.Node.UUID}
	}
	s, err := f.w.Spools.Create(ctx, to, o, replaces)
	if err != nil {
		return err
	}
	data, err := os.Open(f.blobPath(sh))
	if err != nil {
		_ = f.w.Spools.Drop(ctx, s)
		return err
	}
	defer func() { _ = data.Close() }()
	buf := make([]byte, 1<<20)
	for off := int64(0); ; {
		got, rerr := data.ReadAt(buf, off)
		if got > 0 {
			if _, err := s.WriteAt(buf[:got], off); err != nil {
				_ = f.w.Spools.Drop(ctx, s)
				return err
			}
			off += int64(got)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				_ = f.w.Spools.Drop(ctx, s)
				return rerr
			}
			break
		}
	}
	s.Commit()
	return f.removeShadow(ctx, sh)
}
