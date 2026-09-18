package opencode

import (
	"bytes"
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

// TestRunOpenCodeExport_MissingBinary pins the classification of the most common
// failure. Publication is covered by the fetchAndCacheExport tests.
func TestRunOpenCodeExport_MissingBinary(t *testing.T) {
	// No t.Parallel: t.Setenv.
	// Empty PATH makes the export fail deterministically without an opencode binary.
	t.Setenv("PATH", "")

	err := runOpenCodeExport(context.Background(), "ses_cached", &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected export to fail with no opencode on PATH")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("runOpenCodeExport error = %v, want exec.ErrNotFound", err)
	}
}

func TestRunOpenCodeExport_WritesStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	// No t.Parallel: t.Setenv.
	const export = `{"info":{"id":"ses_ok"},"messages":[]}`
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '" + export + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)

	var output bytes.Buffer
	if err := runOpenCodeExport(context.Background(), "ses_ok", &output); err != nil {
		t.Fatalf("runOpenCodeExport failed: %v", err)
	}
	if output.String() != export {
		t.Fatalf("exported transcript = %q, want %q", output.String(), export)
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
