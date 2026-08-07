package shellhook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// PreferencesPath returns the absolute path of the preferences file.
func PreferencesPath() string {
	return filepath.Join(userdirs.Config(), preferencesFileName)
}

// LoadPreferences reads the user's shell hook preferences.
//
// A missing file is not an error: it yields ModeOff, which is what an
// un-installed hook must see. A malformed file is an error so `shellhook
// status` can report it; the hot path treats any error as ModeOff.
func LoadPreferences() (*Preferences, error) {
	path := PreferencesPath()
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from userdirs, not user input
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Preferences{Version: PreferencesVersion, Mode: ModeOff}, nil
		}
		return nil, fmt.Errorf("reading shell hook preferences: %w", err)
	}

	var prefs Preferences
	if err := json.Unmarshal(data, &prefs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if !prefs.Mode.Valid() {
		prefs.Mode = ModeOff
	}
	if prefs.Version == 0 {
		prefs.Version = PreferencesVersion
	}
	return &prefs, nil
}

// SavePreferences writes the user's shell hook preferences atomically.
func SavePreferences(prefs *Preferences) error {
	if prefs == nil {
		return errors.New("nil preferences")
	}
	if prefs.Version == 0 {
		prefs.Version = PreferencesVersion
	}
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding shell hook preferences: %w", err)
	}
	return writeFileAtomic(PreferencesPath(), append(data, '\n'))
}

// writeFileAtomic writes data to path via a temp file in the same directory
// followed by a rename, so a concurrent reader never sees a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// No-op once the rename succeeded; cleans up on every failure path.
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}
	return nil
}
