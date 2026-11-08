package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
default_project: alpha
public_key: toppub
secret_key: topsec
projects:
  alpha:
    public_key: apub
    secret_key: asec
  Beta:
    public_key: bpub
    secret_key: bsec
    cdn_base: https://cdn.example.com
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearEnv(t *testing.T) {
	for _, k := range []string{"UPLOADCARE_PUBLIC_KEY", "UPLOADCARE_SECRET_KEY", "UPLOADCARE_PROJECT", "UPLOADCARE_CDN_BASE"} {
		t.Setenv(k, "")
	}
}

func TestLoadPrecedence(t *testing.T) {
	path := writeConfig(t, sample)
	tests := []struct {
		name    string
		opts    Options
		env     map[string]string
		wantPub string
		wantPrj string
		wantCDN string
	}{
		{name: "default project", wantPub: "apub", wantPrj: "alpha"},
		{name: "project flag, case-insensitive", opts: Options{Project: "beta"}, wantPub: "bpub", wantPrj: "Beta", wantCDN: "https://cdn.example.com"},
		{name: "project env", env: map[string]string{"UPLOADCARE_PROJECT": "Beta"}, wantPub: "bpub", wantPrj: "Beta"},
		{name: "direct keys beat projects", opts: Options{Project: "beta", PublicKey: "fpub", SecretKey: "fsec"}, wantPub: "fpub"},
		{name: "env keys", env: map[string]string{"UPLOADCARE_PUBLIC_KEY": "epub", "UPLOADCARE_SECRET_KEY": "esec"}, wantPub: "epub"},
		{name: "cdn flag beats project cdn", opts: Options{Project: "Beta", CDNBase: "https://flag.example.com"}, wantPub: "bpub", wantPrj: "Beta", wantCDN: "https://flag.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			tt.opts.ConfigPath = path
			tt.opts.CacheDir = "/cache"
			cfg, err := Load(tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.PublicKey != tt.wantPub || cfg.Project != tt.wantPrj {
				t.Errorf("got pub=%q project=%q, want pub=%q project=%q", cfg.PublicKey, cfg.Project, tt.wantPub, tt.wantPrj)
			}
			if tt.wantCDN != "" && cfg.CDNBase != tt.wantCDN {
				t.Errorf("cdn = %q, want %q", cfg.CDNBase, tt.wantCDN)
			}
		})
	}
}

func TestLoadTopLevelKeysAndDerivedCDN(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, "public_key: demopublickey\nsecret_key: s\n")
	cfg, err := Load(Options{ConfigPath: path, CacheDir: "/c"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cfg.CDNBase, ".ucarecd.net") {
		t.Errorf("cdn = %q, want derived *.ucarecd.net", cfg.CDNBase)
	}
}

func TestLoadErrors(t *testing.T) {
	clearEnv(t)
	path := writeConfig(t, sample)
	if _, err := Load(Options{ConfigPath: path, Project: "gamma"}); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown project: err = %v", err)
	}
	if _, err := Load(Options{ConfigPath: path, PublicKey: "only"}); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("half keys: err = %v", err)
	}
	if _, err := Load(Options{ConfigPath: filepath.Join(t.TempDir(), "missing.yaml")}); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("missing file: err = %v", err)
	}
}

func TestLoadCacheDir(t *testing.T) {
	clearEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", filepath.Join(os.Getenv("HOME"), "xdg"))
	cfg, err := Load(Options{ConfigPath: writeConfig(t, sample)})
	if err != nil {
		t.Fatal(err)
	}
	root, _ := os.UserCacheDir()
	if want := filepath.Join(root, "ucfs", "apub"); cfg.CacheDir != want {
		t.Errorf("cache dir = %q, want %q", cfg.CacheDir, want)
	}
}
