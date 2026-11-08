package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

func writable(t *testing.T, w *vfs.Writes, files ...fake.File) *vfstest.Env {
	t.Helper()
	env := vfstest.New(t, files...)
	if w == nil {
		w = &vfs.Writes{}
	}
	env.Writable(t, spool.Options{Metadata: map[string]string{"ucfs:test": "1"}}, w)
	return env
}

func walk(t *testing.T, env *vfstest.Env, p string) vfs.Entry {
	t.Helper()
	e, err := env.FS.Walk(context.Background(), p)
	if err != nil {
		t.Fatalf("walk %s: %v", p, err)
	}
	return e
}

// put writes a file through the filesystem and waits for its upload.
func put(t *testing.T, env *vfstest.Env, dir, name string, body []byte) vfs.Entry {
	t.Helper()
	ctx := context.Background()
	d := walk(t, env, dir)
	e, err := env.FS.Create(ctx, d.Node, name)
	if err != nil {
		t.Fatalf("create %s/%s: %v", dir, name, err)
	}
	if _, err := env.FS.WriteAt(ctx, e.Node, body, 0); err != nil {
		t.Fatal(err)
	}
	if err := env.FS.Truncate(ctx, e.Node, int64(len(body))); err != nil {
		t.Fatalf("truncate to the same length: %v", err)
	}
	if err := env.FS.Flush(ctx, e.Node); err != nil {
		t.Fatalf("flush %s/%s: %v", dir, name, err)
	}
	return e
}

func list(t *testing.T, env *vfstest.Env, dir string) []string {
	t.Helper()
	entries, err := env.FS.ReadDir(context.Background(), walk(t, env, dir).Node)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	slices.Sort(out)
	return out
}

func read(t *testing.T, env *vfstest.Env, n vfs.Node) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 1<<20)
	for {
		got, err := env.FS.ReadAt(context.Background(), n, buf, int64(len(out)))
		out = append(out, buf[:got]...)
		if errors.Is(err, io.EOF) || (err == nil && got == 0) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func onlyUpload(t *testing.T, env *vfstest.Env, name string) fake.File {
	t.Helper()
	var match []fake.File
	files, _ := env.Index.Files(context.Background())
	for _, f := range files {
		if sf, ok := env.Fake.File(f.UUID); ok && sf.Name == name && !sf.Removed {
			match = append(match, sf)
		}
	}
	if len(match) != 1 {
		t.Fatalf("%d live uploads named %q", len(match), name)
	}
	return match[0]
}

func TestTreeCopy(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil, file(1, "foreign.jpg", 10))
	tree := walk(t, env, "tree")
	if _, err := env.FS.Mkdir(ctx, tree.Node, "photos"); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("photo"), 300000)
	created := put(t, env, "tree/photos", "photo.jpg", body)

	e := walk(t, env, "tree/photos/photo.jpg")
	if e.Node != created.Node {
		t.Errorf("node changed by the upload: %+v → %+v", created.Node, e.Node)
	}
	up := onlyUpload(t, env, "photo.jpg")
	if up.Metadata["ucfs:path"] != "/photos/photo.jpg" || up.Metadata["ucfs:test"] != "1" || !up.Stored {
		t.Errorf("uploaded %+v", up)
	}
	before, _ := env.Fake.CDNStats()
	if got := read(t, env, e.Node); !bytes.Equal(got, body) {
		t.Errorf("read back %d bytes, content mismatch", len(got))
	}
	if after, _ := env.Fake.CDNStats(); after != before {
		t.Errorf("reading a file just written made %d CDN requests", after-before)
	}
	if en, ok := env.Journal.Last("tree/photos/photo.jpg"); !ok || en.UUID != up.UUID {
		t.Errorf("journal: %+v %v", en, ok)
	}
	log := read(t, env, walk(t, env, ".ucfs/uploads.log").Node)
	if !strings.Contains(string(log), "\ttree/photos/photo.jpg\t"+up.UUID+"\t1500000\timage/jpeg\n") {
		t.Errorf("uploads.log = %q", log)
	}
	if got := list(t, env, "tree"); !slices.Equal(got, []string{"_inbox", "photos"}) {
		t.Errorf("tree/ = %q", got)
	}
	if got := list(t, env, "tree/_inbox"); !slices.Equal(got, []string{"foreign.jpg"}) {
		t.Errorf("tree/_inbox = %q", got)
	}
	if _, err := env.FS.Walk(ctx, "files/"+up.UUID+"_photo.jpg"); err != nil {
		t.Errorf("files/ entry: %v", err)
	}
	if _, err := env.FS.Walk(ctx, ".metadata_never_index"); err != nil {
		t.Errorf(".metadata_never_index: %v", err)
	}
}

