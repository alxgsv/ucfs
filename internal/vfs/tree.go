package vfs

import (
	"context"
	"errors"
	"hash/fnv"
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
)

// treeTTL is short: an overwrite or a rename changes what a tree name
// stands for.
const treeTTL = time.Second

// treeMount is the mount path of a tree directory: "/" is tree, "/a" tree/a.
func treeMount(dir string) string {
	if dir == "/" {
		return TreeDir
	}
	return TreeDir + dir
}

// treeChild joins a tree directory and a name into a tree path.
func treeChild(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// naturalIno is the inode number of a tree file the mount never wrote:
// distinct from its files/ node, so tools that compare inodes do not take
// the two for hard links.
func naturalIno(uuid string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("tree\x00" + uuid))
	return h.Sum64()
}

// objectNode is the tree node of an uploaded file.
func objectNode(file index.File) Node {
	if file.Object.Ino != 0 {
		return Node{Kind: KindTreeFile, UUID: file.Object.Key, Ino: file.Object.Ino}
	}
	return Node{Kind: KindTreeFile, UUID: file.UUID, Ino: naturalIno(file.UUID)}
}

func spoolNode(in spool.Info) Node {
	return Node{Kind: KindTreeFile, UUID: in.Key, Ino: in.Ino}
}

// occupant is what an object holds now: a spool while it is written, then
// the uploaded file.
type occupant struct {
	spool *spool.Spool
	file  index.File
}

func (f *FS) occupant(ctx context.Context, n Node) (occupant, error) {
	if f.w != nil {
		if s := f.w.Spools.ByIno(n.Ino); s != nil {
			return occupant{spool: s}, nil
		}
	}
	uuid, o, err := f.ix.ObjectByIno(ctx, n.Ino)
	switch {
	case err == nil && o.Key != n.UUID:
		return occupant{}, syscall.ESTALE
	case err == nil:
	case errors.Is(err, index.ErrNotFound) && n.UUID != "" && naturalIno(n.UUID) == n.Ino:
		uuid = n.UUID
	case errors.Is(err, index.ErrNotFound):
		return occupant{}, syscall.ENOENT
	default:
		return occupant{}, f.ioErr("object lookup", err)
	}
	file, err := f.file(ctx, uuid)
	if err != nil {
		return occupant{}, err
	}
	return occupant{file: file}, nil
}

func (f *FS) fileMode() os.FileMode {
	if f.w != nil {
		return 0o644
	}
	return 0o444
}

func (f *FS) occupantAttr(o occupant) Attr {
	if o.spool != nil {
		in := o.spool.Info()
		return Attr{Mode: 0o644, Size: in.Size, Mtime: in.Modified, TTL: 0}
	}
	return Attr{Mode: f.fileMode(), Size: o.file.Size, Mtime: o.file.UploadedAt, TTL: treeTTL}
}

func (f *FS) spoolEntry(s *spool.Spool) Entry {
	in := s.Info()
	return Entry{Name: path.Base(in.Path), Node: spoolNode(in), Attr: Attr{Mode: 0o644, Size: in.Size, Mtime: in.Modified}}
}

// spoolEntries lists the spools directly in dir, a mount path.
func (f *FS) spoolEntries(dir string) []Entry {
	if f.w == nil {
		return nil
	}
	var out []Entry
	for _, s := range f.w.Spools.In(dir) {
		out = append(out, f.spoolEntry(s))
	}
	return out
}

// readObject reads what a tree file holds: the spool's bytes while it is
// written, the uploaded file through the cache after. A spool that finished
// between the lookup and the read is looked up again.
func (f *FS) readObject(ctx context.Context, n Node, p []byte, off int64) (int, error) {
	for range 2 {
		o, err := f.occupant(ctx, n)
		if err != nil {
			return 0, err
		}
		if o.spool == nil {
			return f.readFile(ctx, Node{Kind: KindFile, UUID: o.file.UUID}, o.file, p, off)
		}
		got, err := o.spool.ReadAt(p, off)
		if !errors.Is(err, syscall.ESTALE) {
			return got, err
		}
	}
	return 0, syscall.ESTALE
}

var suffixRe = regexp.MustCompile(`^(.*)~([0-9a-f]{8})(\.[^.~]*)?$`)

// suffixed tells apart files that share a name: "photo.jpg" of uuid
// 1a2b3c4d-… becomes "photo~1a2b3c4d.jpg".
func suffixed(name, uuid string) string {
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	tail := "~" + uuid[:8] + ext
	return truncate(stem, nameMax-len(tail)) + tail
}

// unsuffix splits a suffixed name into the shared name and the uuid prefix.
func unsuffix(name string) (base, prefix string, ok bool) {
	m := suffixRe.FindStringSubmatch(name)
	if m == nil || m[1] == "" {
		return "", "", false
	}
	return m[1] + m[3], m[2], true
}

