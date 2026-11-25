//go:build linux

package room

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func cloneFile(output, input *os.File) error {
	return unix.IoctlFileClone(int(output.Fd()), int(input.Fd()))
}

func reflinkUnsupported(err error) bool {
	return errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) ||
		errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EPERM)
}

// RENAME_NOREPLACE makes the commit atomic without replacing even an empty
// preexisting directory. A preflight Stat plus ordinary Rename is not enough.
func commitSnapshot(staging, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, staging, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
