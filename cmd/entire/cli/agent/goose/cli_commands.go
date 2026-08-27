package goose

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gooseCommandTimeout bounds every `goose` invocation Entire makes. These run on
// hook paths, so a hung agent CLI must not hang the user's turn.
const gooseCommandTimeout = 30 * time.Second

// Indirection points so tests can exercise the callers without a real binary.
var (
	runGooseExportToFileFn = runGooseExportToFile
	runGooseImportFn       = runGooseImport
)

// runGooseExportToFile runs `goose session export` and redirects stdout to
// outputPath.
//
// --format json is mandatory: export defaults to markdown, which is lossy and
// cannot be imported back.
func runGooseExportToFile(ctx context.Context, sessionID, outputPath string) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, gooseCommandTimeout)
	defer cancel()

	//nolint:gosec // outputPath is built by the caller under .entire/tmp
	file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create export file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("failed to close export file: %w", closeErr)
		}
	}()

	cmd := exec.CommandContext(ctx, "goose", "session", "export",
		"--session-id", sessionID, "--format", "json")
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.Remove(outputPath)
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("goose session export timed out after %s", gooseCommandTimeout)
		}
		return fmt.Errorf("goose session export failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// runGooseImport runs `goose session import <file>` to restore a session into
// Goose's SQLite database.
func runGooseImport(ctx context.Context, exportFilePath string) error {
	ctx, cancel := context.WithTimeout(ctx, gooseCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "goose", "session", "import", exportFilePath)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("goose session import timed out after %s", gooseCommandTimeout)
		}
		return fmt.Errorf("goose session import failed: %w (output: %s)", err, string(output))
	}
	return nil
}
