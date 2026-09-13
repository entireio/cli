package gitrepo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Empty-index / empty-tree detection: the last line of defence for issue #2111.
//
// Git treats a `.git/index` that cannot be opened because it does not exist —
// ENOENT, and ONLY ENOENT; every other errno calls die_errno — as an *empty*
// index. A `git commit` that reads the index in that state records the empty
// tree, exits 0, and prints no warning: a commit that deletes every tracked
// file. go-git v6 has the same rule (storage/filesystem.IndexStorage.Index
// returns a zero-entry index on os.ErrNotExist), so neither implementation will
// ever raise this for us.
//
// The window opens whenever some process replaces `.git/index` by rename while
// another reads it, on a filesystem where rename-over-existing is not atomic
// against a concurrent lookup — Docker Desktop / virtiofs / gRPC-FUSE bind
// mounts, i.e. devcontainers, measured at 9.9% of opens during continuous
// replacement versus 0 on ext4. Entire stopped contributing one such write in
// #2143 (`git status` without --no-optional-locks), but the writers Entire does
// not control — the user's own `git status`, another tool's, a file watcher's,
// N agents each running their own hooks — keep the window open. Detection is
// what is left.
//
// Everything here is read-only. It never blocks a commit, never rewrites an
// index, and never returns an error to a hook: the contract that Entire cannot
// fail a user's commit is unchanged. What it removes is the silence.

const (
	// indexSignature is the 4-byte magic at the start of a git index file.
	indexSignature = "DIRC"

	// indexHeaderLen is the fixed header: signature, version, entry count,
	// each 4 bytes big-endian.
	indexHeaderLen = 12

	// indexLinkExtension marks a split index. Its entries live in a shared
	// index file, so a zero entry count in this file says nothing about how
	// much is staged. Extensions begin immediately after the header when the
	// entry count is zero, which is the only case this file needs to read.
	indexLinkExtension = "link"

	// hazardSurvivorSample is how many still-present paths the warning names.
	hazardSurvivorSample = 3

	// hazardSurvivorScanLimit bounds the tree walk that looks for a surviving
	// path. A commit that really does delete every tracked file finds nothing,
	// and this runs on the per-commit hook path, so the walk must not scale
	// with repository size. If the first hazardSurvivorScanLimit tracked paths
	// are all gone from disk, the deletion is taken to be real.
	hazardSurvivorScanLimit = 512
)

// ErrNotAnIndexFile reports that a file named as a git index does not carry the
// index format's header. Distinct from "the index is empty": nothing is known
// about what is staged, so no caller may draw a conclusion from it.
var ErrNotAnIndexFile = errors.New("not a git index file")

// IndexRecordsNoEntries reports whether the git index file at path records zero
// entries.
//
// It reads the 12-byte header rather than decoding the index: this runs on
// every commit, and the answer needed is a count. A missing file is NOT
// reported as empty — that is precisely the conflation this package exists to
// break — it is returned as the os.ErrNotExist it is, for the caller to treat
// as "unknown".
//
// A split index (`link` extension) reports false: its entries live in a shared
// index file, so a zero count here is not an empty index.
func IndexRecordsNoEntries(path string) (bool, error) {
	// Stat before open, and require a regular file. An index that is a FIFO, a
	// socket or a device is not an index, and opening a FIFO with no writer
	// blocks forever — this runs on a commit hook, has no deadline of its own,
	// and exists for the filesystem class that hangs. An allowlist, not a list
	// of known-bad types. Stat, not Lstat: git follows a symlinked index, so
	// this resolves the link and judges what it points at. A path swapped
	// between the stat and the open is not defended against here — nothing git
	// or any tool does produces it, and git opens the same path first.
	info, err := os.Stat(path) //nolint:gosec // path comes from GIT_INDEX_FILE or the resolved git dir
	if err != nil {
		return false, err //nolint:wrapcheck // callers distinguish os.ErrNotExist and add their own context
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("index at %s is a %s: %w", path, describeIndexMode(info.Mode()), ErrNotAnIndexFile)
	}

	f, err := os.Open(path) //nolint:gosec // path comes from GIT_INDEX_FILE or the resolved git dir
	if err != nil {
		return false, err //nolint:wrapcheck // callers distinguish os.ErrNotExist and add their own context
	}
	defer f.Close()

	// A short header is a malformed index; anything else is an I/O failure and
	// keeps its own error, so a caller's log says which of the two happened.
	var header [indexHeaderLen]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, fmt.Errorf("index %s is shorter than its header: %w", path, ErrNotAnIndexFile)
		}
		return false, fmt.Errorf("read index header of %s: %w", path, err)
	}
	if string(header[0:4]) != indexSignature {
		return false, fmt.Errorf("index header of %s: %w", path, ErrNotAnIndexFile)
	}
	if version := binary.BigEndian.Uint32(header[4:8]); version < 2 || version > 4 {
		return false, fmt.Errorf("index version %d in %s: %w", version, path, ErrNotAnIndexFile)
	}
	if binary.BigEndian.Uint32(header[8:12]) > 0 {
		return false, nil
	}

	// Zero entries. With no entries to skip, any extension block starts at the
	// end of the header, so one more 4-byte read settles whether this is a
	// split index. A short read here means there are no extensions, only the
	// trailing checksum — a genuinely empty index.
	//
	// Only the FIRST extension is inspected, which is sound because git writes
	// `link` first (write_split_index in read-cache.c) and measured across
	// every zero-entry index git 2.54 produces the four bytes are `link`,
	// `TREE`, or trailing checksum bytes. A future git that reorders extensions
	// would make this over-report empty, i.e. warn where it should not — the
	// direction that costs a message rather than a miss.
	var extension [4]byte
	if _, err := io.ReadFull(f, extension[:]); err == nil && string(extension[:]) == indexLinkExtension {
		return false, nil
	}
	return true, nil
}