func TestOverwriteKeepsTheObject(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil)
	first := put(t, env, "tree", "doc.txt", []byte("v1"))
	v1 := onlyUpload(t, env, "doc.txt")
	if err := env.Client.SetMetadata(ctx, v1.UUID, "camera", "Canon"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Index.Sync(ctx, env.Client, true); err != nil {
		t.Fatal(err)
	}

	// vim on macOS saves in place: truncate to zero, write, COMMIT.
	if err := env.FS.Truncate(ctx, first.Node, 0); err != nil {
		t.Fatal(err)
	}
	if got := list(t, env, "tree"); !slices.Equal(got, []string{"_inbox", "doc.txt"}) {
		t.Errorf("during the overwrite tree/ = %q", got)
	}
	if _, err := env.FS.WriteAt(ctx, first.Node, []byte("version two"), 0); err != nil {
		t.Fatal(err)
	}
	if err := env.FS.Flush(ctx, first.Node); err != nil {
		t.Fatal(err)
	}
	v2 := onlyUpload(t, env, "doc.txt")
	if v2.UUID == v1.UUID || v2.Metadata["camera"] != "Canon" {
		t.Errorf("new version %+v", v2)
	}
	if old, _ := env.Fake.File(v1.UUID); !old.Removed {
		t.Error("the replaced version was not deleted")
	}
	if e := walk(t, env, "tree/doc.txt"); e.Node != first.Node || string(read(t, env, e.Node)) != "version two" {
		t.Errorf("tree/doc.txt = %+v", e)
	}
	if got := list(t, env, "tree"); !slices.Equal(got, []string{"_inbox", "doc.txt"}) {
		t.Errorf("tree/ = %q", got)
	}
}

func TestSafeSaveThroughScratchDir(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil)
	put(t, env, "tree", "note.txt", []byte("old"))
	old := onlyUpload(t, env, "note.txt")
	tree := walk(t, env, "tree")
	stats := env.Fake.UploadStats()

	// NSDocument: write into a scratch directory beside the file, rename
	// over the original, remove the directory.
	scratch, err := env.FS.Mkdir(ctx, tree.Node, "(A Document Being Saved By TextEdit)")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := env.FS.Create(ctx, scratch.Node, "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.WriteAt(ctx, tmp.Node, []byte("saved by TextEdit"), 0); err != nil {
		t.Fatal(err)
	}
	if err := env.FS.Commit(ctx, tmp.Node); err != nil {
		t.Fatal(err)
	}
	if err := env.FS.Rename(ctx, scratch.Node, "note.txt", tree.Node, "note.txt"); err != nil {
		t.Fatal(err)
	}
	if err := env.FS.Rmdir(ctx, tree.Node, "(A Document Being Saved By TextEdit)"); err != nil {
		t.Fatal(err)
	}
	waitUploads(t, env)
	now := onlyUpload(t, env, "note.txt")
	if now.UUID == old.UUID || now.Metadata["ucfs:path"] != "/note.txt" {
		t.Errorf("saved %+v", now)
	}
	if string(read(t, env, walk(t, env, "tree/note.txt").Node)) != "saved by TextEdit" {
		t.Error("content mismatch")
	}
	if f, _ := env.Fake.File(old.UUID); !f.Removed {
		t.Error("the replaced version was not deleted")
	}
	if got := env.Fake.UploadStats().Base - stats.Base; got != 1 {
		t.Errorf("the save made %d uploads", got)
	}
	if got := list(t, env, "tree"); !slices.Equal(got, []string{"_inbox", "note.txt"}) {
		t.Errorf("tree/ = %q", got)
	}
}

