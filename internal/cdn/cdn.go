// Package cdn reads file content from the project's CDN.
package cdn

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
)

var ErrNotFound = errors.New("not found on CDN")

// BadRequestError is a CDN 400: the operations do not apply to the file, or
// an argument is wrong. Message is the CDN's explanation.
type BadRequestError struct {
	URL     string
	Message string
}

func (e *BadRequestError) Error() string { return "cdn " + e.URL + ": 400 " + e.Message }

// StatusError is a CDN answer the client has no meaning for, such as 403,
// 429 or a 5xx; api.Errno maps it by status.
type StatusError struct {
	Method, URL string
	Status      int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("cdn %s %s: %d %s", e.Method, e.URL, e.Status, http.StatusText(e.Status))
}

type Client struct {
	base               string
	hc                 *http.Client
	lim                *Limiter
	heads, gets, bytes atomic.Int64
}

// New returns a client for the CDN base URL. A nil transport means a clone of
// http.DefaultTransport with more idle connections per host, since every
// concurrent block fetch goes to the same host. HEADs take a slot of lim,
// when given; range reads are the cache's to schedule.
func New(base string, rt http.RoundTripper, lim *Limiter) *Client {
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConnsPerHost = 64
		rt = t
	}
	return &Client{base: strings.TrimSuffix(base, "/"), hc: &http.Client{Transport: rt}, lim: lim}
}

// URL is the CDN URL of an original (ops empty) or of a variant, ops being a
// path such as "-/resize/1200x/-/format/webp/". Arguments come from path
// components, so they are escaped; "," and ":" stay as the URL API writes them.
func (c *Client) URL(uuid, ops string) string {
	return c.base + (&url.URL{Path: "/" + uuid + "/" + ops}).EscapedPath()
}

type Info struct {
	Size int64
	ETag string
	MIME string
}

func (c *Client) Head(ctx context.Context, uuid, ops string) (Info, error) {
	if c.lim != nil {
		release, err := c.lim.Acquire(ctx, uuid+"/"+ops)
		if err != nil {
			return Info{}, err
		}
		defer release()
	}
	resp, err := c.do(ctx, http.MethodHead, uuid, ops, "")
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadRequest {
		// A HEAD answer has no body, and the body holds the reason.
		return Info{}, c.badRequest(ctx, uuid, ops)
	}
	if err := c.statusErr(resp, http.StatusOK); err != nil {
		return Info{}, err
	}
	if resp.ContentLength < 0 {
		return Info{}, fmt.Errorf("cdn HEAD %s: no Content-Length", c.URL(uuid, ops))
	}
	return Info{Size: resp.ContentLength, ETag: resp.Header.Get("ETag"), MIME: resp.Header.Get("Content-Type")}, nil
}

func (c *Client) badRequest(ctx context.Context, uuid, ops string) error {
	bad := &BadRequestError{URL: c.URL(uuid, ops)}
	resp, err := c.do(ctx, http.MethodGet, uuid, ops, "")
	if err != nil {
		return bad
	}
	defer func() { _ = resp.Body.Close() }()
	errors.As(c.statusErr(resp, http.StatusOK), &bad)
	return bad
}

// ReadRange fills p with the bytes at off and returns the ETag they came
// with. The caller sizes p to end at or before EOF, so a short body is an
// error.
func (c *Client) ReadRange(ctx context.Context, uuid, ops string, p []byte, off int64) (int, string, error) {
	if len(p) == 0 {
		return 0, "", nil
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1)
	resp, err := c.do(ctx, http.MethodGet, uuid, ops, rng)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	etag := resp.Header.Get("ETag")
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if start, ok := rangeStart(resp.Header.Get("Content-Range")); !ok || start != off {
			return 0, etag, fmt.Errorf("cdn %s: Content-Range %q does not start at %d", c.URL(uuid, ops), resp.Header.Get("Content-Range"), off)
		}
	case http.StatusOK:
		// The range was ignored and the body starts at 0; only usable from 0.
		if off != 0 {
			return 0, etag, fmt.Errorf("cdn %s: range ignored", c.URL(uuid, ops))
		}
	case http.StatusRequestedRangeNotSatisfiable:
		return 0, etag, io.EOF
	default:
		return 0, etag, c.statusErr(resp, http.StatusPartialContent)
	}
	n, err := io.ReadFull(resp.Body, p)
	c.bytes.Add(int64(n))
	slog.Debug("cdn read", "uuid", uuid, "ops", ops, "off", off, "bytes", n)
	return n, etag, err
}

// SameETag compares ETags weakly (RFC 9110 §8.8.3.2): the CDN marks variant
// ETags weak and the comparison ignores that.
func SameETag(a, b string) bool {
	return strings.TrimPrefix(a, "W/") == strings.TrimPrefix(b, "W/")
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
	if method == http.MethodHead {
		c.heads.Add(1)
	} else {
		c.gets.Add(1)
	}
	return c.hc.Do(req)
}

// maxErrorBody bounds what is read of an error page; the CDN's are about 1 KiB.
const maxErrorBody = 64 << 10

func (c *Client) statusErr(resp *http.Response, want int) error {
	switch resp.StatusCode {
	case want:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusBadRequest:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		c.bytes.Add(int64(len(body)))
		return &BadRequestError{URL: resp.Request.URL.String(), Message: errorMessage(body)}
	default:
		return &StatusError{Method: resp.Request.Method, URL: resp.Request.URL.String(), Status: resp.StatusCode}
	}
}

// errorMessage is the text of the CDN's HTML error page without the title
// and the trailing link to the documentation.
func errorMessage(page []byte) string {
	s := string(page)
	if i := strings.Index(s, "<body"); i >= 0 {
		s = s[i:]
	}
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
			b.WriteByte(' ')
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	text := strings.Join(strings.Fields(html.UnescapeString(b.String())), " ")
	text = strings.TrimPrefix(text, "Bad Request ")
	if i := strings.LastIndex(text, " See "); i >= 0 {
		text = text[:i]
	}
	return text
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
	Requests    int64
	Heads, Gets int64
	// Bytes counts response bodies read: content and error pages.
	Bytes int64
}

func (c *Client) Stats() Stats {
	h, g := c.heads.Load(), c.gets.Load()
	return Stats{Requests: h + g, Heads: h, Gets: g, Bytes: c.bytes.Load()}
}
