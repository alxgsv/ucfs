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
	Path  string `json:"path"`
	State string `json:"state"`
	UUID  string `json:"uuid,omitempty"`
	Ops   string `json:"ops,omitempty"`
	Name  string `json:"name,omitempty"`
	Size  int64  `json:"size"`
	MIME  string `json:"mime,omitempty"`
	// Spool is the state of a pending upload and Error why it failed.
	Spool string `json:"spool,omitempty"`
	Error string `json:"error,omitempty"`
}

// Resolve tells what a path relative to the mount root stands for, from the
// index, the aliases, the spools and, for a path that no longer resolves,
// the upload journal.
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
	switch n.Kind {
	case KindFile, KindVariant:
		file, err := f.file(ctx, n.UUID)
		if err != nil {
			return r, err
		}
		r.State, r.UUID, r.Ops, r.Name, r.Size, r.MIME = StateUploaded, file.UUID, n.Ops, file.Name, e.Attr.Size, file.MIME
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
			return r, nil
		}
		r.State, r.UUID, r.Name, r.Size, r.MIME = StateUploaded, o.file.UUID, o.file.Name, o.file.Size, o.file.MIME
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
