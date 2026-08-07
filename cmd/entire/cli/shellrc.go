package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Shell rc-file plumbing shared by `entire completion` setup and
// `entire shellhook install`.
//
// Both features add a line to the user's rc file, and both must be able to do
// so without touching anything else in it. Everything written here is a
// "marked block": a comment line the CLI owns, followed by the line(s) it
// manages. Removal only ever deletes a block it recognizes by that marker, so
// a hand-edited rc file cannot be corrupted by an uninstall.

// shellKind identifies a supported shell.
type shellKind string

const (
	shellZsh  shellKind = "zsh"
	shellBash shellKind = "bash"
	shellFish shellKind = "fish"
)

// errUnsupportedShell is returned when the user's shell is not one we can
// generate rc-file lines for.
var errUnsupportedShell = errors.New("unsupported shell")

// supportedShellNames lists the shells shellRCTarget understands, for use in
// user-facing error messages.
const supportedShellNames = "zsh, bash, fish"

// shellRCTarget resolves a shell to its kind, display name, and rc file.
//
// shell may be an explicit name ("zsh"), a path ("/bin/zsh"), or empty to fall
// back to $SHELL. Matching is by substring to accept both forms, mirroring
// what the completion setup has always done.
func shellRCTarget(shell string) (kind shellKind, displayName, rcFile string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	if shell == "" {
		shell = os.Getenv("SHELL")
	}

	switch {
	case strings.Contains(shell, string(shellZsh)):
		return shellZsh, "Zsh", filepath.Join(home, ".zshrc"), nil
	case strings.Contains(shell, string(shellBash)):
		// Preserve the long-standing preference: on a machine that has a
		// .bash_profile, that is the file a login shell reads.
		bashRC := filepath.Join(home, ".bashrc")
		if _, statErr := os.Stat(filepath.Join(home, ".bash_profile")); statErr == nil {
			bashRC = filepath.Join(home, ".bash_profile")
		}
		return shellBash, "Bash", bashRC, nil
	case strings.Contains(shell, string(shellFish)):
		return shellFish, "Fish", filepath.Join(home, ".config", "fish", "config.fish"), nil
	default:
		return "", "", "", errUnsupportedShell
	}
}

// rcFileContains reports whether the rc file exists and contains needle.
// An unreadable or missing file counts as "not present".
func rcFileContains(rcFile, needle string) bool {
	//nolint:gosec // G304: rcFile is constructed from home dir + known filename, not user input
	content, err := os.ReadFile(rcFile)
	if err != nil {
		return false
	}
	return strings.Contains(string(content), needle)
}

// isMarkerConfigured reports whether a marked block with this marker is
// already present in the rc file.
func isMarkerConfigured(rcFile, marker string) bool {
	return rcFileContains(rcFile, marker)
}

// appendMarkedBlock appends a marker comment and its managed line to the rc
// file, creating the file (and its directory) if needed.
//
// It appends unconditionally; callers guard with isMarkerConfigured so an
// install stays idempotent.
func appendMarkedBlock(rcFile, marker, line string) error {
	if err := os.MkdirAll(filepath.Dir(rcFile), 0o700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	//nolint:gosec // G302: Shell rc files need 0644 for user readability
	f, err := os.OpenFile(rcFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString("\n" + marker + "\n" + line + "\n"); err != nil {
		return fmt.Errorf("writing block: %w", err)
	}
	return nil
}

// removeMarkedBlock deletes every block introduced by marker from the rc file
// and reports whether anything was removed. A missing rc file is not an error.
//
// A block is the marker line plus the following run of non-blank lines, along
// with the blank separator appendMarkedBlock inserted before it. Lines outside
// such a block are never touched.
func removeMarkedBlock(rcFile, marker string) (bool, error) {
	//nolint:gosec // G304: rcFile is constructed from home dir + known filename, not user input
	content, err := os.ReadFile(rcFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", rcFile, err)
	}

	kept, removed := stripMarkedBlocks(strings.Split(string(content), "\n"), marker)
	if !removed {
		return false, nil
	}

	//nolint:gosec // G306: Shell rc files need 0644 for user readability
	if err := os.WriteFile(rcFile, []byte(joinRCLines(kept)), 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", rcFile, err)
	}
	return true, nil
}

// stripMarkedBlocks removes marker-introduced blocks from lines.
func stripMarkedBlocks(lines []string, marker string) (kept []string, removed bool) {
	kept = make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != marker {
			kept = append(kept, lines[i])
			continue
		}
		removed = true
		// Consume the block body: everything up to the next blank line.
		for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
			i++
		}
		// Drop the blank separator appendMarkedBlock wrote before the marker,
		// so repeated install/uninstall cycles do not accumulate blank lines.
		if n := len(kept); n > 0 && strings.TrimSpace(kept[n-1]) == "" {
			kept = kept[:n-1]
		}
	}
	return kept, removed
}

// joinRCLines reassembles rc-file lines, normalizing a file that is now empty
// (or entirely blank) to the empty string and otherwise ending with exactly
// one newline.
func joinRCLines(lines []string) string {
	out := strings.Join(lines, "\n")
	if strings.TrimSpace(out) == "" {
		return ""
	}
	return strings.TrimRight(out, "\n") + "\n"
}
