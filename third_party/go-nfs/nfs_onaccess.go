// Modified for ucfs: a handler implementing AccessFilter narrows what ACCESS
// grants by the caller's AUTH_UNIX uid. Upstream: github.com/willscott/go-nfs
// c092ae79c64e.

package nfs

import (
	"bytes"
	"context"
	"encoding/binary"

	"github.com/go-git/go-billy/v5"
	"github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func onAccess(ctx context.Context, w *response, userHandle Handler) error {
	w.errorFmt = opAttrErrorFormatter
	roothandle, err := xdr.ReadOpaque(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}
	fs, path, err := userHandle.FromHandle(roothandle)
	if err != nil {
		return &NFSStatusError{NFSStatusStale, err}
	}
	mask, err := xdr.ReadUint32(w.req.Body)
	if err != nil {
		return &NFSStatusError{NFSStatusInval, err}
	}

	writer := bytes.NewBuffer([]byte{})
	if err := xdr.Write(writer, uint32(NFSStatusOk)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := WritePostOpAttrs(writer, tryStat(fs, path)); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}

	if !billy.CapabilityCheck(fs, billy.WriteCapability) {
		mask = mask & (1 | 2 | 0x20)
	}
	if f, ok := userHandle.(AccessFilter); ok {
		uid, known := authUnixUID(w.req.Header.Cred)
		mask = f.FilterAccess(uid, known, mask)
	}

	if err := xdr.Write(writer, mask); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	if err := w.Write(writer.Bytes()); err != nil {
		return &NFSStatusError{NFSStatusServerFault, err}
	}
	return nil
}

// AccessFilter lets a handler narrow what ACCESS grants. NFS clients decide
// from the ACCESS answer whether a local user may open a file, so a filter
// that grants nothing to other uids keeps other users of the client
// machine out of the mount, as FUSE does without allow_other. A raw RPC
// client can still claim any uid.
type AccessFilter interface {
	// FilterAccess returns the access bits granted to a caller; known is
	// false for credentials other than AUTH_UNIX.
	FilterAccess(uid uint32, known bool, mask uint32) uint32
}

// authUnixUID reads the uid of AUTH_UNIX credentials (RFC 5531 appendix A):
// stamp, machine name as an XDR string, uid.
func authUnixUID(cred rpc.Auth) (uint32, bool) {
	const authUnix = 1
	b := cred.Body
	if cred.Flavor != authUnix || len(b) < 8 {
		return 0, false
	}
	n := binary.BigEndian.Uint32(b[4:8])
	off := 8 + (uint64(n)+3)&^3
	if uint64(len(b)) < off+4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[off : off+4]), true
}
