package investigate

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// openInBrowser opens a local file path in the user's default browser/viewer.
//
// Unlike login.go's openBrowser (which is HTTP-scheme-only), this accepts a
// filesystem path so `investigate show --html` can launch the generated
// findings.html. It no-ops under test so the suite never spawns a real
// browser, and detaches the launched process so the CLI does not block on it.
func openInBrowser(ctx context.Context, path string) error {
	if interactive.UnderTest() {
		return nil
	}

	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{path}
	case "linux":
		command, args = "xdg-open", []string{path}
	case "windows":
		command, args = "cmd", []string{"/c", "start", "", path}
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start viewer command %q: %w", command, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release viewer process: %w", err)
	}
	return nil
}
