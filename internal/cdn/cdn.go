// Package cdn reads file content from the project's CDN.
package cdn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

var ErrNotFound = errors.New("not found on CDN")

type Client struct {
	base     string
	hc       *http.Client
	requests atomic.Int64
	bytes    atomic.Int64
}

// New returns a client for the CDN base URL. A nil transport means a clone of
// http.DefaultTransport with more idle connections per host, since every
// concurrent block fetch goes to the same host.
func New(base string, rt http.RoundTripper) *Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConnsPerHost = 64
		rt = t
	}
	return &Client{base: strings.TrimSuffix(base, "/"), hc: &http.Client{Transport: rt}}
}

// URL is the CDN URL of an original (ops empty) or of a variant.
func (c *Client) URL(uuid, ops string) string {
	return c.base + "/" + uuid + "/" + ops
}

type Info struct {
	Size int64
	ETag string
	MIME string
}

func (c *Client) Head(ctx context.Context, uuid, ops string) (Info, error) {
	resp, err := c.do(ctx, http.MethodHead, uuid, ops, "")
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusErr(resp, http.StatusOK); err != nil {
		return Info{}, err
	}
	return Info{Size: resp.ContentLength, ETag: resp.Header.Get("ETag"), MIME: resp.Header.Get("Content-Type")}, nil
}

// ReadRange fills p with the bytes at off. The caller sizes p to end at or
// before EOF, so a short body is an error.
func (c *Client) ReadRange(ctx context.Context, uuid, ops string, p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1)
	resp, err := c.do(ctx, http.MethodGet, uuid, ops, rng)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if start, ok := rangeStart(resp.Header.Get("Content-Range")); !ok || start != off {
			return 0, fmt.Errorf("cdn %s: Content-Range %q does not start at %d", c.URL(uuid, ops), resp.Header.Get("Content-Range"), off)
		}
	case http.StatusOK:
		// The range was ignored and the body starts at 0; only usable from 0.
		if off != 0 {
			return 0, fmt.Errorf("cdn %s: range ignored", c.URL(uuid, ops))
		}
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, io.EOF
	default:
		return 0, statusErr(resp, http.StatusPartialContent)
	}
	n, err := io.ReadFull(resp.Body, p)
	c.bytes.Add(int64(n))
	slog.Debug("cdn read", "uuid", uuid, "ops", ops, "off", off, "bytes", n)
	return n, err
}

func (c *Client) do(ctx context.Context, method, uuid, ops, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL(uuid, ops), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ucfs")
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	c.requests.Add(1)
	return c.hc.Do(req)
}

func statusErr(resp *http.Response, want int) error {
	switch resp.StatusCode {
	case want:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("cdn %s %s: %s", resp.Request.Method, resp.Request.URL, resp.Status)
	}
}

// rangeStart parses the first byte position of "bytes a-b/size".
func rangeStart(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, false
	}
	a, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(a, 10, 64)
	return n, err == nil
}

type Stats struct {
	Requests int64
	Bytes    int64
}

func (c *Client) Stats() Stats {
	return Stats{Requests: c.requests.Load(), Bytes: c.bytes.Load()}
}
