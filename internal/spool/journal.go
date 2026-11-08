package spool

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Journal is the upload log shown as .ucfs/uploads.log: one line per
// completed upload, with tab-separated time, path, uuid, size and MIME type.
// Tabs, newlines and backslashes in a path are escaped as \t, \n and \\.
type Journal struct {
	Path string
	mu   sync.Mutex
}

// Entry is one journal line.
type Entry struct {
	Time time.Time
	Path string
	UUID string
	Size int64
	MIME string
}

var (
	escaper   = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`)
	unescaper = strings.NewReplacer(`\\`, `\`, `\t`, "\t", `\n`, "\n")
)

func (j *Journal) Append(e Entry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.OpenFile(j.Path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "%s\t%s\t%s\t%d\t%s\n", e.Time.UTC().Format(time.RFC3339), escaper.Replace(e.Path), e.UUID, e.Size, e.MIME)
	return errors.Join(werr, f.Close())
}

// Last returns the newest entry for a path.
func (j *Journal) Last(path string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f, err := os.Open(j.Path)
	if err != nil {
		return Entry{}, false
	}
	defer func() { _ = f.Close() }()
	var last Entry
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) != 5 {
			continue
		}
		if p := unescaper.Replace(fields[1]); p == path {
			t, _ := time.Parse(time.RFC3339, fields[0])
			size, _ := strconv.ParseInt(fields[3], 10, 64)
			last, found = Entry{Time: t, Path: p, UUID: fields[2], Size: size, MIME: fields[4]}, true
		}
	}
	return last, found
}
