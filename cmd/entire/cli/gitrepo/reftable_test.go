package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	gitstorage "github.com/go-git/go-git/v6/storage"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// initReftableRepo creates a repository using the reftable ref backend via the
// git CLI, or skips the test when the installed git is too old to support it.
// It returns the repo dir and the initial commit hash.
func initReftableRepo(t *testing.T, name, content string) (string, string) {
	t.Helper()
	return initReftableRepoWithFormat(t, "", name, content)
}

// initReftableRepoWithFormat is initReftableRepo with an explicit git object
// format. An empty objectFormat uses git's default (sha1); "sha256" exercises
// the sha256 hash, which additionally makes git write extensions.objectformat
// into the repo config. The test is skipped when the installed git cannot
// initialize the requested reftable + object-format combination.
func initReftableRepoWithFormat(t *testing.T, objectFormat, name, content string) (string, string) {
	t.Helper()
	repoDir := t.TempDir()

	initArgs := []string{"init", "-b", "main", "--ref-format=reftable"}
	if objectFormat != "" {
		initArgs = append(initArgs, "--object-format="+objectFormat)
	}
	initArgs = append(initArgs, repoDir)
	initCmd := exec.Command("git", initArgs...) //nolint:noctx // test capability probe
	initCmd.Env = gitenv.Isolated()
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("git does not support reftable repositories: %v\n%s", err, out)
	}

	git := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(gitenv.Run(t, repoDir, args...))
	}

	if got := git("rev-parse", "--show-ref-format"); got != "reftable" {
		t.Skipf("git initialized ref format %q, not reftable", got)
	}
	if objectFormat != "" {
		if got := git("rev-parse", "--show-object-format"); got != objectFormat {
			t.Skipf("git initialized object format %q, not %q", got, objectFormat)
		}
	}
	git("config", "user.name", "Test User")
	git("config", "user.email", "test@example.com")
	git("config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	git("add", name)
	git("commit", "-m", "initial")

	return repoDir, git("rev-parse", "HEAD")
}

// setRepoConfig sets a local git config key in an existing repo, using an
// isolated global/system config so the developer's real git config is never
// read or written (matching the reftable test helpers).
func setRepoConfig(t *testing.T, repoDir, key, value string) {
	t.Helper()
	gitenv.Run(t, repoDir, "config", key, value)
}

// reftableCommit adds a file and commits it in an existing reftable repo,
// returning the new HEAD hash. The repo's user identity is already configured
// by initReftableRepo, so only an isolated global/system config is supplied.
func reftableCommit(t *testing.T, repoDir, name, content string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	gitenv.Run(t, repoDir, "add", name)
	gitenv.Run(t, repoDir, "commit", "-m", content)
	return strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", "HEAD"))
}

// TestCheckAndSetReference_ConflictVsError verifies that CheckAndSetReference
// maps only a genuine compare-and-swap conflict to storage.ErrReferenceHasChanged
// and surfaces unrelated failures (a new value pointing at a nonexistent object)
// as themselves. Callers such as strategy.atomicSetV1Ref branch on that sentinel
// to decide whether a privacy-critical push aborts because of concurrency or a
// real storage error, so misclassifying an I/O/object failure as a conflict is a
// correctness bug. Regression for the coarse error mapping in #547.
func TestCheckAndSetReference_ConflictVsError(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err)
	defer repo.Close()

	refName := plumbing.ReferenceName("refs/entire/cas")
	head := plumbing.NewHash(headHash)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, head)))

	// A distinct, real object to swap the ref to.
	secondHash := plumbing.NewHash(reftableCommit(t, repoDir, "second.txt", "second\n"))
	require.NotEqual(t, head, secondHash)

	// Correct compare-and-swap succeeds: ref is at head, swap head -> second.
	require.NoError(t, repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, secondHash),
		plumbing.NewHashReference(refName, head),
	))

	// Genuine conflict: ref is now at second, but we claim it is still at head.
	err = repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, head),
		plumbing.NewHashReference(refName, head),
	)
	require.ErrorIs(t, err, gitstorage.ErrReferenceHasChanged,
		"a stale expected-old value must be reported as a CAS conflict")

	// Non-conflict failure: the expected-old value is correct (ref is at second),
	// but the new value points at a nonexistent object. git rejects the object,
	// not the CAS, so this must NOT be reported as a concurrency conflict.
	bogus := plumbing.NewHash("1111111111111111111111111111111111111111")
	err = repo.Storer.CheckAndSetReference(
		plumbing.NewHashReference(refName, bogus),
		plumbing.NewHashReference(refName, secondHash),
	)
	require.Error(t, err)
	require.NotErrorIs(t, err, gitstorage.ErrReferenceHasChanged,
		"a nonexistent-object write must not be misreported as a CAS conflict")

	// The failed CAS must not have advanced the ref.
	cur, err := repo.Storer.Reference(refName)
	require.NoError(t, err)
	require.Equal(t, secondHash, cur.Hash())
}

