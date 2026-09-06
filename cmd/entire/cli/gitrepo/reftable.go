package gitrepo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/storer"
	gogitstorage "github.com/go-git/go-git/v6/storage"
	gitfilesystem "github.com/go-git/go-git/v6/storage/filesystem"
)

// reftableGitTimeout bounds each git plumbing invocation the reftable storer
// makes. Ref reads/writes against the local reftable stack are fast; the
// timeout only guards against a wedged git process.
const reftableGitTimeout = 30 * time.Second

// inspectRepoUsesReftable reports whether the repository at the given git directories
// stores its references using the reftable backend rather than the classic
// loose-files + packed-refs layout.
//
// go-git (through the vendored v6 alpha) has no reftable reader: its filesystem
// storer reads refs from .git/refs, .git/packed-refs and .git/HEAD, none of
// which are authoritative in a reftable repository. Detection lets us route ref
// operations through the git CLI instead (see reftableStorer).
//
// A reftable repository is identified by the presence of a "reftable/"
// directory under either the worktree git dir or the common git dir. Git 2.45+
// creates this directory for `git init --ref-format=reftable` and for
// `git refs migrate --ref-format=reftable`. Checking the directory avoids
// parsing config and works for linked worktrees, where the shared stack lives
// under the common git dir.
func inspectRepoUsesReftable(dotGitPath, commonGitPath string) (bool, error) {
	candidates := []string{filepath.Join(dotGitPath, "reftable")}
	if commonGitPath != "" && commonGitPath != dotGitPath {
		candidates = append(candidates, filepath.Join(commonGitPath, "reftable"))
	}
	for _, dir := range candidates {
		// Lstat, not Stat: git creates reftable/ as a real directory, so a
		// symlink in its place is not a genuine reftable stack. Lstat inspects
		// the entry itself rather than following the link, so a symlink reports
		// IsDir()==false and is correctly not treated as a reftable repository.
		info, err := os.Lstat(dir)
		if err == nil {
			if info.IsDir() {
				return true, nil
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect reftable directory %s: %w", dir, err)
		}
	}
	return false, nil
}

// reftableStorer adapts a filesystem-backed go-git storage so it can open and
// operate on repositories that use the reftable ref backend. Object, config,
// index, shallow and module storage keep flowing through the embedded
// filesystem storage (reftable only changes ref storage, not object storage);
// the reference-storer methods are overridden to shell out to the git CLI,
// which is the only reftable reader/writer available to us.
//
// It also advertises reftable support via the ExtensionChecker interface so
// go-git's extension verification does not reject the repository on open.
//
// TODO: remove this entire type (and its wiring in repository.go) once go-git
// gains a built-in reftable reader/writer. It exists only because the vendored
// go-git has no reftable backend, so ref operations must shell out to the git
// CLI. When upstream supports reftable natively, the plain filesystem storer
// handles these repositories and this shim can be deleted.
type reftableStorer struct {
	*gitfilesystem.Storage

	gitDir string

	// runGitFn runs a git plumbing command and returns trimmed stdout, raw
	// stderr, and the exec error. It is overridable in tests to simulate
	// spawn/timeout/exit failures deterministically; in production it is nil and
	// runGit dispatches to execGit.
	runGitFn func(args ...string) (string, []byte, error)
}

var (
	_ gogitstorage.Storer = (*reftableStorer)(nil)
	// The concrete methods below satisfy storer.ReferenceStorer, overriding the
	// embedded filesystem implementation.
	_ storer.ReferenceStorer = (*reftableStorer)(nil)
)

// newReftableStorer wraps a filesystem storage with reftable-aware reference
// handling. gitDir must be the repository's git directory (for a linked
// worktree, the worktree's git dir); git resolves the shared reftable stack
// from its commondir automatically.
func newReftableStorer(fs *gitfilesystem.Storage, gitDir string) *reftableStorer {
	return &reftableStorer{Storage: fs, gitDir: gitDir}
}

// SupportsExtension lets go-git open a repository that declares the
// extensions.refstorage=reftable extension, and preserves the embedded
// filesystem storage's support for every other extension it recognises
// (objectformat=sha1/sha256, worktreeconfig).
//
// Defining this method here shadows the promoted *gitfilesystem.Storage
// method, so it must delegate: without the fallback, a reftable repository
// that also declares objectformat=sha256 or worktreeConfig would be rejected
// by go-git's extension verification with ErrUnknownExtension, since it only
// consults the storer's SupportsExtension. Reftable only changes ref storage,
// not object storage, so object-format support is unaffected.
func (s *reftableStorer) SupportsExtension(name, value string) bool {
	if strings.EqualFold(name, "refstorage") {
		return true
	}
	return s.Storage.SupportsExtension(name, value)
}

// runGit runs a git plumbing command scoped to this repository's git dir and
// returns trimmed stdout, raw stderr, and the exec error. Only ref plumbing
// (for-each-ref, symbolic-ref, update-ref, rev-parse) is used, none of which
// trigger git hooks, so this cannot recurse back into entire. Tests may inject
// runGitFn to simulate failures; production dispatches to execGit.
func (s *reftableStorer) runGit(args ...string) (string, []byte, error) {
	if s.runGitFn != nil {
		return s.runGitFn(args...)
	}
	return s.execGit(args...)
}

func (s *reftableStorer) execGit(args ...string) (string, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), reftableGitTimeout)
	defer cancel()

	full := append([]string{"--git-dir", s.gitDir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitPlumbingEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// On our timeout the process is killed and Run reports the kill as an
	// *exec.ExitError, which would be indistinguishable from a genuine
	// non-zero exit. Re-wrap with the context error so callers (refLookupAbsent)
	// never mistake a wedged git for a definitive "ref not found".
	if err != nil && ctx.Err() != nil {
		err = fmt.Errorf("git %s timed out after %s: %w", strings.Join(args, " "), reftableGitTimeout, ctx.Err())
	}
	return strings.TrimRight(stdout.String(), "\n"), stderr.Bytes(), err
}

// gitPlumbingEnv builds the environment for a reftable git plumbing command.
// LC_ALL=C / LANG=C force untranslated (English) diagnostics so the stderr
// classification (isRefCASConflict, and RemoveReference's idempotency check)
// stays correct on a localized machine — git's error messages are i18n'd, and
// matching translated text would silently misclassify CAS conflicts and delete
// failures. GIT_TERMINAL_PROMPT=0 keeps git non-interactive. The forced values
// are appended last so they override anything the caller's environment set
// (os/exec keeps the last value for a duplicate key). Mirrors the sibling
// shell-out in checkpoint/shadow_ref.go.
func gitPlumbingEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
		"LANG=C",
	)
}

