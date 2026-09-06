package paths

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The four ways validating `.entire` can fail. Each is identified positively
// and carries a different remedy, which is why they are separate sentinels
// rather than one error plus an else branch: callers print the fix, and telling
// someone to reinstall git because their filesystem returned EACCES sends them
// after the wrong thing. A caller matching none of these must offer no remedy
// rather than guess at one.
var (
	// ErrEntireDirNotDirectory reports that `.entire` exists and is not a real
	// directory. The remedy is to inspect and replace the path.
	ErrEntireDirNotDirectory = errors.New("not a directory")

	// ErrEntireDirUnsupportedEntry reports that an entry directly under
	// `.entire` is neither a regular file nor a directory. The remedy is to
	// inspect that entry and replace it, which is the same shape as
	// ErrEntireDirNotDirectory's but not the same sentence:
	// `.entire/settings.json` is not required to be a directory, so telling
	// someone it is not one names the wrong problem.
	ErrEntireDirUnsupportedEntry = errors.New("not a regular file or directory")

	// ErrEntireDirUnreadable reports that `.entire` could not be inspected at
	// all — a permission failure, an I/O error, a dead mount. Nothing is known
	// about what is at the path. The remedy is ownership, permissions, or the
	// filesystem itself.
	ErrEntireDirUnreadable = errors.New("cannot be inspected")

	// ErrRepositoryUnresolved reports that the worktree root could not be
	// determined for a reason other than there being no repository, so there is
	// no `.entire` path to inspect yet. The remedy is git.
	ErrRepositoryUnresolved = errors.New("cannot determine which repository this directory belongs to")
)

// ValidateEntireDirAt reports whether worktreeRoot's `.entire` is safe to read
// and write through. It is safe when the path is absent (Entire is not enabled
// here yet, or `enable` is about to create it), or is a real directory whose own
// entries are real files and directories. Anything else is a broken repo and
// the caller must not touch the path.
//
// The stat is Lstat, not Stat, so a symlink is rejected even when it points at
// a perfectly good directory. `.entire` holds session metadata, transcripts,
// and the settings that decide what gets redacted before it is committed, so a
// path someone else controls the far end of is not a path we write through.
//
// The same reasoning covers one level down, so the entries directly inside are
// checked too, and there the rule is an allowlist: Entire only ever creates
// regular files and directories under `.entire`, so anything else arrived some
// other way. A symlinked `.entire/metadata` redirects transcripts, and a
// symlinked `.entire/settings.local.json` redirects the file that names the
// command Entire executes at pre-push. See validateEntireDirEntries for why the
// scan stops there, and unsupportedEntryType for the one type it tolerates.
//
// The settings package refuses a symlinked settings file at the read itself as
// well (readConfined). Neither check subsumes the other: this one stops a
// command before it does anything, and covers the subdirectories no settings
// read touches, while that one covers the many callers that reach settings.Load
// without passing through a command's pre-run.
//
// A stat error other than "not exist" is also a failure. It is not evidence
// that the invariant is violated, but neither is it evidence that it holds, and
// the caller's next move is to write there.
func ValidateEntireDirAt(worktreeRoot string) error {
	path := filepath.Join(worktreeRoot, EntireDir) // entire-join-ok: validation probes the literal worktree dir by design

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%s %w: %w", path, ErrEntireDirUnreadable, err)
	case !info.Mode().IsDir():
		return fmt.Errorf("%s is %s, %w", path, describeMode(info.Mode()), ErrEntireDirNotDirectory)
	}

	return validateEntireDirEntries(path)
}

