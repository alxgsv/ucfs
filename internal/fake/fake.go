// Package fake provides httptest doubles of the Uploadcare REST API and CDN
// for tests.
package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	restHost   = "api.uploadcare.com"
	uploadHost = "upload.uploadcare.com"
	// fromLayout is what uploadcare-go sends; the API's own next links carry
	// microseconds and a zone, which time.RFC3339Nano also parses.
	fromLayout = "2006-01-02T15:04:05"
)

// File is a file as the fake project holds it.
type File struct {
	UUID     string
	Name     string
	Size     int64
	MIME     string
	Uploaded time.Time
	Stored   bool
	Removed  bool
	Width    int
	Height   int
	Metadata map[string]string
}

// Server is a fake Uploadcare project.
type Server struct {
	*httptest.Server

	mu          sync.Mutex
	files       []File
	throttle    int
	requests    int
	latency     map[string]time.Duration
	cdnReqs     int
	cdnHeads    int
	cdnBytes    int64
	variantsGen int
}

func New(t testing.TB) *Server {
	s := &Server{latency: map[string]time.Duration{}}
	rest := http.NewServeMux()
	rest.HandleFunc("GET /files/", s.listFiles)
	rest.HandleFunc("GET /files/{uuid}/", s.fileInfo)
	mux := http.NewServeMux()
	mux.Handle("/", s.count(rest))
	mux.HandleFunc("GET /cdn/{uuid}/{ops...}", s.serveCDN)
	mux.HandleFunc("HEAD /cdn/{uuid}/{ops...}", s.serveCDN)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// CDNBase is the base URL of the fake CDN.
func (s *Server) CDNBase() string { return s.URL + "/cdn" }

// Content is the deterministic body the fake CDN serves for a file.
func Content(uuid string, size int64) []byte {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uuid))
	r := rand.New(rand.NewPCG(h.Sum64(), 0))
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// BadArg is the argument the fake CDN rejects with 400, as the real one
// rejects "-/resize/abc/".
const BadArg = "bad"

// Variant is what the fake CDN serves for an operations path such as
// "-/resize/100x/-/format/webp/": deterministic bytes, a size derived from
// the original's, and a weak ETag. ChangeVariants moves every variant to
// new bytes and ETag of the same size.
func (s *Server) Variant(uuid, ops string) (body []byte, etag, mime string) {
	f, _ := s.find(uuid)
	s.mu.Lock()
	gen := s.variantsGen
	s.mu.Unlock()
	return variant(f, ops, gen)
}

func variant(f File, ops string, gen int) (body []byte, etag, mime string) {
	key := fmt.Sprintf("%s|%s|%d", f.UUID, ops, gen)
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	mime = f.MIME
	if i := strings.LastIndex(ops, "/format/"); i >= 0 {
		mime = "image/" + strings.SplitN(ops[i+len("/format/"):], "/", 2)[0]
	}
	return Content(key, f.Size/2+int64(len(ops))), fmt.Sprintf(`W/"%016x"`, h.Sum64()), mime
}

// ChangeVariants makes the CDN render every variant anew, as an engine
// update would.
func (s *Server) ChangeVariants() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.variantsGen++
}

// SetLatency delays every CDN response for one file.
func (s *Server) SetLatency(uuid string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latency[uuid] = d
}

// Remove marks a file removed, as a soft delete would.
func (s *Server) Remove(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.files {
		if s.files[i].UUID == uuid {
			s.files[i].Removed = true
		}
	}
}

// CDNStats returns the CDN requests served and body bytes written.
func (s *Server) CDNStats() (requests int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cdnReqs, s.cdnBytes
}

// CDNMethods splits the CDN requests served into HEADs and GETs.
func (s *Server) CDNMethods() (heads, gets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cdnHeads, s.cdnReqs - s.cdnHeads
}

func (s *Server) find(uuid string) (File, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.files {
		if f.UUID == uuid {
			return f, true
		}
	}
	return File{}, false
}

