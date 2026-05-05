//go:build windows

package plugin

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// buildExecCommand returns an *exec.Cmd for the plugin executable.
//
// Binary and local plugins exec directly. Script plugins (git-cloned shell
// scripts) need shebang interpretation, which Windows lacks; we route them
// through sh.exe (gh's approach). The user must have a POSIX sh on PATH —
// Git for Windows ships one.
func buildExecCommand(ctx context.Context, p *Plugin, args []string) (*exec.Cmd, error) {
	if p.Kind == KindScript && !looksExecutable(p.ExecPath) {
		sh, err := exec.LookPath("sh.exe")
		if err != nil {
			return nil, errors.New("script plugins on Windows require sh.exe on PATH (e.g. via Git for Windows)")
		}
		full := append([]string{p.ExecPath}, args...)
		return exec.CommandContext(ctx, sh, full...), nil
	}
	return exec.CommandContext(ctx, p.ExecPath, args...), nil
}

// looksExecutable returns true if the file ends with a Windows-recognized
// executable extension. We use this to decide whether a script plugin entry
// is actually a compiled .exe (run directly) or a shebang script (route
// through sh.exe).
func looksExecutable(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".exe", ".bat", ".cmd", ".com":
		return true
	}
	return false
}
