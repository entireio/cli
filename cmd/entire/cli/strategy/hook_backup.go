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

// errHooksLockUnavailable: the hooks lock could not be used at all (no lock
// directory, or a lock file that cannot be opened), as opposed to being held.
var errHooksLockUnavailable = errors.New("git hooks lock unavailable")

// acquireHooksLock takes the hooks lock for hooksDir. Only a timeout means
// another Entire process holds it; any other failure wraps
// errHooksLockUnavailable. A nil lockRoot means the lock directory could not
// be opened.
func acquireHooksLock(ctx context.Context, lockRoot *os.Root, hooksDir string) (func(), error) {
	if lockRoot == nil {
		return nil, fmt.Errorf("%w: no lock directory", errHooksLockUnavailable)
	}
	name, err := hooksLockFile(hooksDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errHooksLockUnavailable, err)
	}
	lockPath := filepath.Join(lockRoot.Name(), name)
	ctx, cancel := context.WithTimeout(ctx, hooksLockTimeout)
	defer cancel()
	release, err := flock.AcquireContextIn(ctx, lockRoot, name)
	switch {
	case err == nil:
		return release, nil
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("another Entire process is changing git hooks (lock %s): %w", lockPath, err)
	case errors.Is(err, context.Canceled):
		return nil, fmt.Errorf("git hooks lock %s: %w", lockPath, err)
	default:
		return nil, fmt.Errorf("%w (%s): %w", errHooksLockUnavailable, lockPath, err)
	}
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
	return replaceWith(root, src, dst, linkOrCopy)
}

// replaceWithOwnCopy is replaceWithCopy without the hard link: dst gets its
// own bytes (or its own symlink), so a hook manager that later writes into
// either name in place leaves the other as it was.
func replaceWithOwnCopy(root *os.Root, src, dst string) error {
	return replaceWith(root, src, dst, copyOrRelink)
}

func replaceWith(root *os.Root, src, dst string, create func(root *os.Root, src, dst string) error) error {
	tmp := tempPrefix + dst
	if err := root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove leftover %s: %w", tmp, err)
	}
	if err := create(root, src, tmp); err != nil {
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
	return copyOrRelink(root, src, dst)
}

// copyOrRelink creates dst as a byte copy of src, or as a symlink to the same
// target when src is a symlink, failing if dst exists.
func copyOrRelink(root *os.Root, src, dst string) error {
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
	return copyHookFile(root, src, dst)
}

// writeHookCopy writes a copied hook's bytes; tests replace it to fail.
var writeHookCopy = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err //nolint:wrapcheck // copyHookFile wraps it
}

