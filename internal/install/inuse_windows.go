package install

import (
	"errors"

	"golang.org/x/sys/windows"
)

// inUse reports whether err means a file is open in another process, as
// WoW's are while it runs.
func inUse(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
