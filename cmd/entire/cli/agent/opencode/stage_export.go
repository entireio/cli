package opencode

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// exportStagePrefix marks a transient, half-written export under .entire/tmp.
//
// The leading "." and the absence of a ".json" suffix are load-bearing: everything
// else in .entire/tmp is created-and-left, and the directory is scanned by name by
// FindActivePreTaskFile (pre-task-*.json) and by `entire clean --all`. A staged
// export must not look like either.
const exportStagePrefix = ".export-"

// renameRetries bounds how long we wait out a Windows sharing violation. On POSIX
// the first attempt either succeeds or fails for a reason retrying cannot fix.
const (
	renameRetries = 5
	renameBackoff = 40 * time.Millisecond
)

// stageExportPath creates an empty staging file in dir for sessionID's export and
// returns its path. The caller must remove it unless it installs it.
func stageExportPath(dir, sessionID string) (string, error) {
	file, err := os.CreateTemp(dir, exportStagePrefix+sessionID+"-*")
	if err != nil {
		return "", fmt.Errorf("failed to create export staging file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("failed to create export staging file: %w", err)
	}
	return file.Name(), nil
}

// renameOverExisting moves staged onto dest, replacing it, and fsyncs dest's
// directory so the replacement survives a crash.
//
// On Windows the replace can lose a race that does not exist on POSIX: os.Rename
// there is MoveFileEx(MOVEFILE_REPLACE_EXISTING), which must delete dest, and Go
// opens files without FILE_SHARE_DELETE — so any concurrent reader of dest (an
// `entire attach` in another terminal, the condensation path reading the same
// transcript) makes the rename fail with a sharing violation where the older
// in-place write would have succeeded. Readers of a transcript hold it briefly, so
// a bounded retry covers the realistic case; a reader that holds it longer gets a
// clear error and a preserved staging file rather than a lost transcript.
func renameOverExisting(staged, dest string) error {
	var err error
	for range renameRetries {
		if err = os.Rename(staged, dest); err == nil {
			syncDir(filepath.Dir(dest))
			return nil
		}
		if !isRenameContention(err) {
			// Deliberately raw: fetchAndCacheExport owns the user-facing wording
			// for a failed install, because it is also the one that knows where
			// the validated export was kept. Phrasing it here too would print
			// the same sentence twice.
			return err //nolint:wrapcheck // caller (fetchAndCacheExport) wraps this
		}
		time.Sleep(renameBackoff)
	}
	return fmt.Errorf("%w (another process may be reading %s)", err, filepath.Base(dest))
}

// syncDir flushes a directory entry so a completed rename is not lost by a crash.
// Best-effort: directory fsync is not supported everywhere (notably Windows), and
// failing to durably record an export is not worth failing the export over.
func syncDir(dir string) {
	// Same shape as the directory fsync in jsonutil.WriteFileAtomic.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: dir is the .entire/tmp path the caller just wrote into, not user input
		_ = d.Sync() //nolint:errcheck // best-effort directory fsync; failure does not roll back the rename
		_ = d.Close()
	}
}
