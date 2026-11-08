package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/fake"
)

// TestSoak keeps a mount busy for UCFS_SOAK (a duration such as 24h) and
// samples the daemon into a CSV at UCFS_SOAK_OUT every UCFS_SOAK_EVERY
// (default 30s): goroutines, file descriptors, heap, RSS, cache size, index
// size and traffic. `make soak` runs it against internal/fake: syncs every
// second, uploads and deletes through the mount, other clients adding,
// removing and changing files and groups, and readers of originals and
// variants. With UCFS_SOAK_LIVE=<project> it mounts that project instead,
// tree/ only, syncing every minute, and uploads a small JPEG every
// UCFS_SOAK_UPLOAD (default 5m) into tree/<UCFS_SOAK_DIR>, which it reads
// in a loop and removes at the end.
func TestSoak(t *testing.T) {
	dur, err := time.ParseDuration(os.Getenv("UCFS_SOAK"))
	if err != nil {
		t.Skip("set UCFS_SOAK to a duration")
	}
	needMounts(t)
	every := envDuration(t, "UCFS_SOAK_EVERY", 30*time.Second)
	out := os.Getenv("UCFS_SOAK_OUT")
	if out == "" {
		out = filepath.Join(t.TempDir(), "soak.csv")
	}
	s := &soak{t: t, deadline: time.Now().Add(dur), errs: map[string]int{}}
	if project := os.Getenv("UCFS_SOAK_LIVE"); project != "" {
		s.live(project)
	} else {
		s.fake()
	}
	s.run(every, out)
}

func envDuration(t *testing.T, key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	return d
}

type soak struct {
	t        *testing.T
	deadline time.Time
	m        *fakeMount
	workers  []func()

	mu   sync.Mutex
	errs map[string]int
	ops  atomic.Int64
}

func (s *soak) done() bool { return time.Now().After(s.deadline) }

// note counts an error by kind. Files removed under a reader are expected
// (ENOENT, ESTALE); anything else is reported at the end.
func (s *soak) note(what string, err error) {
	if err == nil {
		s.ops.Add(1)
		return
	}
	kind := what + ": " + err.Error()
	var errno syscall.Errno
	if errors.As(err, &errno) {
		kind = what + ": " + errno.Error()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[kind]++
}

// every runs fn every d until the deadline.
func (s *soak) every(d time.Duration, fn func()) {
	s.workers = append(s.workers, func() {
		for !s.done() {
			fn()
			time.Sleep(d)
		}
	})
}

func (s *soak) fake() {
	t := s.t
	srv := fake.New(t)
	srv.SetCDNLatency(10 * time.Millisecond)
	start := time.Now().Add(-48 * time.Hour)
	var n atomic.Int64
	newFile := func() fake.File {
		i := n.Add(1)
		size := int64(20<<10 + rand.IntN(300<<10))
		if i%10 == 0 {
			size = int64(1<<20 + rand.IntN(6<<20))
		}
		return fake.File{UUID: fmt.Sprintf("50a40000-0000-4000-8000-%012d", i), Name: fmt.Sprintf("photo-%06d.jpg", i),
			Size: size, MIME: "image/jpeg", Width: 800, Height: 600, Stored: true,
			Uploaded: start.Add(time.Duration(i) * time.Second), Tags: []string{fmt.Sprintf("t%d", i%5)}}
	}
	var foreign []string
	var fmu sync.Mutex
	for range 300 {
		f := newFile()
		srv.Add(f)
		foreign = append(foreign, f.UUID)
	}
	var groups []string
	for i := range 5 {
		groups = append(groups, srv.AddGroup(start.Add(time.Duration(i)*time.Hour), foreign[i], foreign[i+1]))
	}
	cache := t.TempDir()
	global := fakeHome(t, srv, cache)
	s.m = mountFake(t, global, cache, "--sync-interval", "1s", "--sweep-interval", "30s", "--meta-ttl", "10s",
		"--views", "files,tree,by-date,tags,groups", "--cache-size", "128MiB", "--upload-metadata", "ucfs:test=1")
	tree := filepath.Join(s.m.dir, "tree", "soak")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}

	// Other clients: uploads, deletes, metadata changes and groups through
	// the API, which the mount sees by syncing.
	s.every(3*time.Second, func() {
		f := newFile()
		f.Uploaded = time.Now()
		srv.Add(f)
		fmu.Lock()
		foreign = append(foreign, f.UUID)
		fmu.Unlock()
	})
	s.every(3*time.Second, func() {
		fmu.Lock()
		defer fmu.Unlock()
		if len(foreign) > 300 {
			i := rand.IntN(len(foreign))
			srv.Remove(foreign[i])
			foreign = slices.Delete(foreign, i, i+1)
		}
	})
	s.every(4*time.Second, func() {
		fmu.Lock()
		u := foreign[rand.IntN(len(foreign))]
		fmu.Unlock()
		srv.SetMetadata(u, "soak", strconv.Itoa(rand.IntN(1000)))
	})
	s.every(time.Minute, func() {
		fmu.Lock()
		a, b := foreign[rand.IntN(len(foreign))], foreign[rand.IntN(len(foreign))]
		fmu.Unlock()
		groups = append(groups, srv.AddGroup(time.Now(), a, b))
		if len(groups) > 10 {
			srv.DeleteGroup(groups[0])
			groups = groups[1:]
		}
	})
	// The mount's own writes: an upload every two seconds, the oldest
	// removed once there are forty.
	var mine []string
	s.every(2*time.Second, func() {
		p := filepath.Join(tree, fmt.Sprintf("w%d.jpg", time.Now().UnixNano()))
		s.note("write", os.WriteFile(p, randomBytes(10<<10+rand.IntN(200<<10)), 0o644))
		mine = append(mine, p)
		if len(mine) > 40 {
			s.note("rm", os.Remove(mine[0]))
			mine = mine[1:]
		}
	})
	s.readers(3)
	s.every(10*time.Second, func() {
		for _, d := range []string{"tree", "tags", "groups", "by-date"} {
			s.note("walk "+d, walk(filepath.Join(s.m.dir, d), 4))
		}
	})
	s.every(15*time.Second, func() {
		fmu.Lock()
		u := foreign[rand.IntN(len(foreign))]
		fmu.Unlock()
		ops := fmt.Sprintf("-/resize/%dx/", 100+rand.IntN(4)*100)
		dir := filepath.Join(s.m.dir, "files", u, filepath.FromSlash(strings.TrimSuffix(ops, "/")))
		entries, err := os.ReadDir(dir)
		if err == nil && len(entries) > 0 {
			_, err = os.ReadFile(filepath.Join(dir, entries[0].Name()))
		}
		s.note("variant", err)
	})
}