func (s *Server) fileInfo(w http.ResponseWriter, r *http.Request) {
	f, ok := s.find(r.PathValue("uuid"))
	if !ok {
		http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(fileJSON(f))
}

func (s *Server) serveCDN(w http.ResponseWriter, r *http.Request) {
	uuid, ops := r.PathValue("uuid"), r.PathValue("ops")
	f, ok := s.find(uuid)
	s.mu.Lock()
	s.cdnReqs++
	if r.Method == http.MethodHead {
		s.cdnHeads++
	}
	delay := s.latency[uuid]
	gen := s.variantsGen
	s.mu.Unlock()
	time.Sleep(delay)
	cw := &countingWriter{ResponseWriter: w}
	defer func() {
		s.mu.Lock()
		s.cdnBytes += cw.n
		s.mu.Unlock()
	}()
	switch {
	case !ok || f.Removed:
		http.NotFound(cw, r)
	case slices.Contains(strings.Split(ops, "/"), BadArg):
		cw.Header().Set("Content-Type", "text/html; charset=UTF-8")
		cw.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(cw, "<html><head><title>400: Bad Request</title></head><body><h3>Bad Request</h3>\n"+
			"<p>Cannot parse url part: %s</p>\n<p>See <a href=\"https://uploadcare.com/docs/\">CDN documentation</a>.</p></body></html>", BadArg)
	case ops == "":
		http.ServeContent(cw, r, "", f.Uploaded, bytes.NewReader(Content(uuid, f.Size)))
	default:
		body, etag, mime := variant(f, ops, gen)
		cw.Header().Set("ETag", etag)
		cw.Header().Set("Content-Type", mime)
		http.ServeContent(cw, r, "", time.Time{}, bytes.NewReader(body))
	}
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// Add puts files into the project; listing order is by upload time.
func (s *Server) Add(files ...File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = append(s.files, files...)
	sort.SliceStable(s.files, func(i, j int) bool { return s.files[i].Uploaded.Before(s.files[j].Uploaded) })
}

// Throttle makes the next n requests answer 429 with Retry-After: 1.
func (s *Server) Throttle(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttle = n
}

// Requests is the number of requests served, throttled ones included.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// Transport routes requests for the real API hosts to this server.
// The CDN needs no rewriting: point the CDN base at CDNBase.
func (s *Server) Transport() http.RoundTripper {
	target, _ := url.Parse(s.URL)
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case restHost, uploadHost:
			req = req.Clone(req.Context())
			req.URL.Scheme, req.URL.Host = target.Scheme, target.Host
		}
		return http.DefaultTransport.RoundTrip(req)
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (s *Server) count(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests++
		throttled := s.throttle > 0
		if throttled {
			s.throttle--
		}
		s.mu.Unlock()
		if throttled {
			w.Header().Set("Retry-After", "1")
			http.Error(w, `{"detail":"Request was throttled."}`, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// listFiles mirrors GET /files/ ordered by datetime_uploaded: from is
// inclusive, and next links continue strictly after the last item.
func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	removed := q.Get("removed") == "true"
	var from time.Time
	after := false
	if v := q.Get("from"); v != "" {
		var err error
		if from, err = time.Parse(time.RFC3339Nano, v); err == nil {
			after = q.Get("after") == "1"
		} else if from, err = time.Parse(fromLayout, v); err != nil {
			http.Error(w, `{"detail":"bad from"}`, http.StatusBadRequest)
			return
		}
	}

	s.mu.Lock()
	var page []File
	more := false
	for _, f := range s.files {
		if f.Removed != removed || f.Uploaded.Before(from) || (after && f.Uploaded.Equal(from)) {
			continue
		}
		if len(page) == limit {
			more = true
			break
		}
		page = append(page, f)
	}
	s.mu.Unlock()

	resp := struct {
		Next    *string           `json:"next"`
		PerPage int               `json:"per_page"`
		Results []json.RawMessage `json:"results"`
	}{PerPage: limit, Results: []json.RawMessage{}}
	for _, f := range page {
		resp.Results = append(resp.Results, fileJSON(f))
	}
	if more {
		next := url.Values{}
		next.Set("limit", strconv.Itoa(limit))
		next.Set("ordering", "datetime_uploaded")
		next.Set("from", page[len(page)-1].Uploaded.UTC().Format(time.RFC3339Nano))
		next.Set("after", "1")
		if removed {
			next.Set("removed", "true")
		}
		link := fmt.Sprintf("https://%s/files/?%s", restHost, next.Encode())
		resp.Next = &link
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func fileJSON(f File) json.RawMessage {
	ts := func(t time.Time) *string {
		if t.IsZero() {
			return nil
		}
		s := t.UTC().Format("2006-01-02T15:04:05.000000Z")
		return &s
	}
	var stored, removed *string
	if f.Stored {
		stored = ts(f.Uploaded)
	}
	if f.Removed {
		removed = ts(f.Uploaded.Add(time.Hour))
	}
	var contentInfo any
	if f.Width > 0 {
		contentInfo = map[string]any{"image": map[string]any{"width": f.Width, "height": f.Height}}
	}
	meta := f.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	b, _ := json.Marshal(map[string]any{
		"uuid":              f.UUID,
		"original_filename": f.Name,
		"size":              f.Size,
		"mime_type":         f.MIME,
		"is_image":          f.Width > 0,
		"is_ready":          true,
		"datetime_uploaded": ts(f.Uploaded),
		"datetime_stored":   stored,
		"datetime_removed":  removed,
		"content_info":      contentInfo,
		"metadata":          meta,
		"url":               fmt.Sprintf("https://%s/files/%s/", restHost, f.UUID),
	})
	return b
}
