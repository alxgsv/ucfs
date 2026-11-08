package api_test

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/uploadcare/uploadcare-go/v2/ucare"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/config"
	"github.com/alxgsv/ucfs/internal/fake"
)

// Every failure mode of the REST API, the Upload API and the CDN maps to
// the errno PLAN item 34 names, through the requests ucfs really sends.
func TestErrnoTable(t *testing.T) {
	ctx := context.Background()
	srv, c := client(t)
	uuid := "11111111-1111-4111-8111-111111111111"
	srv.Add(fake.File{UUID: uuid, Name: "a.jpg", Size: 4096, MIME: "image/jpeg", Uploaded: time.Now(), Stored: true})
	cd := cdn.New(srv.CDNBase(), nil, nil)
	read := func() error {
		_, _, err := cd.ReadRange(ctx, uuid, "", make([]byte, 16), 0)
		return err
	}

	cases := []struct {
		name string
		err  func() error
		want syscall.Errno
	}{
		{"REST 404", func() error { _, err := c.Info(ctx, "22222222-2222-4222-8222-222222222222"); return err }, syscall.ENOENT},
		{"REST 403 permission_error", func() error {
			srv.DenyREST(true)
			defer srv.DenyREST(false)
			_, err := c.Info(ctx, uuid)
			return err
		}, syscall.EACCES},
		{"REST 401", func() error { return ucare.AuthError{APIError: ucare.APIError{StatusCode: 401}} }, syscall.EACCES},
		{"REST 429 past the Retry-After cap", func() error {
			srv.ThrottleREST(120)
			defer srv.ThrottleREST(0)
			_, err := c.Info(ctx, uuid)
			return err
		}, syscall.EAGAIN},
		{"REST 5xx", func() error { return ucare.APIError{StatusCode: 502, Detail: "Bad Gateway"} }, syscall.EIO},
		{"Upload API 5xx", func() error {
			srv.FailUploads(3)
			_, err := c.Upload(ctx, api.Upload{Name: "x.jpg", Size: 1, MIME: "image/jpeg", Data: strings.NewReader("x")})
			return err
		}, syscall.EIO},
		{"Upload API 429", func() error { return &api.UploadError{Status: 429} }, syscall.EAGAIN},
		{"network", func() error {
			down, err := api.New(&config.Config{PublicKey: "pk", SecretKey: "sk"}, refused{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = down.Info(ctx, uuid)
			return err
		}, syscall.EIO},
		{"CDN 404", func() error { srv.FailCDN(404); defer srv.FailCDN(0); return read() }, syscall.ENOENT},
		{"CDN 403", func() error { srv.FailCDN(403); defer srv.FailCDN(0); return read() }, syscall.EACCES},
		{"CDN 429", func() error { srv.FailCDN(429); defer srv.FailCDN(0); return read() }, syscall.EAGAIN},
		{"CDN 503", func() error { srv.FailCDN(503); defer srv.FailCDN(0); return read() }, syscall.EIO},
		{"CDN 400, a bad variant", func() error {
			_, _, err := cd.ReadRange(ctx, uuid, "-/resize/"+fake.BadArg+"/", make([]byte, 16), 0)
			return err
		}, syscall.EINVAL},
		{"CDN content changed", func() error { return &cache.ChangedError{} }, syscall.EIO},
		{"traffic budget", func() error { return cache.ErrBudget }, syscall.EDQUOT},
		{"read-only view", func() error { return &fs.PathError{Op: "create", Path: "by-date/x", Err: syscall.EROFS} }, syscall.EROFS},
		{"unsupported", func() error { return syscall.EOPNOTSUPP }, syscall.EOPNOTSUPP},
		{"degraded: denied", func() error { return api.ErrDenied }, syscall.EACCES},
		{"degraded: throttled", func() error { return api.ErrThrottled }, syscall.EAGAIN},
		{"interrupted", func() error { return context.Canceled }, syscall.EINTR},
	}
	for _, tc := range cases {
		err := tc.err()
		if got := api.Errno(err); got != tc.want {
			t.Errorf("%s: %v maps to %v, want %v", tc.name, err, got, tc.want)
		}
	}
}

// refused fails every request as a connection to a closed port does.
type refused struct{}

func (refused) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// A throttled request is retried as Retry-After asks and fails with EAGAIN
// only once the retries are spent.
func TestThrottleRetriesThenEAGAIN(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out five one-second Retry-After answers")
	}
	srv, c := client(t)
	uuid := "11111111-1111-4111-8111-111111111111"
	srv.Add(fake.File{UUID: uuid, Name: "a.jpg", Size: 1, Uploaded: time.Now()})
	srv.Throttle(2)
	if _, err := c.Info(context.Background(), uuid); err != nil {
		t.Fatalf("two 429s then 200: %v", err)
	}
	srv.Throttle(6)
	_, err := c.Info(context.Background(), uuid)
	if api.Errno(err) != syscall.EAGAIN {
		t.Fatalf("six 429s: %v, want EAGAIN", err)
	}
	if st := c.Stats(); st.Throttled != 8 {
		t.Fatalf("throttled answers counted %d, want 8", st.Throttled)
	}
}

// The client keeps the REST state its answers showed: denied with the
// API's words, throttled, and ok again; Degraded turns it into the error
// writes fail with once it lasted long enough.
func TestHealth(t *testing.T) {
	ctx := context.Background()
	srv, c := client(t)
	uuid := "11111111-1111-4111-8111-111111111111"
	srv.Add(fake.File{UUID: uuid, Name: "a.jpg", Size: 1, Uploaded: time.Now()})
	if h := c.Health(); h.State != api.StateOK {
		t.Fatalf("fresh client: %+v", h)
	}
	srv.DenyREST(true)
	_, _ = c.Info(ctx, uuid)
	_, _ = c.Info(ctx, uuid)
	h := c.Health()
	if h.State != api.StateDenied || !strings.Contains(h.Detail, "permission_error") {
		t.Fatalf("after 403s: %+v", h)
	}
	if err := h.Degraded(h.Since.Add(time.Second), time.Minute); err != nil {
		t.Fatalf("denied for a second, interval a minute: %v", err)
	}
	err := h.Degraded(h.Since.Add(time.Minute), time.Minute)
	if !errors.Is(err, api.ErrDenied) || api.Errno(err) != syscall.EACCES {
		t.Fatalf("denied for the interval: %v", err)
	}
	srv.DenyREST(false)
	srv.ThrottleREST(120)
	_, _ = c.Info(ctx, uuid)
	if h := c.Health(); h.State != api.StateThrottled || api.Errno(h.Degraded(h.Since, 0)) != syscall.EAGAIN {
		t.Fatalf("after a 429: %+v", h)
	}
	srv.ThrottleREST(0)
	if _, err := c.Info(ctx, "22222222-2222-4222-8222-222222222222"); api.Errno(err) != syscall.ENOENT {
		t.Fatal(err)
	}
	if h := c.Health(); h.State != api.StateOK {
		t.Fatalf("a 404 shows the API answering: %+v", h)
	}
}

// At most MaxREST REST requests are in flight; the rest wait.
func TestRESTBound(t *testing.T) {
	srv, c := client(t)
	var ids []string
	for i := range 3 * api.MaxREST {
		id := strings.Replace("11111111-1111-4111-8111-1111111111xx", "xx", string(rune('a'+i/16))+string(rune('a'+i%16)), 1)
		ids = append(ids, id)
		srv.Add(fake.File{UUID: id, Name: "a.jpg", Size: 1, Uploaded: time.Now()})
	}
	srv.SetRESTLatency(50 * time.Millisecond)
	errs := make(chan error, len(ids))
	start := time.Now()
	for _, id := range ids {
		go func() { _, err := c.Info(context.Background(), id); errs <- err }()
	}
	for range ids {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if d, peak := time.Since(start), srv.PeakREST(); peak > api.MaxREST || d < 3*50*time.Millisecond {
		t.Fatalf("%d requests: peak %d in flight, %v; want at most %d and three rounds", len(ids), peak, d, api.MaxREST)
	}
}
