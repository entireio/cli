package strategy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// hooksLockTimeout bounds the wait; the moves under the lock take milliseconds.
const hooksLockTimeout = time.Second

// hooksLockFile names the lock for one hooks directory, kept in the per-user
// cache. It is keyed by the resolved directory, not by repository: a
// core.hooksPath can be shared by several repositories, and they must all
// take the same lock.
func hooksLockFile(hooksDir string) (string, error) {
	abs, err := filepath.Abs(hooksDir)
	if err != nil {
		return "", fmt.Errorf("resolve hooks dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve hooks dir: %w", err)
	}
	sum := sha256.Sum256([]byte(resolved))
	return "hooks-" + hex.EncodeToString(sum[:8]) + ".lock", nil
}

func acquireHooksLock(ctx context.Context, lockRoot *os.Root, hooksDir string) (func(), error) {
	name, err := hooksLockFile(hooksDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, hooksLockTimeout)
	defer cancel()
	release, err := flock.AcquireContextIn(ctx, lockRoot, name)
	if err != nil {
		return nil, fmt.Errorf("another Entire process is changing git hooks (lock %s): %w", filepath.Join(lockRoot.Name(), name), err)
	}
	return release, nil
}

// errLinkedHookOverLegacy: a symlinked hook sits over Entire's hook in .legacy.
var errLinkedHookOverLegacy = errors.New("cannot verify a linked hook")

// errHookChangedDuringInstall: something replaced the hook after Entire read it.
var errHookChangedDuringInstall = errors.New("hook changed during install")

// afterHookBackup runs between backing a hook up and replacing it; tests swap
// the hook here.
var afterHookBackup = func(string) {}

// hookUnchanged reports whether name is still what the install classified as
// class and, for a foreign hook, backed up, so the write replaces nothing that
// lacks a backup. A writer outside Entire's lock can still land between this
// check and the rename; this narrows that gap to the write itself.
func hookUnchanged(root *os.Root, name string, class hookClassification) bool {
	switch class {
	case hookForeign:
		return sameHookFile(root, name, name+backupSuffix)
	case hookOurs:
		return carriesEntireMarker(root, name)
	case hookAbsent:
		return !hookFileExists(root, name)
	}
	return false
}

// backupAction is what prepareHookBackup did with a foreign hook.
type backupAction int

const (
	backupCreated      backupAction = iota // <hook>.pre-entire now holds it
	backupReplacedSame                     // identical to <hook>.pre-entire
	backupRotated                          // a different backup was kept as an older copy
)

const (
	// legacySuffix is where pre-commit moves a hook it replaces; its wrapper
	// runs that file before its own checks.
	legacySuffix = ".legacy"
	// keepSuffix holds Entire's copy of the user's hook that it put in
	// .legacy, because the next `pre-commit install` moves a hook onto .legacy.
	keepSuffix = legacySuffix + backupSuffix
	// tempPrefix names Entire's in-flight copies; installs sweep leftovers.
	tempPrefix = ".entire-tmp-"
	// olderCopyStampLayout names older backups; no colons, for Windows.
	olderCopyStampLayout = "20060102T150405Z"
)

// prepareHookBackup makes <name>.pre-entire hold the foreign hook at name. It
// only links or copies, never moves name away, so name keeps a runnable hook
// until the caller's atomic write replaces it, and a process that dies at any
// step leaves every version on disk. Nothing is fsynced, so that holds for a
// killed or crashed process, not for power loss or a kernel crash. A different existing backup is kept as
// <name>.pre-entire.<timestamp> unless an identical older copy exists. The
// caller holds the hooks lock.
func prepareHookBackup(root *os.Root, name string, now time.Time) (backupAction, string, error) {
	backup := name + backupSuffix
	if hookFileExists(root, backup) && sameHookFile(root, name, backup) {
		return backupReplacedSame, "", nil
	}
	action, older := backupCreated, ""
	if hookFileExists(root, backup) && !carriesEntireMarker(root, backup) {
		action = backupRotated
		if older = identicalOlderCopy(root, backup); older == "" {
			var err error
			if older, err = keepOlderCopy(root, backup, now); err != nil {
				return 0, "", err
			}
		}
	}
	if err := replaceWithCopy(root, name, backup); err != nil {
		return 0, "", err
	}
	return action, older, nil
}

