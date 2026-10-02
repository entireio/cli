package strategy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// backupAction is what prepareHookBackup did with a foreign hook; the value is
// also the log field.
type backupAction string

const (
	backupNone            backupAction = ""                  // no foreign hook
	backupCreated         backupAction = "created"           // moved to <hook>.pre-entire
	backupRotated         backupAction = "rotated"           // replaced a different backup, which was kept
	backupReplacedSame    backupAction = "replaced_same"     // identical to the backup
	backupKeptAsideLegacy backupAction = "kept_aside_legacy" // pre-commit migration state, see entireLegacyCopy
)

// legacySuffix is where pre-commit keeps a hook it replaces; its wrapper runs
// that copy first ("migration mode").
const legacySuffix = ".legacy"

// rotationStampLayout names older backups; no colons, for Windows.
const rotationStampLayout = "20060102T150405Z"

type hookBackupResult struct {
	Action    backupAction
	OlderCopy string // where the displaced file was kept (backupRotated, backupKeptAsideLegacy)
	Chain     bool   // Entire's hook should call <hook>.pre-entire
	// SelfBackup: <hook>.pre-entire carries Entire's marker, so chaining to it
	// would make the hook call itself; Chain is false.
	SelfBackup bool
}

// prepareHookBackup makes room for Entire's hook at name without losing a
// foreign hook. If a different backup already exists, it is kept as
// <hook>.pre-entire.<timestamp> and the current hook becomes the backup.
//
// Check-then-act, not atomic: concurrent installs (parallel agent turns, linked
// worktrees) can interleave. The marker re-check before each rename and the
// refusal to chain to a marker-carrying backup keep the worst case to an
// unchained install, never a hook that calls itself.
func prepareHookBackup(root *os.Root, name string, now time.Time) (hookBackupResult, error) {
	backupName := name + backupSuffix
	class, err := classifyExistingHook(root, name)
	if err != nil {
		return hookBackupResult{}, &unclassifiedHookError{err: err}
	}

	var res hookBackupResult
	if class == hookForeign {
		switch {
		case !hookFileExists(root, backupName):
			if err := renameIfStillForeign(root, name, backupName); err != nil {
				return hookBackupResult{}, err
			}
			res.Action = backupCreated
		case entireLegacyCopy(root, name):
			// Keep the backup that runs; save the foreign hook beside it rather
			// than overwrite it.
			older, err := freeRotationName(root, backupName, now)
			if err != nil {
				return hookBackupResult{}, err
			}
			if err := renameIfStillForeign(root, name, older); err != nil {
				return hookBackupResult{}, err
			}
			res.Action, res.OlderCopy = backupKeptAsideLegacy, older
		case sameHookFile(root, name, backupName):
			res.Action = backupReplacedSame
		default:
			older, err := freeRotationName(root, backupName, now)
			if err != nil {
				return hookBackupResult{}, err
			}
			if err := root.Rename(backupName, older); err != nil {
				return hookBackupResult{}, fmt.Errorf("failed to keep older backup %s: %w", backupName, err)
			}
			if err := renameIfStillForeign(root, name, backupName); err != nil {
				return hookBackupResult{}, err
			}
			res.Action, res.OlderCopy = backupRotated, older
		}
	}

	if hookFileExists(root, backupName) {
		res.SelfBackup = carriesEntireMarker(root, backupName)
		res.Chain = !res.SelfBackup
	}
	return res, nil
}

// unclassifiedHookError: the existing hook could not be read to tell whether
// it is Entire's, so the install refuses to replace it.
type unclassifiedHookError struct{ err error }

func (e *unclassifiedHookError) Error() string { return e.err.Error() }
func (e *unclassifiedHookError) Unwrap() error { return e.err }

// renameIfStillForeign backs up name unless another process has just installed
// Entire's hook there.
func renameIfStillForeign(root *os.Root, name, backupName string) error {
	if carriesEntireMarker(root, name) {
		return nil
	}
	if err := root.Rename(name, backupName); err != nil {
		return fmt.Errorf("failed to back up %s: %w", name, err)
	}
	return nil
}

