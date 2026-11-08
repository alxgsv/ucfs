package vfs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

// configure mounts the given views with the vfstest defaults.
func configure(env *vfstest.Env, views ...string) {
	env.FS.Configure(vfs.Options{Views: views, DateZone: time.UTC, CDNBase: env.Fake.CDNBase(), Remote: env.Client})
}

func entryNames(entries []vfs.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func readdir(t *testing.T, env *vfstest.Env, p string) []string {
	t.Helper()
	e := walk(t, env, p)
	entries, err := env.FS.ReadDir(context.Background(), e.Node)
	if err != nil {
		t.Fatalf("readdir %s: %v", p, err)
	}
	for _, en := range entries {
		got, err := env.FS.Lookup(context.Background(), e.Node, en.Name)
		if err != nil || got.Node != en.Node {
			t.Errorf("lookup %s/%s = %+v, %v; want %+v", p, en.Name, got.Node, err, en.Node)
		}
	}
	return entryNames(entries)
}

func TestViewsFlag(t *testing.T) {
	env := vfstest.New(t, file(1, "a.jpg", 5))
	ctx := context.Background()
	if got, want := readdir(t, env, ""), []string{"files", "tree", "by-date", ".ucfs", ".metadata_never_index"}; !slices.Equal(got, want) {
		t.Errorf("default root = %q, want %q", got, want)
	}
	for _, name := range []string{"tags", "groups"} {
		if _, err := env.FS.Walk(ctx, name); !errors.Is(err, syscall.ENOENT) {
			t.Errorf("%s/ without the view = %v, want ENOENT", name, err)
		}
	}
	configure(env, vfs.ViewGroups, vfs.ViewTags, vfs.ViewFiles)
	if got, want := readdir(t, env, ""), []string{"files", "tags", "groups", ".ucfs", ".metadata_never_index"}; !slices.Equal(got, want) {
		t.Errorf("root = %q, want %q", got, want)
	}
	for _, name := range []string{"tree", "by-date"} {
		if _, err := env.FS.Walk(ctx, name); !errors.Is(err, syscall.ENOENT) {
			t.Errorf("%s/ without the view = %v, want ENOENT", name, err)
		}
	}
	if got := env.FS.Views(); !slices.Equal(got, []string{"files", "tags", "groups"}) {
		t.Errorf("Views() = %q", got)
	}
}

func TestByDate(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2025, 12, d, h, 30, 0, 0, time.UTC) }
	files := []fake.File{
		{UUID: uuid(1), Name: "new-year.jpg", Size: 1, Uploaded: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{UUID: uuid(2), Name: "late.jpg", Size: 2, Uploaded: day(31, 20)},
		{UUID: uuid(3), Name: "early.jpg", Size: 3, Uploaded: day(5, 1)},
		{UUID: uuid(4), Name: "gone.jpg", Size: 4, Uploaded: day(5, 2), Removed: true},
		{UUID: uuid(5), Name: "dir/x.jpg", Size: 5, Uploaded: day(5, 3)},
	}
	env := vfstest.New(t, files...)
	ctx := context.Background()

	if got := readdir(t, env, "by-date"); !slices.Equal(got, []string{"2025", "2026"}) {
		t.Errorf("by-date = %q", got)
	}
	if got := readdir(t, env, "by-date/2025"); !slices.Equal(got, []string{"12"}) {
		t.Errorf("by-date/2025 = %q", got)
	}
	if got := readdir(t, env, "by-date/2025/12"); !slices.Equal(got, []string{"05", "31"}) {
		t.Errorf("by-date/2025/12 = %q", got)
	}
	want := []string{uuid(3) + "_early.jpg", uuid(5) + "_dir／x.jpg"}
	if got := readdir(t, env, "by-date/2025/12/05"); !slices.Equal(got, want) {
		t.Errorf("by-date/2025/12/05 = %q, want %q", got, want)
	}
	for _, p := range []string{
		"by-date/2024", "by-date/2025/11", "by-date/2025/12/06", "by-date/2025/12/5", "by-date/25",
		"by-date/2025/13", "by-date/2025/02/30", "by-date/2025/12/05/" + uuid(4) + "_gone.jpg",
		"by-date/2025/12/31/" + uuid(3) + "_early.jpg", "by-date/2025/12/05/" + uuid(3) + "_other.jpg",
	} {
		if _, err := env.FS.Walk(ctx, p); !errors.Is(err, syscall.ENOENT) {
			t.Errorf("walk %s = %v, want ENOENT", p, err)
		}
	}
	e := walk(t, env, "by-date/2025/12/05/"+uuid(3)+"_early.jpg")
	if e.Node.Kind != vfs.KindDateFile || e.Attr.Size != 3 {
		t.Errorf("entry = %+v", e)
	}
	buf := make([]byte, 3)
	if n, err := env.FS.ReadAt(ctx, e.Node, buf, 0); n != 3 || !bytes.Equal(buf, fake.Content(uuid(3), 3)) {
		t.Errorf("read = %d, %v", n, err)
	}

	// Six hours east, the evening upload of the 31st is on New Year's Day,
	// with the midnight one.
	env.FS.Configure(vfs.Options{DateZone: time.FixedZone("UTC+6", 6*3600)})
	want = []string{uuid(1) + "_new-year.jpg", uuid(2) + "_late.jpg"}
	if got := readdir(t, env, "by-date/2026/01/01"); !slices.Equal(got, want) {
		t.Errorf("by-date/2026/01/01 in UTC+6 = %q, want %q", got, want)
	}
	if reqs, _ := env.Fake.CDNStats(); reqs != 1 {
		t.Errorf("CDN requests = %d, want the one read", reqs)
	}
}

