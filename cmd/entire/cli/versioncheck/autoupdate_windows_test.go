//go:build windows

package versioncheck

import (
	"context"
	"testing"
)

func TestRealRunInstaller_WindowsNotImplemented(t *testing.T) {
	t.Parallel()
	err := realRunInstaller(context.Background(), "echo hi")
	want := "auto-update is not implemented on Windows"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}