// carriesEntireMarker: name is a regular file containing Entire's marker.
func carriesEntireMarker(root *os.Root, name string) bool {
	data, err := osroot.ReadFileNoFollow(root, name)
	return err == nil && strings.Contains(string(data), entireHookMarker)
}

// entireLegacyCopy reports whether pre-commit keeps Entire's hook as
// <hook>.legacy. Rotating would chain that copy into pre-commit's own wrapper,
// and pre-commit then fails every commit ("installed in migration mode").
func entireLegacyCopy(root *os.Root, name string) bool {
	return carriesEntireMarker(root, name+legacySuffix)
}

// sameHookFile: a and b are regular files with identical bytes, or symlinks to
// the same target. Anything else, including unreadable files, is different.
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

// freeRotationName returns <backupName>.<timestamp>, adding -2, -3, … if taken:
// rename silently replaces an existing destination on Unix and Windows.
func freeRotationName(root *os.Root, backupName string, now time.Time) (string, error) {
	base := backupName + "." + now.UTC().Format(rotationStampLayout)
	candidate := base
	for i := 2; i < 1000; i++ {
		if _, err := root.Lstat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
	}
	return "", fmt.Errorf("no free name for an older copy of %s", backupName)
}

// rotatedHookCopies lists older backups of managed hooks in root, sorted.
func rotatedHookCopies(root *os.Root) ([]string, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open hooks dir: %w", err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read hooks dir: %w", err)
	}
	var copies []string
	for _, e := range entries {
		if slices.ContainsFunc(gitHookNames, func(hook string) bool {
			return strings.HasPrefix(e.Name(), hook+backupSuffix+".")
		}) {
			copies = append(copies, e.Name())
		}
	}
	slices.Sort(copies)
	return copies, nil
}

// hooksDisplayDir shows hooksDir relative to the worktree root when inside it.
func hooksDisplayDir(ctx context.Context, hooksDir string) string {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return hooksDir
	}
	rel, err := filepath.Rel(root, hooksDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return hooksDir
	}
	return rel
}

// reportHookBackup tells the user what happened to their hook. Stderr only:
// this also runs on agent turns, and some agents read hook stdout as context or
// JSON. Changes are logged too, since an agent may never show stderr.
func reportHookBackup(ctx context.Context, hook, displayDir string, res hookBackupResult) {
	backupPath := filepath.Join(displayDir, hook+backupSuffix)
	switch res.Action {
	case backupCreated:
		fmt.Fprintf(os.Stderr, "[entire] Your existing %s hook still runs: it was moved to %s and Entire's %s runs first, then yours. Removing Entire (entire disable --uninstall) puts it back.\n",
			hook, backupPath, hook)
	case backupRotated:
		fmt.Fprintf(os.Stderr, "[entire] %s changed since Entire backed it up. The current version now runs after Entire's; the older copy was kept as %s and no longer runs.\n",
			hook, filepath.Join(displayDir, res.OlderCopy))
	case backupKeptAsideLegacy:
		fmt.Fprintf(os.Stderr, "[entire] %s was replaced by another tool while pre-commit keeps Entire's previous hook as %s%s. Your %s still runs; the replacing hook was kept as %s and does not run.\n",
			hook, hook, legacySuffix, backupPath, filepath.Join(displayDir, res.OlderCopy))
	case backupNone, backupReplacedSame:
	}
	if res.Action != backupNone {
		logging.Info(ctx, "git hook backup", slog.String("hook", hook), slog.String("action", string(res.Action)))
	}
	if res.SelfBackup {
		fmt.Fprintf(os.Stderr, "[entire] Warning: %s contains Entire's own hook, so it is not run (it would call itself). Move it aside if it should not be there.\n", backupPath)
		logging.Warn(ctx, "git hook backup carries entire marker; not chaining", slog.String("hook", hook))
	}
}
