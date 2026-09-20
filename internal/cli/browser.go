package cli

import (
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

// openBrowser asks the desktop to open a URL.
//
// This is the one place AIProbe shells out, and it is worth being explicit
// about why it is not a violation of the no-network rule: the URL is always the
// loopback address the server just bound, and the command hands it to the user's
// own desktop rather than fetching anything itself. It is also opt-in behind
// --open, and a failure is reported rather than retried.
func openBrowser(url string, log io.Writer) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil && log != nil {
		fmt.Fprintf(log, "could not open a browser (%v); open %s yourself\n", err, url)
	}
}
