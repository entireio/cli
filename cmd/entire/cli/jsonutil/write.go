package jsonutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to filePath atomically by writing to a temp file
// in the same directory, fsyncing it, renaming into place, and fsyncing the
// parent directory. A crash or signal mid-write leaves the original file
// intact rather than a truncated partial — important for config files like
// .entire/settings.json that callers expect to remain parseable across
// interrupted writes.
//
// The fsync between Write and Close guarantees the temp file's bytes are on
// disk before the rename takes effect; without it, some filesystems (notably
// ext4 with non-default mount options) can surface the rename as completed
// while the file is still empty after a hard crash.
//
// The parent-directory fsync after rename guarantees the rename's directory
// entry is durable. Without it, the file contents are on disk but the
// directory may still point to the pre-rename state after a crash, so the
// "leaves the original intact" promise would silently break. Windows does
// not support directory fsync; we make this step best-effort so the call
// does not fail on platforms where the operation is a no-op.
//
// perm is applied to the temp file via Chmod before rename so the final file
// lands with the requested permission regardless of the temp file's default.
func WriteFileAtomic(filePath string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	tmp, err := os.CreateTemp(dir, base+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", filePath, err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", filePath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", filePath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", filePath, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", filePath, err)
	}
	if err := os.Rename(tmpName, filePath); err != nil {
		return fmt.Errorf("rename temp to %s: %w", filePath, err)
	}
	removeTmp = false
	// Best-effort: the rename succeeded, so don't propagate failures here.
	// Directory fsync isn't supported on Windows, and on POSIX an error
	// after a successful rename would mislead callers who already have the
	// file in place.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: dir is filepath.Dir of caller-supplied filePath, not user input
		_ = d.Sync() //nolint:errcheck // best-effort directory fsync; failure does not roll back the rename
		_ = d.Close()
	}
	return nil
}