// validateEntireDirEntries rejects anything other than a regular file or a
// directory sitting directly inside a `.entire` already established to be a
// real directory.
//
// The entries one level down are Entire's own — `logs`, `metadata`, `tmp`, and
// the two settings files — so the reasoning that rejects a symlinked `.entire`
// applies to them unchanged: the far end is outside Entire's control, and for
// the settings files it decides what may be committed.
//
// Deliberately not recursive. Walking deeper would mean traversing every
// session's transcripts on every command, and the checkpoint writer already
// skips symlinks as it walks the metadata directory.
func validateEntireDirEntries(dir string) error {
	entries, err := os.ReadDir(dir)

	// Checked before err, not after. os.ReadDir returns what it managed to read
	// alongside a partial-read error, and an unsupported entry among those is a
	// positive finding — a stronger statement than "the listing failed", and
	// one with an actionable remedy.
	if entryErr := firstUnsupportedEntry(dir, entries); entryErr != nil {
		return entryErr
	}
	if err != nil {
		return fmt.Errorf("%s %w: %w", dir, ErrEntireDirUnreadable, err)
	}
	return nil
}

// firstUnsupportedEntry names the first entry that is neither a regular file
// nor a directory and counts the rest, or returns nil when there is none.
//
// One error naming one entry, rather than one per entry: the remedy is the same
// for all of them, and a user who has to rerun the command once per planted
// entry pays for our formatting choice. The named entry is the first in
// os.ReadDir's sorted order, so the message is deterministic.
//
// No Lstat of our own. DirEntry.Type() comes from the directory read itself on
// the platforms that report a type there, and where the filesystem does not,
// os.ReadDir does the Lstat internally — skipping an entry that vanished
// between the read and the stat, and surfacing any other failure as the
// partial-read error validateEntireDirEntries reports. Adding an Lstat here
// would reintroduce the vanished-entry race that os.ReadDir already handles.
func firstUnsupportedEntry(dir string, entries []os.DirEntry) error {
	var first os.DirEntry
	others := 0
	for _, entry := range entries {
		if !unsupportedEntryType(entry.Type()) {
			continue
		}
		if first != nil {
			others++
			continue
		}
		first = entry
	}
	if first == nil {
		return nil
	}

	err := unsupportedEntryError(filepath.Join(dir, first.Name()), first.Type())
	if others > 0 {
		err = fmt.Errorf("%w%s", err, otherUnsupportedClause(others, dir))
	}
	return err
}

// unsupportedEntryType reports whether an entry directly under `.entire` is a
// type Entire never creates there. It is an allowlist — regular files and
// directories are the whole of Entire's own layout — so a mode bit nobody has
// considered yet is refused rather than waved through, which is the safer
// direction for a path holding transcripts and the redaction settings.
//
// fs.ModeIrregular is the deliberate exception, and it has to be an exception
// rather than an allowlist member because Windows overloads it. Go maps every
// reparse tag it has no category for onto that one bit (see the default arm of
// fileStat.mode in os/types_windows.go), which lands NTFS directory junctions
// and OneDrive Files On-Demand placeholders in the same bucket. Refusing the
// bucket would hard-fail every command in a repo inside a synced folder, with a
// remedy the user cannot act on, and the placeholder arrives with nobody
// attacking anything. The junction it would also catch cannot arrive by
// checkout at all — git has no tree-object mode for one — so planting it
// already requires local code execution, at which point this check is not what
// stands in the way. Tolerating the ambiguous bit is the cheaper mistake.
//
// So the bit is masked off rather than matched on, because it does not arrive
// alone. Go only skips ModeDir for a reparse tag that is a name surrogate (the
// !isReparseTagNameSurrogate branch of the same function), and the cloud tags
// are not surrogates, so a placeholder directory reports ModeDir|ModeIrregular
// while a junction, which is a surrogate, reports ModeIrregular by itself.
// `.entire`'s own entries are mostly directories, so demanding the bit stand
// alone would reject `metadata`, `logs`, and `tmp` in exactly the synced folder
// this exception exists to keep working.
//
// What masking does not do is excuse the rest of the field: anything carrying a
// type we reject is rejected whatever else it carries, ModeIrregular included.
//
// The remainder is then compared against the whole type field, fs.FileMode.
// Type(), and not tested with IsRegular and IsDir. Those two examine single
// bits rather than the field: IsDir is `mode&ModeDir != 0`. A mode carrying
// ModeDir alongside ModeSymlink or ModeNamedPipe therefore satisfies IsDir, as
// does the all-bits-set mode os.ReadDir's direntType returns when a type cannot
// be resolved at all. An allowlist a rejected type can enter by also setting an
// accepted bit is not an allowlist. Permission bits and the non-type bits above
// ModeType (setuid, sticky, and friends) sit outside the field, so they do not
// affect the answer, which is what IsRegular already did.
func unsupportedEntryType(mode fs.FileMode) bool {
	t := mode.Type() &^ fs.ModeIrregular
	return t != 0 && t != fs.ModeDir
}

