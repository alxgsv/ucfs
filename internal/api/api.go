// Package api is the thin layer over uploadcare-go that the rest of ucfs uses.
package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/uploadcare/uploadcare-go/v2/file"
	"github.com/uploadcare/uploadcare-go/v2/group"
	"github.com/uploadcare/uploadcare-go/v2/metadata"
	"github.com/uploadcare/uploadcare-go/v2/project"
	"github.com/uploadcare/uploadcare-go/v2/tag"
	"github.com/uploadcare/uploadcare-go/v2/ucare"

	"github.com/alxgsv/ucfs/internal/config"
)

// PageSize is the REST maximum for GET /files/.
const PageSize = 1000

const (
	restHost   = "api.uploadcare.com"
	uploadHost = "upload.uploadcare.com"
)

// File is the subset of REST file info that the index keeps.
type File struct {
	UUID       string
	Name       string
	Size       int64
	MIME       string
	IsImage    bool
	IsReady    bool
	UploadedAt time.Time
	StoredAt   time.Time
	RemovedAt  time.Time
	Width      int
	Height     int
	Metadata   map[string]string
	// Tags is nil when the file info has no tags field: the project lacks
	// the tags feature.
	Tags []string
}

type Client struct {
	uc      ucare.Client
	files   file.Service
	meta    metadata.Service
	tags    tag.Service
	groups  group.Service
	project project.Service
	stats   *Stats
	// http and pubKey serve the Upload API, which ucfs calls directly.
	http   *http.Client
	pubKey string
}

// New builds a client with throttle retries enabled. A nil base transport
// means http.DefaultTransport; tests pass a fake's transport.
func New(cfg *config.Config, base http.RoundTripper) (*Client, error) {
	if base == nil {
		base = http.DefaultTransport
	}
	stats := &Stats{}
	hc := &http.Client{Transport: &countingTransport{base: base, stats: stats, rest: make(chan struct{}, MaxREST)},
		Timeout: 2 * time.Minute}
	ucfg, err := ucare.NewConfig(
		ucare.APICreds{PublicKey: cfg.PublicKey, SecretKey: cfg.SecretKey},
		ucare.WithHTTPClient(hc),
		ucare.WithCDNBase(cfg.CDNBase),
		ucare.WithUserAgent("ucfs"),
		ucare.WithRetry(&ucare.RetryConfig{MaxRetries: 5, MaxWaitSeconds: 60}),
	)
	if err != nil {
		return nil, err
	}
	uc, err := ucare.NewClient(ucare.APICreds{PublicKey: cfg.PublicKey, SecretKey: cfg.SecretKey}, ucfg)
	if err != nil {
		return nil, err
	}
	return &Client{uc: uc, files: file.NewService(uc), meta: metadata.NewService(uc), tags: tag.NewService(uc),
		groups: group.NewService(uc), project: project.NewService(uc), stats: stats, http: hc, pubKey: cfg.PublicKey}, nil
}

// Stats returns the HTTP counters of this client.
func (c *Client) Stats() StatsSnapshot { return c.stats.Snapshot() }

// Health is how the REST API answered lately.
func (c *Client) Health() Health { return c.stats.health() }

type ListOptions struct {
	// From is inclusive at second precision; callers dedupe by UUID.
	From    time.Time
	Removed bool
}

// ListFiles walks GET /files/ in datetime_uploaded order and calls fn for
// each file. Pages are fetched lazily as fn consumes them.
func (c *Client) ListFiles(ctx context.Context, o ListOptions, fn func(File) error) error {
	params := file.ListParams{
		Limit:   ucare.Uint64(PageSize),
		OrderBy: ucare.String("datetime_uploaded"),
	}
	if !o.From.IsZero() {
		// The library formats from without a zone, so it must already be UTC.
		from := o.From.UTC()
		params.StartingFrom = &from
	}
	if o.Removed {
		params.Removed = ucare.Bool(true)
	}
	list, err := c.files.List(ctx, params)
	if err != nil {
		return err
	}
	for list.Next() {
		info, err := list.ReadResult()
		if err != nil {
			return err
		}
		if err := fn(fromInfo(info)); err != nil {
			return err
		}
	}
	return nil
}