func waitUploads(t *testing.T, env *vfstest.Env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(env.Spools.List()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("spools left: %+v", env.Spools.List())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRenameAndRemove(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil)
	tree := walk(t, env, "tree")
	a, _ := env.FS.Mkdir(ctx, tree.Node, "a")
	e := put(t, env, "tree/a", "one.jpg", []byte("1"))
	put(t, env, "tree/a", "two.jpg", []byte("2"))
	one := onlyUpload(t, env, "one.jpg")

	reqs := env.Fake.Requests()
	if err := env.FS.Rename(ctx, a.Node, "one.jpg", tree.Node, "uno.jpg"); err != nil {
		t.Fatal(err)
	}
	if got := env.Fake.Requests() - reqs; got != 1 {
		t.Errorf("rename made %d requests, want one metadata PUT", got)
	}
	if f, _ := env.Fake.File(one.UUID); f.Metadata["ucfs:path"] != "/uno.jpg" || f.Name != "one.jpg" {
		t.Errorf("renamed file %+v", f)
	}
	if got := walk(t, env, "tree/uno.jpg"); got.Node != e.Node {
		t.Errorf("rename changed the node: %+v", got.Node)
	}

	if err := env.FS.Rename(ctx, tree.Node, "a", tree.Node, "b"); err != nil {
		t.Fatal(err)
	}
	if got := list(t, env, "tree/b"); !slices.Equal(got, []string{"two.jpg"}) {
		t.Errorf("tree/b = %q", got)
	}
	if _, err := env.FS.Walk(ctx, "tree/a"); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("tree/a after the rename: %v", err)
	}

	b := walk(t, env, "tree/b")
	if err := env.FS.Remove(ctx, b.Node, "two.jpg"); err != nil {
		t.Fatal(err)
	}
	two := onlyUploadOrNone(env, "two.jpg")
	if two != "" {
		t.Errorf("two.jpg still live: %s", two)
	}
	if got := list(t, env, "tree/b"); len(got) != 0 {
		t.Errorf("tree/b after rm = %q", got)
	}
	if err := env.FS.Rmdir(ctx, tree.Node, "b"); err != nil {
		t.Errorf("rmdir of the emptied directory: %v", err)
	}
}

func onlyUploadOrNone(env *vfstest.Env, name string) string {
	files, _ := env.Index.Files(context.Background())
	for _, f := range files {
		if sf, ok := env.Fake.File(f.UUID); ok && sf.Name == name && !sf.Removed {
			return sf.UUID
		}
	}
	return ""
}

