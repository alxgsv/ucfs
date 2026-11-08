// Package fake provides httptest doubles of the Uploadcare REST API for tests.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
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

	mu       sync.Mutex
	files    []File
	throttle int
	requests int
}

func New(t testing.TB) *Server {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /files/", s.listFiles)
	s.Server = httptest.NewServer(s.count(mux))
	t.Cleanup(s.Close)
	return s
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