// replaceWithCopy atomically replaces dst with a link to (or copy of) src.
func replaceWithCopy(root *os.Root, src, dst string) error {
	tmp := tempPrefix + dst
	if err := root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove leftover %s: %w", tmp, err)
	}
	if err := linkOrCopy(root, src, tmp); err != nil {
		return err
	}
	if err := root.Rename(tmp, dst); err != nil {
		return errors.Join(fmt.Errorf("replace %s: %w", dst, err), root.Remove(tmp))
	}
	return nil
}

// linkOrCopy creates dst holding src, failing if dst exists. A hard link keeps
// a symlink a symlink; where links are unsupported it copies instead.
func linkOrCopy(root *os.Root, src, dst string) error {
	err := root.Link(src, dst)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return err //nolint:wrapcheck // callers check fs.ErrExist
	}
	info, err := root.Lstat(src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := root.Readlink(src)
		if err != nil {
			return fmt.Errorf("copy %s: %w", src, err)
		}
		return root.Symlink(target, dst) //nolint:wrapcheck // callers check fs.ErrExist
	}
	data, err := osroot.ReadFileNoFollow(root, src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	f, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err //nolint:wrapcheck // callers check fs.ErrExist
	}
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, root.Chmod(dst, 0o755)); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}

// keepOlderCopy links backup to a free <backup>.<timestamp>[-N] name; the link
// fails rather than replace a name another process took.
func keepOlderCopy(root *os.Root, backup string, now time.Time) (string, error) {
	base := backup + "." + now.UTC().Format(olderCopyStampLayout)
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		err := linkOrCopy(root, backup, name)
		if err == nil {
			return name, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("keep an older copy of %s: %w", backup, err)
		}
	}
	return "", fmt.Errorf("no free name for an older copy of %s", backup)
}

func identicalOlderCopy(root *os.Root, backup string) string {
	copies, err := olderHookCopies(root)
	if err != nil {
		return ""
	}
	for _, c := range copies {
		if strings.HasPrefix(c, backup+".") && sameHookFile(root, c, backup) {
			return c
		}
	}
	return ""
}

// olderHookCopies lists <hook>.pre-entire.<timestamp> files in root, sorted.
func olderHookCopies(root *os.Root) ([]string, error) {
	names, err := hookDirNames(root)
	if err != nil {
		return nil, err
	}
	var copies []string
	for _, n := range names {
		if slices.ContainsFunc(gitHookNames, func(hook string) bool {
			return strings.HasPrefix(n, hook+backupSuffix+".")
		}) {
			copies = append(copies, n)
		}
	}
	slices.Sort(copies)
	return copies, nil
}

// removeLeftoverTemps deletes copies an interrupted install left behind.
func removeLeftoverTemps(root *os.Root) error {
	names, err := hookDirNames(root)
	if err != nil {
		return err
	}
	for _, n := range names {
		if strings.HasPrefix(n, tempPrefix) {
			if err := root.Remove(n); err != nil {
				return fmt.Errorf("remove leftover %s: %w", n, err)
			}
		}
	}
	return nil
}

func hookDirNames(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open hooks dir: %w", err)
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("read hooks dir: %w", err)
	}
	return names, nil
}

// sameHookFile: a and b are regular files with identical bytes, or symlinks to
// the same target.
func sameHookFile(root *os.Root, a, b string) bool {
	infoA, errA := root.Lstat(a)
	infoB, errB := root.Lstat(b)
	if errA != nil || errB != nil {
		return false
	}
	linkA, linkB := infoA.Mode()&fs.ModeSymlink != 0, infoB.Mode()&fs.ModeSymlink != 0
	if linkA != linkB {
		return false
	}
	if linkA {
		targetA, errA := root.Readlink(a)
		targetB, errB := root.Readlink(b)
		return errA == nil && errB == nil && targetA == targetB
	}
	dataA, errA := osroot.ReadFileNoFollow(root, a)
	dataB, errB := osroot.ReadFileNoFollow(root, b)
	return errA == nil && errB == nil && bytes.Equal(dataA, dataB)
}

// carriesEntireMarker: name is a regular file containing Entire's marker.
func carriesEntireMarker(root *os.Root, name string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	return err == nil && bytes.Contains(data, []byte(entireHookMarker))
}

// preCommitSignatures identify pre-commit's wrapper: its header line and the
// IDs pre-commit's is_our_script accepts (CURRENT_HASH and PRIOR_HASHES).
var preCommitSignatures = []string{
	"# File generated by pre-commit: https://pre-commit.com",
	"138fd403232d2ddd5efb44317e38bf03",
	"4d9958c90bc262f47553e2c073f14cfe",
	"d8ee923c46731b42cd95cc869add4062",
	"49fd668cb42069aa1b6048464be5d395",
	"79f09a650522a87b0da915d0d983b2de",
	"e358c9dae00eac5d06b38dfdb1e33a8c",
}

