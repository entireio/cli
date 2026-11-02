package strategy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// worktreeIncarnationFile names the marker Entire keeps in a linked worktree's
// git admin directory (<common>/worktrees/<id>/). Git derives a worktree's ID
// from its directory name and reuses it once the old worktree is removed, so
// the ID alone can't tell a worktree from a later one of the same name. The
// admin directory has exactly the lifetime needed: `git worktree move` and
// `repair` keep it, `remove`, `prune` and `add -f` over a missing worktree
// delete it. A record of edits carries the marker it was made under, and
// counts only while the worktree still holds it.
const worktreeIncarnationFile = "entire-incarnation"

// errMalformedMarker is a marker Entire didn't write: anything other than 32
// hex digits.
var errMalformedMarker = errors.New("malformed worktree marker")

// worktreeCommonDirs caches each worktree root's git common dir for the
// process: a hook checks several records against the same worktree, and the
// answer, a `git rev-parse` away, doesn't change while it runs.
var worktreeCommonDirs sync.Map

// worktreeIncarnation returns the marker of the linked worktree at root, whose
// git ID is worktreeID, creating one when create is set and there is none. The
// main worktree (ID "") can't be removed and re-added, and has none: it
// returns "".
func worktreeIncarnation(ctx context.Context, root, worktreeID string, create bool) (string, error) {
	if worktreeID == "" {
		return "", nil
	}
	admin, _, closeAdmin, err := openWorktreeAdmin(ctx, root, worktreeID)
	if err != nil {
		return "", err
	}
	defer closeAdmin()

	marker, err := readIncarnation(admin)
	if err == nil || !create || !errors.Is(err, fs.ErrNotExist) {
		return marker, err
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate worktree marker: %w", err)
	}
	if err := installIncarnation(admin, hex.EncodeToString(buf[:])); err != nil {
		return "", err
	}
	return readIncarnation(admin)
}

// openWorktreeAdmin opens the git admin directory of the linked worktree at
// root, whose git ID is worktreeID, and returns it with its path (for
// resolving paths git records relative to it, never for I/O).
func openWorktreeAdmin(ctx context.Context, root, worktreeID string) (admin *os.Root, adminPath string, closeAdmin func(), err error) {
	// The ID comes from the worktree's .git file: data, not a resolved path.
	if worktreeID == "" || worktreeID == "." || worktreeID == ".." || strings.ContainsAny(worktreeID, "/\\\x00") {
		return nil, "", nil, fmt.Errorf("invalid worktree id %q", worktreeID)
	}
	// Resolved from root itself, not the process's directory or root's .git
	// file: the admin directory is the one git uses for root.
	commonDir, cached := worktreeCommonDirs.Load(root)
	if !cached {
		resolved, err := gitCommonDirForWorktree(ctx, root)
		if err != nil {
			return nil, "", nil, err
		}
		commonDir, _ = worktreeCommonDirs.LoadOrStore(root, resolved)
	}
	commonPath, ok := commonDir.(string)
	if !ok {
		return nil, "", nil, errors.New("cached git common dir is not a path")
	}
	common, err := gitdir.OpenAt(commonPath)
	if err != nil {
		return nil, "", nil, fmt.Errorf("open git common dir: %w", err)
	}
	// Not a shared root: remove + re-add replaces this directory, and a cached
	// handle would keep reading the old one.
	admin, closeAdmin, err = osroot.OpenDirNoSymlinks(common, "worktrees/"+worktreeID)
	if err != nil {
		return nil, "", nil, fmt.Errorf("open worktree admin dir: %w", err)
	}
	return admin, filepath.Join(commonPath, "worktrees", worktreeID), closeAdmin, nil
}

// installIncarnation writes marker unless admin already has one, in which
// case a concurrent hook won and its marker stands. The marker is written to
// a temp file and hard-linked into place, so a reader never sees it partly
// written; where the filesystem has no hard links it is created in place
// exclusively, and a reader that catches it partly written reads it as
// malformed, which counts as no match until the write completes.
func installIncarnation(admin *os.Root, marker string) error {
	tmp, tmpName, err := jsonutil.CreateTempIn(admin, worktreeIncarnationFile)
	if err != nil {
		return fmt.Errorf("create worktree marker: %w", err)
	}
	defer func() { _ = admin.Remove(tmpName) }() //nolint:errcheck // best-effort; the link leaves this name behind either way
	if err := writeAndClose(tmp, marker); err != nil {
		return fmt.Errorf("write worktree marker: %w", err)
	}
	linkErr := admin.Link(tmpName, worktreeIncarnationFile)
	if linkErr == nil || errors.Is(linkErr, fs.ErrExist) {
		return nil
	}
	f, err := admin.OpenFile(worktreeIncarnationFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("install worktree marker: %w", errors.Join(linkErr, err))
	}
	if err := writeAndClose(f, marker); err != nil {
		return fmt.Errorf("write worktree marker: %w", err)
	}
	return nil
}

func writeAndClose(f *os.File, data string) error {
	_, err := f.WriteString(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

// MarkWorktree returns the marker a record of edits in the worktree at root
// (git ID worktreeID) is made under, creating it if the worktree has none.
// The main worktree's is "".
func MarkWorktree(ctx context.Context, root, worktreeID string) (string, error) {
	return worktreeIncarnation(ctx, root, worktreeID, true)
}

// readIncarnation reads the marker in admin, refusing a symlink or anything
// other than the 32 hex digits worktreeIncarnation writes.
func readIncarnation(admin *os.Root) (string, error) {
	data, err := osroot.ReadFileNoFollow(admin, worktreeIncarnationFile)
	if err != nil {
		return "", err //nolint:wrapcheck // callers test for fs.ErrNotExist
	}
	marker := string(data)
	if decoded, err := hex.DecodeString(marker); err != nil || len(decoded) != 16 {
		return "", errMalformedMarker
	}
	return marker, nil
}

// worktreeEntryCurrent reports whether work, a session's record for the
// worktree at root (keyed key), was made in that worktree as it is now rather
// than in an earlier one git has since removed and given the same ID.
//
// err is set when that can't be told (git or the filesystem failed); current
// is then false, since missing a link costs less than linking someone else's
// commit, but the record isn't known to be stale either, so nothing may drop
// it. A record without a marker, or a worktree with none, a malformed one, or
// a symlink in its place, is known not to match.
func worktreeEntryCurrent(ctx context.Context, root, key string, work *session.WorktreeWork) (current bool, err error) {
	if work == nil {
		return false, nil
	}
	if key == session.WorktreeKey("") {
		return true, nil
	}
	if work.Incarnation == "" {
		return false, nil
	}
	marker, err := worktreeIncarnation(ctx, root, key, false)
	switch {
	case err == nil:
		return marker == work.Incarnation, nil
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, errMalformedMarker), errors.Is(err, osroot.ErrSymlinkedPath):
		return false, nil
	default:
		return false, err
	}
}

// worktreeEntryLinks is worktreeEntryCurrent for deciding whether a record
// may link a commit: only when it is known to be current.
func worktreeEntryLinks(ctx context.Context, root, key string, work *session.WorktreeWork) bool {
	current, err := worktreeEntryCurrent(ctx, root, key, work)
	return current && err == nil
}
