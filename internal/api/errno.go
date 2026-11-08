package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/uploadcare/uploadcare-go/v2/metadata"
	"github.com/uploadcare/uploadcare-go/v2/ucare"

	"github.com/alxgsv/ucfs/internal/cdn"
)

// Errno maps a failure of the REST API, the Upload API or the CDN to the
// errno a filesystem call reports. It is the one table ucfs maps them with:
//
//	404                                       ENOENT
//	401, 403 (also "API denied", ErrDenied)   EACCES
//	429 after the Retry-After retries         EAGAIN (NFS3ERR_JUKEBOX over NFS)
//	5xx, network errors, a changed ETag       EIO
//	CDN 400 (a variant it cannot render)      EINVAL
//	413, "too large"                          EFBIG
//	400 "not allowed" (upload file type)      EPERM
//	other 4xx                                 EINVAL
//	metadata and tag limits                   ENOSPC, E2BIG, ENAMETOOLONG
//
// An error that already carries an errno keeps it: the traffic budget's
// EDQUOT, ErrDenied, ErrThrottled, and EROFS or EOPNOTSUPP from the
// filesystem itself. uploadcare-go checks metadata keys and values before
// sending them.
func Errno(err error) syscall.Errno {
	var errno syscall.Errno
	var uerr *UploadError
	var apiErr ucare.APIError
	var throttle ucare.ThrottleError
	var bad *cdn.BadRequestError
	var cdnErr *cdn.StatusError
	var urlErr *url.Error
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	// The HTTP client wraps network errors in url.Error. A refused
	// connection carries ECONNREFUSED, which no file call should report.
	case errors.As(err, &urlErr):
		return syscall.EIO
	case errors.As(err, &errno):
		return errno
	case errors.As(err, &throttle):
		return syscall.EAGAIN
	case errors.Is(err, cdn.ErrNotFound):
		return syscall.ENOENT
	case errors.As(err, &bad):
		return syscall.EINVAL
	case errors.As(err, &cdnErr):
		return statusErrno(cdnErr.Status, "")
	case errors.Is(err, metadata.ErrInvalidKey):
		return syscall.EINVAL
	case errors.Is(err, metadata.ErrValueTooLong):
		return syscall.E2BIG
	case errors.As(err, &uerr):
		return statusErrno(uerr.Status, uerr.Message)
	case errors.As(err, &apiErr):
		return statusErrno(apiErr.StatusCode, apiErr.Detail)
	}
	return syscall.EIO
}

// statusErrno maps an API status and message. The metadata and tag limits
// answer 400 with messages such as "Number of metadata keys per file has
// been exceeded", "Value length of key `k` is exceeded" (checked before
// "Length of key", which it contains) and "Too many tags: 51 (max 50)".
func statusErrno(status int, msg string) syscall.Errno {
	msg = strings.ToLower(msg)
	switch {
	case status == http.StatusBadRequest && (strings.Contains(msg, "number of metadata keys") || strings.Contains(msg, "too many tags")):
		return syscall.ENOSPC
	case status == http.StatusBadRequest && strings.Contains(msg, "value length"):
		return syscall.E2BIG
	case status == http.StatusBadRequest && (strings.Contains(msg, "length of key") || strings.Contains(msg, "tag too long")):
		return syscall.ENAMETOOLONG
	case status == http.StatusTooManyRequests:
		return syscall.EAGAIN
	case status == http.StatusRequestEntityTooLarge || strings.Contains(msg, "too large") || strings.Contains(msg, "size exceed"):
		return syscall.EFBIG
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return syscall.EACCES
	case status == http.StatusNotFound:
		return syscall.ENOENT
	case status == http.StatusBadRequest && strings.Contains(msg, "not allowed"):
		return syscall.EPERM
	case status >= 400 && status < 500:
		return syscall.EINVAL
	}
	return syscall.EIO
}
