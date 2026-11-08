// Package config resolves credentials, project selection and local directories.
//
// It reads the uploadcare-cli config file (~/.uploadcare/config.yaml) with the
// same precedence as the CLI, so one credential store serves both tools.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/uploadcare/uploadcare-go/v2/ucare"
	"go.yaml.in/yaml/v3"
)

var (
	ErrNoCredentials   = errors.New("no project credentials found")
	ErrProjectNotFound = errors.New("project not found in config")
	ErrInsecureConfig  = errors.New("config file is readable by group or others")
)

// Options carries values from command-line flags; empty fields are unset.
type Options struct {
	ConfigPath string
	Project    string
	PublicKey  string
	SecretKey  string
	CDNBase    string
	CacheDir   string
	// InsecureConfig reads a config file that group or others may read,
	// which Load refuses otherwise: it holds secret keys.
	InsecureConfig bool
}

// Config is the resolved configuration of one mounted project.
type Config struct {
	// Project is the name from the config file, empty when keys were given directly.
	Project   string
	PublicKey string
	SecretKey string
	CDNBase   string
	CacheDir  string
}

type fileProject struct {
	PublicKey string `yaml:"public_key"`
	SecretKey string `yaml:"secret_key"`
	CDNBase   string `yaml:"cdn_base"`
}

type file struct {
	DefaultProject string                 `yaml:"default_project"`
	PublicKey      string                 `yaml:"public_key"`
	SecretKey      string                 `yaml:"secret_key"`
	CDNBase        string                 `yaml:"cdn_base"`
	Projects       map[string]fileProject `yaml:"projects"`
}

// DefaultPath is the uploadcare-cli config file location.
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".uploadcare", "config.yaml")
}

// Load resolves credentials in uploadcare-cli order: flag or env keys, then
// --project/UPLOADCARE_PROJECT, then default_project, then top-level keys.
func Load(o Options) (*Config, error) {
	path := o.ConfigPath
	if path == "" {
		path = DefaultPath()
	}
	f, err := readFile(path, o.InsecureConfig)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	pub := first(o.PublicKey, os.Getenv("UPLOADCARE_PUBLIC_KEY"))
	sec := first(o.SecretKey, os.Getenv("UPLOADCARE_SECRET_KEY"))
	var projectCDN string
	switch {
	case pub != "" || sec != "":
		cfg.PublicKey, cfg.SecretKey = pub, sec
	default:
		name := first(o.Project, os.Getenv("UPLOADCARE_PROJECT"), f.DefaultProject)
		if name != "" {
			p, key, ok := f.lookup(name)
			if !ok {
				return nil, fmt.Errorf("%w: %q in %s", ErrProjectNotFound, name, path)
			}
			cfg.Project = key
			cfg.PublicKey, cfg.SecretKey, projectCDN = p.PublicKey, p.SecretKey, p.CDNBase
		} else {
			cfg.PublicKey, cfg.SecretKey = f.PublicKey, f.SecretKey
		}
	}
	if cfg.PublicKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("%w: both public and secret keys are required; set --public-key/--secret-key, UPLOADCARE_PUBLIC_KEY/UPLOADCARE_SECRET_KEY, or a project in %s", ErrNoCredentials, path)
	}

	cfg.CDNBase = first(o.CDNBase, os.Getenv("UPLOADCARE_CDN_BASE"), projectCDN, f.CDNBase)
	uc, err := ucare.NewConfig(ucare.APICreds{PublicKey: cfg.PublicKey}, ucare.WithCDNBase(cfg.CDNBase))
	if err != nil {
		return nil, err
	}
	cfg.CDNBase = uc.CDNBase

	cfg.CacheDir = o.CacheDir
	if cfg.CacheDir == "" {
		root, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("cache directory: %w", err)
		}
		cfg.CacheDir = filepath.Join(root, "ucfs", cfg.PublicKey)
	}
	return cfg, nil
}

// IndexPath is the SQLite index file inside the cache directory.
func (c *Config) IndexPath() string { return filepath.Join(c.CacheDir, "index.db") }

func readFile(path string, insecure bool) (*file, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &file{}, nil
	}
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 && !insecure {
		return nil, fmt.Errorf("%w: %s has mode %04o; run chmod 600 %s, or pass --insecure-config to use it anyway",
			ErrInsecureConfig, path, perm, path)
	}
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &f, nil
}

// lookup matches case-insensitively because the CLI reads the file through
// viper, which lowercases map keys.
func (f *file) lookup(name string) (fileProject, string, bool) {
	if p, ok := f.Projects[name]; ok {
		return p, name, true
	}
	for k, p := range f.Projects {
		if strings.EqualFold(k, name) {
			return p, k, true
		}
	}
	return fileProject{}, "", false
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
