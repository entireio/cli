package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/redact"

	"github.com/charmbracelet/x/ansi"
)

// openCodeCommandTimeout is the maximum time to wait for opencode CLI commands.
const openCodeCommandTimeout = 30 * time.Second

const openCodeErrorDetailMaxRunes = 300

type openCodeExportError struct {
	message string
	cause   error
}

func (e *openCodeExportError) Error() string { return e.message }
func (e *openCodeExportError) Unwrap() error { return e.cause }

// runOpenCodeExportToFile runs `opencode export <sessionID>` and redirects stdout
// to outputPath. This avoids pipe/stdout capture truncation bugs in some opencode versions.
//
// outputName is relative to root, the shared .entire root, and must be a staging
// name, never a live transcript: `opencode export` can fail after writing a
// partial payload, and can exit 0 having written nothing at all. Callers own the
// validate-then-install step — see fetchAndCacheExport, which is the only caller
// and stages under .entire/tmp.
//
// opencode never sees the name: it inherits the already-opened file as stdout, so
// the root's containment covers the whole write even though the payload is
// produced by another process.
//
// The fsync before close is what makes the caller's rename durable: without it
// some filesystems can surface the rename as complete while the file is still
// empty after a hard crash, which would destroy the transcript the staging exists
// to protect. Same reasoning as jsonutil.WriteFileAtomic.
func runOpenCodeExportToFile(ctx context.Context, root *os.Root, sessionID, outputName string) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	file, err := root.OpenFile(outputName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create export file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("failed to close export file: %w", closeErr)
		}
	}()

	cmd := exec.CommandContext(ctx, "opencode", "export", sessionID)
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return classifyOpenCodeExportError(ctx, runErr, stderr.String(), sessionID)
	}

	if syncErr := file.Sync(); syncErr != nil {
		return fmt.Errorf("failed to flush export file: %w", syncErr)
	}

	return nil
}

func classifyOpenCodeExportError(ctx context.Context, err error, stderr, sessionID string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &openCodeExportError{
			message: fmt.Sprintf("OpenCode export timed out after %s. Try again.", openCodeCommandTimeout),
			cause:   context.DeadlineExceeded,
		}
	}

	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return &openCodeExportError{
			message: "OpenCode is not installed or is not available in PATH.",
			cause:   err,
		}
	}
	if errors.Is(err, os.ErrPermission) {
		return &openCodeExportError{
			message: "OpenCode could not be started because of insufficient permissions.",
			cause:   err,
		}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		detail := formatOpenCodeErrorDetail(stderr)
		if strings.HasPrefix(strings.ToLower(detail), "session not found") {
			return &openCodeExportError{
				message: fmt.Sprintf("OpenCode session %q was not found. Check the session ID and try again.", sessionID),
				cause:   err,
			}
		}
		if detail != "" {
			return &openCodeExportError{
				message: fmt.Sprintf("OpenCode could not export session %q: %s", sessionID, detail),
				cause:   err,
			}
		}
		return &openCodeExportError{
			message: fmt.Sprintf("OpenCode could not export session %q.", sessionID),
			cause:   err,
		}
	}

	return &openCodeExportError{message: "OpenCode export could not be started.", cause: err}
}

func formatOpenCodeErrorDetail(stderr string) string {
	for _, rawLine := range strings.Split(stderr, "\n") {
		line := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, ansi.Strip(rawLine))
		line = strings.TrimSpace(line)
		if len(line) < len("error:") || !strings.EqualFold(line[:len("error:")], "error:") {
			continue
		}

		detail := strings.Join(strings.Fields(redact.String(line[len("error:"):])), " ")
		runes := []rune(detail)
		if len(runes) > openCodeErrorDetailMaxRunes {
			return string(runes[:openCodeErrorDetailMaxRunes]) + "…"
		}
		return detail
	}
	return ""
}

// runOpenCodeSessionDelete runs `opencode session delete <sessionID>` to remove
// a session from OpenCode's database. Returns nil on success or if the session
// doesn't exist (nothing to delete).
func runOpenCodeSessionDelete(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "session", "delete", sessionID)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("opencode session delete timed out after %s", openCodeCommandTimeout)
		}
		// "Session not found" means the session doesn't exist — nothing to delete.
		if strings.Contains(strings.ToLower(string(output)), "session not found") {
			return nil
		}
		return fmt.Errorf("opencode session delete failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// runOpenCodeImport runs `opencode import <file>` to import a session into
// OpenCode's database. The import preserves the original session ID
// from the export file.
func runOpenCodeImport(ctx context.Context, exportFilePath string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "import", exportFilePath)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("opencode import timed out after %s", openCodeCommandTimeout)
		}
		return fmt.Errorf("opencode import failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// nativeSessionListEntry mirrors one row of `opencode session list --format
// json` (fields verified against OpenCode 1.18.16, per entireio/cli#1992):
// session ID, title, working directory, and last-update time. UpdatedAt is
// epoch milliseconds, matching SessionInfo's CreatedAt/UpdatedAt in the
// export format.
type nativeSessionListEntry struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	UpdatedAt int64  `json:"updatedAt"`
}

// runOpenCodeSessionList runs `opencode session list --format json` and
// parses its output. This lists every session in OpenCode's own store
// (untracked-by-Entire ones included), unscoped — callers filter to a
// worktree themselves.
//
// Known caveat (entireio/cli#1992): on some OpenCode versions this command
// can rewrite .opencode/package-lock.json as a side effect of enumerating
// sessions (upstream tracked as anomalyco/opencode#37435 and its fix
// anomalyco/opencode#37477). Entire runs the documented command as-is; it
// does not itself guard against or detect that mutation.
func runOpenCodeSessionList(ctx context.Context) ([]nativeSessionListEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "session", "list", "--format", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, classifyOpenCodeSessionListError(ctx, err, stderr.String())
	}

	var entries []nativeSessionListEntry
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		return nil, fmt.Errorf("failed to parse opencode session list output: %w", err)
	}
	return entries, nil
}

// openCodeSessionListError is the `opencode session list` counterpart of
// openCodeExportError: a concise, display-safe message that still retains its
// cause via Unwrap, so callers can errors.Is/As through it (e.g. to detect a
// missing opencode binary) without parsing the message text.
type openCodeSessionListError struct {
	message string
	cause   error
}

func (e *openCodeSessionListError) Error() string { return e.message }
func (e *openCodeSessionListError) Unwrap() error { return e.cause }

// classifyOpenCodeSessionListError turns a failed `opencode session list`
// invocation into a concise, display-safe error, mirroring
// classifyOpenCodeExportError's shape without the export-specific messages.
func classifyOpenCodeSessionListError(ctx context.Context, err error, stderr string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &openCodeSessionListError{
			message: fmt.Sprintf("OpenCode session list timed out after %s. Try again.", openCodeCommandTimeout),
			cause:   context.DeadlineExceeded,
		}
	}

	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return &openCodeSessionListError{
			message: "OpenCode is not installed or is not available in PATH.",
			cause:   err,
		}
	}
	if errors.Is(err, os.ErrPermission) {
		return &openCodeSessionListError{
			message: "OpenCode could not be started because of insufficient permissions.",
			cause:   err,
		}
	}

	if detail := formatOpenCodeErrorDetail(stderr); detail != "" {
		return &openCodeSessionListError{
			message: "OpenCode could not list sessions: " + detail,
			cause:   err,
		}
	}
	return &openCodeSessionListError{message: "OpenCode could not list sessions.", cause: err}
}
