package api_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"

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
