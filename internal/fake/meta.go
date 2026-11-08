package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The limits and messages below are the live API's (2026-11-08).
const (
	maxMetaKeys  = 50
	maxKeyLen    = 64
	maxValueLen  = 512
	maxTags      = 50
	maxTagLen    = 100
	groupsLayout = "2006-01-02T15:04:05"
)

var tagRe = regexp.MustCompile(`^[a-z0-9._-]+$`)

// Group is a file group as the fake project holds it.
type Group struct {
	ID      string
	Created time.Time
	Files   []string
}

// NoTags makes file info carry no tags field, as for a project without the
// tags feature.
func (s *Server) NoTags() { s.noTags.Store(true) }

// SetMetadata changes a file's metadata as another client would.
func (s *Server) SetMetadata(uuid, key, value string) {
	s.update(uuid, func(f *File) { f.Metadata = with(f.Metadata, key, &value) })
}

// SetUploaded moves a file's upload time, keeping the listing in upload
// order.
func (s *Server) SetUploaded(uuid string, t time.Time) {
	s.update(uuid, func(f *File) { f.Uploaded = t })
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.SliceStable(s.files, func(i, j int) bool { return s.files[i].Uploaded.Before(s.files[j].Uploaded) })
}

// SetTags changes a file's tags as another client would.
func (s *Server) SetTags(uuid string, tags ...string) {
	s.update(uuid, func(f *File) { f.Tags = slices.Clone(tags) })
}

// AddGroup creates a group of files, as the Upload API's group endpoint
// does, and returns its id.
func (s *Server) AddGroup(created time.Time, files ...string) string {
	g := Group{ID: fmt.Sprintf("%s~%d", newUUID(), len(files)), Created: created.UTC(), Files: slices.Clone(files)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups = append(s.groups, g)
	sort.SliceStable(s.groups, func(i, j int) bool { return s.groups[i].Created.Before(s.groups[j].Created) })
	return g.ID
}

// with returns a copy of m with key set to *v, or removed when v is nil, so
// a map handed out in JSON is never changed underneath.
func with(m map[string]string, key string, v *string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, x := range m {
		out[k] = x
	}
	if v == nil {
		delete(out, key)
	} else {
		out[key] = *v
	}
	return out
}

func detail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": msg})
}

