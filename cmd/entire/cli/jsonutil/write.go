package jsonutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// WriteFileAtomicIn is WriteFileAtomic confined to root: name is resolved
// relative to root by the kernel, so neither the temp file nor the rename
// target can escape it. It is the form every .entire writer uses, because the
// names under .entire are built from agent-supplied session and tool-use IDs.
//
// The durability sequence is identical to WriteFileAtomic — write, fsync,
// close, chmod, rename, best-effort parent-directory fsync — and the same
// reasoning applies to each step. The only difference is that os.Root has no
// CreateTemp, so the unique temp name is drawn here and created with O_EXCL:
// a collision retries rather than clobbering a concurrent writer's temp file.
func WriteFileAtomicIn(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	dir := path.Dir(name)
	tmp, tmpName, err := CreateTempIn(root, name)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", name, err)
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = root.Remove(tmpName) //nolint:errcheck // best-effort cleanup of a temp file the rename did not consume
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", name, err)
	}
	if err := root.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", name, err)
	}
	if err := root.Rename(tmpName, name); err != nil {
		return fmt.Errorf("rename temp to %s: %w", name, err)
	}
	removeTmp = false
	// Best-effort, for the reasons WriteFileAtomic gives.
	if d, err := root.Open(dir); err == nil {
		_ = d.Sync() //nolint:errcheck // best-effort directory fsync; failure does not roll back the rename
		_ = d.Close()
	}
	return nil
}

// tempNameAttempts bounds the O_EXCL retry loop in createTempIn.
const tempNameAttempts = 100

// CreateTempIn creates a uniquely named, exclusively created temp file next to
// name inside root, and returns it with its root-relative name. It is the
// os.Root counterpart to os.CreateTemp, which has no Root form.
//
// O_EXCL rather than a fixed "<name>.tmp": concurrent hook processes write the
// same session and checkpoint files, and a shared temp path corrupts whichever
// write lands second.
func CreateTempIn(root *os.Root, name string) (*os.File, string, error) {
	dir, base := path.Split(name)
	var buf [8]byte
	for range tempNameAttempts {
		if _, err := rand.Read(buf[:]); err != nil {
			return nil, "", fmt.Errorf("read random suffix: %w", err)
		}
		candidate := dir + base + "." + hex.EncodeToString(buf[:]) + ".tmp"
		f, err := root.OpenFile(candidate, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, candidate, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err //nolint:wrapcheck // caller names the file; the os error carries op and path
		}
	}
	return nil, "", fmt.Errorf("exhausted %d temp name attempts for %s", tempNameAttempts, name)
}