// isPreCommitWrapper: name is a regular file that is pre-commit's wrapper for
// hook, in the form that runs <hook>.legacy: it calls hook-impl for this hook
// type and passes --hook-dir (without it pre-commit skips .legacy).
func isPreCommitWrapper(root *os.Root, name, hook string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		return false
	}
	signed := slices.ContainsFunc(preCommitSignatures, func(sig string) bool {
		return bytes.Contains(data, []byte(sig))
	})
	return signed && bytes.Contains(data, []byte("hook-impl")) &&
		hasHookTypeArg(data, hook) && bytes.Contains(data, []byte("--hook-dir"))
}

// hasHookTypeArg finds --hook-type=<hook> as a whole token, however the
// template quotes it: `--hook-type=commit-msg)` (Bash) or `'--hook-type=commit-msg'`
// (pre-commit 2.x's Python template).
func hasHookTypeArg(data []byte, hook string) bool {
	arg := []byte("--hook-type=" + hook)
	for {
		i := bytes.Index(data, arg)
		if i < 0 {
			return false
		}
		data = data[i+len(arg):]
		if len(data) == 0 || !isHookNameByte(data[0]) {
			return true
		}
	}
}

func isHookNameByte(b byte) bool {
	return b == '-' || b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// reclaimFromPreCommit undoes `pre-commit install` moving Entire's hook to
// <hook>.legacy. Entire does not rely on pre-commit to run its hook: pre-commit
// gives .legacy no stdin for most hook types and skips it in some modes. So
// .legacy goes back to the user's own hook (which pre-commit runs from there),
// and the caller then backs up the wrapper and writes Entire's hook, giving
// Entire -> pre-commit -> the user's hook. Reports whether it changed anything.
func reclaimFromPreCommit(root *os.Root, hook string) (bool, error) {
	legacy, backup, keep := hook+legacySuffix, hook+backupSuffix, hook+keepSuffix
	if !carriesEntireMarker(root, legacy) {
		return false, nil
	}
	if info, err := root.Lstat(hook); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		// Possibly a linked pre-commit wrapper, which runs .legacy: backing it up
		// and chaining to it would loop through Entire's hook. Entire does not
		// read through the link to find out, so it changes nothing.
		return false, fmt.Errorf("%w: %s is a symbolic link and %s holds Entire's hook, "+
			"so chaining to the link may run Entire's hook again; replace the link with the file it points to, "+
			"or remove %s, then re-run 'entire enable'", errLinkedHookOverLegacy, hook, legacy, legacy)
	}
	if !isPreCommitWrapper(root, hook, hook) {
		return false, nil
	}
	userHookInBackup := hookFileExists(root, backup) &&
		!isPreCommitWrapper(root, backup, hook) && !carriesEntireMarker(root, backup)
	if userHookInBackup {
		if err := replaceWithCopy(root, backup, keep); err != nil {
			return false, err
		}
	}
	if !hookFileExists(root, keep) {
		if err := root.Remove(legacy); err != nil {
			return false, fmt.Errorf("remove Entire's %s: %w", legacy, err)
		}
		return true, nil
	}
	if err := replaceWithCopy(root, keep, legacy); err != nil {
		return false, err
	}
	if userHookInBackup {
		// Now held by .legacy and the keep copy; frees .pre-entire for the wrapper.
		if err := root.Remove(backup); err != nil {
			return false, fmt.Errorf("remove %s: %w", backup, err)
		}
	}
	return true, nil
}

// restoreLegacy undoes reclaimFromPreCommit on uninstall: .legacy gets the
// user's hook back if pre-commit moved Entire's onto it, and the keep copy goes.
func restoreLegacy(root *os.Root, hook string) error {
	legacy, keep := hook+legacySuffix, hook+keepSuffix
	keepExists := hookFileExists(root, keep)
	if carriesEntireMarker(root, legacy) {
		if !keepExists {
			return root.Remove(legacy) //nolint:wrapcheck // caller adds the hook name
		}
		return root.Rename(keep, legacy) //nolint:wrapcheck // caller adds the hook name
	}
	if !keepExists {
		return nil
	}
	if sameHookFile(root, keep, legacy) {
		return root.Remove(keep) //nolint:wrapcheck // caller adds the hook name
	}
	return nil
}
