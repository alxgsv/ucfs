// Modified for ucfs: COMMIT calls Committer.Commit, and errors carrying an errno
// map to the matching NFS status (see statusFromError). Upstream:
// github.com/willscott/go-nfs c092ae79c64e.

package nfs

import (
	"bytes"
	"context"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

// Committer is implemented by a filesystem that wants COMMIT, the only
// close-like event of NFSv3. WRITE then replies UNSTABLE when the client asks
// for it, so the client follows its writes with a COMMIT, which calls Commit
// with the file's path.
type Committer interface {
	Commit(path string) error
}

// onCommit calls Commit on a Committer; otherwise it is a no-op, as writes
// always reach the backing store.
func onCommit(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = wccDataErrorFormatter
	handle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	// The conn will drain the unread offset and count arguments.

	fs, path, err := userHandle.FromHandle(handle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		return &NFSStatusError{NFSStatusServerFault, os.ErrPermission}
	}

	if c, ok := fs.(Committer); ok {
		if err := c.Commit(fs.Join(path...)); err != nil {
			return &NFSStatusError{statusFromError(err, NFSStatusIO), err}
		}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return err
	}

	// no pre-op cache data.
	if err := xdr.Write(writer, uint32(0)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	// write the 8 bytes of write verification.
	if err := xdr.Write(writer, w.Server.ID); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}
