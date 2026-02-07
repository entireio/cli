// Package gitutil provides git CLI shims that return go-git types.
//
// go-git v5's worktree.Status() reads and rewrites the index file.
// When called from git hook paths (post-commit, prepare-commit-msg),
// this can corrupt the index by writing stale cache-tree entries that
// reference objects pruned by GC. These shims use git CLI instead,
// which reads the index without rewriting it.
//
// See ENT-242 for details. When go-git v6 fixes index corruption,
// the shim functions can be swapped back to native go-git calls.
package gitutil

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v5"
)

// WorktreeStatus returns the worktree status using git CLI instead of go-git.
//
// Parses `git status --porcelain -z` (NUL-delimited, handles special chars)
// and returns the same git.Status type for drop-in compatibility with
// callers that previously used worktree.Status().
//
// Uses -uall to list individual untracked files instead of collapsing
// directories, matching go-git's worktree.Status() behavior.
//
// When go-git v6 fixes index corruption (ENT-242), swap to:
//
//	w, _ := repo.Worktree(); return w.Status()
func WorktreeStatus(repo *git.Repository) (git.Status, error) {
	output, err := runGitCmd(repo, "git status", "status", "--porcelain", "-z", "-uall")
	if err != nil {
		return nil, err
	}
	return parsePorcelainStatus(string(output)), nil
}

// StagedFileNames returns just the names of files staged for commit.
// Uses `git diff --cached --name-only -z` which is lighter than a full status.
//
// When go-git v6 fixes index corruption (ENT-242), swap to:
//
//	w, _ := repo.Worktree(); s, _ := w.Status(); filter for Staging != Unmodified
func StagedFileNames(repo *git.Repository) ([]string, error) {
	output, err := runGitCmd(repo, "git diff --cached", "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return nil, err
	}
	return parseNulDelimitedNames(string(output)), nil
}

// runGitCmd runs a git command in the worktree root, capturing stderr for
// diagnostics on failure.
func runGitCmd(repo *git.Repository, label string, args ...string) ([]byte, error) {
	repoRoot, err := worktreeRoot(repo)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s failed in %s: %w: %s", label, repoRoot, err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

// worktreeRoot returns the filesystem root of the repo's worktree.
func worktreeRoot(repo *git.Repository) (string, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree: %w", err)
	}
	return wt.Filesystem.Root(), nil
}

// parseNulDelimitedNames splits NUL-delimited output into a string slice,
// dropping empty elements (e.g., trailing NUL).
func parseNulDelimitedNames(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "\x00")
	var names []string
	for _, p := range parts {
		if p != "" {
			names = append(names, p)
		}
	}
	return names
}

// porcelainStatusCode maps a single byte from `git status --porcelain` XY output
// to the corresponding go-git StatusCode.
func porcelainStatusCode(b byte) git.StatusCode {
	switch b {
	case ' ':
		return git.Unmodified
	case 'M':
		return git.Modified
	case 'T':
		// Type change — go-git has no dedicated code; treat as Modified.
		return git.Modified
	case 'A':
		return git.Added
	case 'D':
		return git.Deleted
	case 'R':
		return git.Renamed
	case 'C':
		return git.Copied
	case '?':
		return git.Untracked
	case '!':
		// Ignored — callers typically skip these; map to Untracked so they
		// are visible rather than silently dropped.
		return git.Untracked
	case 'U':
		return git.UpdatedButUnmerged
	default:
		return git.Unmodified
	}
}

// parsePorcelainStatus parses NUL-delimited `git status --porcelain -z` output
// into a git.Status map. The format is:
//
//	XY filename\0            — for most entries
//	XY newname\0oldname\0    — for renames (R) and copies (C)
func parsePorcelainStatus(raw string) git.Status {
	status := make(git.Status)
	if raw == "" {
		return status
	}

	entries := strings.Split(raw, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 3 {
			continue // trailing empty element or malformed
		}

		x := entry[0] // staging status
		y := entry[1] // worktree status
		filename := entry[3:]

		fs := &git.FileStatus{
			Staging:  porcelainStatusCode(x),
			Worktree: porcelainStatusCode(y),
		}

		// Renames and copies have a second NUL-separated entry (the old name).
		// Check both X (staging) and Y (worktree) columns defensively.
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			if i+1 < len(entries) && entries[i+1] != "" {
				fs.Extra = entries[i+1]
				i++ // skip old-name entry
			}
		}

		status[filename] = fs
	}

	return status
}
