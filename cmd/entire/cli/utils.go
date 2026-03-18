package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/charmbracelet/huh"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// IsAccessibleMode returns true if accessibility mode should be enabled.
// This checks the ACCESSIBLE environment variable.
// Set ACCESSIBLE=1 (or any non-empty value) to enable accessible mode,
// which uses simpler prompts that work better with screen readers.
func IsAccessibleMode() bool {
	return os.Getenv("ACCESSIBLE") != ""
}

// entireTheme returns the Dracula theme for consistent styling.
func entireTheme() *huh.Theme {
	return huh.ThemeDracula()
}

// NewAccessibleForm creates a new huh form with accessibility mode
// enabled if the ACCESSIBLE environment variable is set.
// Note: WithAccessible() is only available on forms, not individual fields.
// Always wrap confirmations and other prompts in a form to enable accessibility.
func NewAccessibleForm(groups ...*huh.Group) *huh.Form {
	form := huh.NewForm(groups...).WithTheme(entireTheme())
	if IsAccessibleMode() {
		form = form.WithAccessible(true)
	}
	return form
}

// handleFormCancellation handles cancellation from huh form prompts.
// If err is a user abort (Ctrl+C), it prints "[action] cancelled." to w and returns nil.
// Otherwise it returns the error as-is.
func handleFormCancellation(w io.Writer, action string, err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		fmt.Fprintf(w, "%s cancelled.\n", action)
		return nil
	}
	return err
}

// printSessionCommand writes a single session resume command line to w.
// It appends a "(most recent)" label to the last entry in a multi-session list,
// and a "# prompt" comment when a prompt is available.
func printSessionCommand(w io.Writer, resumeCmd, prompt string, isMulti, isLast bool) {
	comment := ""
	if isMulti && isLast {
		if prompt != "" {
			comment = fmt.Sprintf("  # %s (most recent)", prompt)
		} else {
			comment = "  # (most recent)"
		}
	} else if prompt != "" {
		comment = "  # " + prompt
	}
	fmt.Fprintf(w, "  %s%s\n", resumeCmd, comment)
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// copyFile copies a file from src to dst using os.Root for traversal-resistant
// writes (Go 1.24+). dst must be absolute and reside under either the repo
// worktree root, the user's home directory (for agent session dirs such as
// ~/.claude/), or the system temp directory (used during tests).
// The kernel enforces that the write cannot escape the allowed directory,
// eliminating TOCTOU races and symlink escapes.
func copyFile(src, dst string) error {
	src = filepath.Clean(src)
	dst = filepath.Clean(dst)

	if !filepath.IsAbs(dst) {
		return fmt.Errorf("copyFile: dst must be absolute, got %q", dst)
	}

	input, err := os.ReadFile(src)
	if err != nil {
		return err //nolint:wrapcheck // already present in codebase
	}

	root, relPath, err := openAllowedRoot(dst)
	if err != nil {
		return err
	}
	defer root.Close()

	if err := osroot.WriteFile(root, relPath, input, 0o600); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	return nil
}

// openAllowedRoot finds the allowed root directory that contains dst and returns
// an os.Root handle along with the relative path within that root.
// Allowed directories: repo worktree root, user home, system temp dir.
// dst is resolved through symlinks before matching to handle macOS /var → /private/var.
func openAllowedRoot(dst string) (*os.Root, string, error) {
	allowed := allowedRootDirs()

	// Resolve the directory portion of dst through symlinks so that e.g.
	// /var/folders/... matches /private/var/folders/... on macOS.
	// Only the parent directory is resolved; the final component may not exist yet.
	resolvedDst := dst
	if r, err := filepath.EvalSymlinks(filepath.Dir(dst)); err == nil {
		resolvedDst = filepath.Join(r, filepath.Base(dst))
	}

	for _, dir := range allowed {
		if !paths.IsSubpath(dir, resolvedDst) {
			continue
		}
		rel, err := filepath.Rel(dir, resolvedDst)
		if err != nil {
			continue
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, "", fmt.Errorf("openAllowedRoot: failed to open root %q: %w", dir, err)
		}
		return root, rel, nil
	}

	return nil, "", fmt.Errorf("openAllowedRoot: dst %q is outside allowed directories", dst)
}

// allowedRootDirs returns the list of directories that copyFile may write to.
// Directories are resolved through symlinks so they match resolved dst paths.
func allowedRootDirs() []string {
	allowed := make([]string, 0, 3)

	if repoRoot, err := paths.WorktreeRoot(context.Background()); err == nil {
		allowed = appendResolved(allowed, repoRoot)
	}
	if home, err := os.UserHomeDir(); err == nil {
		allowed = appendResolved(allowed, home)
	}
	if tmpDir := os.TempDir(); tmpDir != "" {
		allowed = appendResolved(allowed, tmpDir)
	}

	return allowed
}

// appendResolved appends dir to the list after resolving symlinks.
// Falls back to the original path if symlink resolution fails.
func appendResolved(dirs []string, dir string) []string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return append(dirs, resolved)
	}
	return append(dirs, dir)
}