// unsupportedEntryError reports that path is a type Entire does not put under
// `.entire`, naming what was found. A symlink goes through SymlinkedEntryError
// so that the message also names where it points, which is what the user needs
// in order to decide what to do with the far end.
func unsupportedEntryError(path string, mode fs.FileMode) error {
	if mode&fs.ModeSymlink != 0 {
		return SymlinkedEntryError(path)
	}
	return fmt.Errorf("%s is %s, %w", path, describeMode(mode), ErrEntireDirUnsupportedEntry)
}

// SymlinkedEntryError reports that path is a symbolic link, naming the target
// when it can be read. A Readlink that fails leaves the entry named, which is
// still enough to act on.
//
// Exported because this sentence has two producers — the `.entire` entry scan
// here and the settings reader, which refuses a symlinked settings file at the
// read itself (see readConfined in the settings package). One function so the
// two cannot drift into describing the same condition differently.
func SymlinkedEntryError(path string) error {
	if target, err := os.Readlink(path); err == nil {
		return fmt.Errorf("%s is a symbolic link to %s, %w", path, target, ErrEntireDirUnsupportedEntry)
	}
	return fmt.Errorf("%s is a symbolic link, %w", path, ErrEntireDirUnsupportedEntry)
}

// otherUnsupportedClause accounts for unsupported entries beyond the one named.
// Number and verb are built together so they cannot disagree, and the wording
// is type-neutral because the entries it counts need not share the named one's
// type.
func otherUnsupportedClause(n int, dir string) string {
	if n == 1 {
		return fmt.Sprintf(" (and 1 other entry under %s is unsupported too)", dir)
	}
	return fmt.Sprintf(" (and %d other entries under %s are unsupported too)", n, dir)
}

// RequireEntireDir validates the current worktree's `.entire`.
//
// Outside a git repository there is no worktree root and so nothing to
// validate, which is not an error: commands that need a repository report its
// absence themselves, with a message about the repository rather than about
// `.entire`. That skip requires git's positive ErrNotARepository verdict.
//
// Every other discovery failure — git missing from PATH, a cancelled context, a
// permission failure, dubious ownership, malformed output — fails closed. Those
// mean "we could not find out", and the consequence of guessing "no repository"
// is not merely a skipped check: settings resolution falls back to a path
// relative to the current directory when the root will not resolve
// (settingsAbsPaths in the settings package), so a guess would read
// ./.entire/settings.json — through the very symlink this exists to reject.
// Refusing to run on a machine whose git is broken is the cheaper mistake.
//
// Deliberately not memoized. The Lstat and the one-level listing are free next
// to the `git rev-parse` that WorktreeRoot runs — measured 8.2µs against a
// ~millisecond subprocess — and a cached "it was fine" is a stale answer in a
// long-lived process such as `entire mcp`.
func RequireEntireDir(ctx context.Context) error {
	root, err := WorktreeRoot(ctx)
	switch {
	case err == nil:
		return ValidateEntireDirAt(root)
	case errors.Is(err, ErrNotARepository):
		return nil
	default:
		return fmt.Errorf("%w, so %s cannot be verified: %w", ErrRepositoryUnresolved, EntireDir, err)
	}
}

// describeMode names what was found. The sentinel supplies the rest of the
// sentence, so these read as the first half of "X is a symbolic link, not a
// directory" or "X is a named pipe, not a regular file or directory".
func describeMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode.IsRegular():
		return "a regular file"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0:
		return "a device"
	default:
		return "of an unsupported type"
	}
}
