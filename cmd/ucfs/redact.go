package main

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/alxgsv/ucfs/internal/config"
)

// secrets replaces the project's key values wherever the log or an error
// message would show them. The public key is in the default cache dir, so
// paths in errors carry it too.
var secrets struct {
	mu sync.RWMutex
	r  *strings.Replacer
}

func hideKeys(cfg *config.Config) {
	var pairs []string
	if cfg.SecretKey != "" {
		pairs = append(pairs, cfg.SecretKey, "<secret key>")
	}
	if cfg.PublicKey != "" {
		pairs = append(pairs, cfg.PublicKey, "<public key>")
	}
	secrets.mu.Lock()
	defer secrets.mu.Unlock()
	secrets.r = strings.NewReplacer(pairs...)
}

func redact(s string) string {
	secrets.mu.RLock()
	defer secrets.mu.RUnlock()
	if secrets.r == nil {
		return s
	}
	return secrets.r.Replace(s)
}

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(redact(a.Value.String()))
	case slog.KindAny:
		a.Value = slog.StringValue(redact(fmt.Sprintf("%+v", a.Value.Any())))
	}
	return a
}
