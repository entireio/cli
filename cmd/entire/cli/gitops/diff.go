package gitops

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DiffTreeFiles returns the set of files changed between two commits using git diff-tree.
// For initial commits (commit1 is empty), use --root mode.
// repoDir is the path to the git repository working tree; the command runs in that directory.
// Returns a map of file paths for O(1) lookup.
func DiffTreeFiles(ctx context.Context, repoDir, commit1, commit2 string) (map[string]struct{}, error) {
	files, err := diffTreeRaw(ctx, repoDir, commit1, commit2)
	if err != nil {
		return nil, err
	}

	result := make(map[string]struct{}, len(files))
	for _, f := range files {
		result[f] = struct{}{}
	}
	return result, nil
}

// DiffTreeFileList returns the list of files changed between two commits using git diff-tree.
// For initial commits (commit1 is empty), use --root mode.
// repoDir is the path to the git repository working tree; the command runs in that directory.
func DiffTreeFileList(ctx context.Context, repoDir, commit1, commit2 string) ([]string, error) {
	return diffTreeRaw(ctx, repoDir, commit1, commit2)
}

// diffTreeRaw runs git diff-tree in the given directory and returns the list of changed file paths.
func diffTreeRaw(ctx context.Context, dir, commit1, commit2 string) ([]string, error) {
	var cmd *exec.Cmd
	if commit1 == "" {
		// Initial commit (no parent): list all files in the tree
		cmd = exec.CommandContext(ctx, "git", "diff-tree", "--root", "--no-commit-id", "-r", "-z", commit2)
	} else {
		cmd = exec.CommandContext(ctx, "git", "diff-tree", "--no-commit-id", "-r", "-z", commit1, commit2)
	}
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff-tree failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return parseDiffTreeOutput(stdout.Bytes()), nil
}

// parseDiffTreeOutput parses null-separated git diff-tree -r -z output.
// The format is: :<old-mode> <new-mode> <old-hash> <new-hash> <status>\0<path>\0
// For renames/copies: :<old-mode> <new-mode> <old-hash> <new-hash> <status>\0<old-path>\0<new-path>\0
func parseDiffTreeOutput(data []byte) []string {
	if len(data) == 0 {
		return nil
	}

	// Split on null bytes
	parts := bytes.Split(data, []byte{0})

	var files []string
	i := 0
	for i < len(parts) {
		part := string(parts[i])

		switch {
		case strings.HasPrefix(part, ":"):
			// This is a status line like ":100644 100644 abc123 def456 M"
			// The next part(s) are the path(s)
			status := extractStatus(part)
			i++
			if i >= len(parts) {
				break
			}

			// First path
			path := string(parts[i])
			if path != "" {
				files = append(files, path)
			}
			i++

			// Renames (R) and copies (C) have a second path
			if (status == 'R' || status == 'C') && i < len(parts) {
				path2 := string(parts[i])
				if path2 != "" && !strings.HasPrefix(path2, ":") {
					files = append(files, path2)
					i++
				}
			}
		default:
			// Skip empty parts (trailing null byte) and unexpected content
			i++
		}
	}

	return files
}

// extractStatus extracts the single-char status from a diff-tree status line.
// Input format: ":100644 100644 abc123... def456... M" or similar
func extractStatus(statusLine string) byte {
	// Status is the last non-space character in the metadata portion
	// Format: :<old-mode> <new-mode> <old-hash> <new-hash> <status-letter>[score]
	trimmed := strings.TrimSpace(statusLine)
	if len(trimmed) == 0 {
		return 0
	}

	// Split by spaces and take the last field
	fields := strings.Fields(trimmed)
	if len(fields) < 5 {
		return 0
	}
	statusField := fields[4]
	if len(statusField) == 0 {
		return 0
	}
	return statusField[0]
}