// pickSuffixed finds the file a suffixed name stands for among those
// sharing the plain name: the one whose uuid starts with the suffix. The
// plain name itself stands for the newest.
func pickSuffixed(group []index.File, prefix string) (index.File, bool) {
	for _, file := range group {
		if strings.HasPrefix(file.UUID, prefix) {
			return file, true
		}
	}
	return index.File{}, false
}

// treeName is the name a file has in its tree directory.
func treeName(file index.File) string {
	_, name, _ := index.ParseTreePath(file.TreePath)
	return truncate(name, nameMax)
}

// taken reports whether a name in a tree directory belongs to something
// other than uploaded files: a spool, a directory or a shadow entry.
func (f *FS) taken(ctx context.Context, dir, name string) (bool, error) {
	if IsShadowName(name) || (dir == "/" && name == InboxDir) {
		return true, nil
	}
	if f.w != nil && f.w.Spools.At(treeMount(treeChild(dir, name))) != nil {
		return true, nil
	}
	return f.ix.TreeDirExists(ctx, treeChild(dir, name))
}

func (f *FS) lookupTree(ctx context.Context, dir, name string) (Entry, error) {
	if dir == "/" && name == InboxDir {
		return Entry{Name: name, Node: Node{Kind: KindInbox}, Attr: f.dirAttr(dirTTL)}, nil
	}
	if IsShadowName(name) {
		return f.lookupShadow(ctx, treeMount(treeChild(dir, name)))
	}
	child := treeChild(dir, name)
	if f.w != nil {
		if s := f.w.Spools.At(treeMount(child)); s != nil {
			e := f.spoolEntry(s)
			e.Name = name
			return e, nil
		}
	}
	isDir, err := f.ix.TreeDirExists(ctx, child)
	if err != nil {
		return Entry{}, f.ioErr("tree lookup", err)
	}
	if isDir {
		return Entry{Name: name, Node: Node{Kind: KindTreeDir, Ops: norm.NFC.String(child)}, Attr: f.writableDirAttr(dirTTL)}, nil
	}
	group, err := f.ix.TreeLookup(ctx, dir, name)
	if err != nil {
		return Entry{}, f.ioErr("tree lookup", err)
	}
	var file index.File
	if len(group) > 0 {
		file = group[0]
	} else {
		base, prefix, sok := unsuffix(name)
		if !sok {
			return Entry{}, syscall.ENOENT
		}
		if group, err = f.ix.TreeLookup(ctx, dir, base); err != nil {
			return Entry{}, f.ioErr("tree lookup", err)
		}
		var ok bool
		if file, ok = pickSuffixed(group, prefix); !ok {
			return Entry{}, syscall.ENOENT
		}
	}
	return Entry{Name: name, Node: objectNode(file), Attr: f.occupantAttr(occupant{file: file})}, nil
}

// readTree lists a tree directory: its subdirectories, spools and shadow
// entries, and its uploaded files, where files that share a name, or whose
// name is taken by one of the others, get a suffix. A file an overwrite in
// progress replaces is left out until the overwrite finishes.
func (f *FS) readTree(ctx context.Context, dir string) ([]Entry, error) {
	var out []Entry
	if dir == "/" {
		out = append(out, Entry{Name: InboxDir, Node: Node{Kind: KindInbox}, Attr: f.dirAttr(dirTTL)})
	}
	subdirs, err := f.ix.TreeSubdirs(ctx, dir)
	if err != nil {
		return nil, f.ioErr("tree listing", err)
	}
	for _, name := range subdirs {
		out = append(out, Entry{Name: name, Node: Node{Kind: KindTreeDir, Ops: treeChild(dir, name)}, Attr: f.writableDirAttr(dirTTL)})
	}
	out = append(out, f.spoolEntries(treeMount(dir))...)
	out, err = f.withShadows(ctx, treeMount(dir), out)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, e := range out {
		taken[norm.NFC.String(e.Name)] = true
	}
	hidden := f.replacing()
	files, err := f.ix.TreeFiles(ctx, dir)
	if err != nil {
		return nil, f.ioErr("tree listing", err)
	}
	return append(out, groupEntries(files, treeName, taken, hidden, f.occupantAttr)...), nil
}

// replacing returns the files that spools stand for until they finish:
// those overwrites in progress replace, and multipart uploads in progress,
// which the API lists before they complete.
func (f *FS) replacing() map[string]bool {
	out := map[string]bool{}
	if f.w == nil {
		return out
	}
	for _, in := range f.w.Spools.List() {
		for _, uuid := range []string{in.Replaces, in.UploadUUID} {
			if uuid != "" {
				out[uuid] = true
			}
		}
	}
	return out
}

// groupEntries names files that arrive grouped by name, newest first.
func groupEntries(files []index.File, nameOf func(index.File) string, taken, hidden map[string]bool, attr func(occupant) Attr) []Entry {
	var out []Entry
	prev := ""
	first := true
	for _, file := range files {
		name := nameOf(file)
		key := norm.NFC.String(name)
		newGroup := first || key != prev
		first, prev = false, key
		if hidden[file.UUID] {
			continue
		}
		if !newGroup || taken[key] {
			name = suffixed(name, file.UUID)
		}
		out = append(out, Entry{Name: name, Node: objectNode(file), Attr: attr(occupant{file: file})})
	}
	return out
}