// TestReftableStorer_RefNamesAreArgvNotShell verifies that ref names are passed
// to git as argv, never interpolated into a shell command line. The injected
// name embeds a backtick command substitution with output redirection; if any
// method shelled out, the shell would create the marker file. Every method must
// instead treat the whole string as a literal ref name that round-trips.
func TestReftableStorer_RefNamesAreArgvNotShell(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err)
	defer repo.Close()

	marker := filepath.Join(t.TempDir(), "PWNED")
	require.NotContains(t, marker, " ", "test temp path must be space-free for a valid ref name")
	// `>marker` is a shell redirection inside a backtick command substitution:
	// it creates marker if (and only if) the name is evaluated by a shell.
	injected := plumbing.ReferenceName("refs/entire/inj`>" + marker + "`tail")
	head := plumbing.NewHash(headHash)

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(injected, head)))
	require.NoFileExists(t, marker, "ref name must not be evaluated by a shell on write")

	got, err := repo.Storer.Reference(injected)
	require.NoError(t, err)
	require.Equal(t, head, got.Hash())
	require.NoFileExists(t, marker, "ref name must not be evaluated by a shell on read")

	iter, err := repo.Storer.IterReferences()
	require.NoError(t, err)
	found := false
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		if r.Name() == injected {
			found = true
		}
		return nil
	}))
	iter.Close()
	require.True(t, found, "injected ref name must appear verbatim in iteration")

	require.NoError(t, repo.Storer.RemoveReference(injected))
	require.NoFileExists(t, marker, "ref name must not be evaluated by a shell on remove")
	_, err = repo.Storer.Reference(injected)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

// git subcommand names used by the runGitFn fakes below (named to satisfy
// goconst, which flags the repeated literals across the injected switches).
const (
	gitSymbolicRef = "symbolic-ref"
	gitRevParse    = "rev-parse"
	gitUpdateRef   = "update-ref"
	gitForEachRef  = "for-each-ref"

	// testHeadHash is an arbitrary valid-looking SHA-1 used as a fixture in the
	// runGitFn fakes below.
	testHeadHash = "20b4de1033d986a83837177f961c80bb799161e6"
)

// realExitError returns a genuine *exec.ExitError (git ran and exited non-zero)
// so runGitFn injections can distinguish "git reported absence" from a spawn or
// timeout failure that never produced an exit code.
func realExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run() //nolint:noctx // test helper
	var ee *exec.ExitError
	require.ErrorAs(t, err, &ee)
	return err
}

// timeoutError mimics the error execGit produces when its context deadline
// fires and the git process is killed.
func timeoutError() error {
	return fmt.Errorf("git rev-parse timed out: %w", context.DeadlineExceeded)
}

// lastEnvValue returns the last value for key in an environment slice, matching
// os/exec's de-duplication (last value wins for a duplicate key).
func lastEnvValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			value, found = v, true
		}
	}
	return value, found
}

