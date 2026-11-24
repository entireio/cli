package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// GeneratedUserFile is an Entire-generated file (an extension or plugin) in
// an agent's user-level config, recognized as Entire's by Marker.
type GeneratedUserFile struct {
	// Home is the agent's user-level directory; when it does not exist the
	// agent is not in use.
	Home string
	// Path is the file.
	Path string
	// Marker is a string only Entire's file contains.
	Marker string
	// Content is what the file should hold.
	Content string
	// Version finds the stamped hook version in the file (one digit group).
	Version *regexp.Regexp
	// RemoveDir also removes the file's directory, when empty, on removal.
	RemoveDir bool
}

// Ensure writes the file when it is missing or differs, under the shared
// user-level lock while wanted holds. A file there that is not Entire's is
// refused, not overwritten.
func (g GeneratedUserFile) Ensure(ctx context.Context, wanted func(context.Context) bool) (int, error) {
	if _, err := os.Stat(g.Home); errors.Is(err, fs.ErrNotExist) {
		return 0, ErrUserHooksNotInUse
	}
	return EnsureUserHookFile(ctx, g.lockPath(), wanted, g.apply)
}

func (g GeneratedUserFile) apply(write bool) (int, error) {
	file := UserConfigFile{Path: g.Path}
	existing, err := file.Read()
	switch {
	case err == nil && string(existing) == g.Content:
		return 0, nil
	case err == nil && !strings.Contains(string(existing), g.Marker):
		return 0, fmt.Errorf("refusing to overwrite foreign file at %s", g.Path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return 0, fmt.Errorf("read %s: %w", g.Path, err)
	}
	if !write {
		return 1, nil
	}
	// The user's own config directory: plain MkdirAll, which follows a
	// symlinked dotfiles directory as the file write does.
	if err := os.MkdirAll(filepath.Dir(g.Path), 0o755); err != nil { //nolint:gosec // the agent reads the file itself
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(g.Path), err)
	}
	if err := file.Write([]byte(g.Content), 0o644); err != nil {
		return 0, err
	}
	return 1, nil
}

// Remove deletes Entire's file (and, with RemoveDir, its directory when
// empty), leaving a file that is not Entire's alone. Reports whether it
// removed one.
func (g GeneratedUserFile) Remove(ctx context.Context) (bool, error) {
	if _, err := os.Stat(g.Home); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	release, err := LockUserHookFile(ctx, g.lockPath())
	if err != nil {
		return false, err
	}
	defer release()
	file := UserConfigFile{Path: g.Path}
	data, err := file.Read()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", g.Path, err)
	}
	if !strings.Contains(string(data), g.Marker) {
		return false, nil
	}
	if err := file.Remove(); err != nil {
		return false, err
	}
	if g.RemoveDir {
		// Fails, as intended, while anything else is in the directory.
		_ = os.Remove(filepath.Dir(g.Path))
	}
	return true, nil
}

// lockPath is in Home, which exists whenever anything is written: the
// file's own directory may not exist yet on a first install.
func (g GeneratedUserFile) lockPath() string {
	return filepath.Join(g.Home, ".entire-user-hooks.lock")
}

// Inspect reports the file's state without writing.
func (g GeneratedUserFile) Inspect() UserHookReport {
	report := UserHookReport{Path: g.Path}
	if _, err := os.Stat(g.Home); errors.Is(err, fs.ErrNotExist) {
		report.State = UserHooksNotInUse
		return report
	}
	data, err := UserConfigFile{Path: g.Path}.Read()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		report.State = UserHooksAbsent
		return report
	case err != nil:
		report.State, report.Err = UserHooksUnavailable, err
		return report
	case !strings.Contains(string(data), g.Marker):
		report.State = UserHooksAbsent
		report.Notes = append(report.Notes, "A file that is not Entire's is in the way: "+g.Path)
		return report
	}
	if g.Version != nil {
		if m := g.Version.FindSubmatch(data); m != nil {
			report.Version, _ = strconv.Atoi(string(m[1])) //nolint:errcheck // the pattern matched digits
		}
	}
	if string(data) == g.Content {
		report.State = UserHooksCurrent
	} else {
		report.State = UserHooksOutdated
	}
	return report
}