// Info fetches one file's current info.
func (c *Client) Info(ctx context.Context, uuid string) (File, error) {
	info, err := c.files.Info(ctx, uuid, nil)
	if err != nil {
		return File{}, err
	}
	return fromInfo(&info), nil
}

// DeleteStorage soft-deletes a file: its info stays, its content goes.
func (c *Client) DeleteStorage(ctx context.Context, uuid string) error {
	_, err := c.files.Delete(ctx, uuid)
	return err
}

func (c *Client) SetMetadata(ctx context.Context, uuid, key, value string) error {
	_, err := c.meta.Set(ctx, uuid, key, value)
	return err
}

func (c *Client) DeleteMetadata(ctx context.Context, uuid, key string) error {
	return c.meta.Delete(ctx, uuid, key)
}

// SetTags replaces a file's tags and returns the list the API stored,
// normalised.
func (c *Client) SetTags(ctx context.Context, uuid string, tags []string) ([]string, error) {
	res, err := c.tags.Replace(ctx, uuid, tags)
	return res.Tags, err
}

// Group is a file group. Members are the files in group order, nil where
// the API returns no file; only GroupInfo fills them.
type Group struct {
	ID        string
	CreatedAt time.Time
	Members   []*File
}

// ListGroups walks GET /groups/ in creation order from a time, inclusive at
// second precision.
func (c *Client) ListGroups(ctx context.Context, from time.Time, fn func(Group) error) error {
	params := group.ListParams{Limit: ucare.Uint64(PageSize), OrderBy: ucare.String(group.OrderByCreatedAtAsc)}
	if !from.IsZero() {
		from = from.UTC()
		params.StartingFrom = &from
	}
	list, err := c.groups.List(ctx, params)
	if err != nil {
		return err
	}
	for list.Next() {
		info, err := list.ReadResult()
		if err != nil {
			return err
		}
		g := Group{ID: info.ID}
		if info.CreatedAt != nil {
			g.CreatedAt = info.CreatedAt.UTC()
		}
		if err := fn(g); err != nil {
			return err
		}
	}
	return nil
}

// GroupInfo reads a group with its members from GET /groups/{id}/.
// uploadcare-go's group.Info leaves out the files, so the request goes
// through the client directly.
func (c *Client) GroupInfo(ctx context.Context, id string) (Group, error) {
	req, err := c.uc.NewRequest(ctx, restHost, http.MethodGet, "/groups/"+id+"/", nil)
	if err != nil {
		return Group{}, err
	}
	var res struct {
		ID        string       `json:"id"`
		CreatedAt time.Time    `json:"datetime_created"`
		Files     []*file.Info `json:"files"`
	}
	if err := c.uc.Do(req, &res); err != nil {
		return Group{}, err
	}
	g := Group{ID: res.ID, CreatedAt: res.CreatedAt.UTC(), Members: make([]*File, len(res.Files))}
	for i, info := range res.Files {
		if info != nil {
			f := fromInfo(info)
			g.Members[i] = &f
		}
	}
	return g, nil
}

