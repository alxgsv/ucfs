package vfs

import (
	"context"
	"errors"
	"syscall"

	"github.com/alxgsv/ucfs/internal/spool"
)

// States a resolved path can be in.
const (
	StateUploaded  = "uploaded"
	StatePending   = "pending"
	StateFailed    = "failed"
	StateLocal     = "local"
	StateDirectory = "directory"
	// StateJournal is a path that no longer resolves, found in the upload
	// journal: an expired alias, or a file renamed or deleted since.
	StateJournal = "journal"
)

// Resolution says what a mount path stands for: the uuid and operations
// path of an uploaded file or variant, or why there is none yet.
type Resolution struct {
	Path   string `json:"path"`
	State  string `json:"state"`
	UUID   string `json:"uuid,omitempty"`
	Ops    string `json:"ops,omitempty"`
	CDNURL string `json:"cdn_url,omitempty"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime,omitempty"`
	// Metadata and Tags are an uploaded file's, or for a pending upload
	// what was set on it so far; null for directories and local files.
	Metadata map[string]string `json:"metadata"`
	Tags     []string          `json:"tags"`
	// Spool is the state of a pending upload and Error why it failed.
	Spool string `json:"spool,omitempty"`
	Error string `json:"error,omitempty"`
}

// Resolve tells what a path relative to the mount root stands for, from the
// index, the aliases, the spools and, for a path that no longer resolves,
// the upload journal. A file's info older than the metadata TTL is fetched
// again, as for extended attributes, since this is how macOS, which has no
// xattrs over NFS, reads metadata.
func (f *FS) Resolve(ctx context.Context, p string, journal *spool.Journal) (Resolution, error) {
	r := Resolution{Path: p}
	e, err := f.Walk(ctx, p)
	if errors.Is(err, syscall.ENOENT) && journal != nil {
		if en, ok := journal.Last(p); ok {
			r.State, r.UUID, r.Size, r.MIME = StateJournal, en.UUID, en.Size, en.MIME
			return r, nil
		}
	}
	if err != nil {
		return r, err
	}
	n := e.Node
	uploaded := func(uuid, ops string, size int64) error {
		d, err := f.details(ctx, uuid, true)
		if err != nil {
			return err
		}
		r.State, r.UUID, r.Ops, r.Name, r.Size, r.MIME = StateUploaded, d.UUID, ops, d.Name, size, d.MIME
		r.CDNURL, r.Metadata, r.Tags = f.cdnURL(d.UUID, ops), d.Metadata, d.Tags
		if r.Tags == nil {
			r.Tags = []string{}
		}
		return nil
	}
	switch n.Kind {
	case KindFile, KindVariant, KindDateFile, KindViewFile:
		ops := ""
		if n.Kind == KindVariant {
			ops = n.Ops
		}
		if err := uploaded(n.UUID, ops, e.Attr.Size); err != nil {
			return r, err
		}
	case KindTreeFile:
		o, err := f.occupant(ctx, n)
		if err != nil {
			return r, err
		}
		if o.spool != nil {
			in := o.spool.Info()
			r.State, r.Spool, r.Error, r.Size = StatePending, in.State, in.Error, in.Size
			if in.State == spool.Failed {
				r.State = StateFailed
			}
			q := o.spool.Queued()
			r.Metadata, r.Tags = q.Meta, []string{}
			if q.Tags != nil {
				r.Tags = *q.Tags
			}
			return r, nil
		}
		if err := uploaded(o.file.UUID, "", o.file.Size); err != nil {
			return r, err
		}
	case KindShadow, KindNeverIndex, KindJournal:
		r.State, r.Size = StateLocal, e.Attr.Size
	default:
		r.State = StateDirectory
		if n.Kind == KindFileDir || n.Kind == KindVariantDir {
			r.UUID, r.Ops = n.UUID, n.Ops
		}
	}
	return r, nil
}
