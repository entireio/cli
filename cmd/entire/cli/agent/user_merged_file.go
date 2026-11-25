package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MergedUserFile is an agent's own user-level config file (a settings or
// hooks file the user also edits) that Entire merges its hooks into. The
// agent supplies the merge; this type owns the shared shape: the "agent not
// in use" check, the lock, the read-only fast path, symlink handling and the
// health report.
type MergedUserFile struct {
	// Path is the file; its directory missing means the agent is not in use.
	Path string
	// MaxBytes, when set, refuses a larger file.
	MaxBytes int
	// Perm is the mode of a newly created file; an existing file keeps its own.
	Perm os.FileMode
	// HasEntire reports whether data holds any Entire hook.
	HasEntire func(data []byte) bool
	// Merge returns data with Entire's hooks merged in and how many entries
	// that changed (0: already current). exists is false for a missing file.
	Merge func(data []byte, exists bool) ([]byte, int, error)
	// Strip returns data without Entire's hooks and whether that changed it.
	Strip func(data []byte) ([]byte, bool, error)
	// Version finds the installed Entire hook version (nil: 0).
	Version func(data []byte) int
}

// configDirMissing reports an agent's user-level directory that does not
// exist: the agent is not in use here, and nothing is created for it.
func configDirMissing(dir string) bool {
	_, err := os.Stat(dir)
	return errors.Is(err, fs.ErrNotExist)
}

func (m MergedUserFile) lockPath() string { return m.Path + ".lock" }

// read returns the file, reporting a missing one as !exists.
func (m MergedUserFile) read() ([]byte, bool, error) {
	data, err := UserConfigFile{Path: m.Path}.Read()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", m.Path, err)
	}
	if m.MaxBytes > 0 && len(data) > m.MaxBytes {
		return nil, false, fmt.Errorf("%s exceeds %d bytes", m.Path, m.MaxBytes)
	}
	return data, true, nil
}

// Ensure merges Entire's hooks in while wanted holds (see
// EnsureUserHookFile), returning how many entries changed, or
// ErrUserHooksNotInUse when the file's directory is absent.
func (m MergedUserFile) Ensure(ctx context.Context, wanted func(context.Context) bool) (int, error) {
	if configDirMissing(filepath.Dir(m.Path)) {
		return 0, userHooksNotInUseError{dir: filepath.Dir(m.Path)}
	}
	return EnsureUserHookFile(ctx, m.lockPath(), wanted, func(write bool) (int, error) {
		data, exists, err := m.read()
		if err != nil {
			return 0, err
		}
		output, count, err := m.Merge(data, exists)
		if err != nil || count == 0 || !write {
			return count, err
		}
		if !exists {
			if err := m.createdRecord().Write(nil, 0o600); err != nil {
				return 0, err
			}
		}
		return count, UserConfigFile{Path: m.Path}.Write(output, m.Perm)
	})
}

// Remove strips Entire's hooks under the lock, keeping everything else, and
// reports whether there were any. before runs under the lock first.
func (m MergedUserFile) Remove(ctx context.Context, before func()) (bool, error) {
	if configDirMissing(filepath.Dir(m.Path)) {
		return false, nil
	}
	release, err := LockUserHookFile(ctx, m.lockPath())
	if err != nil {
		return false, err
	}
	defer release()
	if before != nil {
		before()
	}
	data, exists, err := m.read()
	if err != nil || !exists || !m.HasEntire(data) {
		return false, err
	}
	output, changed, err := m.Strip(data)
	if err != nil || !changed {
		return false, err
	}
	file := UserConfigFile{Path: m.Path}
	record := m.createdRecord()
	if holdsNothing(output) && record.exists() {
		// Entire created the file and its hooks were all it held: remove it
		// rather than leave {} behind. A file the user had stays.
		if err := file.Remove(); err != nil {
			return false, err
		}
		_ = record.Remove() //nolint:errcheck // a stale record only matters once the file is recreated, which rewrites it
		return true, nil
	}
	if err := file.Write(output, m.Perm); err != nil {
		return false, err
	}
	_ = record.Remove() //nolint:errcheck // see above
	return true, nil
}

// createdRecord marks, next to the file, that Entire created it, so removal
// deletes only a file Entire made (as GeneratedUserFile records the
// directories it creates).
func (m MergedUserFile) createdRecord() UserConfigFile {
	return UserConfigFile{Path: m.Path + ".entire-created"}
}

func (u UserConfigFile) exists() bool {
	_, err := os.Lstat(u.Path)
	return err == nil
}

// holdsNothing reports JSON that is an object with no keys, or whose only
// keys hold empty objects (Codex's {"hooks": {}}).
func holdsNothing(data []byte) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return false
	}
	for _, value := range doc {
		var inner map[string]json.RawMessage
		if json.Unmarshal(value, &inner) != nil || len(inner) > 0 {
			return false
		}
	}
	return true
}

// Inspect reports the file's health without writing.
func (m MergedUserFile) Inspect() UserHookReport {
	report := UserHookReport{Path: m.Path}
	if configDirMissing(filepath.Dir(m.Path)) {
		report.State = UserHooksNotInUse
		return report
	}
	data, exists, err := m.read()
	switch {
	case err != nil:
		report.State, report.Err = UserHooksUnavailable, err
		return report
	case !exists || !m.HasEntire(data):
		report.State = UserHooksAbsent
		return report
	}
	if m.Version != nil {
		report.Version = m.Version(data)
	}
	_, count, err := m.Merge(data, true)
	switch {
	case err != nil:
		report.State, report.Err = UserHooksUnavailable, err
	case count > 0:
		report.State = UserHooksOutdated
	default:
		report.State = UserHooksCurrent
	}
	return report
}
