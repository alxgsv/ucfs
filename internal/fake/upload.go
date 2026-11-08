package fake

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const partSize = 5 << 20

// uploads is the Upload API side of the fake: what was uploaded, the
// multipart uploads in progress and the failures tests asked for.
type uploads struct {
	content map[string][]byte
	pending map[string]*multipartUpload
	reject  func(name string) bool
	noParts bool
	fail    int
	latency time.Duration
	stats   UploadStats
}

type multipartUpload struct {
	file  File
	parts map[int][]byte
}

// UploadStats counts Upload API requests by kind.
type UploadStats struct {
	Base, Start, Parts, Complete int
	// Store is the UPLOADCARE_STORE value of the last upload.
	Store string
}

// RejectUploads makes uploads whose name matches answer 400 "Uploading of
// these file types is not allowed.", as projects that restrict file types do.
func (s *Server) RejectUploads(match func(name string) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.reject = match
}

// RejectParts makes multipart part PUTs answer 403, which leaves uploads
// started and never completed.
func (s *Server) RejectParts(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.noParts = on
}

// FailUploads makes the next n Upload API requests, parts included, answer 500.
func (s *Server) FailUploads(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.fail = n
}

// SetUploadLatency delays every Upload API request, parts included.
func (s *Server) SetUploadLatency(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.latency = d
}

func (s *Server) UploadStats() UploadStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.up.stats
}

// File returns a file of the project.
func (s *Server) File(uuid string) (File, bool) { return s.find(uuid) }

// Body returns what the CDN serves for a file.
func (s *Server) Body(uuid string) []byte {
	f, _ := s.find(uuid)
	return s.content(f)
}

// Pending returns the uuids of multipart uploads started and not completed;
// the project lists them as files that are not ready.
func (s *Server) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id := range s.up.pending {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func (s *Server) content(f File) []byte {
	b := make([]byte, f.Size)
	r, _ := s.original(f)
	_, _ = r.ReadAt(b, 0)
	return b
}

// md5Limit is the largest generated file whose ETag is its MD5, as the
// CDN's is for base uploads; larger ones get a tag derived from the uuid,
// so a range of a large file costs only the bytes asked for.
const md5Limit = 16 << 20

// original is what the CDN serves for a file: an upload's bytes, or
// content generated from the uuid and the ChangeOriginal count, and its
// ETag.
func (s *Server) original(f File) (*io.SectionReader, string) {
	s.mu.Lock()
	b, uploaded := s.up.content[f.UUID]
	gen := s.originalGen[f.UUID]
	s.mu.Unlock()
	if uploaded && gen == 0 {
		return io.NewSectionReader(bytes.NewReader(b), 0, int64(len(b))), fmt.Sprintf(`"%x"`, md5.Sum(b))
	}
	key := f.UUID
	if gen > 0 {
		key += "#" + strconv.Itoa(gen)
	}
	r := io.NewSectionReader(generated(key), 0, f.Size)
	if f.Size > md5Limit {
		h := fnv.New64a()
		_, _ = h.Write([]byte(key))
		return r, fmt.Sprintf(`"%016x%016x"`, h.Sum64(), f.Size)
	}
	s.mu.Lock()
	etag, ok := s.etags[key]
	s.mu.Unlock()
	if !ok {
		etag = fmt.Sprintf(`"%x"`, md5.Sum(Content(key, f.Size)))
		s.mu.Lock()
		s.etags[key] = etag
		s.mu.Unlock()
	}
	return r, etag
}

func newUUID() string {
	return fmt.Sprintf("%08x-%04x-4%03x-8%03x-%012x", rand.Uint32(), rand.Uint32N(1<<16), rand.Uint32N(1<<12),
		rand.Uint32N(1<<12), rand.Uint64N(1<<48))
}

// uploadGate applies the configured latency and failures; it returns false
// when it answered the request.
func (s *Server) uploadGate(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	delay := s.up.latency
	fail := s.up.fail > 0
	if fail {
		s.up.fail--
	}
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return false
		}
	}
	if fail {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return false
	}
	return true
}

// readForm reads an upload form: the fields, and the file part's raw name and
// content when there is one.
func readForm(r *http.Request) (fields map[string]string, name string, body []byte, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, "", nil, err
	}
	fields = map[string]string{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return fields, name, body, nil
		}
		if err != nil {
			return nil, "", nil, err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, "", nil, err
		}
		_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if fn, ok := params["filename"]; ok {
			name, body = fn, data
			continue
		}
		fields[part.FormName()] = string(data)
	}
}

