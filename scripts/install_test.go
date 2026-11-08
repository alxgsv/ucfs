package scripts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// release serves what goreleaser publishes: checksums.txt and one archive
// per platform, the host's holding a stand-in ucfs that prints its version.
type release struct {
	files map[string][]byte
	mu    sync.Mutex
	got   []string
}

func newRelease(t *testing.T, version string) (*release, *httptest.Server) {
	t.Helper()
	r := &release{files: map[string][]byte{}}
	var sums strings.Builder
	for _, plat := range []string{runtime.GOOS + "_" + runtime.GOARCH, "plan9_amd64"} {
		name := fmt.Sprintf("ucfs_%s_%s.tar.gz", version, plat)
		r.files[name] = archive(t, "#!/bin/sh\necho 'ucfs "+version+" (stand-in)'\n")
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(r.files[name]), name)
	}
	r.files["checksums.txt"] = []byte(sums.String())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.got = append(r.got, req.URL.Path)
		b, ok := r.files[strings.TrimPrefix(req.URL.Path, "/v/")]
		r.mu.Unlock()
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

func archive(t *testing.T, script string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"ucfs": script, "README.md": "readme\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// install runs install.sh with a clean environment plus env.
func install(t *testing.T, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"install.sh"}, args...)...)
	cmd.Env = append([]string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstall(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	rel, srv := newRelease(t, "1.2.3")
	base := srv.URL + "/v"

	t.Run("latest from base URL", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "made", "bin")
		out, err := install(t, nil, "--base-url", base+"/", "--install-dir", dir)
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		v, err := exec.Command(filepath.Join(dir, "ucfs"), "version").Output()
		if err != nil || string(v) != "ucfs 1.2.3 (stand-in)\n" {
			t.Fatalf("installed ucfs version = %q, %v", v, err)
		}
		for _, want := range []string{"version: 1.2.3", "Checksum verified", "is not in PATH"} {
			if !strings.Contains(out, want) {
				t.Errorf("output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("version and env", func(t *testing.T) {
		envDir, flagDir := t.TempDir(), t.TempDir()
		env := []string{"UCFS_BASE_URL=" + base, "INSTALL_DIR=" + envDir, "VERSION=0.0.1"}
		for _, args := range [][]string{{"--version=v1.2.3"}, {"--version", "1.2.3"}} {
			out, err := install(t, env, append(args, "--install-dir", flagDir)...)
			if err != nil || !strings.Contains(out, "version: 1.2.3") {
				t.Fatalf("%q: %v: %s", args, err, out)
			}
		}
		if _, err := os.Stat(filepath.Join(flagDir, "ucfs")); err != nil {
			t.Error("--install-dir did not win over INSTALL_DIR:", err)
		}
		if _, err := os.Stat(filepath.Join(envDir, "ucfs")); err == nil {
			t.Error("installed into INSTALL_DIR despite --install-dir")
		}
		out, err := install(t, []string{"UCFS_BASE_URL=" + base, "INSTALL_DIR=" + envDir, "VERSION=1.2.3"})
		if err != nil {
			t.Fatalf("env only: %v: %s", err, out)
		}
		if _, err := os.Stat(filepath.Join(envDir, "ucfs")); err != nil {
			t.Error("INSTALL_DIR ignored:", err)
		}
	})

	t.Run("uninstall", func(t *testing.T) {
		dir := t.TempDir()
		if out, err := install(t, nil, "--base-url", base, "--install-dir", dir); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		out, err := install(t, []string{"UNINSTALL=1"}, "--install-dir", dir)
		if err != nil || !strings.Contains(out, "Uninstalled ucfs from "+dir) {
			t.Fatalf("uninstall: %v: %s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "ucfs")); !os.IsNotExist(err) {
			t.Error("ucfs still installed:", err)
		}
		if out, err := install(t, nil, "--uninstall", "--install-dir", dir); err != nil || !strings.Contains(out, "not installed") {
			t.Errorf("second uninstall: %v: %s", err, out)
		}
	})

	failures := []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"--prefix", "/usr"}, "unknown argument: --prefix"},
		{"flag without value", []string{"--base-url", base, "--version"}, "--version needs a value"},
		{"missing version", []string{"--base-url", base, "--version", "9.9.9"}, "failed to download"},
		{"no checksums", []string{"--base-url", srv.URL + "/nothing-here"}, "could not download"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out, err := install(t, nil, append(tc.args, "--install-dir", dir)...)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Errorf("err %v, output lacks %q:\n%s", err, tc.want, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "ucfs")); !os.IsNotExist(err) {
				t.Error("installed anyway")
			}
		})
	}

	t.Run("checksum mismatch", func(t *testing.T) {
		name := fmt.Sprintf("ucfs_1.2.3_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
		tampered := archive(t, "#!/bin/sh\necho tampered\n")
		rel.mu.Lock()
		good := rel.files[name]
		rel.files[name] = tampered
		rel.mu.Unlock()
		defer func() {
			rel.mu.Lock()
			rel.files[name] = good
			rel.mu.Unlock()
		}()
		dir := t.TempDir()
		out, err := install(t, nil, "--base-url", base, "--install-dir", dir)
		if err == nil || !strings.Contains(out, "checksum mismatch") {
			t.Fatalf("err %v:\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "ucfs")); !os.IsNotExist(err) {
			t.Error("installed a tampered archive")
		}
	})

	rel.mu.Lock()
	defer rel.mu.Unlock()
	for _, p := range rel.got {
		if !strings.HasPrefix(p, "/v/") && !strings.HasPrefix(p, "/nothing-here/") {
			t.Errorf("request outside the base URL: %s", p)
		}
	}
}