// refLookupAbsent reports whether a failed ref lookup (rev-parse --verify
// --quiet, update-ref) means the reference is genuinely absent rather than a
// failure to consult git at all. Only genuine absence may map to the
// plumbing.ErrReferenceNotFound / idempotent-delete sentinels; a spawn failure,
// timeout, or I/O error must be surfaced so a transient git failure is never
// mistaken for a missing ref (which would let the strategy orphan a checkpoint
// ref or drop a link).
//
// git ran and reported absence iff it exited non-zero AND stayed silent: under
// --quiet a missing ref produces no error output, whereas a fatal/I/O failure
// exits non-zero with a "fatal: ..." message, and a spawn failure or our
// timeout is not an *exec.ExitError at all.
func refLookupAbsent(err error, stderr []byte) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return len(bytes.TrimSpace(stderr)) == 0
}

// SetReference stores a reference, dispatching to symbolic-ref for symbolic
// references and update-ref for hash references.
func (s *reftableStorer) SetReference(ref *plumbing.Reference) error {
	if ref == nil {
		return nil
	}
	if ref.Type() == plumbing.SymbolicReference {
		if _, stderr, err := s.runGit("symbolic-ref", "--end-of-options", ref.Name().String(), ref.Target().String()); err != nil {
			return fmt.Errorf("reftable set symbolic ref %s: %s: %w", ref.Name(), strings.TrimSpace(string(stderr)), err)
		}
		return nil
	}
	if _, stderr, err := s.runGit("update-ref", "--end-of-options", ref.Name().String(), ref.Hash().String()); err != nil {
		return fmt.Errorf("reftable set ref %s: %s: %w", ref.Name(), strings.TrimSpace(string(stderr)), err)
	}
	return nil
}

