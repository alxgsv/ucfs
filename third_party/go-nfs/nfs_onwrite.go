// Modified for ucfs: WRITE replies UNSTABLE to a Committer and commits a stable
// WRITE, and errors carrying an errno map to the matching NFS status (see
// statusFromError). Upstream: github.com/willscott/go-nfs c092ae79c64e.

package nfs

import (
	"bytes"
	"context"
	"io"
	"math"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// writeStability is the level of durability requested with the write
type writeStability uint32

const (
	unstable writeStability = 0
	dataSync writeStability = 1
	fileSync writeStability = 2
)

type writeArgs struct {
	Handle []byte
	Offset uint64
	Count  uint32
	How    uint32
	Data   []byte
}

func onWrite(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	var req writeArgs
	if err := xdr.Read(w.req.Body, &req); err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	fs, path, err := userHandle.FromHandle(req.Handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusROFS, os.ErrPermission}
	}
	if len(req.Data) > math.MaxInt32 || req.Count > math.MaxInt32 {
		return &NFSStatusError{NFSStatusFBig, os.ErrInvalid}
	}
	if req.How != uint32(unstable) && req.How != uint32(dataSync) && req.How != uint32(fileSync) {
		return &NFSStatusError{NFSStatusInval, os.ErrInvalid}
	}

	// stat first for pre-op wcc.
	fullPath := fs.Join(path...)
	info, err := fs.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &NFSStatusError{NFSStatusNoEnt, err}
		}
		return &NFSStatusError{statusFromError(err, NFSStatusAccess), err}
	}
	if !info.Mode().IsRegular() {
		return &NFSStatusError{NFSStatusInval, os.ErrInvalid}
	}
	preOpCache := ToFileAttribute(info, fullPath).AsCache()

	// now the actual op.
	file, err := fs.OpenFile(fs.Join(path...), os.O_RDWR, info.Mode().Perm())
	if err != nil {
		return &NFSStatusError{statusFromError(err, NFSStatusAccess), err}
	}
	if req.Offset > 0 {
		if _, err := file.Seek(int64(req.Offset), io.SeekStart); err != nil {
			return &NFSStatusError{NFSStatusIO, err}
		}
	}
	end := req.Count
	if len(req.Data) < int(end) {
		end = uint32(len(req.Data))
	}
	writtenCount, err := file.Write(req.Data[:end])
	if err != nil {
		Log.Errorf("Error writing: %v", err)
		return &NFSStatusError{statusFromError(err, statusFromWriteError(err)), err}
	}
	if err := file.Close(); err != nil {
		Log.Errorf("error closing: %v", err)
		return &NFSStatusError{statusFromError(err, statusFromWriteError(err)), err}
	}
	committed := fileSync
	if c, ok := fs.(Committer); ok {
		// A stable WRITE is a write and a commit at once: macOS sends a
		// file that fits in one WRITE as a single FILE_SYNC WRITE and no
		// COMMIT.
		if req.How == uint32(unstable) {
			committed = unstable
		} else if err := c.Commit(fullPath); err != nil {
			return &NFSStatusError{statusFromError(err, NFSStatusIO), err}
		}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := WriteWcc(writer, preOpCache, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, uint32(writtenCount)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, committed); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := xdr.Write(writer, w.Server.ID); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