// newFile builds the file an upload form describes, or answers 400.
func (s *Server) newFile(w http.ResponseWriter, fields map[string]string, name string, size int64) (File, bool) {
	s.mu.Lock()
	reject := s.up.reject
	s.up.stats.Store = fields["UPLOADCARE_STORE"]
	s.mu.Unlock()
	if reject != nil && reject(name) {
		http.Error(w, "Uploading of these file types is not allowed.", http.StatusBadRequest)
		return File{}, false
	}
	f := File{UUID: newUUID(), Name: name, Size: size, Uploaded: time.Now().UTC(),
		Stored: fields["UPLOADCARE_STORE"] != "0", Metadata: map[string]string{}}
	for k, v := range fields {
		if key, ok := strings.CutPrefix(k, "metadata["); ok {
			if utf8.RuneCountInString(v) > 512 {
				http.Error(w, "metadata value is too long", http.StatusBadRequest)
				return File{}, false
			}
			f.Metadata[strings.TrimSuffix(key, "]")] = v
		}
	}
	if t := fields["tags"]; t != "" {
		f.Tags = strings.Split(t, ",")
	}
	return f, true
}

func detectMIME(body []byte) string {
	return strings.SplitN(http.DetectContentType(body), ";", 2)[0]
}

// baseUpload keeps only what follows the last \ or / of the filename, as
// the real Upload API does (Spike D).
func (s *Server) baseUpload(w http.ResponseWriter, r *http.Request) {
	if !s.uploadGate(w, r) {
		return
	}
	fields, name, body, err := readForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if i := strings.LastIndexAny(name, `\/`); i >= 0 {
		name = name[i+1:]
	}
	f, ok := s.newFile(w, fields, name, int64(len(body)))
	if !ok {
		return
	}
	f.MIME = detectMIME(body)
	s.mu.Lock()
	s.up.stats.Base++
	if s.up.content == nil {
		s.up.content = map[string][]byte{}
	}
	s.up.content[f.UUID] = body
	s.mu.Unlock()
	s.Add(f)
	writeJSON(w, map[string]string{"file": f.UUID})
}

func (s *Server) multipartStart(w http.ResponseWriter, r *http.Request) {
	if !s.uploadGate(w, r) {
		return
	}
	fields, _, _, err := readForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	size, _ := strconv.ParseInt(fields["size"], 10, 64)
	if size <= 10<<20 {
		http.Error(w, "File size is too small for multipart upload.", http.StatusBadRequest)
		return
	}
	f, ok := s.newFile(w, fields, fields["filename"], size)
	if !ok {
		return
	}
	f.MIME = fields["content_type"]
	var urls []string
	for n := 1; int64(n-1)*partSize < size; n++ {
		urls = append(urls, fmt.Sprintf("https://%s/part/%s/%d", partsHost, f.UUID, n))
	}
	s.mu.Lock()
	s.up.stats.Start++
	if s.up.pending == nil {
		s.up.pending = map[string]*multipartUpload{}
	}
	s.up.pending[f.UUID] = &multipartUpload{file: f, parts: map[int][]byte{}}
	s.mu.Unlock()
	f.NotReady = true
	s.Add(f)
	writeJSON(w, map[string]any{"uuid": f.UUID, "parts": urls})
}

func (s *Server) putPart(w http.ResponseWriter, r *http.Request) {
	if !s.uploadGate(w, r) {
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.stats.Parts++
	mp, ok := s.up.pending[r.PathValue("uuid")]
	if !ok || s.up.noParts {
		http.Error(w, "no such upload", http.StatusForbidden)
		return
	}
	mp.parts[n] = body
}

func (s *Server) multipartComplete(w http.ResponseWriter, r *http.Request) {
	if !s.uploadGate(w, r) {
		return
	}
	fields, _, _, err := readForm(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.up.stats.Complete++
	mp, ok := s.up.pending[fields["uuid"]]
	var body []byte
	if ok {
		for n := 1; n <= len(mp.parts); n++ {
			body = append(body, mp.parts[n]...)
		}
	}
	switch {
	case !ok:
		s.mu.Unlock()
		http.Error(w, "File is not found.", http.StatusNotFound)
		return
	case int64(len(body)) != mp.file.Size:
		s.mu.Unlock()
		http.Error(w, "File size mismatch.", http.StatusBadRequest)
		return
	}
	delete(s.up.pending, fields["uuid"])
	if s.up.content == nil {
		s.up.content = map[string][]byte{}
	}
	s.up.content[mp.file.UUID] = body
	s.mu.Unlock()
	s.update(mp.file.UUID, func(f *File) { f.NotReady = false })
	writeJSON(w, map[string]any{"uuid": mp.file.UUID, "mime_type": mp.file.MIME, "size": mp.file.Size,
		"original_filename": mp.file.Name, "is_stored": mp.file.Stored, "is_ready": true})
}

func (s *Server) update(uuid string, fn func(*File)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.files {
		if s.files[i].UUID == uuid {
			fn(&s.files[i])
			return true
		}
	}
	return false
}

func (s *Server) deleteStorage(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if !s.update(uuid, func(f *File) { f.Removed = true }) {
		http.Error(w, `{"detail":"Not found."}`, http.StatusNotFound)
		return
	}
	f, _ := s.find(uuid)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.fileJSON(f))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Files returns the project's files that are not removed.
func (s *Server) Files() []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []File
	for _, f := range s.files {
		if !f.Removed {
			out = append(out, f)
		}
	}
	return out
}
