package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"syscall"

	"github.com/uploadcare/uploadcare-go/v2/metadata"
	"github.com/uploadcare/uploadcare-go/v2/ucare"
)

// Errno maps an API error to the errno a filesystem call reports. A
// throttled request that ran out of retries is EAGAIN, which NFS carries as
// JUKEBOX; network errors and 5xx answers are EIO. uploadcare-go checks
// metadata keys and values before sending them.
func Errno(err error) syscall.Errno {
	var uerr *UploadError
	var apiErr ucare.APIError
	var throttle ucare.ThrottleError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.As(err, &throttle):
		return syscall.EAGAIN
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
