package jsonutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

const (
	renameAttempts = 5
	renameBackoff  = 40 * time.Millisecond
)

// PublishError reports that a complete, validated staging file could not be
// installed at its destination. Ownership of StagedPath transfers to the
// caller, which must recover or remove it.
type PublishError struct {
	StagedPath  string
	destination string
	err         error
}

func (e *PublishError) Error() string {
	return fmt.Sprintf("rename temp to %s: %v", e.destination, e.err)
}

func (e *PublishError) Unwrap() error { return e.err }

// WriteFileAtomicStream atomically replaces filePath with bytes written by
// produce after validate accepts the completed staging file. The helper owns
// the staging file's full lifecycle: create, produce, sync, close, validate,
// chmod, rename, and best-effort parent-directory fsync.
//
// The writer and reader belong to the helper and are valid only for the
// duration of their callbacks. Each callback is invoked at most once.
//
// Producer and validator errors are returned unchanged. Failures before the
// rename leave the destination untouched and make a best-effort attempt to
// remove the staging file. A rename failure instead returns *PublishError and
// transfers ownership of its already-validated StagedPath to the caller.
func WriteFileAtomicStream(
	ctx context.Context,
	filePath string,
	perm fs.FileMode,
	produce func(io.Writer) error,
	validate func(io.Reader) error,
) error {
	if produce == nil {
		return fmt.Errorf("produce callback for %s is nil", filePath)
	}
	if validate == nil {
		return fmt.Errorf("validate callback for %s is nil", filePath)
	}

	return writeFileAtomic(ctx, filePath, perm, produce, validate, atomicWriteConfig{
		ops:                          defaultAtomicWriteOps(),
		tempPattern:                  "." + filepath.Base(filePath) + ".*.tmp",
		retainStagedOnPublishFailure: true,
	})
}

// WriteFileAtomicStreamIn is WriteFileAtomicStream confined to root. It rejects
// symlinked parent components and pins the parent for the whole publication.
// name follows the same path rules as WriteFileAtomicIn. PublishError.StagedPath
// is rooted at root.Name(), for recovery and display rather than confined I/O.
func WriteFileAtomicStreamIn(
	ctx context.Context,
	root *os.Root,
	name string,
	perm fs.FileMode,
	produce func(io.Writer) error,
	validate func(io.Reader) error,
) error {
	if produce == nil {
		return fmt.Errorf("produce callback for %s is nil", name)
	}
	if validate == nil {
		return fmt.Errorf("validate callback for %s is nil", name)
	}
	parent, leaf, closeParent, err := osroot.OpenParentNoSymlinks(root, name)
	if err != nil {
		return fmt.Errorf("open parent for %s: %w", name, err)
	}
	defer closeParent()

	return writeFileAtomic(ctx, filepath.Join(parent.Name(), leaf), perm, produce, validate, atomicWriteConfig{
		ops:                          confinedAtomicWriteOps(parent, leaf),
		retainStagedOnPublishFailure: true,
	})
}

func confinedAtomicWriteOps(parent *os.Root, leaf string) atomicWriteOps {
	// The parent is already pinned; only sibling names from this operation reach
	// these functions. Never resolve the file's display path again for I/O.
	return atomicWriteOps{
		createTemp: func(_, _ string) (syncWriteCloser, error) {
			file, _, err := CreateTempIn(parent, "."+leaf)
			return file, err
		},
		open: func(name string) (io.ReadCloser, error) {
			return parent.Open(filepath.Base(name))
		},
		chmod: func(name string, mode fs.FileMode) error {
			return parent.Chmod(filepath.Base(name), mode)
		},
		rename: func(oldName, newName string) error {
			return parent.Rename(filepath.Base(oldName), filepath.Base(newName))
		},
		remove: func(name string) error {
			return parent.Remove(filepath.Base(name))
		},
		openDir: func(string) (syncCloser, error) {
			return parent.Open(".")
		},
		isRenameContention: isRenameContention,
		wait:               waitForRenameRetry,
	}
}

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
	produce := func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write temp for %s: %w", filePath, err)
		}
		return nil
	}

	return writeFileAtomic(context.Background(), filePath, perm, produce, nil, atomicWriteConfig{
		ops:         defaultAtomicWriteOps(),
		tempPattern: filepath.Base(filePath) + ".*.tmp",
		skipSync:    true,
	})
}