// TestGitPlumbingEnv_ForcesCLocale verifies that reftable git plumbing always
// runs under a C locale, so git's stderr is never translated and the
// English-substring classification (isRefCASConflict, RemoveReference
// idempotency) is correct even when the caller's environment is localized. The
// forced values must win over any inherited LANG/LC_ALL/LC_MESSAGES.
func TestGitPlumbingEnv_ForcesCLocale(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LC_MESSAGES", "fr_FR.UTF-8")

	env := gitPlumbingEnv()
	for key, want := range map[string]string{"LC_ALL": "C", "LANG": "C", "GIT_TERMINAL_PROMPT": "0"} {
		got, found := lastEnvValue(env, key)
		require.Truef(t, found, "%s must be set", key)
		require.Equalf(t, want, got, "effective %s must be forced regardless of the caller's environment", key)
	}
}

// TestRefLookupAbsent_IsLocaleIndependent verifies the absence classifier keys
// on exit code + empty stderr, so it is correct in any locale: a translated,
// non-empty diagnostic is still surfaced as a real error, not absence.
func TestRefLookupAbsent_IsLocaleIndependent(t *testing.T) {
	t.Parallel()
	germanFatal := []byte("schwerwiegend: Referenz existiert nicht\n")
	require.False(t, refLookupAbsent(realExitError(t), germanFatal),
		"a non-empty (translated) diagnostic must be surfaced regardless of language")
	require.True(t, refLookupAbsent(realExitError(t), nil),
		"exit non-zero with empty stderr is absence in any locale")
	require.True(t, refLookupAbsent(realExitError(t), []byte("   \n")),
		"whitespace-only stderr counts as empty")
	require.False(t, refLookupAbsent(timeoutError(), nil),
		"a timeout is never absence")
}

// TestReference_ClassifiesLookupFailures verifies that Reference maps only a
// genuine "git ran and the ref is absent" result to ErrReferenceNotFound, and
// surfaces spawn/timeout/I-O failures instead. Reporting a transient git
// failure as "ref not found" can make the strategy treat a live checkpoint ref
// as absent (orphan reset, lost linkage), so this distinction is load-bearing.
func TestReference_ClassifiesLookupFailures(t *testing.T) {
	t.Parallel()
	name := plumbing.ReferenceName("refs/entire/probe")

	// symbolic-ref always fails "not symbolic" so every case exercises the
	// rev-parse hash path, whose result is controlled per test.
	storerWith := func(revParseOut string, revParseStderr []byte, revParseErr error) *reftableStorer {
		return &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, realExitError(t)
			case gitRevParse:
				return revParseOut, revParseStderr, revParseErr
			default:
				return "", nil, nil
			}
		}}
	}

	t.Run("genuine absence maps to ErrReferenceNotFound", func(t *testing.T) {
		t.Parallel()
		_, err := storerWith("", nil, realExitError(t)).Reference(name)
		require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	})

	t.Run("timeout is surfaced, not absent", func(t *testing.T) {
		t.Parallel()
		_, err := storerWith("", nil, timeoutError()).Reference(name)
		require.Error(t, err)
		require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
	})

	t.Run("spawn failure is surfaced, not absent", func(t *testing.T) {
		t.Parallel()
		spawn := &exec.Error{Name: "git", Err: errors.New("executable file not found in $PATH")}
		_, err := storerWith("", nil, spawn).Reference(name)
		require.Error(t, err)
		require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
	})

	t.Run("non-zero exit WITH stderr is surfaced, not absent", func(t *testing.T) {
		t.Parallel()
		_, err := storerWith("", []byte("fatal: unable to read reftable stack\n"), realExitError(t)).Reference(name)
		require.Error(t, err)
		require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
	})

	t.Run("transient symbolic-ref probe failure is surfaced, not downgraded to a hash", func(t *testing.T) {
		t.Parallel()
		revParseCalled := false
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, timeoutError() // probe times out on a possibly-symbolic ref
			case gitRevParse:
				revParseCalled = true
				return testHeadHash, nil, nil // would wrongly succeed
			default:
				return "", nil, nil
			}
		}}
		_, err := s.Reference(plumbing.ReferenceName("HEAD"))
		require.Error(t, err)
		require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound)
		require.False(t, revParseCalled,
			"a transient symbolic-ref probe failure must be surfaced, not silently downgraded via rev-parse")
	})
}