func TestShadowFiles(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil)
	tree := walk(t, env, "tree")
	for _, name := range []string{".DS_Store", "._photo.jpg", ".photo.jpg.swp"} {
		e, err := env.FS.Create(ctx, tree.Node, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.FS.WriteAt(ctx, e.Node, []byte("local"), 0); err != nil {
			t.Fatal(err)
		}
		if err := env.FS.Flush(ctx, e.Node); err != nil {
			t.Fatal(err)
		}
		if got := read(t, env, e.Node); string(got) != "local" {
			t.Errorf("%s = %q", name, got)
		}
	}
	if _, err := env.FS.Mkdir(ctx, walk(t, env, "").Node, ".TemporaryItems"); err != nil {
		t.Fatal(err)
	}
	tmp := walk(t, env, ".TemporaryItems")
	if _, err := env.FS.Create(ctx, tmp.Node, "scratch.txt"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if st := env.Fake.UploadStats(); st.Base+st.Start != 0 {
		t.Errorf("shadow files uploaded: %+v", st)
	}
	if got := list(t, env, "tree"); !slices.Equal(got, []string{".DS_Store", "._photo.jpg", ".photo.jpg.swp", "_inbox"}) {
		t.Errorf("tree/ = %q", got)
	}
	if st := env.FS.WriteStats(); st.ShadowCreates != 4 || st.ShadowWrites != 3 || st.ShadowBytes != 15 {
		t.Errorf("shadow stats %+v", st)
	}
	if err := env.FS.Remove(ctx, tree.Node, ".DS_Store"); err != nil {
		t.Fatal(err)
	}

	// A shadow file renamed to an ordinary name is a save: it uploads.
	if err := env.FS.Rename(ctx, tmp.Node, "scratch.txt", tree.Node, "kept.txt"); err != nil {
		t.Fatal(err)
	}
	e := walk(t, env, "tree/kept.txt")
	if _, err := env.FS.WriteAt(ctx, e.Node, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	waitUploads(t, env)
	onlyUpload(t, env, "kept.txt")
}

func TestWriteProtection(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil, file(1, "fixture.jpg", 10))
	root, files, inbox := walk(t, env, ""), walk(t, env, "files"), walk(t, env, "tree/_inbox")
	fixture := "files/" + uuid(1) + "_fixture.jpg"
	tests := []struct {
		name string
		err  error
		op   func() error
	}{
		{"rm files/<uuid>_name", syscall.EPERM, func() error { return env.FS.Remove(ctx, files.Node, uuid(1)+"_fixture.jpg") }},
		{"rm tree/_inbox/name", syscall.EPERM, func() error { return env.FS.Remove(ctx, inbox.Node, "fixture.jpg") }},
		{"truncate an inbox file", syscall.EPERM, func() error {
			return env.FS.Truncate(ctx, walk(t, env, "tree/_inbox/fixture.jpg").Node, 0)
		}},
		{"mv out of tree/_inbox", syscall.EPERM, func() error {
			return env.FS.Rename(ctx, inbox.Node, "fixture.jpg", walk(t, env, "tree").Node, "f.jpg")
		}},
		{"write files/<uuid>_name", syscall.EROFS, func() error { return env.FS.OpenWrite(ctx, walk(t, env, fixture).Node, true) }},
		{"create in files/<uuid>/", syscall.EROFS, func() error {
			_, err := env.FS.Create(ctx, walk(t, env, "files/"+uuid(1)).Node, "x")
			return err
		}},
		{"create in tree/_inbox", syscall.EPERM, func() error { _, err := env.FS.Create(ctx, inbox.Node, "x.jpg"); return err }},
		{"create at the root", syscall.EPERM, func() error { _, err := env.FS.Create(ctx, root.Node, "x.jpg"); return err }},
		{"backslash in a name", syscall.EINVAL, func() error {
			_, err := env.FS.Create(ctx, walk(t, env, "tree").Node, `a\b.txt`)
			return err
		}},
	}
	for _, tt := range tests {
		if err := tt.op(); !errors.Is(err, tt.err) {
			t.Errorf("%s: %v, want %v", tt.name, err, tt.err)
		}
	}
	if f, _ := env.Fake.File(uuid(1)); f.Removed || len(f.Metadata) != 0 {
		t.Errorf("fixture changed: %+v", f)
	}
}

func TestForeignNeedsOptIn(t *testing.T) {
	ctx := context.Background()
	env := writable(t, &vfs.Writes{Foreign: true}, file(1, "a.jpg", 10), file(2, "b.jpg", 10))
	files := walk(t, env, "files")
	if err := env.FS.Remove(ctx, files.Node, uuid(1)+"_a.jpg"); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(uuid(1)); !f.Removed {
		t.Error("not removed")
	}
	if err := env.FS.Rename(ctx, walk(t, env, "tree/_inbox").Node, "b.jpg", walk(t, env, "tree").Node, "b.jpg"); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(uuid(2)); f.Metadata["ucfs:path"] != "/b.jpg" {
		t.Errorf("moved out of the inbox: %+v", f)
	}
}

func TestAliasInFiles(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil)
	created := put(t, env, "files", "photo.jpg", []byte("aliased"))
	up := onlyUpload(t, env, "photo.jpg")
	e := walk(t, env, "files/photo.jpg")
	if e.Node != created.Node || string(read(t, env, e.Node)) != "aliased" {
		t.Errorf("files/photo.jpg = %+v", e)
	}
	if got := list(t, env, "files"); !slices.Equal(got, []string{up.UUID + "_photo.jpg"}) {
		t.Errorf("files/ = %q", got)
	}
	if got := list(t, env, "tree/_inbox"); !slices.Equal(got, []string{"photo.jpg"}) {
		t.Errorf("tree/_inbox = %q", got)
	}
	if err := env.FS.Remove(ctx, walk(t, env, "files").Node, "photo.jpg"); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(up.UUID); !f.Removed {
		t.Error("rm of the alias did not delete the file")
	}
}