// search asks the fake's POST /files/search/ for files carrying all tags,
// as the acceptance compares tags/<tag>/ with the live search API.
func search(t *testing.T, env *vfstest.Env, tags ...string) []string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"tags": map[string][]string{"all": tags}, "limit": 100})
	req, _ := http.NewRequest(http.MethodPost, env.Fake.URL+"/files/search/", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Results []struct {
			UUID string `json:"uuid"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	var uuids []string
	for _, r := range out.Results {
		uuids = append(uuids, r.UUID)
	}
	slices.Sort(uuids)
	return uuids
}

func TestTags(t *testing.T) {
	files := []fake.File{
		{UUID: uuid(1), Name: "a.jpg", Size: 1, Uploaded: t0, Tags: []string{"approved", "cat"}},
		{UUID: uuid(2), Name: "b.jpg", Size: 1, Uploaded: t0.Add(time.Second), Tags: []string{"approved"}},
		{UUID: uuid(3), Name: "c.jpg", Size: 1, Uploaded: t0.Add(2 * time.Second), Tags: []string{"dog", ".."}},
		{UUID: uuid(4), Name: "d.jpg", Size: 1, Uploaded: t0.Add(3 * time.Second), Tags: []string{"approved"}, Removed: true},
		{UUID: uuid(5), Name: "e.jpg", Size: 1, Uploaded: t0.Add(4 * time.Second)},
	}
	env := vfstest.New(t, files...)
	configure(env, vfs.ViewTags)
	ctx := context.Background()

	if got := readdir(t, env, "tags"); !slices.Equal(got, []string{"approved", "cat", "dog"}) {
		t.Errorf("tags = %q", got)
	}
	got := readdir(t, env, "tags/approved")
	if want := []string{uuid(1) + "_a.jpg", uuid(2) + "_b.jpg"}; !slices.Equal(got, want) {
		t.Errorf("tags/approved = %q, want %q", got, want)
	}
	var listed []string
	for _, name := range got {
		listed = append(listed, name[:36])
	}
	if found := search(t, env, "approved"); !slices.Equal(listed, found) {
		t.Errorf("tags/approved lists %q, search for tags.all=[approved] finds %q", listed, found)
	}
	for _, p := range []string{"tags/none", "tags/approved/" + uuid(3) + "_c.jpg", "tags/approved/" + uuid(4) + "_d.jpg"} {
		if _, err := env.FS.Walk(ctx, p); !errors.Is(err, syscall.ENOENT) {
			t.Errorf("walk %s = %v, want ENOENT", p, err)
		}
	}
	if _, err := env.FS.Lookup(ctx, walk(t, env, "tags").Node, ".."); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("tag .. = %v, want ENOENT", err)
	}
	if e := walk(t, env, "tags/cat/"+uuid(1)+"_a.jpg"); e.Node.Kind != vfs.KindViewFile {
		t.Errorf("kind = %v", e.Node.Kind)
	}
}

func TestGroups(t *testing.T) {
	var files []fake.File
	for i := 1; i <= 12; i++ {
		files = append(files, fake.File{UUID: uuid(i), Name: fmt.Sprintf("p%d.jpg", i), Size: int64(i), Uploaded: t0.Add(time.Duration(i) * time.Second)})
	}
	env := vfstest.New(t, files...)
	configure(env, vfs.ViewGroups)
	ctx := context.Background()
	var members []string
	for i := 12; i >= 1; i-- {
		members = append(members, uuid(i))
	}
	big := env.Fake.AddGroup(t0.Add(time.Hour), members...)
	small := env.Fake.AddGroup(t0.Add(2*time.Hour), uuid(1), uuid(2))
	if n, err := env.Index.SyncGroups(ctx, env.Client); err != nil || n != 2 {
		t.Fatalf("group sync = %d, %v", n, err)
	}
	if n, err := env.Index.SyncGroups(ctx, env.Client); err != nil || n != 0 {
		t.Fatalf("second group sync = %d, %v", n, err)
	}
	if got := readdir(t, env, "groups"); !slices.Equal(got, []string{big, small}) {
		t.Errorf("groups = %q", got)
	}

	before := env.Fake.Requests()
	got := readdir(t, env, "groups/"+big)
	if len(got) != 12 || got[0] != "00_p12.jpg" || got[11] != "11_p1.jpg" || !slices.IsSorted(got) {
		t.Errorf("groups/%s = %q", big, got)
	}
	readdir(t, env, "groups/"+big)
	if n := env.Fake.Requests() - before; n != 1 {
		t.Errorf("listing a group twice made %d requests, want one group info", n)
	}
	if st := env.FS.MetaStats(); st.GroupFetches != 1 {
		t.Errorf("group fetches = %d", st.GroupFetches)
	}

	// A removed member leaves a gap in the numbering.
	env.Fake.Remove(uuid(1))
	if err := env.Index.MarkRemoved(ctx, uuid(1)); err != nil {
		t.Fatal(err)
	}
	if got := readdir(t, env, "groups/"+small); !slices.Equal(got, []string{"1_p2.jpg"}) {
		t.Errorf("groups/%s with a removed member = %q", small, got)
	}

	// A group created after the last sync opens by id; a deleted one goes.
	fresh := env.Fake.AddGroup(t0.Add(3*time.Hour), uuid(3))
	if got := readdir(t, env, "groups/"+fresh); !slices.Equal(got, []string{"0_p3.jpg"}) {
		t.Errorf("unsynced group = %q", got)
	}
	unknown := "00000000-0000-4000-8000-000000000999~3"
	for _, p := range []string{"groups/" + unknown, "groups/not-a-group", "groups/" + big + "/12_p1.jpg", "groups/" + big + "/00_p1.jpg"} {
		if _, err := env.FS.Walk(ctx, p); !errors.Is(err, syscall.ENOENT) {
			t.Errorf("walk %s = %v, want ENOENT", p, err)
		}
	}
	req, _ := http.NewRequest(http.MethodDelete, env.Fake.URL+"/groups/"+small+"/", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete group: %v", err)
	}
	if err := env.Index.DeleteGroup(ctx, small); err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.Walk(ctx, "groups/"+small); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("deleted group = %v, want ENOENT", err)
	}
}

func TestViewsAreReadOnly(t *testing.T) {
	env := writable(t, nil, file(1, "a.jpg", 5))
	env.Fake.SetTags(uuid(1), "x")
	if err := env.Index.PutInfo(context.Background(), time.Now(), apiFile(t, env, uuid(1))); err != nil {
		t.Fatal(err)
	}
	configure(env, vfs.AllViews...)
	ctx := context.Background()
	day := "by-date/" + t0.Format("2006/01/02")
	name := uuid(1) + "_a.jpg"
	for _, dir := range []string{day, "tags/x", "by-date", "tags", "groups"} {
		d := walk(t, env, dir)
		if _, err := env.FS.Create(ctx, d.Node, "new.jpg"); !errors.Is(err, syscall.EROFS) {
			t.Errorf("create in %s = %v, want EROFS", dir, err)
		}
		if _, err := env.FS.Mkdir(ctx, d.Node, "sub"); !errors.Is(err, syscall.EROFS) {
			t.Errorf("mkdir in %s = %v, want EROFS", dir, err)
		}
	}
	for _, dir := range []string{day, "tags/x"} {
		d := walk(t, env, dir)
		e := walk(t, env, dir+"/"+name)
		if err := env.FS.Remove(ctx, d.Node, name); !errors.Is(err, syscall.EROFS) {
			t.Errorf("rm in %s = %v, want EROFS", dir, err)
		}
		if err := env.FS.Rename(ctx, d.Node, name, walk(t, env, "tree").Node, "a.jpg"); !errors.Is(err, syscall.EROFS) {
			t.Errorf("mv out of %s = %v, want EROFS", dir, err)
		}
		if _, err := env.FS.WriteAt(ctx, e.Node, []byte("x"), 0); !errors.Is(err, syscall.EROFS) {
			t.Errorf("write in %s = %v, want EROFS", dir, err)
		}
		if err := env.FS.OpenWrite(ctx, e.Node, false); !errors.Is(err, syscall.EROFS) {
			t.Errorf("open for writing in %s = %v, want EROFS", dir, err)
		}
	}
	if err := env.FS.Rmdir(ctx, walk(t, env, "by-date").Node, t0.Format("2006")); !errors.Is(err, syscall.EROFS) {
		t.Errorf("rmdir in by-date = %v, want EROFS", err)
	}
	if err := env.FS.Rename(ctx, walk(t, env, "tree/_inbox").Node, "a.jpg", walk(t, env, day).Node, "a.jpg"); !errors.Is(err, syscall.EROFS) {
		t.Errorf("mv into by-date = %v, want EROFS", err)
	}
}

func TestViewPaths(t *testing.T) {
	env := vfstest.New(t, fake.File{UUID: uuid(1), Name: "a.jpg", Size: 1, Uploaded: t0, Tags: []string{"x"}})
	configure(env, vfs.AllViews...)
	g := env.Fake.AddGroup(t0, uuid(1))
	ctx := context.Background()
	for _, p := range []string{
		"by-date", "by-date/2026", "by-date/2026/03/04", "by-date/2026/03/04/" + uuid(1) + "_a.jpg",
		"tags", "tags/x", "tags/x/" + uuid(1) + "_a.jpg", "groups", "groups/" + g, "groups/" + g + "/0_a.jpg",
	} {
		e := walk(t, env, p)
		got, err := env.FS.Path(ctx, e.Node)
		if err != nil || strings.Join(got, "/") != p {
			t.Errorf("path of %s = %q, %v", p, got, err)
		}
		env.FS.Remember(ctx, e.Node)
		kind, id := e.Node.Kind, e.Node.ID()
		if n, err := env.FS.NodeByID(ctx, kind, e.Node.UUID, id); err != nil || n != e.Node {
			t.Errorf("node of %s by id = %+v, %v; want %+v", p, n, err, e.Node)
		}
	}
}