// TestIterReferences_HEADFailureSurfaces verifies the HEAD-resolution path in
// IterReferences surfaces a real git failure rather than silently dropping HEAD,
// while still omitting a genuinely-absent HEAD and preserving detached HEAD.
func TestIterReferences_HEADFailureSurfaces(t *testing.T) {
	t.Parallel()

	t.Run("git failure resolving HEAD is surfaced", func(t *testing.T) {
		t.Parallel()
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef, gitRevParse:
				return "", nil, timeoutError()
			default:
				return "", nil, nil
			}
		}}
		_, err := s.IterReferences()
		require.Error(t, err)
	})

	t.Run("detached HEAD resolves to a hash reference", func(t *testing.T) {
		t.Parallel()
		hash := testHeadHash
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, realExitError(t) // detached: not symbolic
			case gitRevParse:
				return hash, nil, nil
			case gitForEachRef:
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		iter, err := s.IterReferences()
		require.NoError(t, err)
		var head *plumbing.Reference
		require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
			if r.Name() == plumbing.HEAD {
				head = r
			}
			return nil
		}))
		iter.Close()
		require.NotNil(t, head, "detached HEAD must be present in iteration")
		require.Equal(t, plumbing.NewHash(hash), head.Hash())
	})

	t.Run("genuinely absent HEAD is omitted without error", func(t *testing.T) {
		t.Parallel()
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, realExitError(t) // not symbolic
			case gitRevParse:
				return "", nil, realExitError(t) // absent: exited non-zero, silent
			case gitForEachRef:
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		iter, err := s.IterReferences()
		require.NoError(t, err)
		count := 0
		require.NoError(t, iter.ForEach(func(_ *plumbing.Reference) error { count++; return nil }))
		iter.Close()
		require.Equal(t, 0, count, "an unborn/absent HEAD must yield no references, not an error")
	})

	t.Run("transient HEAD symbolic-ref probe failure is surfaced, not downgraded to a hash", func(t *testing.T) {
		t.Parallel()
		revParseCalled := false
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, timeoutError() // HEAD probe times out on a branch
			case gitRevParse:
				revParseCalled = true
				return testHeadHash, nil, nil // would wrongly succeed
			case gitForEachRef:
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		_, err := s.IterReferences()
		require.Error(t, err)
		require.False(t, revParseCalled,
			"a transient HEAD symbolic-ref probe failure must be surfaced, not downgraded to a hash HEAD")
	})
}

// TestRemoveReference_DeleteFailureNotSwallowed verifies that a failed deletion
// is only treated as idempotent success when git ran and reported the ref
// already absent (exit 0, or an explicit "does not exist"). A non-zero exit with
// empty stderr — as produced by a killed/timed-out git — must surface as an
// error, not a phantom successful deletion.
func TestRemoveReference_DeleteFailureNotSwallowed(t *testing.T) {
	t.Parallel()
	name := plumbing.ReferenceName("refs/entire/rm")

	storerWith := func(updateRefStderr []byte, updateRefErr error) *reftableStorer {
		return &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, realExitError(t) // not symbolic -> update-ref -d path
			case gitUpdateRef:
				return "", updateRefStderr, updateRefErr
			default:
				return "", nil, nil
			}
		}}
	}

	t.Run("exit 0 is idempotent success", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, storerWith(nil, nil).RemoveReference(name))
	})

	t.Run("explicit does-not-exist is idempotent success", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, storerWith([]byte("error: refs/entire/rm does not exist"), realExitError(t)).RemoveReference(name))
	})

	t.Run("non-zero exit with empty stderr is an error", func(t *testing.T) {
		t.Parallel()
		require.Error(t, storerWith(nil, realExitError(t)).RemoveReference(name),
			"a delete failure with empty stderr must not be swallowed as success")
	})

	t.Run("timeout is an error", func(t *testing.T) {
		t.Parallel()
		require.Error(t, storerWith(nil, timeoutError()).RemoveReference(name))
	})
}

