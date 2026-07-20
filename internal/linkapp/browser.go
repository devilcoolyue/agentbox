package linkapp

import (
	"os/exec"
	"runtime"
)

// OpenBrowser points the user's default browser at the panel. Best-effort: on
// a headless box there is nothing to open, and the caller has already printed
// the URL, so a failure here is not worth reporting.
func OpenBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		// Reap the child so it does not linger as a zombie for the life of the
		// panel process.
		go func() { _ = cmd.Wait() }()
	}
}