// IsNotFound reports a REST 404.
func IsNotFound(err error) bool {
	var apiErr ucare.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// AutostoreEnabled reports the project setting that decides whether uploads
// are stored without an explicit store request.
func (c *Client) AutostoreEnabled(ctx context.Context) (bool, error) {
	info, err := c.project.Info(ctx)
	return info.AutostoreEnabled, err
}

func fromInfo(info *file.Info) File {
	f := File{
		UUID:     info.ID,
		Name:     info.OriginalFileName,
		Size:     int64(info.Size),
		MIME:     info.MimeType,
		IsImage:  info.IsImage,
		IsReady:  info.IsReady,
		Metadata: info.Metadata,
		Tags:     info.Tags,
	}
	if info.UploadedAt != nil {
		f.UploadedAt = info.UploadedAt.UTC()
	}
	if info.StoredAt != nil {
		f.StoredAt = info.StoredAt.UTC()
	}
	if info.RemovedAt != nil {
		f.RemovedAt = info.RemovedAt.UTC()
	}
	if ci := info.ContentInfo; ci != nil && ci.Image != nil {
		f.Width, f.Height = int(ci.Image.Width), int(ci.Image.Height)
	}
	return f
}

// MaxREST bounds the REST requests in flight, so a walk that reads the
// info of many files, such as getfattr -R, cannot flood the API. Upload API
// requests and parts have their own bounds (spool and upload concurrency).
const MaxREST = 4

// API states a Health reports. Denied is a 401 or a 403 such as
// "Operation denied" (code permission_error), which a project gets once
// over its usage limit; Throttled is a 429.
const (
	StateOK        = "ok"
	StateDenied    = "denied"
	StateThrottled = "throttled"
)

// Health is the state of the REST API as its latest answers showed it:
// Since is when the state began, Detail what the API said.
type Health struct {
	State  string
	Since  time.Time
	Detail string
}

// Degraded reports a state other than ok that has lasted at least d, and
// the error writes fail with while it lasts.
func (h Health) Degraded(now time.Time, d time.Duration) error {
	if h.State == StateOK || now.Sub(h.Since) < d {
		return nil
	}
	if h.State == StateDenied {
		return fmt.Errorf("%w since %s: %s", ErrDenied, h.Since.Format(time.RFC3339), h.Detail)
	}
	return fmt.Errorf("%w since %s", ErrThrottled, h.Since.Format(time.RFC3339))
}

var (
	ErrDenied    = fmt.Errorf("API denied: %w", syscall.EACCES)
	ErrThrottled = fmt.Errorf("API throttled: %w", syscall.EAGAIN)
)

// Stats counts what the client sent, including requests that the library's
// throttle retry hides from callers.
type Stats struct {
	mu        sync.Mutex
	requests  int
	rest      int
	upload    int
	parts     int
	throttled int
	total     time.Duration
	max       time.Duration
	state     Health
}

// StatsSnapshot splits Requests into REST, Upload API and multipart part
// PUTs, which go to storage hosts.
type StatsSnapshot struct {
	Requests  int
	REST      int
	Upload    int
	Parts     int
	Throttled int
	Total     time.Duration
	Max       time.Duration
}

func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return StatsSnapshot{Requests: s.requests, REST: s.rest, Upload: s.upload, Parts: s.parts,
		Throttled: s.throttled, Total: s.total, Max: s.max}
}

func (s *Stats) health() Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.State == "" {
		return Health{State: StateOK}
	}
	return s.state
}

func (s *Stats) record(host string, d time.Duration, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	switch host {
	case restHost:
		s.rest++
	case uploadHost:
		s.upload++
	default:
		s.parts++
	}
	if status == http.StatusTooManyRequests {
		s.throttled++
	}
	s.total += d
	s.max = max(s.max, d)
}

// observe moves the REST state by an answer's status. A 5xx says nothing
// about access, and neither does a network error, which never gets here.
func (s *Stats) observe(status int, detail string) {
	state := StateOK
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		state = StateDenied
	case status == http.StatusTooManyRequests:
		state = StateThrottled
	case status >= 500:
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state.State != state {
		s.state = Health{State: state, Since: time.Now()}
	}
	s.state.Detail = detail
}

type countingTransport struct {
	base  http.RoundTripper
	stats *Stats
	// rest holds a token per REST request in flight, until its body is
	// closed.
	rest chan struct{}
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	isREST := req.URL.Host == restHost
	if isREST {
		select {
		case t.rest <- struct{}{}:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	t.stats.record(req.URL.Host, time.Since(start), status)
	if !isREST {
		return resp, err
	}
	if err != nil {
		<-t.rest
		return resp, err
	}
	detail := ""
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		detail = peekDetail(resp)
	}
	t.stats.observe(status, detail)
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: func() { <-t.rest }}
	return resp, nil
}

// peekDetail reads the start of an error answer for the status line of
// `ucfs status` and puts it back for the caller.
func peekDetail(resp *http.Response) string {
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return strings.TrimSpace(string(head))
}

type releasingBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