// describeIndexMode names a non-regular index for the message that refuses it.
func describeIndexMode(mode os.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&os.ModeNamedPipe != 0:
		return "named pipe"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeDevice != 0:
		return "device"
	default:
		return mode.Type().String()
	}
}

// hazardPhase says whether the commit that records no files is still being
// prepared or has already been written.
type hazardPhase int

const (
	hazardBeforeCommit hazardPhase = iota
	hazardAfterCommit
)

// EmptyTreeHazard is a commit that records no files at all while the files it
// removes are still present in the worktree.
//
// The two halves are what make it a hazard rather than an intention: a user who
// means to stop tracking everything has usually deleted the files too, and a
// user who means to delete the files has deleted them. Files on disk and no
// files in the commit is the signature of the index having been read as empty
// when it was not.
type EmptyTreeHazard struct {
	phase hazardPhase

	// Commit is the abbreviated hash of the offending commit, empty before the
	// commit exists.
	Commit string

	// SurvivingPaths are up to hazardSurvivorSample paths the commit removes
	// that are still present in the worktree.
	SurvivingPaths []string
}

// DetectEmptyIndexHazard reports the hazard from inside a commit hook, before
// the commit object exists, or nil when there is nothing to report.
//
// indexPath must be the index git is committing from — the value git exports as
// GIT_INDEX_FILE to pre-commit, prepare-commit-msg and commit-msg. That file is
// authoritative by the time prepare-commit-msg runs: git reads the index in
// prepare_index(), re-reads it after the pre-commit hook (so a pre-commit hook
// that stages a file does affect the commit — measured), and from there on
// writes the commit's tree from that in-core copy. Nothing after
// prepare-commit-msg can change it: restoring a good index from a commit-msg
// hook was tried and the empty tree was still committed. So there is no
// time-of-check/time-of-use gap left between what this sees and what lands.
//
// The 12-byte index-header read comes first and the repository is opened only
// if it says zero entries, so a normal commit pays one open(2) and a 16-byte
// read for this.
//
// Fails silent in every direction it cannot resolve: no index path, an
// unreadable index, an unopenable repository, an unborn or unreadable HEAD, a
// HEAD that records no files of its own. Each of those means the check cannot
// conclude anything, and this runs on a path that must never make noise it
// cannot justify.
func DetectEmptyIndexHazard(ctx context.Context, worktreeRoot, indexPath string) *EmptyTreeHazard {
	logCtx := logging.WithComponent(ctx, "checkpoint")

	if indexPath == "" || worktreeRoot == "" {
		return nil
	}
	if !filepath.IsAbs(indexPath) {
		// Git runs hooks from the top of the worktree and names the index
		// relative to it.
		indexPath = filepath.Join(worktreeRoot, indexPath)
	}

	empty, err := IndexRecordsNoEntries(indexPath)
	if err != nil {
		logging.Debug(logCtx, "empty-index guard: index not readable, skipping",
			slog.String("index", indexPath),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if !empty {
		return nil
	}

	repo, err := OpenPath(worktreeRoot)
	if err != nil {
		logging.Debug(logCtx, "empty-index guard: repository not openable, skipping",
			slog.String("worktree_root", worktreeRoot),
			slog.String("error", err.Error()),
		)
		return nil
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		// Unborn HEAD: the first commit of a repository legitimately starts
		// from an empty index.
		return nil
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil
	}

	surviving := survivingPaths(worktreeRoot, tree)
	if len(surviving) == 0 {
		return nil
	}
	return &EmptyTreeHazard{phase: hazardBeforeCommit, SurvivingPaths: surviving}
}

// DetectEmptyTreeCommitHazard reports the hazard from the post-commit hook,
// after the commit has been written, or nil when there is nothing to report.
//
// This is the second net rather than a duplicate of the first: it catches a
// commit written by a path that runs no prepare-commit-msg hook at all, and it
// is the point at which Entire can still decline to act on the commit.
func DetectEmptyTreeCommitHazard(_ context.Context, worktreeRoot string, commit *object.Commit) *EmptyTreeHazard {
	if commit == nil {
		return nil
	}
	tree, err := commit.Tree()
	if err != nil || len(tree.Entries) > 0 {
		return nil
	}
	// A root commit that records no files removes nothing.
	if commit.NumParents() == 0 {
		return nil
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return nil
	}
	parentTree, err := parent.Tree()
	if err != nil || len(parentTree.Entries) == 0 {
		return nil
	}

	surviving := survivingPaths(worktreeRoot, parentTree)
	if len(surviving) == 0 {
		return nil
	}
	return &EmptyTreeHazard{
		phase:          hazardAfterCommit,
		Commit:         shortHash(commit.Hash.String()),
		SurvivingPaths: surviving,
	}
}

// survivingPaths returns up to hazardSurvivorSample paths from tree that still
// exist in the worktree, scanning at most hazardSurvivorScanLimit entries.
//
// The names come from a git tree, which may have been fetched from a remote, so
// they are resolved through the worktreedir root rather than joined onto the
// worktree path — the rule for every read of a working file named by something
// other than Entire.
//
// Lstat, not Stat: the question is whether a path is still present, and a
// dangling symlink is present.
func survivingPaths(worktreeRoot string, tree *object.Tree) []string {
	if worktreeRoot == "" || tree == nil {
		return nil
	}
	root, err := worktreedir.OpenAt(worktreeRoot)
	if err != nil {
		return nil
	}

	found := make([]string, 0, hazardSurvivorSample)
	scanned := 0
	files := tree.Files()
	defer files.Close()
	for {
		file, err := files.Next()
		if err != nil {
			break
		}
		scanned++
		if scanned > hazardSurvivorScanLimit {
			break
		}
		name, err := worktreedir.Name(worktreeRoot, file.Name)
		if err != nil {
			continue
		}
		if _, err := root.Lstat(name); err == nil {
			found = append(found, file.Name)
			if len(found) == hazardSurvivorSample {
				break
			}
		}
	}
	return found
}

func shortHash(hash string) string {
	const abbrev = 7
	if len(hash) > abbrev {
		return hash[:abbrev]
	}
	return hash
}

// Message renders the warning shown to the user.
//
// Both phases can fire for one commit — prepare-commit-msg then post-commit —
// so only the first carries the explanation; the second is the receipt, and
// says what Entire did about it. Both name the recovery, because either may be
// the only one a given commit path reaches.
func (h *EmptyTreeHazard) Message() string {
	var b strings.Builder
	paths := strings.Join(h.SurvivingPaths, ", ")

	switch h.phase {
	case hazardBeforeCommit:
		b.WriteString("\nEntire: WARNING - this commit is about to record an EMPTY tree.\n")
		b.WriteString("  Git read an empty index, so this commit deletes every tracked file - yet\n")
		b.WriteString("  those files are still here: " + paths + "\n")
		b.WriteString("  A concurrent git process can replace .git/index while git reads it, and git\n")
		b.WriteString("  treats an index it cannot find as an empty one: silently, exit code 0. Seen\n")
		b.WriteString("  on Docker Desktop / virtiofs / gRPC-FUSE bind mounts, i.e. dev containers.\n")
	case hazardAfterCommit:
		b.WriteString("\nEntire: WARNING - the commit you just made (" + h.Commit + ") records an EMPTY tree.\n")
		b.WriteString("  It deletes every file its parent tracked, yet those files are still here:\n")
		b.WriteString("  " + paths + "\n")
		b.WriteString("  Entire has left this session's checkpoints alone so the undo below is clean.\n")
	}

	b.WriteString("  Undo it:  git reset --mixed HEAD~1      Prevent it:  GIT_OPTIONAL_LOCKS=0\n")
	b.WriteString("  Meant to stop tracking every file? Then nothing is wrong - carry on.\n\n")

	return b.String()
}

// Report writes the warning where the user will see it and records it in the
// log.
//
// Stdout, not stderr: the prepare-commit-msg and post-commit hooks Entire
// installs redirect stderr to /dev/null so a hook failure can never mix into a
// user's git output, and git neither reads nor interprets a git hook's stdout.
// A hook that has no terminal at all (a GUI client, CI, an agent subprocess)
// still leaves the record in the log.
func (h *EmptyTreeHazard) Report(ctx context.Context, out io.Writer) {
	logging.Error(logging.WithComponent(ctx, "checkpoint"),
		"empty-tree commit detected: git read the index as empty while tracked files are present",
		slog.String("phase", h.phaseName()),
		slog.String("commit", h.Commit),
		slog.Any("surviving_paths", h.SurvivingPaths),
	)
	if out == nil {
		return
	}
	fmt.Fprint(out, h.Message())
}

func (h *EmptyTreeHazard) phaseName() string {
	if h.phase == hazardAfterCommit {
		return "after-commit"
	}
	return "before-commit"
}
