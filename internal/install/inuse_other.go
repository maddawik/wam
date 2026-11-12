//go:build !windows

package install

import (
	"errors"
	"syscall"
)

// inUse reports whether err means a file is busy in another process.
func inUse(err error) bool {
	return errors.Is(err, syscall.EBUSY)
}
