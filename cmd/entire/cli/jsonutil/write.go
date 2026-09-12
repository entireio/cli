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
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// WriteFileAtomic writes data to filePath atomically by writing to a temp file
// in the same directory and renaming it into place. A crash or signal mid-write
// leaves the original file intact rather than a truncated partial — important
// for config files like .entire/settings.json that callers expect to remain
// parseable across interrupted writes.
//
// The rename is what provides that, and it is deliberately NOT paired with an
// fsync. The property every caller here needs is "a reader never sees a torn
// file", which the rename gives on its own. fsync buys something different —
// that the bytes survive a power loss — and nothing written through this
// function is worth that price: settings, session state, caches and manifests
// are all reconstructible, and losing the last write to one costs a repeated
// command, not data. The price is not small, measured at 14x on a 4KiB payload
// (1.02ms against 71µs), and session state is written on every agent hook.
//
// What is given up, precisely: on a filesystem that reorders the rename ahead
// of the data write, a hard power loss can leave a zero-length file where the
// old contents used to be. Every reader here treats an unparseable or empty
// file as absent and rebuilds it.
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
	return nil
}

// WriteFileAtomicIn is WriteFileAtomic confined to root: name is resolved
// relative to root by the kernel, so neither the temp file nor the rename
// target can escape it. It is the form every .entire writer uses, because the
// names under .entire are built from agent-supplied session and tool-use IDs.
//
// The sequence is identical to WriteFileAtomic — write, close, chmod, rename,
// and no fsync — and the same reasoning applies to each step. Two differences:
// os.Root has no CreateTemp, so the unique temp name is drawn here and created
// with O_EXCL (a collision retries rather than clobbering a concurrent writer's
// temp file); and the parent directory is opened once, up front, with every
// component checked for symlinks, so the temp file, the chmod and the rename
// all act on the same pinned directory rather than re-resolving name each time.
//
// name must be a valid slash-separated path beneath root (see fs.ValidPath):
// "./x", "x//y" and "x/./y" are rejected rather than cleaned.
func WriteFileAtomicIn(root *os.Root, name string, data []byte, perm fs.FileMode) error {
	parent, leaf, closeParent, err := osroot.OpenParentNoSymlinks(root, name)
	if err != nil {
		return fmt.Errorf("open parent for %s: %w", name, err)
	}
	defer closeParent()

	tmp, tmpName, err := CreateTempIn(parent, leaf)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", name, err)
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = parent.Remove(tmpName) //nolint:errcheck // best-effort cleanup of a temp file the rename did not consume
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", name, err)
	}
	if err := parent.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", name, err)
	}
	if err := parent.Rename(tmpName, leaf); err != nil {
		return fmt.Errorf("rename temp to %s: %w", name, err)
	}
	removeTmp = false
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

// tempNameAttempts bounds the O_EXCL retry loop in createTempIn.
const tempNameAttempts = 100

// tempSuffix ends every name CreateTempIn hands out.
const tempSuffix = ".tmp"

// tempRandomHexLen is the length of the random component CreateTempIn inserts,
// in hex characters: 8 bytes, so 16.
const tempRandomHexLen = 16

// IsTempName reports whether name was produced by CreateTempIn.
//
// It exists because an atomic write leaves its temp file in the SAME directory
// as its target, and one of those directories — .entire/metadata/<session> — is
// walked wholesale into every checkpoint tree. A hook killed between
// CreateTempIn and Rename (an agent's hook timeout, Codex's session-end process
// tree kill, a crash) leaves the temp behind, and without this the walk redacts
// it, commits it, and pushes it on every checkpoint from then on.
//
// The match is the whole shape CreateTempIn produces — "<base>.<16 hex>.tmp" —
// not a bare ".tmp" suffix, so a file a user or an agent legitimately named
// something.tmp is still captured.
func IsTempName(name string) bool {
	base := path.Base(name)
	rest, ok := strings.CutSuffix(base, tempSuffix)
	if !ok {
		return false
	}
	dot := strings.LastIndexByte(rest, '.')
	if dot < 0 {
		return false
	}
	hexPart := rest[dot+1:]
	if len(hexPart) != tempRandomHexLen {
		return false
	}
	for i := range len(hexPart) {
		if !isHexDigit(hexPart[i]) {
			return false
		}
	}
	// A temp name is always "<something>.<hex>.tmp": CreateTempIn appends to a
	// base it was given, so an empty base means this is not one of ours.
	return dot > 0
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

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
		candidate := dir + base + "." + hex.EncodeToString(buf[:]) + tempSuffix
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