// TestRemoveReference_SymbolicProbeFailureSurfaced verifies that the
// symbolic-ref -q probe classifies its failure: a genuine "not a symbolic ref"
// (exit non-zero, empty stderr) falls through to update-ref -d, but a transient
// failure (timeout/spawn/I-O) is surfaced and never routed into update-ref -d.
// Routing a transient failure into update-ref -d is destructive: on a symbolic
// ref (e.g. HEAD) update-ref -d deletes the ref it points at, silently losing a
// branch pointer.
func TestRemoveReference_SymbolicProbeFailureSurfaced(t *testing.T) {
	t.Parallel()
	name := plumbing.ReferenceName("refs/entire/rm")

	t.Run("transient probe failure is surfaced, never routed to update-ref -d", func(t *testing.T) {
		t.Parallel()
		updateRefCalled := false
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, timeoutError()
			case gitUpdateRef:
				updateRefCalled = true
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		err := s.RemoveReference(name)
		require.Error(t, err)
		require.False(t, updateRefCalled,
			"a transient symbolic-ref probe failure must not fall through to the destructive update-ref -d")
	})

	t.Run("fatal probe error with stderr is surfaced, not routed to update-ref -d", func(t *testing.T) {
		t.Parallel()
		updateRefCalled := false
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", []byte("fatal: unable to read reftable stack\n"), realExitError(t)
			case gitUpdateRef:
				updateRefCalled = true
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		require.Error(t, s.RemoveReference(name))
		require.False(t, updateRefCalled)
	})

	t.Run("genuine not-symbolic falls through to update-ref -d", func(t *testing.T) {
		t.Parallel()
		updateRefCalled := false
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch args[0] {
			case gitSymbolicRef:
				return "", nil, realExitError(t) // exit non-zero, empty stderr = not symbolic
			case gitUpdateRef:
				updateRefCalled = true
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		require.NoError(t, s.RemoveReference(name))
		require.True(t, updateRefCalled, "a non-symbolic ref must be deleted via update-ref -d")
	})

	t.Run("symbolic ref is deleted via symbolic-ref -d, never update-ref -d", func(t *testing.T) {
		t.Parallel()
		symbolicDeleteCalled, updateRefCalled := false, false
		s := &reftableStorer{gitDir: "unused", runGitFn: func(args ...string) (string, []byte, error) {
			switch {
			case args[0] == gitSymbolicRef && len(args) > 1 && args[1] == "-d":
				symbolicDeleteCalled = true
				return "", nil, nil
			case args[0] == gitSymbolicRef: // the -q probe
				return "refs/heads/main", nil, nil
			case args[0] == "update-ref":
				updateRefCalled = true
				return "", nil, nil
			default:
				return "", nil, nil
			}
		}}
		require.NoError(t, s.RemoveReference(name))
		require.True(t, symbolicDeleteCalled, "a symbolic ref must be deleted with symbolic-ref -d")
		require.False(t, updateRefCalled, "a symbolic ref must not be deleted with update-ref -d")
	})
}

// TestOpenPath_ReftableRepository verifies that a reftable repository, which
// go-git's filesystem storer cannot open, is opened successfully and that ref
// read/write/list/remove all round-trip through the git-CLI-backed storer.
func TestOpenPath_ReftableRepository(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err, "reftable repository should open")
	defer repo.Close()

	// HEAD resolves to the real branch, not the reftable .invalid stub.
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", head.Name().String())
	require.Equal(t, headHash, head.Hash().String())

	// Write a new ref via go-git (routed through git update-ref) and read it back.
	newRef := plumbing.NewHashReference(plumbing.ReferenceName("refs/entire/test/one"), head.Hash())
	require.NoError(t, repo.Storer.SetReference(newRef))

	got, err := repo.Storer.Reference(newRef.Name())
	require.NoError(t, err)
	require.Equal(t, head.Hash(), got.Hash())

	// The new ref appears in iteration.
	iter, err := repo.Storer.IterReferences()
	require.NoError(t, err)
	found := false
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		if r.Name() == newRef.Name() {
			found = true
		}
		return nil
	}))
	iter.Close()
	require.True(t, found, "written ref should appear in IterReferences")

	// Removal round-trips, and removing again is a no-op.
	require.NoError(t, repo.Storer.RemoveReference(newRef.Name()))
	_, err = repo.Storer.Reference(newRef.Name())
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	require.NoError(t, repo.Storer.RemoveReference(newRef.Name()))
}

