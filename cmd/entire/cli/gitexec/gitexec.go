// Package gitexec runs the git CLI from inside the codebase. Callers that
// need plain stdout from `git <args>` (e.g. parsing `rev-parse HEAD`
// output) get a shared exec.Command + stderr-capture + error-wrap helper.
package gitexec

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Run runs `git <args>` in repoRoot and returns stdout as a string.
// stderr is captured separately and surfaced in the error wrap on non-zero
// exit. Stdout and stderr are NOT combined — git emits warnings on stderr
// even on successful commands (shallow-clone notices, safe.directory
// advisories, etc.) and merging them would corrupt parsed output (e.g.,
// strconv.Atoi on the result of `rev-list --count` would fail).
func Run(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		stderrTxt := strings.TrimSpace(stderr.String())
		if stderrTxt != "" {
			return "", fmt.Errorf("git %s: %w (stderr: %s)", args[0], err, stderrTxt)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

// HeadSHA returns the current HEAD commit hash as a 40-char hex string.
func HeadSHA(ctx context.Context, repoRoot string) (string, error) {
	out, err := Run(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}
