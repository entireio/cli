//go:build !windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/stretchr/testify/require"
)

func TestValidateBranchName_CanceledDuringNativeInterpretation(t *testing.T) {
	// Process-global PATH and environment changes make this test serial.
	gitenv.IsolateRepository(t)
	bin := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	// exec replaces the shell, so CommandContext kills the only child and no
	// descendant retains its pipes. The marker proves cancellation happens after
	// the native fallback starts, rather than exercising only the entry guard.
	script := "#!/bin/sh\nprintf ready > \"$ENTIRE_TEST_VALIDATION_READY\"\nexec /bin/sleep 60\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin)
	t.Setenv("ENTIRE_TEST_VALIDATION_READY", ready)
	t.Chdir(t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- ValidateBranchName(ctx, "@{-1}")
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "native fallback did not start")
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		require.NotContains(t, err.Error(), "invalid branch name")
	case <-time.After(5 * time.Second):
		t.Fatal("native validation did not stop after cancellation")
	}
}
