package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
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

// runOpenCodeExportToFile runs the session export for the installed OpenCode
// (`opencode export <id>` on 1.x, `opencode session export --standalone <id>`
// on 2.x; see detectOpenCodeCLI) and redirects stdout to outputPath. This
// avoids pipe/stdout capture truncation bugs in some opencode versions.
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

	cli := detectOpenCodeCLI(ctx)
	stderr, runErr := runOpenCodeExport(ctx, file, cli.exportArgs(sessionID))
	if runErr != nil && cli.guessed && ctx.Err() == nil {
		// The version could not be read, so the command was a guess; the
		// other OpenCode's command may be the one this binary accepts.
		if _, seekErr := file.Seek(0, 0); seekErr == nil && file.Truncate(0) == nil {
			other := openCodeCLI{legacy: !cli.legacy}
			if retryStderr, retryErr := runOpenCodeExport(ctx, file, other.exportArgs(sessionID)); retryErr == nil {
				runErr = nil
			} else {
				stderr, runErr = retryStderr, retryErr
			}
		}
	}
	if runErr != nil {
		return classifyOpenCodeExportError(ctx, runErr, stderr, sessionID)
	}

	if syncErr := file.Sync(); syncErr != nil {
		return fmt.Errorf("failed to flush export file: %w", syncErr)
	}

	return nil
}

// runOpenCodeExport runs one `opencode` export command with stdout to file
// and returns its stderr and error.
func runOpenCodeExport(ctx context.Context, file *os.File, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "opencode", args...)
	// The npm launcher runs the real binary as a child that inherits stderr;
	// killing only the launcher would leave Run waiting on that pipe past the
	// deadline, and a commit's fetch budget depends on this cut-off. The new
	// process group leaves the terminal's foreground group: a first Ctrl-C
	// cancels ctx and so the export, but a forced second one exits the CLI
	// without reaching it, which leaves the export and its staging file.
	execx.TerminateOnCancel(cmd)
	// Exports run inside git hooks (condensation in post-commit),
	// whose GIT_DIR / GIT_INDEX_FILE would otherwise reach OpenCode's own git
	// calls (project detection) and point them at the hook's state.
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
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
		if strings.HasPrefix(strings.ToLower(detail), "session not found") || reportsSessionNotFound(stderr) {
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

// reportsSessionNotFound reports OpenCode 2's missing-session line, which
// carries no "Error:" prefix ("Session not found: ses_…").
func reportsSessionNotFound(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ansi.Strip(line))), "session not found") {
			return true
		}
	}
	return false
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

// openCodeSessionExists reports whether OpenCode has sessionID, by exporting
// it and discarding the output.
func openCodeSessionExists(ctx context.Context, sessionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", detectOpenCodeCLI(ctx).exportArgs(sessionID)...)
	execx.TerminateOnCancel(cmd)
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && reportsSessionNotFound(stderr.String()) {
			return false, nil
		}
		return false, classifyOpenCodeExportError(ctx, err, stderr.String(), sessionID)
	}
	return true, nil
}

// runOpenCodeSessionImport runs OpenCode 2's `opencode session import
// --standalone <file>` from dir, which becomes the session's location. A
// session that already exists is left as it is (OpenCode reports it and
// exits 0).
func runOpenCodeSessionImport(ctx context.Context, exportFilePath, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "session", "import", "--standalone", exportFilePath)
	execx.TerminateOnCancel(cmd)
	cmd.Dir = dir
	cmd.Env = gitrepo.EnvWithoutRepoOverrides()
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("opencode session import timed out after %s", openCodeCommandTimeout)
		}
		return fmt.Errorf("opencode session import failed: %w (output: %s)", err, formatOpenCodeErrorDetail(string(output)))
	}
	return nil
}