func TestDuplicateTreePaths(t *testing.T) {
	ctx := context.Background()
	older := file(1, "a.jpg", 1)
	older.UUID = "1a2b3c4d-0000-4000-8000-000000000001"
	older.Metadata = map[string]string{"ucfs:path": "/x/a.jpg"}
	newer := file(2, "a.jpg", 2)
	newer.Metadata = map[string]string{"ucfs:path": "/x/a.jpg"}
	env := writable(t, nil, older, newer)
	want := []string{"a.jpg", "a~1a2b3c4d.jpg"}
	slices.Sort(want)
	if got := list(t, env, "tree/x"); !slices.Equal(got, want) {
		t.Errorf("tree/x = %q, want %q", got, want)
	}
	if e := walk(t, env, "tree/x/a.jpg"); e.Attr.Size != 2 {
		t.Errorf("plain name is not the newest: %+v", e)
	}
	if e := walk(t, env, "tree/x/a~1a2b3c4d.jpg"); e.Attr.Size != 1 {
		t.Errorf("suffixed name: %+v", e)
	}
	p, err := env.FS.Path(ctx, walk(t, env, "tree/x/a~1a2b3c4d.jpg").Node)
	if err != nil || strings.Join(p, "/") != "tree/x/a~1a2b3c4d.jpg" {
		t.Errorf("path = %q, %v", p, err)
	}
}

func TestReadOnlyMountRefusesWrites(t *testing.T) {
	env := vfstest.New(t)
	ctx := context.Background()
	if _, err := env.FS.Create(ctx, walk(t, env, "tree").Node, "x"); !errors.Is(err, syscall.EROFS) {
		t.Errorf("create: %v", err)
	}
	if _, err := env.FS.Mkdir(ctx, walk(t, env, "tree").Node, "x"); !errors.Is(err, syscall.EROFS) {
		t.Errorf("mkdir: %v", err)
	}
}

func TestHandlesOfObjects(t *testing.T) {
	ctx := context.Background()
	env := writable(t, nil)
	d := walk(t, env, "tree")
	e, err := env.FS.Create(ctx, d.Node, "h.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.WriteAt(ctx, e.Node, []byte("h"), 0); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		n, err := env.FS.NodeByID(ctx, e.Node.Kind, e.Node.UUID, e.Node.ID())
		if err != nil || n != e.Node {
			t.Errorf("%s: node by id = %+v, %v; want %+v", when, n, err, e.Node)
		}
	}
	check("while spooled")
	if err := env.FS.Flush(ctx, e.Node); err != nil {
		t.Fatal(err)
	}
	check("after the upload")
	if err := env.FS.Remove(ctx, d.Node, "h.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.NodeByID(ctx, e.Node.Kind, e.Node.UUID, e.Node.ID()); err == nil {
		t.Error("node of a removed file still resolves")
	}
}
