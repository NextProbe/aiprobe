//go:build !windows

package cli

import "os"

// enableVirtualTerminal is a no-op everywhere but Windows: escape sequences need
// no opting in. The hook exists so the colour decision in colorEnabled has one
// shape on every platform.
func enableVirtualTerminal(*os.File) bool { return true }