// copyHookFile creates dst as a byte copy of src. A failed write removes dst,
// so no partial hook is left under a name that reads as a complete version.
func copyHookFile(root *os.Root, src, dst string) error {
	data, err := osroot.ReadFileNoFollow(root, src)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	f, err := root.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err //nolint:wrapcheck // callers check fs.ErrExist
	}
	werr := writeHookCopy(f, data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, root.Chmod(dst, 0o755)); err != nil {
		return errors.Join(fmt.Errorf("copy %s to %s: %w", src, dst, err), root.Remove(dst))
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
	legacy, backup := hook+legacySuffix, hook+backupSuffix
	if !carriesEntireMarker(root, legacy) {
		return false, refreshKeepCopy(root, legacy)
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
	_, err := moveUserHookInto(root, hook, legacy, userHookInBackup, func() error {
		if err := root.Remove(legacy); err != nil {
			return fmt.Errorf("remove Entire's %s: %w", legacy, err)
		}
		return nil
	})
	return err == nil, err
}

// moveUserHookInto puts the user's hook into moved, the file a hook manager's
// wrapper runs where Entire's hook used to be, and keeps a copy of it as
// <moved>.pre-entire, because the manager's next install moves whatever hook
// it finds onto moved again. userHookInBackup says .pre-entire holds the
// user's hook rather than the wrapper; it is cleared, so the wrapper can be
// backed up there. With no user hook anywhere, empty decides what moved holds.
// moved and the keep copy are separate files, not links to one, because a
// manager that writes its moved file in place would overwrite both. Reports
// whether moved now holds the user's hook.
func moveUserHookInto(root *os.Root, hook, moved string, userHookInBackup bool, empty func() error) (bool, error) {
	backup, keep := hook+backupSuffix, moved+backupSuffix
	if err := removeEntireKeepCopy(root, keep); err != nil {
		return false, err
	}
	if userHookInBackup {
		if err := replaceWithOwnCopy(root, backup, keep); err != nil {
			return false, err
		}
	}
	if !hookFileExists(root, keep) {
		return false, empty()
	}
	if err := replaceWithOwnCopy(root, keep, moved); err != nil {
		return false, err
	}
	if userHookInBackup {
		// Now held by moved and the keep copy; frees .pre-entire for the wrapper.
		if err := root.Remove(backup); err != nil {
			return false, fmt.Errorf("remove %s: %w", backup, err)
		}
	}
	return true, nil
}

// removeEntireKeepCopy removes a keep copy that holds Entire's hook rather than
// the user's, as one hard-linked to its moved file does once a manager writes
// Entire's hook into that file in place. Restoring it would run Entire's hook
// from the manager's wrapper.
func removeEntireKeepCopy(root *os.Root, keep string) error {
	if !carriesEntireMarker(root, keep) {
		return nil
	}
	if err := root.Remove(keep); err != nil {
		return fmt.Errorf("remove %s, which holds Entire's hook: %w", keep, err)
	}
	return nil
}

// refreshKeepCopy updates moved's keep copy to the user's hook now in moved,
// so an edit the user made there survives the manager's next install moving
// Entire's hook onto moved. It touches only a keep copy Entire made, and only
// while moved holds a hook that is not Entire's.
func refreshKeepCopy(root *os.Root, moved string) error {
	keep := moved + backupSuffix
	if !hookFileExists(root, keep) || !hookFileExists(root, moved) ||
		carriesEntireMarker(root, moved) || sameHookFile(root, moved, keep) {
		return nil
	}
	return replaceWithOwnCopy(root, moved, keep)
}

// noopMovedHook replaces Entire's hook in a file a hook manager's wrapper runs
// by name when there is no user hook to put there. The file stays because the
// wrapper may run it without checking that it exists.
const noopMovedHook = "#!/bin/sh\n# Left by Entire for another hook manager's wrapper to run. It does nothing, and can be deleted once that manager no longer runs it.\nexit 0\n"

// movedHookCopies lists the <hook>.<suffix> files a hook manager other than
// pre-commit may have moved Entire's hook to: not the backup, its older or
// keep copies, or pre-commit's .legacy, which reclaimFromPreCommit owns.
func movedHookCopies(root *os.Root, hook string) ([]string, error) {
	names, err := hookDirNames(root)
	if err != nil {
		return nil, err
	}
	backup := hook + backupSuffix
	var moved []string
	for _, n := range names {
		if !strings.HasPrefix(n, hook+".") || n == backup || strings.HasPrefix(n, backup+".") ||
			strings.HasSuffix(n, backupSuffix) || n == hook+legacySuffix {
			continue
		}
		moved = append(moved, n)
	}
	slices.Sort(moved)
	return moved, nil
}

// runsByName: name is a regular file, not Entire's, that names moved, which is
// how a hook manager's wrapper runs the hook it moved aside.
func runsByName(root *os.Root, name, moved string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	return err == nil && !bytes.Contains(data, []byte(entireHookMarker)) && namesFile(data, moved)
}

// namesFile finds file as a whole file name in data, not inside a longer one:
// commit-msg.old in commit-msg.older or prepare-commit-msg.old is a different
// file, and taking it for the one a wrapper runs would put the user's hook
// where nothing runs it.
func namesFile(data []byte, file string) bool {
	name := []byte(file)
	for i := 0; ; {
		j := bytes.Index(data[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		if (start == 0 || !isFileNameByte(data[start-1])) && (end == len(data) || !isFileNameByte(data[end])) {
			return true
		}
		i = start + 1
	}
}

func isFileNameByte(b byte) bool {
	return b == '.' || isHookNameByte(b)
}

// reclaimFromWrappers is reclaimFromPreCommit for any other hook manager that
// moves the hook it finds to <hook>.<suffix> and puts a wrapper in front that
// runs that file by name. Left alone, the wrapper becomes Entire's backup, so
// Entire's hook runs twice, and the user's hook Entire had backed up is
// rotated to an older copy that nothing runs. Taking the moved copy back for
// the user's hook gives Entire -> wrapper -> the user's hook, each once.
//
// It also repairs what earlier releases left when Entire already chains to
// the wrapper: the moved copy is Entire's hook again, run a second time. The
// user's hook those releases rotated away cannot be told from any other older
// copy, so it stays where it is. Reports the moved copies it changed.
func reclaimFromWrappers(root *os.Root, hook string) (wrapperReclaim, error) {
	var reclaimed wrapperReclaim
	moved, err := movedHookCopies(root, hook)
	if err != nil {
		return reclaimed, err
	}
	backup := hook + backupSuffix
	for _, m := range moved {
		if !carriesEntireMarker(root, m) {
			if err := refreshKeepCopy(root, m); err != nil {
				return reclaimed, err
			}
			continue
		}
		var userHookInBackup bool
		switch {
		case runsByName(root, hook, m):
			userHookInBackup = hookFileExists(root, backup) &&
				!carriesEntireMarker(root, backup) && !runsByName(root, backup, m)
		case carriesEntireMarker(root, hook) && runsByName(root, backup, m):
		default:
			continue
		}
		userHook, err := moveUserHookInto(root, hook, m, userHookInBackup, func() error {
			_, err := writeHookFile(root, m, noopMovedHook)
			return err
		})
		if err != nil {
			return reclaimed, err
		}
		if userHook {
			reclaimed.userHook = append(reclaimed.userHook, m)
		} else {
			reclaimed.placeholder = append(reclaimed.placeholder, m)
		}
	}
	return reclaimed, nil
}

// wrapperReclaim is what reclaimFromWrappers changed: the moved copies that
// now hold the user's hook, and those that now hold noopMovedHook.
type wrapperReclaim struct {
	userHook, placeholder []string
}

func (r wrapperReclaim) changed() bool {
	return len(r.userHook) > 0 || len(r.placeholder) > 0
}

// notice tells the user what reclaimFromWrappers changed for hook. older are
// hook's older backups: with a placeholder in the moved copy, the user's hook
// may be one of them, rotated away by a release before reclaimFromWrappers.
func (r wrapperReclaim) notice(hook string, older []string) string {
	var b strings.Builder
	if len(r.userHook) > 0 {
		fmt.Fprintf(&b, "[entire] Another hook manager had moved Entire's %s hook to %s; Entire's hook runs first again, then that manager's, then your own hook.\n",
			hook, strings.Join(r.userHook, ", "))
	}
	if len(r.placeholder) > 0 {
		fmt.Fprintf(&b, "[entire] Another hook manager had moved Entire's %s hook to %s; Entire's hook runs first again, then that manager's, which now runs a hook that does nothing there.\n",
			hook, strings.Join(r.placeholder, ", "))
		if len(older) > 0 {
			fmt.Fprintf(&b, "[entire] Older copies of your %s hook no longer run: %s\n", hook, strings.Join(older, ", "))
		}
	}
	return b.String()
}

// olderCopiesOf lists hook's older backups; a listing failure yields none.
func olderCopiesOf(root *os.Root, hook string) []string {
	copies, err := olderHookCopies(root)
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(copies, func(c string) bool {
		return !strings.HasPrefix(c, hook+backupSuffix+".")
	})
}

// refreshMovedEntireHooks rewrites copies of Entire's hook that a hook
// manager moved aside (any <hook>.<suffix> carrying Entire's marker, other
// than the backup and its older copies) to content, which never chains. A
// manager's wrapper that runs such a copy becomes Entire's backup on the next
// install, and a copy written before the chain guard existed chains straight
// back to that backup, so commits would recurse forever. It reports the names
// it rewrote.
func refreshMovedEntireHooks(root *os.Root, hook, content string) ([]string, error) {
	names, err := hookDirNames(root)
	if err != nil {
		return nil, err
	}
	backup := hook + backupSuffix
	var rewritten []string
	for _, n := range names {
		if !strings.HasPrefix(n, hook+".") || n == backup || strings.HasPrefix(n, backup+".") {
			continue
		}
		if !carriesEntireMarker(root, n) {
			continue
		}
		written, err := writeHookFile(root, n, content)
		if err != nil {
			return rewritten, err
		}
		if written {
			rewritten = append(rewritten, n)
		}
	}
	return rewritten, nil
}

// restoreMovedHooks undoes, on uninstall, what hook managers and Entire's
// reclaims left in copies of hook: pre-commit's .legacy, then every copy
// another manager's wrapper runs. Both run, since both managers may have moved
// Entire's hook, and a copy left holding it runs on after uninstall.
func restoreMovedHooks(root *os.Root, hook string) error {
	if err := restoreLegacy(root, hook); err != nil {
		return err
	}
	return restoreWrapped(root, hook)
}

// restoreLegacy undoes reclaimFromPreCommit on uninstall: .legacy gets the
// user's hook back if pre-commit moved Entire's onto it, and the keep copy goes.
func restoreLegacy(root *os.Root, hook string) error {
	legacy, keep, backup := hook+legacySuffix, hook+keepSuffix, hook+backupSuffix
	keepExists := hookFileExists(root, keep)
	if carriesEntireMarker(root, legacy) {
		if !keepExists {
			// pre-commit installed over Entire before any reclaim: the user's
			// hook is still in .pre-entire, which pre-commit does not run, so
			// it goes to .legacy, which it does.
			if isPreCommitWrapper(root, hook, hook) && hookFileExists(root, backup) &&
				!carriesEntireMarker(root, backup) && !isPreCommitWrapper(root, backup, hook) {
				return root.Rename(backup, legacy) //nolint:wrapcheck // caller adds the hook name
			}
			return root.Remove(legacy) //nolint:wrapcheck // caller adds the hook name
		}
		return root.Rename(keep, legacy) //nolint:wrapcheck // caller adds the hook name
	}
	if keepExists && sameHookFile(root, keep, legacy) {
		return root.Remove(keep) //nolint:wrapcheck // caller adds the hook name
	}
	return nil
}

// restoreWrapped undoes reclaimFromWrappers on uninstall: each file a hook
// manager's wrapper runs by name gets the user's hook back if the manager
// moved Entire's onto it, and the keep copy goes. A moved copy of Entire's
// hook that no install has reclaimed yet becomes noopMovedHook, so the wrapper
// the caller restores does not go on running Entire's hook.
func restoreWrapped(root *os.Root, hook string) error {
	moved, err := movedHookCopies(root, hook)
	if err != nil {
		return err
	}
	backup := hook + backupSuffix
	for _, m := range moved {
		keep := m + backupSuffix
		if err := removeEntireKeepCopy(root, keep); err != nil {
			return err
		}
		keepExists := hookFileExists(root, keep)
		switch {
		case carriesEntireMarker(root, m) && keepExists:
			err = root.Rename(keep, m)
		case carriesEntireMarker(root, m) && runsByName(root, hook, m):
			// The manager installed over Entire before any reclaim: the user's
			// hook is still in .pre-entire, which the wrapper does not run.
			if hookFileExists(root, backup) && !carriesEntireMarker(root, backup) && !runsByName(root, backup, m) {
				err = root.Rename(backup, m)
			} else {
				_, err = writeHookFile(root, m, noopMovedHook)
			}
		case carriesEntireMarker(root, m) && carriesEntireMarker(root, hook) && runsByName(root, backup, m):
			// An earlier release already chained to the wrapper, which the
			// caller restores to the hook path. The user's hook it rotated away
			// cannot be told from any other older copy.
			_, err = writeHookFile(root, m, noopMovedHook)
		case keepExists && sameHookFile(root, keep, m):
			err = root.Remove(keep)
		}
		if err != nil {
			return fmt.Errorf("restore %s: %w", m, err)
		}
	}
	return nil
}