// CheckAndSetReference performs a compare-and-swap update. When old is non-nil
// the update is conditioned on the current value, mirroring go-git's atomic
// semantics; a genuine mismatch is reported as storage.ErrReferenceHasChanged.
//
// Only a real compare-and-swap conflict maps to that sentinel. Callers such as
// strategy.atomicSetV1Ref treat ErrReferenceHasChanged as "another worktree
// advanced the ref" and abort the push as a concurrency event, while wrapping
// every other error as a genuine failure. Mapping an unrelated failure (bad
// object, invalid ref name, lock contention, timeout, git spawn failure) to the
// conflict sentinel would misreport a storage error as a benign race, so those
// are surfaced as themselves.
func (s *reftableStorer) CheckAndSetReference(newRef, old *plumbing.Reference) error {
	if newRef == nil {
		return nil
	}
	if newRef.Type() == plumbing.SymbolicReference {
		// Symbolic refs (e.g. HEAD) have no CAS form in update-ref; set directly.
		return s.SetReference(newRef)
	}
	if old == nil {
		return s.SetReference(newRef)
	}
	_, stderr, err := s.runGit("update-ref", "--end-of-options", newRef.Name().String(), newRef.Hash().String(), old.Hash().String())
	if err == nil {
		return nil
	}
	if isRefCASConflict(stderr) {
		return gogitstorage.ErrReferenceHasChanged
	}
	return fmt.Errorf("reftable CAS ref %s: %s: %w", newRef.Name(), strings.TrimSpace(string(stderr)), err)
}

// isRefCASConflict reports whether git update-ref stderr indicates a
// compare-and-swap conflict: the stored value was not the expected old value
// ("... is at X but expected Y"), or a create-if-absent update found the ref
// already present ("reference already exists"). These are the only failures
// that mean the reference changed concurrently. Object/name/lock/spawn errors
// carry different messages and must not be misclassified as conflicts.
func isRefCASConflict(stderr []byte) bool {
	msg := strings.ToLower(string(stderr))
	return strings.Contains(msg, "but expected") ||
		strings.Contains(msg, "reference already exists")
}

// Reference returns the reference with the given name, preserving symbolic refs
// (such as HEAD) so go-git can resolve them itself.
func (s *reftableStorer) Reference(name plumbing.ReferenceName) (*plumbing.Reference, error) {
	// Symbolic refs: symbolic-ref exits 0 and prints the target only for a
	// genuine symbolic ref; -q makes it exit non-zero silently for a non-symbolic
	// name. Classify the probe failure the same way as elsewhere: a genuine "not
	// a symbolic ref" (exit non-zero, empty stderr) falls through to the hash
	// lookup, but a spawn/timeout/I-O failure is surfaced rather than silently
	// downgrading a symbolic ref (e.g. HEAD on a branch) to a Hash reference
	// named "HEAD", which would make callers read the repo as detached.
	target, symStderr, symErr := s.runGit("symbolic-ref", "-q", "--end-of-options", name.String())
	switch {
	case symErr == nil && target != "":
		return plumbing.NewSymbolicReference(name, plumbing.ReferenceName(target)), nil
	case symErr != nil && !refLookupAbsent(symErr, symStderr):
		return nil, fmt.Errorf("reftable probe symbolic ref %s: %s: %w", name, strings.TrimSpace(string(symStderr)), symErr)
	}

	// Hash refs: rev-parse --verify resolves the ref to the object it points at.
	// "^0" would peel tags; we want the ref's direct target, so verify the name
	// as-is. A non-existent ref exits non-zero silently.
	out, stderr, err := s.runGit("rev-parse", "--verify", "--quiet", "--end-of-options", name.String())
	if err != nil {
		if refLookupAbsent(err, stderr) {
			return nil, plumbing.ErrReferenceNotFound
		}
		return nil, fmt.Errorf("reftable resolve ref %s: %s: %w", name, strings.TrimSpace(string(stderr)), err)
	}
	if out == "" {
		return nil, plumbing.ErrReferenceNotFound
	}
	h := plumbing.NewHash(out)
	if h.IsZero() {
		return nil, plumbing.ErrReferenceNotFound
	}
	return plumbing.NewHashReference(name, h), nil
}