func (s *Server) getMetadata(w http.ResponseWriter, r *http.Request) {
	f, ok := s.find(r.PathValue("uuid"))
	if !ok {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	meta := f.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	writeJSON(w, meta)
}

func (s *Server) getMetadataKey(w http.ResponseWriter, r *http.Request) {
	f, ok := s.find(r.PathValue("uuid"))
	v, has := f.Metadata[r.PathValue("key")]
	if !ok || !has {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	writeJSON(w, v)
}

func (s *Server) putMetadata(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var v string
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		detail(w, http.StatusBadRequest, "invalid value")
		return
	}
	switch {
	case utf8.RuneCountInString(key) > maxKeyLen:
		detail(w, http.StatusBadRequest, fmt.Sprintf("Length of key `%s` is exceeded. The limit is %d.", key, maxKeyLen))
		return
	case v == "":
		detail(w, http.StatusBadRequest, fmt.Sprintf("Value of key `%s` can not be empty.", key))
		return
	case utf8.RuneCountInString(v) > maxValueLen:
		detail(w, http.StatusBadRequest, fmt.Sprintf("Value length of key `%s` is exceeded. The limit is %d.", key, maxValueLen))
		return
	}
	full := false
	if !s.update(r.PathValue("uuid"), func(f *File) {
		if _, has := f.Metadata[key]; !has && len(f.Metadata) >= maxMetaKeys {
			full = true
			return
		}
		f.Metadata = with(f.Metadata, key, &v)
	}) {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	if full {
		detail(w, http.StatusBadRequest, "Number of metadata keys per file has been exceeded. The limit is 50.")
		return
	}
	writeJSON(w, v)
}

// deleteMetadata answers 204 whether the key existed or not, as the API does.
func (s *Server) deleteMetadata(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !s.update(r.PathValue("uuid"), func(f *File) { f.Metadata = with(f.Metadata, key, nil) }) {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// normalizeTags mirrors the API: lowercased, trimmed, deduplicated, and
// validated; the error is the API's 400 body.
func normalizeTags(tags []string) ([]string, any) {
	out := []string{}
	for i, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		var msg string
		switch {
		case t == "":
			msg = "This field may not be blank."
		case utf8.RuneCountInString(t) > maxTagLen:
			msg = fmt.Sprintf("Tag too long: %d chars (max %d).", utf8.RuneCountInString(t), maxTagLen)
		case !tagRe.MatchString(t):
			msg = "Tag contains invalid characters. Allowed: Latin letters, digits, hyphen, underscore, dot."
		}
		if msg != "" {
			return nil, map[string]any{"tags": map[string][]string{strconv.Itoa(i): {msg}}}
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if len(out) > maxTags {
		return nil, map[string]any{"tags": []string{fmt.Sprintf("Too many tags: %d (max %d).", len(out), maxTags)}}
	}
	return out, nil
}

func (s *Server) getTags(w http.ResponseWriter, r *http.Request) {
	f, ok := s.find(r.PathValue("uuid"))
	if !ok {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	tags := f.Tags
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, map[string]any{"tags": tags})
}

func (s *Server) putTags(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Tags == nil {
		detail(w, http.StatusBadRequest, "tags must be a list")
		return
	}
	tags, bad := normalizeTags(in.Tags)
	if bad != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(bad)
		return
	}
	var old []string
	if !s.update(r.PathValue("uuid"), func(f *File) { old, f.Tags = f.Tags, tags }) {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	added, deleted := []string{}, []string{}
	for _, t := range tags {
		if !slices.Contains(old, t) {
			added = append(added, t)
		}
	}
	for _, t := range old {
		if !slices.Contains(tags, t) {
			deleted = append(deleted, t)
		}
	}
	sort.Strings(added)
	sort.Strings(deleted)
	writeJSON(w, map[string]any{"tags": tags, "added": added, "deleted": deleted})
}

// search mirrors POST /files/search/ for tags any/all/none and exact
// metadata matches, without the asynchronous indexing.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tags *struct {
			Any  []string `json:"any"`
			All  []string `json:"all"`
			None []string `json:"none"`
		} `json:"tags"`
		Exact  map[string][]string `json:"exact"`
		Limit  int                 `json:"limit"`
		Offset int                 `json:"offset"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || (in.Tags == nil && len(in.Exact) == 0) {
		detail(w, http.StatusBadRequest, "At least one condition is required.")
		return
	}
	if in.Limit <= 0 || in.Limit > 100 {
		in.Limit = 20
	}
	match := func(f File) bool {
		if f.Removed {
			return false
		}
		if t := in.Tags; t != nil {
			has := func(tag string) bool { return slices.Contains(f.Tags, strings.ToLower(tag)) }
			if len(t.Any) > 0 && !slices.ContainsFunc(t.Any, has) {
				return false
			}
			for _, tag := range t.All {
				if !has(tag) {
					return false
				}
			}
			if slices.ContainsFunc(t.None, has) {
				return false
			}
		}
		for field, values := range in.Exact {
			key, ok := strings.CutPrefix(field, "metadata[")
			if !ok {
				continue
			}
			if v, has := f.Metadata[strings.TrimSuffix(key, "]")]; !has || !slices.Contains(values, v) {
				return false
			}
		}
		return true
	}
	s.mu.Lock()
	var found []File
	for _, f := range s.files {
		if match(f) {
			found = append(found, f)
		}
	}
	s.mu.Unlock()
	resp := struct {
		Next    *string           `json:"next"`
		Total   int               `json:"total"`
		PerPage int               `json:"per_page"`
		Results []json.RawMessage `json:"results"`
	}{Total: len(found), PerPage: in.Limit, Results: []json.RawMessage{}}
	for i := in.Offset; i < len(found) && i < in.Offset+in.Limit; i++ {
		resp.Results = append(resp.Results, s.fileJSON(found[i]))
	}
	writeJSON(w, resp)
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var from time.Time
	after := false
	if v := q.Get("from"); v != "" {
		var err error
		if from, err = time.Parse(time.RFC3339Nano, v); err == nil {
			after = q.Get("after") == "1"
		} else if from, err = time.Parse(groupsLayout, v); err != nil {
			detail(w, http.StatusBadRequest, "bad from")
			return
		}
	}
	s.mu.Lock()
	var page []Group
	more := false
	for _, g := range s.groups {
		if g.Created.Before(from) || (after && g.Created.Equal(from)) {
			continue
		}
		if len(page) == limit {
			more = true
			break
		}
		page = append(page, g)
	}
	s.mu.Unlock()
	resp := struct {
		Next    *string          `json:"next"`
		PerPage int              `json:"per_page"`
		Results []map[string]any `json:"results"`
	}{PerPage: limit, Results: []map[string]any{}}
	for _, g := range page {
		resp.Results = append(resp.Results, groupJSON(g))
	}
	if more {
		next := url.Values{}
		next.Set("limit", strconv.Itoa(limit))
		next.Set("ordering", "datetime_created")
		next.Set("from", page[len(page)-1].Created.Format(time.RFC3339Nano))
		next.Set("after", "1")
		link := fmt.Sprintf("https://%s/groups/?%s", restHost, next.Encode())
		resp.Next = &link
	}
	writeJSON(w, resp)
}

func groupJSON(g Group) map[string]any {
	return map[string]any{
		"id":               g.ID,
		"datetime_created": g.Created.Format("2006-01-02T15:04:05.000000Z"),
		"files_count":      len(g.Files),
		"cdn_url":          "https://ucarecdn.com/" + g.ID + "/",
		"url":              fmt.Sprintf("https://%s/groups/%s/", restHost, g.ID),
	}
}

func (s *Server) findGroup(id string) (Group, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		if g.ID == id {
			return g, true
		}
	}
	return Group{}, false
}

// groupInfo answers with the members' file info, null for removed ones.
func (s *Server) groupInfo(w http.ResponseWriter, r *http.Request) {
	g, ok := s.findGroup(r.PathValue("id"))
	if !ok {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	out := groupJSON(g)
	files := []json.RawMessage{}
	for _, uuid := range g.Files {
		f, ok := s.find(uuid)
		if !ok || f.Removed {
			files = append(files, json.RawMessage("null"))
			continue
		}
		files = append(files, s.fileJSON(f))
	}
	out["files"] = files
	writeJSON(w, out)
}

// DeleteGroup deletes a group as another client would.
func (s *Server) DeleteGroup(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.groups)
	s.groups = slices.DeleteFunc(s.groups, func(g Group) bool { return g.ID == id })
	return n != len(s.groups)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if !s.DeleteGroup(r.PathValue("id")) {
		detail(w, http.StatusNotFound, "Not found.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createGroup is the Upload API's POST /group/ with files[N]=<uuid>.
func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var files []string
	for i := 0; ; i++ {
		v := r.Form.Get(fmt.Sprintf("files[%d]", i))
		if v == "" {
			break
		}
		files = append(files, v)
	}
	if len(files) == 0 {
		http.Error(w, "no files[N] uuids specified", http.StatusBadRequest)
		return
	}
	id := s.AddGroup(time.Now(), files...)
	g, _ := s.findGroup(id)
	writeJSON(w, groupJSON(g))
}
