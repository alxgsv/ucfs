package api_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/uploadcare/uploadcare-go/v2/ucare"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/config"
	"github.com/alxgsv/ucfs/internal/fake"
)

func client(t *testing.T) (*fake.Server, *api.Client) {
	t.Helper()
	srv := fake.New(t)
	c, err := api.New(&config.Config{PublicKey: "demopublickey", SecretKey: "secret", CDNBase: srv.CDNBase()}, srv.Transport())
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestBaseUpload(t *testing.T) {
	srv, c := client(t)
	body := []byte("hello, ucfs")
	res, err := c.Upload(context.Background(), api.Upload{Name: `say "hi" café.txt`, Size: int64(len(body)), MIME: "text/plain",
		Data: bytes.NewReader(body), Metadata: map[string]string{"ucfs:path": "/docs/hi.txt", "ucfs:test": "1"}, Tags: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	f, ok := srv.File(res.UUID)
	if !ok {
		t.Fatalf("uploaded %s, not in the project", res.UUID)
	}
	if f.Name != `say "hi" café.txt` || !f.Stored || f.Metadata["ucfs:path"] != "/docs/hi.txt" || f.Metadata["ucfs:test"] != "1" ||
		strings.Join(f.Tags, ",") != "a,b" {
		t.Errorf("uploaded file = %+v", f)
	}
	if st := srv.UploadStats(); st.Base != 1 || st.Store != "1" {
		t.Errorf("upload stats = %+v, want one stored base upload", st)
	}
	if !bytes.Equal(srv.Body(res.UUID), body) {
		t.Error("content mismatch")
	}
	if s := c.Stats(); s.Upload != 1 || s.REST != 0 {
		t.Errorf("client stats = %+v", s)
	}
}

func TestMultipartUpload(t *testing.T) {
	srv, c := client(t)
	body := bytes.Repeat([]byte("0123456789abcdef"), (12<<20)/16+7)
	var started string
	res, err := c.Upload(context.Background(), api.Upload{Name: "big.bin", Size: int64(len(body)), MIME: "image/jpeg",
		Data: bytes.NewReader(body), Multipart: true, Started: func(uuid string) { started = uuid }})
	if err != nil {
		t.Fatal(err)
	}
	if started != res.UUID || res.MIME != "image/jpeg" {
		t.Errorf("started %q, finished %+v", started, res)
	}
	if !bytes.Equal(srv.Body(res.UUID), body) {
		t.Error("content mismatch")
	}
	if st := srv.UploadStats(); st.Start != 1 || st.Parts != 3 || st.Complete != 1 {
		t.Errorf("upload stats = %+v, want start, 3 parts, complete", st)
	}
	if s := c.Stats(); s.Upload != 2 || s.Parts != 3 {
		t.Errorf("client stats = %+v", s)
	}
}

func TestUploadErrors(t *testing.T) {
	srv, c := client(t)
	up := func(name string) error {
		_, err := c.Upload(context.Background(), api.Upload{Name: name, Size: 1, MIME: "text/plain", Data: strings.NewReader("x")})
		return err
	}
	srv.RejectUploads(func(name string) bool { return strings.HasSuffix(name, ".exe") })
	if err := up("x.exe"); api.Errno(err) != syscall.EPERM {
		t.Errorf("rejected type: %v → %v, want EPERM", err, api.Errno(err))
	}
	srv.FailUploads(1)
	if err := up("x.txt"); api.Errno(err) != syscall.EIO {
		t.Errorf("500: %v → %v, want EIO", err, api.Errno(err))
	}
	srv.Throttle(1)
	if err := up("x.txt"); err != nil {
		t.Errorf("one 429 is retried: %v", err)
	}
	if !errors.Is(syscall.EAGAIN, api.Errno(&api.UploadError{Status: 429})) {
		t.Error("429 after retries is not EAGAIN")
	}
}

func TestMetadataTagsAndGroups(t *testing.T) {
	srv, c := client(t)
	ctx := context.Background()
	srv.Add(fake.File{UUID: "00000000-0000-4000-8000-000000000001", Name: "a.jpg", Size: 1},
		fake.File{UUID: "00000000-0000-4000-8000-000000000002", Name: "b.jpg", Size: 1})
	uuid := "00000000-0000-4000-8000-000000000001"

	errnos := map[string]syscall.Errno{
		strings.Repeat("k", 65): syscall.EINVAL,
		"empty":                 syscall.EINVAL,
		"long":                  syscall.E2BIG,
	}
	values := map[string]string{strings.Repeat("k", 65): "v", "empty": "", "long": strings.Repeat("v", 513)}
	for key, want := range errnos {
		if err := c.SetMetadata(ctx, uuid, key, values[key]); api.Errno(err) != want {
			t.Errorf("set %.10s: %v (%v), want %v", key, err, api.Errno(err), want)
		}
	}
	for i := range 50 {
		if err := c.SetMetadata(ctx, uuid, "k"+strings.Repeat("x", i), "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetMetadata(ctx, uuid, "one-more", "v"); api.Errno(err) != syscall.ENOSPC {
		t.Errorf("51st key: %v, want ENOSPC", err)
	}

	tags, err := c.SetTags(ctx, uuid, []string{"B", "a", "b"})
	if err != nil || strings.Join(tags, ",") != "b,a" {
		t.Errorf("tags = %q, %v", tags, err)
	}
	many := make([]string, 51)
	for i := range many {
		many[i] = "t" + strings.Repeat("x", i)
	}
	if _, err := c.SetTags(ctx, uuid, many); err == nil {
		t.Error("51 tags accepted")
	}

	id := srv.AddGroup(time.Now(), uuid, "00000000-0000-4000-8000-000000000002")
	var listed []string
	if err := c.ListGroups(ctx, time.Time{}, func(g api.Group) error {
		listed = append(listed, g.ID)
		return nil
	}); err != nil || len(listed) != 1 || listed[0] != id {
		t.Errorf("groups = %q, %v", listed, err)
	}
	srv.Remove("00000000-0000-4000-8000-000000000002")
	g, err := c.GroupInfo(ctx, id)
	if err != nil || g.ID != id || len(g.Members) != 2 || g.Members[0].Name != "a.jpg" || g.Members[1] != nil {
		t.Errorf("group info = %+v, %v", g, err)
	}
	if _, err := c.GroupInfo(ctx, "00000000-0000-4000-8000-000000000009~1"); !api.IsNotFound(err) {
		t.Errorf("missing group: %v", err)
	}
}

// The live API's 400 messages for the metadata and tag limits (2026-11-08).
func TestLimitErrnos(t *testing.T) {
	for msg, want := range map[string]syscall.Errno{
		"Number of metadata keys per file has been exceeded. The limit is 50.": syscall.ENOSPC,
		"Length of key `kkk` is exceeded. The limit is 64.":                    syscall.ENAMETOOLONG,
		"Value length of key `v513` is exceeded. The limit is 512.":            syscall.E2BIG,
		"Value of key `empty` can not be empty.":                               syscall.EINVAL,
		`{"tags":["Too many tags: 51 (max 50)."]}`:                             syscall.ENOSPC,
		`{"tags":{"0":["Tag too long: 101 chars (max 100)."]}}`:                syscall.ENAMETOOLONG,
	} {
		if got := api.Errno(ucare.APIError{StatusCode: 400, Detail: msg}); got != want {
			t.Errorf("%q = %v, want %v", msg, got, want)
		}
	}
}