// IterReferences returns an iterator over every reference in the repository,
// including HEAD, matching the behaviour of the filesystem storer.
func (s *reftableStorer) IterReferences() (storer.ReferenceIter, error) {
	refs := make([]*plumbing.Reference, 0, 16)

	// HEAD (symbolic on a branch, detached hash otherwise) is not emitted by
	// for-each-ref, so resolve it explicitly first, classifying each probe:
	// a genuine "not symbolic" (exit non-zero, empty stderr) means HEAD is
	// detached, so resolve it as a hash; a spawn/timeout/I-O failure is surfaced
	// rather than silently dropping HEAD or downgrading a symbolic HEAD to a
	// hash; and a genuinely absent HEAD (unborn/empty repo) is omitted, matching
	// the filesystem storer.
	headTarget, headSymStderr, headSymErr := s.runGit("symbolic-ref", "-q", "--end-of-options", "HEAD")
	switch {
	case headSymErr == nil && headTarget != "":
		refs = append(refs, plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.ReferenceName(headTarget)))
	case headSymErr != nil && !refLookupAbsent(headSymErr, headSymStderr):
		return nil, fmt.Errorf("reftable probe HEAD symbolic ref: %s: %w", strings.TrimSpace(string(headSymStderr)), headSymErr)
	default:
		// HEAD is detached or genuinely not symbolic: resolve it as a hash.
		out, stderr, headErr := s.runGit("rev-parse", "--verify", "--quiet", "--end-of-options", "HEAD")
		switch {
		case headErr == nil && out != "":
			if h := plumbing.NewHash(out); !h.IsZero() {
				refs = append(refs, plumbing.NewHashReference(plumbing.HEAD, h))
			}
		case headErr != nil && !refLookupAbsent(headErr, stderr):
			return nil, fmt.Errorf("reftable resolve HEAD: %s: %w", strings.TrimSpace(string(stderr)), headErr)
		}
	}

	// All refs under refs/. %(symref) is non-empty only for symbolic refs so we
	// preserve their symbolic nature (e.g. refs/remotes/origin/HEAD).
	out, stderr, err := s.runGit("for-each-ref", "--format=%(objectname) %(refname) %(symref)")
	if err != nil {
		return nil, fmt.Errorf("reftable iterate refs: %s: %w", strings.TrimSpace(string(stderr)), err)
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			continue
		}
		objectName, refName := fields[0], fields[1]
		symref := ""
		if len(fields) == 3 {
			symref = strings.TrimSpace(fields[2])
		}
		if symref != "" {
			refs = append(refs, plumbing.NewSymbolicReference(plumbing.ReferenceName(refName), plumbing.ReferenceName(symref)))
			continue
		}
		h := plumbing.NewHash(objectName)
		if h.IsZero() {
			continue
		}
		refs = append(refs, plumbing.NewHashReference(plumbing.ReferenceName(refName), h))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reftable scan refs: %w", err)
	}

	return storer.NewReferenceSliceIter(refs), nil
}

// RemoveReference deletes a reference. Deleting a missing ref is treated as a
// success so callers can remove idempotently, matching go-git's semantics.
func (s *reftableStorer) RemoveReference(name plumbing.ReferenceName) error {
	// A symbolic ref must be deleted with symbolic-ref -d; update-ref -d on a
	// symbolic ref deletes the ref it points at instead (e.g. update-ref -d HEAD
	// deletes the current branch). The symbolic-ref -q probe reports absence the
	// same way as a lookup — exit non-zero with empty stderr — so classify its
	// failure: only a genuine "not a symbolic ref / not found" may fall through
	// to update-ref -d. A spawn/timeout/I-O failure must be surfaced rather than
	// assumed non-symbolic, or a transient error would be routed into a
	// destructive delete of the wrong ref.
	target, probeStderr, probeErr := s.runGit("symbolic-ref", "-q", "--end-of-options", name.String())
	switch {
	case probeErr == nil && target != "":
		if _, stderr, delErr := s.runGit("symbolic-ref", "-d", "--end-of-options", name.String()); delErr != nil {
			return fmt.Errorf("reftable remove symbolic ref %s: %s: %w", name, strings.TrimSpace(string(stderr)), delErr)
		}
		return nil
	case probeErr != nil && !refLookupAbsent(probeErr, probeStderr):
		return fmt.Errorf("reftable probe symbolic ref %s: %s: %w", name, strings.TrimSpace(string(probeStderr)), probeErr)
	}

	// name is not a symbolic ref (or does not exist): delete it as a hash ref.
	_, stderr, err := s.runGit("update-ref", "-d", "--end-of-options", name.String())
	if err == nil {
		return nil
	}
	// git update-ref exits 0 when deleting an already-absent ref, so reaching
	// here means the delete actually failed. Only an explicit "does not exist"
	// is idempotent success; an empty stderr must NOT be swallowed, because a
	// killed/timed-out git also produces no stderr and would otherwise be
	// silently reported as a successful deletion.
	msg := strings.ToLower(strings.TrimSpace(string(stderr)))
	if strings.Contains(msg, "does not exist") || strings.Contains(msg, "not exist") {
		return nil
	}
	return fmt.Errorf("reftable remove ref %s: %s: %w", name, strings.TrimSpace(string(stderr)), err)
}

// CountLooseRefs returns 0: reftable has no loose refs, and go-git only uses
// this count to decide whether to pack loose refs, which is a no-op here.
func (s *reftableStorer) CountLooseRefs() (int, error) {
	return 0, nil
}

// PackRefs is a no-op: the reftable backend maintains its own compaction, so
// there is nothing for go-git to pack.
func (s *reftableStorer) PackRefs() error {
	return nil
}
