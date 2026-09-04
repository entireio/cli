//go:build windows

package versioncheck

import (
	"context"
	"errors"
)

// installerAutoRuns is false on Windows: a running entire.exe cannot replace
// itself, so the command is printed for the user to run after entire exits.
const (
	installerAutoRuns  = false
	manualRunQualifier = " the following when entire is not running"
)

// realRunInstaller is not implemented on Windows. maybeAutoUpdate never
// reaches here: a running entire.exe cannot replace itself, so Windows
// prints the command and returns. Do not implement this with cmd.exe —
// UpdateCommandForCurrentBinary's Windows strings are PowerShell one-liners
// that cmd /C would split on | and &. Lifting print-only needs
// powershell -NoProfile -Command, or in-process replace.
func realRunInstaller(_ context.Context, _ string) error {
	return errors.New("auto-update is not implemented on Windows")
}
