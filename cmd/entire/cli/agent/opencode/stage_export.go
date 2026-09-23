package opencode

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// stageExportPath creates an empty staging file for sessionID's export under
// dirName inside root, and returns its name relative to root. The caller must
// remove it unless it installs it.
//
// os.Root has no CreateTemp, so the unique suffix is drawn here and the file is
// created with O_EXCL: a collision retries rather than truncating a concurrent
// export's staging file.
func stageExportPath(root *os.Root, dirName, sessionID string) (string, error) {
	var suffix [8]byte
	for range stageNameAttempts {
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", fmt.Errorf("failed to create export staging file: %w", err)
		}
		name := dirName + "/" + exportStagePrefix + sessionID + "-" + hex.EncodeToString(suffix[:])
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("failed to create export staging file: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = root.Remove(name) //nolint:errcheck // best-effort cleanup of a staging file we failed to open
			return "", fmt.Errorf("failed to create export staging file: %w", err)
		}
		return name, nil
	}
	return "", fmt.Errorf("failed to create export staging file: exhausted %d name attempts", stageNameAttempts)
}

// stageNameAttempts bounds the O_EXCL retry loop in stageExportPath.
const stageNameAttempts = 100

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
func renameOverExisting(root *os.Root, staged, dest string) error {
	var err error
	for range renameRetries {
		if err = root.Rename(staged, dest); err == nil {
			syncDir(root, path.Dir(dest))
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
	return fmt.Errorf("%w (another process may be reading %s)", err, path.Base(dest))
}

// syncDir flushes a directory entry so a completed rename is not lost by a crash.
// Best-effort: directory fsync is not supported everywhere (notably Windows), and
// failing to durably record an export is not worth failing the export over.
func syncDir(root *os.Root, dirName string) {
	// Same shape as the directory fsync in jsonutil.WriteFileAtomic.
	if d, err := root.Open(dirName); err == nil {
		_ = d.Sync() //nolint:errcheck // best-effort directory fsync; failure does not roll back the rename
		_ = d.Close()
	}
}
