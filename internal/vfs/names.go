package vfs

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/index"
)

const (
	uuidLen = 36
	nameMax = 255
	// unnamed stands in for an empty original_filename; a bare <uuid> is
	// the reserved files/<uuid>/ directory.
	unnamed = "unnamed"
)

// nameReplacer maps the two bytes a path component cannot hold to the
// characters rclone uses for them.
var nameReplacer = strings.NewReplacer("/", "／", "\x00", "␀")

// DisplayName is a file's name in files/: <uuid>_<original_filename>, with
// unrepresentable bytes mapped and the result cut to NAME_MAX on a rune
// boundary. The uuid prefix keeps names unique after truncation.
func DisplayName(f index.File) string {
	name := nameReplacer.Replace(f.Name)
	if name == "" {
		name = unnamed
	}
	return truncate(f.UUID+"_"+name, nameMax)
}

// baseName is a file's original name as one path component of files/<uuid>/
// and the variants view. It is mapped like DisplayName; names a directory
// cannot hold as an entry, and "-", which is the variants directory, become
// unnamed.
func baseName(f index.File) string {
	name := nameReplacer.Replace(f.Name)
	switch name {
	case "", ".", "..", VariantsDir:
		name = unnamed
	}
	return truncate(name, nameMax)
}

// leafName names the variant in an operation or argument directory: the
// original's name with the extension of the format the operations select.
func leafName(f index.File, ops string) string {
	name := baseName(f)
	ext := cdn.OutputExt(ops)
	if ext == "" {
		return name
	}
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		name = name[:i]
	}
	return truncate(name, nameMax-1-len(ext)) + "." + ext
}

// truncate cuts s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:max(n, 0)]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// parseName splits <uuid>[_<name>]. ok is false when name does not start with
// a uuid.
func parseName(name string) (uuid string, hasName, ok bool) {
	if len(name) < uuidLen || !isUUID(name[:uuidLen]) {
		return "", false, false
	}
	switch {
	case len(name) == uuidLen:
		return name, false, true
	case name[uuidLen] == '_':
		return name[:uuidLen], true, true
	}
	return "", false, false
}

func isUUID(s string) bool {
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

// sameName compares under NFC: the server keeps names as uploaded, while a
// macOS client mounted with nfc sends NFC even for names it listed as NFD.
func sameName(a, b string) bool {
	return a == b || norm.NFC.String(a) == norm.NFC.String(b)
}