// inboxName is a file's name in tree/_inbox: its original name, mapped as
// in files/<uuid>/.
func inboxName(file index.File) string { return baseName(file) }

// inboxKeys are the original names that can show as name in tree/_inbox.
func inboxKeys(name string) []string {
	keys := []string{name, strings.NewReplacer("／", "/", "␀", "\x00").Replace(name)}
	if name == unnamed {
		keys = append(keys, "", ".", "..", VariantsDir)
	}
	return keys
}

func (f *FS) lookupInbox(ctx context.Context, name string) (Entry, error) {
	if IsShadowName(name) {
		return f.lookupShadow(ctx, TreeDir+"/"+InboxDir+"/"+name)
	}
	match := func(name string) ([]index.File, error) {
		all, err := f.ix.InboxLookup(ctx, inboxKeys(name)...)
		var group []index.File
		for _, file := range all {
			if sameName(inboxName(file), name) {
				group = append(group, file)
			}
		}
		return group, err
	}
	group, err := match(name)
	if err != nil {
		return Entry{}, f.ioErr("inbox lookup", err)
	}
	var file index.File
	if len(group) > 0 {
		file = group[0]
	} else {
		base, prefix, sok := unsuffix(name)
		if !sok {
			return Entry{}, syscall.ENOENT
		}
		if group, err = match(base); err != nil {
			return Entry{}, f.ioErr("inbox lookup", err)
		}
		var ok bool
		if file, ok = pickSuffixed(group, prefix); !ok {
			return Entry{}, syscall.ENOENT
		}
	}
	return Entry{Name: name, Node: objectNode(file), Attr: f.occupantAttr(occupant{file: file})}, nil
}

func (f *FS) readInbox(ctx context.Context) ([]Entry, error) {
	files, err := f.ix.InboxFiles(ctx)
	if err != nil {
		return nil, f.ioErr("inbox listing", err)
	}
	out, err := f.withShadows(ctx, TreeDir+"/"+InboxDir, nil)
	if err != nil {
		return nil, err
	}
	taken := map[string]bool{}
	for _, e := range out {
		taken[norm.NFC.String(e.Name)] = true
	}
	return append(out, groupEntries(files, inboxName, taken, f.replacing(), f.occupantAttr)...), nil
}

// objectPath is the mount path of an object, for front-ends that address
// files by path: the spool's path, the file's tree path, a live alias in
// files/, or its name in tree/_inbox.
func (f *FS) objectPath(ctx context.Context, n Node) ([]string, error) {
	o, err := f.occupant(ctx, n)
	if err != nil {
		return nil, err
	}
	if o.spool != nil {
		return split(o.spool.Info().Path), nil
	}
	file := o.file
	if dir, name, ok := index.ParseTreePath(file.TreePath); ok {
		taken, err := f.taken(ctx, dir, name)
		if err != nil {
			return nil, f.ioErr("tree path", err)
		}
		group, err := f.ix.TreeLookup(ctx, dir, name)
		if err != nil {
			return nil, f.ioErr("tree path", err)
		}
		if taken || len(group) == 0 || group[0].UUID != file.UUID {
			name = suffixed(name, file.UUID)
		}
		return append(split(treeMount(norm.NFC.String(dir))), name), nil
	}
	if alias, err := f.ix.AliasOf(ctx, FilesDir, file.UUID, f.now()); err == nil {
		return []string{FilesDir, alias}, nil
	}
	name := inboxName(file)
	all, err := f.ix.InboxLookup(ctx, inboxKeys(name)...)
	if err != nil {
		return nil, f.ioErr("inbox path", err)
	}
	for _, other := range all {
		if sameName(inboxName(other), name) {
			if other.UUID != file.UUID {
				name = suffixed(name, file.UUID)
			}
			break
		}
	}
	return []string{TreeDir, InboxDir, name}, nil
}

// lookupWritten resolves a name written in files/: a spool still being
// written, a live alias of an uploaded file, or a shadow file.
func (f *FS) lookupWritten(ctx context.Context, name string) (Entry, error) {
	if IsShadowName(name) {
		return f.lookupShadow(ctx, FilesDir+"/"+name)
	}
	if f.w != nil {
		if s := f.w.Spools.At(FilesDir + "/" + name); s != nil {
			e := f.spoolEntry(s)
			e.Name = name
			return e, nil
		}
	}
	uuid, err := f.ix.Alias(ctx, FilesDir, name, f.now())
	switch {
	case errors.Is(err, index.ErrNotFound):
		return Entry{}, syscall.ENOENT
	case err != nil:
		return Entry{}, f.ioErr("alias lookup", err)
	}
	file, err := f.file(ctx, uuid)
	if err != nil {
		return Entry{}, err
	}
	return Entry{Name: name, Node: objectNode(file), Attr: f.occupantAttr(occupant{file: file})}, nil
}
