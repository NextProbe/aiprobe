//go:build windows

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows consoles do not interpret ANSI escape sequences until the mode is set
// explicitly. Without this, every coloured report on a Windows terminal prints
// literal escape sequences instead of colour.
//
// This is done with syscall directly rather than golang.org/x/sys/windows to keep
// the dependency list at one entry. The two calls used here are in the standard
// library on Windows, so nothing is vendored for it.
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// enableVirtualTerminal turns on escape-sequence processing for a console handle
// and reports whether colour is safe to emit.
//
// Returning false is the important case. Windows Terminal and modern conhost
// accept the mode change; an old console or a redirected handle does not, and
// there the honest answer is that this stream cannot render colour, so the caller
// falls back to plain text.
func enableVirtualTerminal(f *os.File) bool {
	handle := syscall.Handle(f.Fd())

	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode))); r == 0 {
		// Not a console at all.
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ := procSetConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
