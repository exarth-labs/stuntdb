package outfile

import (
	"errors"
	"syscall"
)

// Windows error codes for a file that another process has open.
const (
	errorAccessDenied     syscall.Errno = 5
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

// transient reports whether a rename failed because another process, such as
// an antivirus scanner or the search indexer, briefly has a file open.
func transient(err error) bool {
	return errors.Is(err, errorAccessDenied) || errors.Is(err, errorSharingViolation) || errors.Is(err, errorLockViolation)
}
