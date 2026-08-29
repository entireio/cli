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
// perm is applied to the OPEN temp file (fd-based chmod) before close, so the
// final file lands with the requested permission with no path-based window —
// the same pattern as copyFileThenRemove's migration writer. The temp's name
// is random and never published until the rename, so its transient state is
// unreachable by path anyway.
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
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp for %s: %w", filePath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", filePath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", filePath, err)
	}
	if err := os.Rename(tmpName, filePath); err != nil {
		return fmt.Errorf("rename temp to %s: %w", filePath, err)
	}
	removeTmp = false
	// Best-effort: the rename succeeded, so don't propagate failures here.
	// Directory fsync isn't supported on Windows, and on POSIX an error
	// after a successful rename would mislead callers who already have the
	// file in place.
	_ = SyncDir(dir) //nolint:errcheck // best-effort: failure cannot roll back the successful rename
	return nil
}

// WriteFileAtomicFollowingSymlinks writes like WriteFileAtomic but resolves a
// symlinked filePath first and writes through to its ultimate target — dotfile
// managers commonly symlink user-level config files, and the atomic rename
// would otherwise replace the link with a regular file, silently detaching the
// managed target. A dangling link (stow/chezmoi create the link before the
// target file exists) is followed to its target so the file is created THROUGH
// the link; only a final component that is genuinely not a symlink falls back
// to the literal path.
//
// When the resolved target already exists its file mode is preserved; perm
// applies only when the write creates the file.
func WriteFileAtomicFollowingSymlinks(filePath string, data []byte, perm fs.FileMode) error {
	target, err := resolveWriteTarget(filePath)
	if err != nil {
		return err
	}
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() {
		perm = info.Mode().Perm()
	}
	if target != filePath {
		// A dotfile manager can link to a target whose directory does not
		// exist yet; the caller only created the LINK's parent. Create the
		// target's parent (user-private) so the write lands through the link.
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("%s resolves through a symlink to %s: create parent: %w", filePath, target, err)
		}
	}
	err = WriteFileAtomic(target, data, perm)
	if err != nil && target != filePath {
		// Name both paths: the failure is about the resolved target (whose
		// directory may be unwritable, or owned by someone else), but the
		// caller and the user only know the link they asked to write.
		return fmt.Errorf("%s resolves through a symlink to %s: %w", filePath, target, err)
	}
	return err
}

// maxSymlinkHops bounds the manual final-component symlink walk in
// resolveWriteTarget; 40 matches the Linux kernel's total-link-traversal cap.
const maxSymlinkHops = 40

// resolveWriteTarget resolves filePath to the path a write should land on,
// following symlinks through the final component even when the link target
// does not exist yet. filepath.EvalSymlinks alone cannot do this: it errors on
// a dangling link exactly like on a missing file, and treating that error as
// "no symlink here" is how an existing dangling settings.json link got
// replaced by a regular file. A relative filePath stays relative —
// EvalSymlinks never absolutizes — so it resolves against the CWD exactly as
// os.WriteFile would. A symlink cycle (a -> a, a -> b -> a) or a chain past the
// hop cap is an error: rename(2) onto a link would otherwise replace the link
// itself with a regular file instead of failing.
func resolveWriteTarget(filePath string) (string, error) {
	path := filePath
	seen := make(map[string]struct{}, 4)
	for range maxSymlinkHops {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, nil
		}
		// Some component of path does not exist. Resolve the parent so the
		// temp file and rename land in the real directory, then follow the
		// final component by hand if it is a dangling symlink.
		if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
			path = filepath.Join(resolvedDir, filepath.Base(path))
		}
		// Cycle detection keys on the CANONICAL form (parent symlinks resolved,
		// cleaned): a cycle spelled two ways — a/link -> ../b/link -> ../a/link,
		// or the same link reached through a linked directory — must be
		// reported as a cycle, not run into the hop cap.
		if _, cycled := seen[path]; cycled {
			return "", fmt.Errorf("resolve %s: symlink cycle at %s", filePath, path)
		}
		seen[path] = struct{}{}
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			// The final component is genuinely absent (create it here) or not
			// a symlink — the literal path is the write target.
			return path, nil //nolint:nilerr // absent final component is the create-here case, not a failure
		}
		target, err := os.Readlink(path)
		if err != nil {
			return path, nil //nolint:nilerr // unreadable link: write to the literal path and let WriteFileAtomic report it
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return "", fmt.Errorf("resolve %s: symlink chain exceeds %d hops", filePath, maxSymlinkHops)
}
