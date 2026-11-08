// Command nfsstatic exports a local directory over loopback NFSv3 with a
// request log. It is the rig for the Phase 0 mount spikes A–C in
// docs/spikes.md and is not part of ucfs.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	nfs "github.com/willscott/go-nfs"
	nfshelper "github.com/willscott/go-nfs/helpers"
)

func main() {
	root := flag.String("root", "", "directory to export (required)")
	populate := flag.Bool("populate", false, "fill -root with spike fixtures and exit")
	addr := flag.String("addr", "127.0.0.1:0", "listen address")
	concurrent := flag.Int("concurrent", 128, "nfs.Server.ConcurrentHandlers")
	slowDelay := flag.Duration("slow-delay", 400*time.Millisecond, "delay per ReadAt on files named slow*")
	readOnly := flag.Bool("read-only", false, "reject writes")
	trace := flag.Bool("trace", false, "go-nfs trace log (every RPC)")
	logStats := flag.Bool("log-stats", false, "log successful stat and lstat calls too")
	flag.Parse()
	if *root == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *populate {
		if err := populateFixtures(*root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *trace {
		nfs.Log.SetLevel(nfs.TraceLevel)
	}

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	st := &stats{read: map[string]int64{}, written: map[string]int64{}, ops: map[string]int{}}
	fs := &logFS{Filesystem: osfs.New(*root, osfs.WithBoundOS()), slowDelay: *slowDelay, readOnly: *readOnly, logStats: *logStats, st: st}
	srv := &nfs.Server{
		Handler:            nfshelper.NewCachingHandler(nfshelper.NewNullAuthHandler(fs), 1<<16),
		ConcurrentHandlers: *concurrent,
	}
	port := l.Addr().(*net.TCPAddr).Port
	fmt.Printf("listening on %s concurrent=%d\n", l.Addr(), *concurrent)
	fmt.Printf("mount: mount_nfs -o port=%d,mountport=%d,vers=3,tcp,locallocks,noresvport 127.0.0.1:/ <dir>\n", port, port)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		if err := srv.Serve(l); err != nil {
			slog.Error("serve", "err", err)
		}
	}()
	<-sig
	st.print()
}

type stats struct {
	mu      sync.Mutex
	read    map[string]int64
	written map[string]int64
	ops     map[string]int
}

func (s *stats) op(name, path string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops[name]++
	switch name {
	case "read":
		s.read[path] += n
	case "write":
		s.written[path] += n
	}
}

func (s *stats) print() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Println("== ops")
	for _, k := range sortedKeys(s.ops) {
		fmt.Printf("%-10s %d\n", k, s.ops[k])
	}
	fmt.Println("== bytes read per path")
	for _, k := range sortedKeys(s.read) {
		fmt.Printf("%12d %s\n", s.read[k], k)
	}
	fmt.Println("== bytes written per path")
	for _, k := range sortedKeys(s.written) {
		fmt.Printf("%12d %s\n", s.written[k], k)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// logFS logs every billy call go-nfs makes. go-nfs re-opens the file for
// each READ and WRITE RPC, so open counts here are RPC counts, not opens.
type logFS struct {
	billy.Filesystem
	slowDelay time.Duration
	readOnly  bool
	logStats  bool
	st        *stats
}

func (f *logFS) logOp(op, path string, err error, args ...any) {
	f.st.op(op, path, 0)
	attrs := append([]any{"op", op, "path", path}, args...)
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	slog.Info("fs", attrs...)
}

func (f *logFS) Capabilities() billy.Capability {
	if f.readOnly {
		return billy.ReadCapability | billy.SeekCapability
	}
	return billy.DefaultCapabilities
}

func (f *logFS) wrap(name string, file billy.File, err error) (billy.File, error) {
	if err != nil {
		return nil, err
	}
	return &logFile{File: file, path: name, fs: f}, nil
}

func (f *logFS) Create(name string) (billy.File, error) {
	file, err := f.Filesystem.Create(name)
	f.logOp("create", name, err)
	return f.wrap(name, file, err)
}

func (f *logFS) Open(name string) (billy.File, error) {
	file, err := f.Filesystem.Open(name)
	if err != nil {
		f.logOp("open", name, err)
	}
	return f.wrap(name, file, err)
}

func (f *logFS) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	file, err := f.Filesystem.OpenFile(name, flag, perm)
	if flag&(os.O_CREATE|os.O_TRUNC) != 0 || err != nil {
		f.logOp("openfile", name, err, "flag", flag)
	}
	return f.wrap(name, file, err)
}

func (f *logFS) Stat(name string) (os.FileInfo, error) {
	fi, err := f.Filesystem.Stat(name)
	f.st.op("stat", name, 0)
	if err != nil || f.logStats {
		f.logOp("stat", name, err)
	}
	return fi, err
}

func (f *logFS) Lstat(name string) (os.FileInfo, error) {
	fi, err := f.Filesystem.Lstat(name)
	f.st.op("lstat", name, 0)
	if err != nil || f.logStats {
		f.logOp("lstat", name, err)
	}
	return fi, err
}

func (f *logFS) ReadDir(name string) ([]os.FileInfo, error) {
	fis, err := f.Filesystem.ReadDir(name)
	f.logOp("readdir", name, err, "entries", len(fis))
	return fis, err
}

func (f *logFS) Rename(from, to string) error {
	err := f.Filesystem.Rename(from, to)
	f.logOp("rename", from, err, "to", to)
	return err
}

func (f *logFS) Remove(name string) error {
	err := f.Filesystem.Remove(name)
	f.logOp("remove", name, err)
	return err
}

func (f *logFS) MkdirAll(name string, perm os.FileMode) error {
	err := f.Filesystem.MkdirAll(name, perm)
	f.logOp("mkdir", name, err)
	return err
}

func (f *logFS) Symlink(target, link string) error {
	err := f.Filesystem.Symlink(target, link)
	f.logOp("symlink", link, err, "target", target)
	return err
}

func (f *logFS) abs(name string) string { return filepath.Join(f.Root(), name) }

func (f *logFS) Chmod(name string, mode os.FileMode) error {
	err := os.Chmod(f.abs(name), mode)
	f.logOp("chmod", name, err, "mode", mode)
	return err
}

func (f *logFS) Lchown(name string, uid, gid int) error {
	f.logOp("lchown", name, nil, "uid", uid, "gid", gid)
	return nil
}

func (f *logFS) Chown(name string, uid, gid int) error {
	f.logOp("chown", name, nil, "uid", uid, "gid", gid)
	return nil
}

func (f *logFS) Chtimes(name string, atime, mtime time.Time) error {
	err := os.Chtimes(f.abs(name), atime, mtime)
	f.logOp("chtimes", name, err)
	return err
}

type logFile struct {
	billy.File
	path string
	fs   *logFS
}

func (l *logFile) ReadAt(p []byte, off int64) (int, error) {
	if strings.HasPrefix(filepath.Base(l.path), "slow") {
		time.Sleep(l.fs.slowDelay)
	}
	n, err := l.File.ReadAt(p, off)
	l.fs.st.op("read", l.path, int64(n))
	slog.Info("fs", "op", "read", "path", l.path, "off", off, "len", len(p), "n", n)
	return n, err
}

func (l *logFile) Write(p []byte) (int, error) {
	n, err := l.File.Write(p)
	l.fs.st.op("write", l.path, int64(n))
	slog.Info("fs", "op", "write", "path", l.path, "n", n)
	return n, err
}

func (l *logFile) Truncate(size int64) error {
	err := l.File.Truncate(size)
	l.fs.logOp("truncate", l.path, err, "size", size)
	return err
}

func populateFixtures(root string) error {
	if err := os.MkdirAll(filepath.Join(root, "photos"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello from nfsstatic\n"), 0o644); err != nil {
		return err
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, name := range []string{"fast.bin", "slow.bin", "fast2.bin", "slow2.bin"} {
		buf := make([]byte, 16<<20)
		for i := range buf {
			buf[i] = byte(rng.Uint32())
		}
		if err := os.WriteFile(filepath.Join(root, name), buf, 0o644); err != nil {
			return err
		}
	}
	for i := range 6 {
		img := gradient(2400, 1600, uint8(i*40))
		f, err := os.Create(filepath.Join(root, "photos", fmt.Sprintf("photo-%d.jpg", i)))
		if err != nil {
			return err
		}
		if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 85}); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	f, err := os.Create(filepath.Join(root, "photos", "diagram.png"))
	if err != nil {
		return err
	}
	if err := png.Encode(f, gradient(1200, 800, 200)); err != nil {
		return err
	}
	return f.Close()
}

func gradient(w, h int, tint uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), tint, 255})
		}
	}
	return img
}