// readers read random files of files/ in full, the cache being smaller than
// what they read, so blocks are evicted and fetched again.
func (s *soak) readers(n int) {
	for range n {
		s.every(200*time.Millisecond, func() {
			entries, err := os.ReadDir(filepath.Join(s.m.dir, "files"))
			if err != nil || len(entries) == 0 {
				s.note("ls files", err)
				return
			}
			e := entries[rand.IntN(len(entries))]
			_, err = os.ReadFile(filepath.Join(s.m.dir, "files", e.Name()))
			s.note("read", err)
		})
	}
}

func walk(dir string, depth int) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() && strings.Count(strings.TrimPrefix(p, dir), string(filepath.Separator)) >= depth {
			return filepath.SkipDir
		}
		return nil
	})
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rand.Uint32())
	}
	return b
}

func (s *soak) live(project string) {
	t := s.t
	dir := os.Getenv("UCFS_SOAK_DIR")
	if dir == "" {
		dir = "p5soak"
	}
	cache := t.TempDir()
	// Debug logging, so the run's log shows whether a key ever reaches it.
	global := []string{"-v", "--project", project, "--cache-dir", cache}
	s.m = mountFake(t, global, cache, "--views", "tree", "--sync-interval", "60s", "--traffic-budget", "50MB",
		"--upload-metadata", "ucfs:test=1")
	tree := filepath.Join(s.m.dir, "tree", dir)
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(tree); err != nil {
			t.Errorf("remove the soak's uploads: %v", err)
		}
	})
	n := 0
	var mine []string
	s.every(envDuration(t, "UCFS_SOAK_UPLOAD", 5*time.Minute), func() {
		n++
		p := filepath.Join(tree, fmt.Sprintf("soak-%02d.jpg", n))
		s.note("write", os.WriteFile(p, testJPEG(n), 0o644))
		mine = append(mine, p)
		if len(mine) > 6 {
			s.note("rm", os.Remove(mine[0]))
			mine = mine[1:]
		}
	})
	s.every(20*time.Second, func() {
		entries, err := os.ReadDir(tree)
		s.note("ls", err)
		for _, e := range entries {
			_, err := os.ReadFile(filepath.Join(tree, e.Name()))
			s.note("read", err)
		}
	})
}

