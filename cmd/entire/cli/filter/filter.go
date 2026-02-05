// Package filter provides content filtering through external commands.
// Used to redact sensitive data (like API keys, secrets) from session transcripts.
package filter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// FilterTimeout is the maximum time allowed for filter command execution.
const FilterTimeout = 30 * time.Second

// ErrFilterNotFound is returned when the filter command executable cannot be found.
var ErrFilterNotFound = errors.New("output filter command not found")

// ErrFilterFailed is returned when the filter command returns a non-zero exit code.
var ErrFilterFailed = errors.New("output filter failed")

// ErrFilterTimeout is returned when the filter command exceeds the timeout.
var ErrFilterTimeout = errors.New("output filter timed out")

// Content pipes content through the configured filter command.
// Returns original content if no filter is configured.
// Returns error if filter is configured but fails (command not found, non-zero exit, timeout).
//
// The filename parameter is provided for context (e.g., logging) but is not passed
// to the filter command. The filter receives content on stdin and writes to stdout.
func Content(ctx context.Context, content []byte, filename string) ([]byte, error) {
	filterCmd := settings.GetOutputFilter()
	if len(filterCmd) == 0 {
		logging.Debug(ctx, "output filter: no filter configured, passing through",
			slog.String("filename", filename))
		return content, nil
	}

	logging.Debug(ctx, "output filter: applying filter",
		slog.String("command", filterCmd[0]),
		slog.String("filename", filename),
		slog.Int("content_size", len(content)))

	filtered, err := runFilter(ctx, content, filterCmd)
	if err != nil {
		logging.Warn(ctx, "output filter: filter failed",
			slog.String("command", filterCmd[0]),
			slog.String("filename", filename),
			slog.String("error", err.Error()))
		return nil, err
	}

	logging.Debug(ctx, "output filter: filter applied successfully",
		slog.String("filename", filename),
		slog.Int("original_size", len(content)),
		slog.Int("filtered_size", len(filtered)))

	return filtered, nil
}

// runFilter executes the filter command with the given content on stdin.
func runFilter(ctx context.Context, content []byte, filterCmd []string) ([]byte, error) {
	// Create context with timeout
	ctx, cancel := context.WithTimeout(ctx, FilterTimeout)
	defer cancel()

	// Build the command
	//nolint:gosec // filterCmd is from trusted settings file
	cmd := exec.CommandContext(ctx, filterCmd[0], filterCmd[1:]...)
	cmd.Stdin = bytes.NewReader(content)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the command
	err := cmd.Run()
	if err != nil {
		// Check for timeout
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w after %s", ErrFilterTimeout, FilterTimeout)
		}

		// Check for command not found
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, fmt.Errorf("%w: %q", ErrFilterNotFound, filterCmd[0])
		}

		// Check for exit error
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderrStr := stderr.String()
			if len(stderrStr) > 200 {
				stderrStr = stderrStr[:200] + "..."
			}
			return nil, fmt.Errorf("%w (exit %d): %s", ErrFilterFailed, exitErr.ExitCode(), stderrStr)
		}

		// Unknown error
		return nil, fmt.Errorf("filter execution failed: %w", err)
	}

	return stdout.Bytes(), nil
}

// Must is like Content but panics on error.
// Use only in contexts where filter failure is truly unrecoverable.
func Must(ctx context.Context, content []byte, filename string) []byte {
	filtered, err := Content(ctx, content, filename)
	if err != nil {
		panic(fmt.Sprintf("filter failed for %s: %v", filename, err))
	}
	return filtered
}
