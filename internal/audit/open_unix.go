//go:build unix

package audit

import "os"

// openAppend opens the audit file for appending, creating it owner-only.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
}
