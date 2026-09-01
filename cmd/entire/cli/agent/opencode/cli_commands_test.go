package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClassifyOpenCodeExportError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		stderr    string
		sessionID string
		want      string
	}{
		{
			name:      "missing executable",
			err:       &exec.Error{Name: "opencode", Err: exec.ErrNotFound},
			sessionID: "ses_missing_binary",
			want:      "OpenCode is not installed or is not available in PATH.",
		},
		{
			name:      "permission denied",
			err:       &os.PathError{Op: "fork/exec", Path: "/private/opencode", Err: os.ErrPermission},
			sessionID: "ses_denied",
			want:      "OpenCode could not be started because of insufficient permissions.",
		},
		{
			name:      "missing session",
			err:       &exec.ExitError{},
			stderr:    "Exporting session: ses_missing\nError: Session not found: ses_missing\n",
			sessionID: "ses_missing",
			want:      `OpenCode session "ses_missing" was not found. Check the session ID and try again.`,
		},
		{
			name:      "useful stderr",
			err:       &exec.ExitError{},
			stderr:    "Exporting session\n\x1b[31mError: Export was rejected by OpenCode\x1b[0m\nDB_PASSWORD=not-a-real-secret\n",
			sessionID: "ses_rejected",
			want:      `OpenCode could not export session "ses_rejected": Export was rejected by OpenCode`,
		},
		{
			name:      "unstructured stderr",
			err:       &exec.ExitError{},
			stderr:    "internal stack trace\nDB_PASSWORD=not-a-real-secret\n",
			sessionID: "ses_opaque",
			want:      `OpenCode could not export session "ses_opaque".`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyOpenCodeExportError(context.Background(), tt.err, tt.stderr, tt.sessionID)
			if got.Error() != tt.want {
				t.Fatalf("classifyOpenCodeExportError error = %q, want %q", got, tt.want)
			}
			if !errors.Is(got, tt.err) {
				t.Fatal("classified error does not retain its cause")
			}
		})
	}
}

func TestClassifyOpenCodeExportError_Timeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	err := classifyOpenCodeExportError(ctx, errors.New("signal: killed"), "", "ses_timeout")
	want := "OpenCode export timed out after 30s. Try again."
	if err.Error() != want {
		t.Fatalf("classifyOpenCodeExportError error = %q, want %q", err, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timeout error does not retain context.DeadlineExceeded")
	}
}

// TestRunOpenCodeExportToFile_MissingBinary pins the classification of the most
// common failure. The runner writes to a staging path the caller owns, so the
// preservation of the live transcript is covered by the fetchAndCacheExport tests
// in lifecycle_test.go, not here.
func TestRunOpenCodeExportToFile_MissingBinary(t *testing.T) {
	// No t.Parallel: t.Setenv.
	root := mustOpenRoot(t, t.TempDir())
	const staged = ".export-ses_cached.json-1"

	// Empty PATH makes the export fail deterministically without an opencode binary.
	t.Setenv("PATH", "")

	err := runOpenCodeExportToFile(context.Background(), root, "ses_cached", staged)
	if err == nil {
		t.Fatal("expected export to fail with no opencode on PATH")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("runOpenCodeExportToFile error = %v, want exec.ErrNotFound", err)
	}
}

func TestRunOpenCodeExportToFile_WritesStdoutToPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	// No t.Parallel: t.Setenv.
	dir := t.TempDir()
	root := mustOpenRoot(t, dir)
	const staged = ".export-ses_ok.json-1"

	const export = `{"info":{"id":"ses_ok"},"messages":[]}`
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '" + export + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)

	if err := runOpenCodeExportToFile(context.Background(), root, "ses_ok", staged); err != nil {
		t.Fatalf("runOpenCodeExportToFile failed: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, staged))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != export {
		t.Fatalf("exported transcript = %q, want %q", string(got), export)
	}
}

func TestRenameOverExisting_ReplacesDestination(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root := mustOpenRoot(t, dir)
	const staged = ".export-ses_x.json-1"
	const dest = "ses_x.json"
	if err := os.WriteFile(filepath.Join(dir, staged), []byte("fresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dest), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := renameOverExisting(root, staged, dest); err != nil {
		t.Fatalf("renameOverExisting failed: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, dest))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Fatalf("destination = %q, want %q", string(got), "fresh")
	}
	if _, err := os.Stat(filepath.Join(dir, staged)); !os.IsNotExist(err) {
		t.Errorf("staged file still present after rename: %v", err)
	}
}

func TestIsRenameContention_NonSharingErrorsAreNotRetried(t *testing.T) {
	t.Parallel()

	// On POSIX this is always false; on Windows only sharing/access violations
	// qualify. A plain ENOENT must never be retried on either.
	if isRenameContention(os.ErrNotExist) {
		t.Error("isRenameContention(ErrNotExist) = true, want false")
	}
}

func TestFormatOpenCodeErrorDetail_Truncates(t *testing.T) {
	t.Parallel()

	detail := formatOpenCodeErrorDetail("Error: " + strings.Repeat("a", openCodeErrorDetailMaxRunes+1))
	want := strings.Repeat("a", openCodeErrorDetailMaxRunes) + "…"
	if detail != want {
		t.Fatalf("formatOpenCodeErrorDetail = %q, want %q", detail, want)
	}
}

// mustOpenRoot opens dir as an os.Root, standing in for the shared .entire root
// the production callers pass.
func mustOpenRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("os.OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}