// testJPEG is a small photo-like JPEG: a gradient with some noise, about
// 20 KB at 800×600.
func testJPEG(seed int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	r := rand.New(rand.NewPCG(uint64(seed), 1))
	for y := range 600 {
		for x := range 800 {
			v := uint8((x + y + seed*37) % 256)
			img.Set(x, y, color.RGBA{v, uint8(y / 3), uint8(255 - x/4), 255})
		}
	}
	for range 2000 {
		img.Set(r.IntN(800), r.IntN(600), color.RGBA{uint8(r.Uint32()), uint8(r.Uint32()), uint8(r.Uint32()), 255})
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: 85})
	return b.Bytes()
}

var soakColumns = []string{"elapsed_s", "goroutines", "fds", "heap_bytes", "rss_bytes", "cache_used", "index_files",
	"db_bytes", "cdn_requests", "cdn_bytes", "rest", "uploads", "ops", "errors"}

func (s *soak) run(every time.Duration, out string) {
	t := s.t
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := csv.NewWriter(f)
	_ = w.Write(soakColumns)
	var wg sync.WaitGroup
	for _, fn := range s.workers {
		wg.Go(fn)
	}
	start := time.Now()
	var rows [][]float64
	for {
		row := s.sample(time.Since(start))
		rows = append(rows, row)
		rec := make([]string, len(row))
		for i, v := range row {
			rec[i] = strconv.FormatFloat(v, 'f', -1, 64)
		}
		_ = w.Write(rec)
		w.Flush()
		if s.done() {
			break
		}
		time.Sleep(min(every, time.Until(s.deadline)+time.Second))
	}
	wg.Wait()
	if prof, err := os.Create(strings.TrimSuffix(out, filepath.Ext(out)) + ".heap"); err == nil {
		_ = pprof.WriteHeapProfile(prof)
		_ = prof.Close()
	}
	t.Logf("samples in %s\n%s", out, growth(rows))
	s.mu.Lock()
	defer s.mu.Unlock()
	t.Logf("%d operations; errors: %v", s.ops.Load(), s.errs)
}

func (s *soak) sample(elapsed time.Duration) []float64 {
	st, err := ctl.Get(s.m.sock)
	if err != nil {
		s.t.Fatalf("status: %v", err)
	}
	s.mu.Lock()
	nerr := 0
	for _, n := range s.errs {
		nerr += n
	}
	s.mu.Unlock()
	return []float64{elapsed.Seconds(), float64(st.Process.Goroutines), float64(st.Process.FDs),
		float64(st.Process.HeapBytes), float64(rss(st.PID)), float64(st.Cache.Used), float64(st.Index.Files),
		float64(st.Index.DBBytes), float64(st.CDN.Requests), float64(st.CDN.Bytes), float64(st.API.REST),
		float64(st.Writes.Uploads), float64(s.ops.Load()), float64(nerr)}
}

// rss is a process's resident set in bytes, from ps, which both macOS and
// Linux have.
func rss(pid int) int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return kb << 10
}

// growth compares, for each gauge, the mean of the second fifth of the run
// (after warm-up) with the mean of the last fifth.
func growth(rows [][]float64) string {
	if len(rows) < 10 {
		return "too few samples to compare"
	}
	n := len(rows) / 5
	mean := func(part [][]float64, col int) float64 {
		sum := 0.0
		for _, r := range part {
			sum += r[col]
		}
		return sum / float64(len(part))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-12s %14s %14s %14s %8s\n", "gauge", "early", "late", "max", "change")
	for col := 1; col <= 7; col++ {
		early, late := mean(rows[n:2*n], col), mean(rows[len(rows)-n:], col)
		peak := 0.0
		for _, r := range rows {
			peak = max(peak, r[col])
		}
		fmt.Fprintf(&b, "%-12s %14.0f %14.0f %14.0f %+7.1f%%\n", soakColumns[col], early, late, peak, 100*(late-early)/max(early, 1))
	}
	return b.String()
}
