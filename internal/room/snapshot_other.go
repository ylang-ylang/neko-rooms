//go:build !linux

package room

import (
	"fmt"
	"os"
)

func cloneFile(output, input *os.File) error { return os.ErrInvalid }
func reflinkUnsupported(err error) bool      { return true }
func commitSnapshot(staging, target string) error {
	return fmt.Errorf("atomic no-replace storage snapshots require Linux")
}
