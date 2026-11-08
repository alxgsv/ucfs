package index

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestDetailsMetaAndTags(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	fs := files(0, 4)
	fs[0].Metadata = map[string]string{"a.b": "1", "ucfs:path": "/x/y.jpg"}
	fs[0].Tags = []string{"cat", "dog"}
	fs[1].Tags = []string{"dog"}
	fs[2].Tags = []string{"dog"}
	fs[2].Removed = true
	srv.Add(fs...)
	if _, err := ix.Sync(ctx, client, false); err != nil {
		t.Fatal(err)
	}
	d, err := ix.Details(ctx, fs[0].UUID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Metadata["a.b"] != "1" || !slices.Equal(d.Tags, []string{"cat", "dog"}) || d.Width != 10 || !d.Stored || !d.IsReady {
		t.Errorf("details = %+v", d)
	}
	if time.Since(d.RefreshedAt) > time.Minute {
		t.Errorf("refreshed at %v", d.RefreshedAt)
	}
	if err := ix.SetMeta(ctx, fs[0].UUID, "a.b", "2"); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetMeta(ctx, fs[1].UUID, "new", "v"); err != nil {
		t.Fatal(err)
	}
	if err := ix.DeleteMeta(ctx, fs[0].UUID, "ucfs:path"); err != nil {
		t.Fatal(err)
	}
	if err := ix.SetMD5(ctx, fs[0].UUID, "0123"); err != nil {
		t.Fatal(err)
	}
	d, _ = ix.Details(ctx, fs[0].UUID)
	if d.Metadata["a.b"] != "2" || d.Metadata["ucfs:path"] != "" || d.MD5 != "0123" {
		t.Errorf("after writes: %+v", d)
	}
	if d, _ := ix.Details(ctx, fs[1].UUID); d.Metadata["new"] != "v" {
		t.Errorf("metadata of a file listed without any = %v", d.Metadata)
	}

	if tags, err := ix.Tags(ctx); err != nil || !slices.Equal(tags, []string{"cat", "dog"}) {
		t.Errorf("tags = %q, %v", tags, err)
	}
	dogs, err := ix.TaggedFiles(ctx, "dog")
	if err != nil || len(dogs) != 2 || dogs[0].UUID != fs[0].UUID || dogs[1].UUID != fs[1].UUID {
		t.Errorf("tagged dog = %+v, %v (the removed one must not count)", dogs, err)
	}
	if err := ix.SetTags(ctx, fs[0].UUID, nil); err != nil {
		t.Fatal(err)
	}
	if has, _ := ix.HasTag(ctx, "cat"); has {
		t.Error("cat survived clearing the tags")
	}

	// A listing replaces what was recorded, but keeps the md5, which
	// listings do not carry.
	if _, err := ix.Sync(ctx, client, true); err != nil {
		t.Fatal(err)
	}
	if d, _ := ix.Details(ctx, fs[0].UUID); d.MD5 != "0123" || d.Metadata["a.b"] != "1" {
		t.Errorf("after a full sync: %+v", d)
	}

	// A project without the tags feature has no tags field.
	srv.NoTags()
	f, err := client.Info(ctx, fs[3].UUID)
	if err != nil || f.Tags != nil {
		t.Fatalf("info without tags = %+v, %v", f, err)
	}
	if err := ix.PutInfo(ctx, time.Now(), f); err != nil {
		t.Fatal(err)
	}
	if d, _ := ix.Details(ctx, fs[3].UUID); d.Tags != nil {
		t.Errorf("tags = %#v, want nil", d.Tags)
	}
}

func TestPutInfoKeepsCursor(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	srv.Add(files(0, 3)...)
	if _, err := ix.Sync(ctx, client, false); err != nil {
		t.Fatal(err)
	}
	before, _ := ix.Cursor(ctx)
	late := files(100, 1)[0]
	srv.Add(late)
	f, _ := client.Info(ctx, late.UUID)
	if err := ix.PutInfo(ctx, time.Now(), f); err != nil {
		t.Fatal(err)
	}
	if after, _ := ix.Cursor(ctx); !after.Equal(before) {
		t.Errorf("cursor moved from %v to %v", before, after)
	}
}

func TestDates(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	fs := files(0, 9)
	fs[0].Removed = true
	srv.Add(fs...)
	if _, err := ix.Sync(ctx, client, false); err != nil {
		t.Fatal(err)
	}
	first, last, ok, err := ix.UploadedSpan(ctx)
	// The removed first file does not count; upload times have seconds.
	if err != nil || !ok || !first.Equal(t0) || !last.Equal(t0.Add(2*time.Second)) {
		t.Errorf("span = %v %v %v %v", first, last, ok, err)
	}
	got, err := ix.FilesBetween(ctx, t0, t0.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	// Three files a second from t0; the first is removed.
	if len(got) != 5 {
		t.Errorf("files in two seconds = %d, want 5", len(got))
	}
	if has, _ := ix.UploadedBetween(ctx, t0.Add(time.Hour), t0.Add(2*time.Hour)); has {
		t.Error("an empty hour has uploads")
	}
}

func TestGroupSync(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	fs := files(0, 3)
	srv.Add(fs...)
	var ids []string
	for i := range 5 {
		// Two groups share a second, so the inclusive cursor lists one again.
		ids = append(ids, srv.AddGroup(t0.Add(time.Duration(i/2)*time.Second+time.Duration(i)*time.Millisecond), fs[i%3].UUID))
	}
	if n, err := ix.SyncGroups(ctx, client); err != nil || n != 5 {
		t.Fatalf("first sync = %d, %v", n, err)
	}
	if n, err := ix.SyncGroups(ctx, client); err != nil || n != 0 {
		t.Fatalf("second sync = %d, %v", n, err)
	}
	ids = append(ids, srv.AddGroup(t0.Add(time.Hour), fs[0].UUID, fs[1].UUID))
	if n, err := ix.SyncGroups(ctx, client); err != nil || n != 1 {
		t.Fatalf("third sync = %d, %v", n, err)
	}
	groups, err := ix.Groups(ctx)
	if err != nil || len(groups) != 6 {
		t.Fatalf("groups = %d, %v", len(groups), err)
	}
	// uploadcare-go reads creation times to the second, so groups created
	// within one second are ordered by id.
	var got []string
	for _, g := range groups {
		got = append(got, g.ID)
		if g.Fetched {
			t.Errorf("group %s fetched by a sync", g.ID)
		}
	}
	if slices.Sort(got); !slices.Equal(got, slices.Sorted(slices.Values(ids))) {
		t.Errorf("groups = %q, want %q", got, ids)
	}
	srv.Remove(fs[1].UUID)
	g, err := client.GroupInfo(ctx, ids[5])
	if err != nil || len(g.Members) != 2 || g.Members[0] == nil || g.Members[1] != nil {
		t.Fatalf("group info = %+v, %v", g, err)
	}
	if err := ix.PutGroup(ctx, g, time.Now()); err != nil {
		t.Fatal(err)
	}
	stored, err := ix.Group(ctx, ids[5])
	if err != nil || !stored.Fetched || !slices.Equal(stored.Files, []string{fs[0].UUID, ""}) {
		t.Errorf("stored group = %+v, %v", stored, err)
	}
	if err := ix.DeleteGroup(ctx, ids[5]); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Group(ctx, ids[5]); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted group = %v", err)
	}
}

func TestSpoolQueued(t *testing.T) {
	ctx := context.Background()
	_, _, ix := setup(t)
	tags := []string{}
	rows := []SpoolRow{
		{ID: "a", Path: "tree/a", State: "writing", Created: t0},
		{ID: "b", Path: "tree/b", State: "writing", Created: t0.Add(time.Second),
			Queued: Queued{Meta: map[string]string{"k": "v"}, Tags: &tags}},
	}
	for _, r := range rows {
		if err := ix.PutSpool(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ix.Spools(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("spools = %+v, %v", got, err)
	}
	if got[0].Queued.Meta != nil || got[0].Queued.Tags != nil {
		t.Errorf("empty queue = %+v", got[0].Queued)
	}
	if q := got[1].Queued; q.Meta["k"] != "v" || q.Tags == nil || len(*q.Tags) != 0 {
		t.Errorf("queue = %+v", q)
	}
}
