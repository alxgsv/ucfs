package fake

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DeniedBody is what the live REST API answered with 403 for every request
// once a project went over its usage limit (2026-11-08).
const DeniedBody = `{"errors":{"non_field_errors":["Operation denied."]},"code":"permission_error"}`

// DenyREST makes every REST request answer 403 with DeniedBody, or stops
// it. The Upload API and the CDN keep serving, as they did live.
func (s *Server) DenyREST(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denied = on
}

// ThrottleREST makes every REST request answer 429 with Retry-After set to
// retryAfter seconds; zero stops it. A Retry-After above uploadcare-go's
// 60 s cap fails a request without waiting.
func (s *Server) ThrottleREST(retryAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.throttleAfter = retryAfter
}

// FailCDN makes every CDN request answer status; zero stops it.
func (s *Server) FailCDN(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cdnStatus = status
}

// ChangeOriginal makes the CDN serve other bytes, with another ETag, for a
// file, as if its content had been replaced under the same uuid.
func (s *Server) ChangeOriginal(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.originalGen[uuid]++
}

// SetRESTLatency delays every REST answer.
func (s *Server) SetRESTLatency(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restLatency = d
}

// PeakREST is the most REST requests the fake served at once.
func (s *Server) PeakREST() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restPeak
}

// restEnter counts a REST request in flight and applies the latency; the
// returned function ends it.
func (s *Server) restEnter(r *http.Request) func() {
	if isUploadPath(r.URL.Path) {
		return func() {}
	}
	s.mu.Lock()
	s.restInflight++
	s.restPeak = max(s.restPeak, s.restInflight)
	d := s.restLatency
	s.mu.Unlock()
	time.Sleep(d)
	return func() {
		s.mu.Lock()
		s.restInflight--
		s.mu.Unlock()
	}
}

func isUploadPath(p string) bool {
	return p == "/base/" || strings.HasPrefix(p, "/multipart/") || strings.HasPrefix(p, "/part/")
}

// restFailure answers a REST request that DenyREST or ThrottleREST
// refuses; it returns false when the request should be served.
func (s *Server) restFailure(w http.ResponseWriter, r *http.Request) bool {
	if isUploadPath(r.URL.Path) {
		return false
	}
	s.mu.Lock()
	denied, after := s.denied, s.throttleAfter
	s.mu.Unlock()
	switch {
	case denied:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(DeniedBody))
	case after > 0:
		w.Header().Set("Retry-After", strconv.Itoa(after))
		http.Error(w, `{"detail":"Request was throttled."}`, http.StatusTooManyRequests)
	default:
		return false
	}
	return true
}
