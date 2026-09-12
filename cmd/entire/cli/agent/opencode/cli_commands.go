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

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
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

// openCodeExportInvocations lists the export subcommands across OpenCode
// versions, newest first. OpenCode 2 moved export under `session`; OpenCode 1
// exposes it at the top level.
var openCodeExportInvocations = [][]string{
	{"session", "export"},
	{"export"},
}

// runOpenCodeExportToFile runs the OpenCode export command and redirects stdout
// to outputName, trying each supported invocation until one produces valid
// session JSON. This avoids pipe/stdout capture truncation bugs in some opencode
// versions.
//
// outputName is relative to root, the shared .entire root, and must be a staging
// name, never a live transcript: the export can fail after writing a partial
// payload, and a version that does not know a subcommand can exit 0 having
// written only help text. Callers own the validate-then-install step — see
// fetchAndCacheExport, which is the only caller and stages under .entire/tmp.
//
// opencode never sees the name: it inherits the already-opened file as stdout, so
// the root's containment covers the whole write even though the payload is
// produced by another process.
//
// The fsync before close is what makes the caller's rename durable: without it
// some filesystems can surface the rename as complete while the file is still
// empty after a hard crash, which would destroy the transcript the staging exists
// to protect. Same reasoning as jsonutil.WriteFileAtomic.
func runOpenCodeExportToFile(ctx context.Context, root *os.Root, sessionID, outputName string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	var lastErr error
	for _, invocation := range openCodeExportInvocations {
		if err := writeOpenCodeExport(ctx, root, outputName, invocation, sessionID); err != nil {
			lastErr = err
			continue
		}
		data, err := entiredir.ReadFile(root, outputName)
		if err != nil {
			lastErr = fmt.Errorf("failed to read export file: %w", err)
			continue
		}
		if openCodeExportLooksValid(data) {
			return nil
		}
		lastErr = &openCodeExportError{
			message: fmt.Sprintf("OpenCode returned invalid transcript data for session %q. Try updating OpenCode and running the command again.", sessionID),
		}
	}
	if lastErr == nil {
		lastErr = &openCodeExportError{message: "OpenCode export could not be started."}
	}
	return lastErr
}

// writeOpenCodeExport runs one export invocation, writing stdout to outputName.
func writeOpenCodeExport(ctx context.Context, root *os.Root, outputName string, invocation []string, sessionID string) (retErr error) {
	file, err := root.OpenFile(outputName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create export file: %w", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("failed to close export file: %w", closeErr)
		}
	}()

	args := append(append([]string{}, invocation...), sessionID)
	cmd := exec.CommandContext(ctx, "opencode", args...)
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

// openCodeExportLooksValid reports whether data is a session object rather than
// an error or help page. Both export versions share the top-level shape.
func openCodeExportLooksValid(data []byte) bool {
	if !json.Valid(data) {
		return false
	}
	var probe struct {
		Info     *json.RawMessage `json:"info"`
		Messages *json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return false
	}
	return probe.Info != nil && probe.Messages != nil
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

// openCodeImportInvocations lists the import subcommands across OpenCode
// versions, newest first.
var openCodeImportInvocations = [][]string{
	{"session", "import"},
	{"import"},
}

// runOpenCodeImport imports a session into OpenCode's database, trying each
// supported invocation. The import preserves the original session ID from the
// export file. A version that does not know a subcommand can exit 0 printing
// help, so help output is treated as "try the next invocation".
func runOpenCodeImport(ctx context.Context, exportFilePath string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	var lastErr error
	for _, invocation := range openCodeImportInvocations {
		args := append(append([]string{}, invocation...), exportFilePath)
		cmd := exec.CommandContext(ctx, "opencode", args...)
		output, err := cmd.CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("opencode import timed out after %s", openCodeCommandTimeout)
		}
		if err == nil && !looksLikeOpenCodeHelp(output) {
			return nil
		}
		if err != nil {
			lastErr = fmt.Errorf("opencode import failed: %w (output: %s)", err, string(output))
		} else {
			lastErr = fmt.Errorf("opencode import is not supported by this OpenCode version (output: %s)", string(output))
		}
	}
	if lastErr == nil {
		lastErr = errors.New("opencode import could not be started")
	}
	return lastErr
}

// looksLikeOpenCodeHelp reports whether output is a command help page. A version
// that does not know a subcommand can exit 0 printing help instead of failing.
func looksLikeOpenCodeHelp(output []byte) bool {
	text := string(output)
	return strings.Contains(text, "DESCRIPTION") && strings.Contains(text, "USAGE")
}
