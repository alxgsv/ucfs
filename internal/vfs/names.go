package vfs

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

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
	s := f.UUID + "_" + name
	if len(s) <= nameMax {
		return s
	}
	s = s[:nameMax]
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