type syncWriteCloser interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

type syncCloser interface {
	Sync() error
	Close() error
}

type atomicWriteOps struct {
	createTemp         func(dir, pattern string) (syncWriteCloser, error)
	open               func(path string) (io.ReadCloser, error)
	chmod              func(path string, mode fs.FileMode) error
	rename             func(oldPath, newPath string) error
	remove             func(path string) error
	openDir            func(path string) (syncCloser, error)
	isRenameContention func(error) bool
	wait               func(context.Context, time.Duration) error
}

type atomicWriteConfig struct {
	skipSync                     bool
	ops                          atomicWriteOps
	tempPattern                  string
	retainStagedOnPublishFailure bool
}

func defaultAtomicWriteOps() atomicWriteOps {
	return atomicWriteOps{
		createTemp: func(dir, pattern string) (syncWriteCloser, error) {
			return os.CreateTemp(dir, pattern)
		},
		open: func(path string) (io.ReadCloser, error) {
			return os.Open(path) //nolint:gosec // the helper opens its own sibling staging path
		},
		chmod:              os.Chmod,
		rename:             os.Rename,
		remove:             os.Remove,
		openDir:            openSyncDir,
		isRenameContention: isRenameContention,
		wait:               waitForRenameRetry,
	}
}

func openSyncDir(path string) (syncCloser, error) {
	dir, err := os.Open(path) //nolint:gosec // path is filepath.Dir of the caller-supplied destination
	if err != nil {
		return nil, fmt.Errorf("open directory for sync: %w", err)
	}
	return dir, nil
}

func waitForRenameRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait to retry rename: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func writeFileAtomic(
	ctx context.Context,
	filePath string,
	perm fs.FileMode,
	produce func(io.Writer) error,
	validate func(io.Reader) error,
	config atomicWriteConfig,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write for %s canceled: %w", filePath, err)
	}

	dir := filepath.Dir(filePath)
	tmp, err := config.ops.createTemp(dir, config.tempPattern)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", filePath, err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = config.ops.remove(tmpName) //nolint:errcheck // cleanup cannot replace the operation's primary error
		}
	}()

	if err := produce(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write for %s canceled after produce: %w", filePath, err)
	}
	if !config.skipSync {
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("sync temp for %s: %w", filePath, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", filePath, err)
	}

	if validate != nil {
		reader, err := config.ops.open(tmpName)
		if err != nil {
			return fmt.Errorf("open temp for validation of %s: %w", filePath, err)
		}
		validateErr := validate(reader)
		closeErr := reader.Close()
		if validateErr != nil {
			return validateErr
		}
		if closeErr != nil {
			return fmt.Errorf("close temp after validation of %s: %w", filePath, closeErr)
		}
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write for %s canceled after validation: %w", filePath, err)
	}
	if err := config.ops.chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", filePath, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write for %s canceled before publish: %w", filePath, err)
	}

	if err := renameWithRetry(ctx, tmpName, filePath, config.ops); err != nil {
		if config.retainStagedOnPublishFailure {
			removeTmp = false
			return &PublishError{StagedPath: tmpName, destination: filePath, err: err}
		}
		return fmt.Errorf("rename temp to %s: %w", filePath, err)
	}
	removeTmp = false
	if !config.skipSync {
		syncDirBestEffort(dir, config.ops)
	}
	return nil
}

func renameWithRetry(ctx context.Context, staged, destination string, ops atomicWriteOps) error {
	var err error
	for attempt := range renameAttempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("rename staging file canceled: %w", ctxErr)
		}
		if err = ops.rename(staged, destination); err == nil {
			return nil
		}
		if !ops.isRenameContention(err) {
			return err
		}
		if attempt == renameAttempts-1 {
			break
		}
		if waitErr := ops.wait(ctx, renameBackoff); waitErr != nil {
			return waitErr
		}
	}
	return fmt.Errorf("%w (another process may be reading %s)", err, filepath.Base(destination))
}

func syncDirBestEffort(dir string, ops atomicWriteOps) {
	d, err := ops.openDir(dir)
	if err != nil {
		return
	}
	_ = d.Sync() //nolint:errcheck // failure does not roll back an already-successful rename
	_ = d.Close()
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
