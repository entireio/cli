package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

func newDoctorLogsCmd() *cobra.Command {
	var tail int
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show recent operational logs",
		Long: `Print operational logs from .entire/logs/entire.log.

By default, prints the last 100 lines. Use --tail N to change.
Use --follow to stream new lines as they are written (Ctrl+C to exit).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repoRoot, err := paths.WorktreeRoot(cmd.Context())
			if err != nil {
				cmd.SilenceUsage = true
				return errors.New("not a git repository")
			}
			logFile := filepath.Join(repoRoot, logging.LogsDir, "entire.log")
			if _, err := os.Stat(logFile); errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(cmd.OutOrStdout(), "No log file at %s yet.\n", logFile)
				return nil
			}
			if err := printTail(cmd.OutOrStdout(), logFile, tail); err != nil {
				return err
			}
			if !follow {
				return nil
			}
			return followFile(cmd.Context(), cmd.OutOrStdout(), logFile)
		},
	}

	cmd.Flags().IntVar(&tail, "tail", 100, "Show the last N lines (use 0 for all)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream new log lines as they are written")
	return cmd
}

func printTail(w io.Writer, path string, n int) error {
	f, err := os.Open(path) //nolint:gosec // path is .entire/logs/entire.log under repo root
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	if n <= 0 {
		_, err := io.Copy(w, f)
		if err != nil {
			return fmt.Errorf("read log: %w", err)
		}
		return nil
	}

	lines, err := readLastNLines(f, n)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := io.WriteString(w, line); err != nil {
			return fmt.Errorf("write line: %w", err)
		}
		if !endsWithNewline(line) {
			if _, err := io.WriteString(w, "\n"); err != nil {
				return fmt.Errorf("write newline: %w", err)
			}
		}
	}
	return nil
}

func endsWithNewline(s string) bool {
	return len(s) > 0 && s[len(s)-1] == '\n'
}

// readLastNLines reads the file as a stream and returns up to n trailing lines.
// For typical log sizes this is fast enough; large files would benefit from a
// reverse-seek implementation but the current logger rotates so files stay small.
func readLastNLines(r io.Reader, n int) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*64), 1024*1024)
	ring := make([]string, 0, n)
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		if len(ring) < n {
			ring = append(ring, line)
		} else {
			ring = append(ring[1:], line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan log: %w", err)
	}
	return ring, nil
}

// followFile polls the log file for appended bytes. It exits cleanly when the
// command's context is cancelled (Ctrl+C in a TTY).
func followFile(ctx context.Context, w io.Writer, path string) error {
	f, err := os.Open(path) //nolint:gosec // path is .entire/logs/entire.log under repo root
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek log: %w", err)
	}

	buf := make([]byte, 4096)
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			for {
				n, err := f.Read(buf)
				if n > 0 {
					if _, werr := w.Write(buf[:n]); werr != nil {
						return fmt.Errorf("write: %w", werr)
					}
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return fmt.Errorf("read: %w", err)
				}
			}
		}
	}
}