// TestOpenPath_Sha256ReftableRepository confirms that a reftable repository
// using the sha256 object format can be opened and that refs round-trip through
// the git-CLI-backed storer.
//
// Such a repository declares TWO extensions in its config:
// extensions.refstorage=reftable AND extensions.objectformat=sha256. go-git's
// verifyExtensions asks the storer's SupportsExtension whether each declared
// extension is supported. The embedded filesystem Storage approves
// objectformat=sha256, but reftableStorer defines its own SupportsExtension
// (to advertise refstorage), which shadows the embedded method by Go's
// promotion rules. As written it approves only refstorage, so objectformat is
// reported unsupported and go-git rejects the open with ErrUnknownExtension.
// The reftable backend thus silently breaks sha256 repositories. This test
// pins the correct behaviour and is the regression guard for that gap in #547.
func TestOpenPath_Sha256ReftableRepository(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepoWithFormat(t, "sha256", "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err, "sha256 reftable repository should open; objectformat extension must stay supported")
	defer repo.Close()

	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", head.Name().String())
	require.Equal(t, headHash, head.Hash().String())

	// A ref write/read round-trips through the git-CLI-backed storer, proving
	// the sha256 repo is not merely openable but usable.
	newRef := plumbing.NewHashReference(plumbing.ReferenceName("refs/entire/sha256"), head.Hash())
	require.NoError(t, repo.Storer.SetReference(newRef))
	got, err := repo.Storer.Reference(newRef.Name())
	require.NoError(t, err)
	require.Equal(t, head.Hash(), got.Hash())
}

// TestOpenPath_WorktreeConfigReftableRepository confirms that a reftable
// repository that also enables the worktreeConfig extension can be opened.
//
// This is the same extension-shadowing gap as
// TestOpenPath_Sha256ReftableRepository: the embedded filesystem Storage
// approves worktreeconfig=true/false, but reftableStorer's own
// SupportsExtension shadows that method and approves only refstorage. A
// reftable repo with extensions.worktreeConfig=true therefore fails to open
// with ErrUnknownExtension. Regression guard for that gap in #547.
func TestOpenPath_WorktreeConfigReftableRepository(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")
	setRepoConfig(t, repoDir, "extensions.worktreeConfig", "true")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err, "reftable repository with worktreeConfig should open; worktreeconfig extension must stay supported")
	defer repo.Close()

	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", head.Name().String())
	require.Equal(t, headHash, head.Hash().String())
}

// TestRepoUsesReftable_Detection checks that reftable detection distinguishes
// reftable repositories from classic files-backend repositories.
func TestRepoUsesReftable_Detection(t *testing.T) {
	t.Parallel()

	reftableRepo, _ := initReftableRepo(t, "a.txt", "a\n")
	usesReftable, err := inspectRepoUsesReftable(filepath.Join(reftableRepo, ".git"), filepath.Join(reftableRepo, ".git"))
	require.NoError(t, err)
	require.True(t, usesReftable)

	filesRepo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(filesRepo, ".git", "refs"), 0o755))
	usesReftable, err = inspectRepoUsesReftable(filepath.Join(filesRepo, ".git"), filepath.Join(filesRepo, ".git"))
	require.NoError(t, err)
	require.False(t, usesReftable)
}
