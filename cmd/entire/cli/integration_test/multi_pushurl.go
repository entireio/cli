//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Helpers for the multi-push-URL scenario: ONE git remote that carries several
// push URLs (remote.<name>.pushurl, which git allows to repeat). git pushes to
// every push URL and — this is the part that matters for checkpoint sync —
// invokes the pre-push hook ONCE PER PUSH URL, passing the same remote NAME as
// $1 each time and the individual URL as $2. Our installed hook forwards only
// $1 (see strategy/hooks.go), so the CLI cannot tell the invocations apart.
// git-branch then hands `git push <name>` the remote name and lets git fan out
// again; git-refs resolves the first push URL itself and targets only that (see
// strategy.resolveRefsPushDestination).
//
// See multi_pushurl_test.go for what that means per checkpoint backend.

// pushQueueFileName mirrors the unexported constant in the checkpoint package.
// Duplicated rather than exported: the queue file name is part of the on-disk
// layout these tests deliberately inspect from the outside.
const pushQueueFileName = "entire-checkpoint-push-queue.jsonl"

// AddSecondPushURL creates a second bare repository and configures remoteName so
// pushes fan out to BOTH the remote's original URL and the new bare repo. It
// returns the new bare repo path.
//
// Note the git subtlety this encodes: configuring ANY pushurl replaces the
// remote's url for push purposes, so the original URL has to be re-added as an
// explicit pushurl. Adding only the new one would silently redirect pushes to
// the new repo instead of fanning out.
//
// The new bare repo is deliberately NOT added as a named remote — the whole
// point is that it is reachable only as a push URL of an existing remote.
func (env *TestEnv) AddSecondPushURL(remoteName string) string {
	env.T.Helper()

	secondBare := env.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(secondBare); err == nil {
		secondBare = resolved
	}

	testutil.RunGit(env.T, secondBare, "init", "--bare")

	// The original URL is re-added explicitly because configuring ANY pushurl
	// replaces url for push purposes — adding only the new one would silently
	// redirect pushes instead of fanning out.
	env.setPushURLs(remoteName, env.RemoteURL(remoteName), secondBare)

	// Guard the setup itself: a helper that quietly configured one push URL
	// would make every fan-out assertion vacuous.
	if got := env.PushURLs(remoteName); len(got) != 2 {
		env.T.Fatalf("expected 2 push URLs on remote %s, got %d: %v", remoteName, len(got), got)
	}

	return secondBare
}

// AddUnreachableSecondPushURL configures remoteName to push to its original URL
// first and a path that does not exist second. Models a mirror that is down or
// whose credentials have expired, in the position where git still reaches the
// healthy URL before failing. Returns the unreachable path.
func (env *TestEnv) AddUnreachableSecondPushURL(remoteName string) string {
	env.T.Helper()
	missing := filepath.Join(env.T.TempDir(), "does-not-exist.git")
	env.setPushURLs(remoteName, env.RemoteURL(remoteName), missing)
	return missing
}

// AddUnreachableFirstPushURL is AddUnreachableSecondPushURL with the unreachable
// path FIRST — the position that matters, because a transport failure makes git
// die() rather than return, so no later URL is attempted at all.
func (env *TestEnv) AddUnreachableFirstPushURL(remoteName string) string {
	env.T.Helper()
	missing := filepath.Join(env.T.TempDir(), "does-not-exist.git")
	env.setPushURLs(remoteName, missing, env.RemoteURL(remoteName))
	return missing
}

// GitPushWithHooksAllowError is GitPushWithHooks without the fatal-on-error
// behavior: it returns git's error instead of failing, so a test can exercise a
// push that partially fails (e.g. one of several push URLs is unreachable, which
// makes git exit non-zero even though the reachable URL received everything).
// git's combined output — including the hook's stderr — is logged either way.
func (env *TestEnv) GitPushWithHooksAllowError(remote, refSpec string) error {
	env.T.Helper()

	env.InstallRealPrePushHook()

	cmd := execx.NonInteractive(env.T.Context(), "git", "push", remote, refSpec)
	cmd.Dir = env.RepoDir
	cmd.Env = env.cliEnv()
	output, err := cmd.CombinedOutput()
	env.T.Logf("git push (with hooks, error allowed) %s %s output: %s", remote, refSpec, output)
	return err //nolint:wrapcheck // test helper: the caller asserts on presence/absence, not identity
}

// setPushURLs appends push URLs to remoteName in the given order and
// re-baselines the .git/config guard (changing push URLs is deliberate here).
//
// Order is the parameter that matters: git iterates push URLs in config order,
// and a transport failure is fatal, so a broken URL first behaves differently
// from the same URL last.
func (env *TestEnv) setPushURLs(remoteName string, urls ...string) {
	env.T.Helper()

	for _, url := range urls {
		cmd := exec.CommandContext(env.T.Context(), "git", "remote", "set-url", "--add", "--push", remoteName, url)
		cmd.Dir = env.RepoDir
		cmd.Env = testutil.GitIsolatedEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			env.T.Fatalf("failed to add push URL %s to remote %s: %v\n%s", url, remoteName, err, output)
		}
	}

	env.setGitConfigBaseline()
}

// RemoteURL returns the fetch URL configured for remoteName.
func (env *TestEnv) RemoteURL(remoteName string) string {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", "remote", "get-url", remoteName)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		env.T.Fatalf("failed to read URL of remote %s: %v", remoteName, err)
	}
	return strings.TrimSpace(string(out))
}

// PushURLs returns every push URL configured for remoteName, in config order.
func (env *TestEnv) PushURLs(remoteName string) []string {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", "remote", "get-url", "--push", "--all", remoteName)
	cmd.Dir = env.RepoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		env.T.Fatalf("failed to read push URLs of remote %s: %v", remoteName, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// DivergeRemoteRef moves ref forward in the bare repo at bareDir by one commit
// that keeps the existing tree, so the ref is no longer an ancestor of the local
// one and the next local push to it is rejected as non-fast-forward. label makes
// the synthetic commit (and therefore the resulting hash) distinct, so two bare
// repos can be diverged independently and never accidentally converge.
//
// Returns the new hash of the ref.
func (env *TestEnv) DivergeRemoteRef(bareDir, ref, label string) string {
	env.T.Helper()

	tip := env.refHash(bareDir, ref)
	if tip == "" {
		env.T.Fatalf("cannot diverge %s in %s: ref does not exist", ref, bareDir)
	}
	tree := env.gitOutput(bareDir, "rev-parse", ref+"^{tree}")

	newHash := env.gitOutput(bareDir, "commit-tree", tree, "-p", tip, "-m", "diverged: "+label)
	env.gitOutput(bareDir, "update-ref", ref, newHash)

	if got := env.refHash(bareDir, ref); got != newHash {
		env.T.Fatalf("diverge %s in %s: ref is %s, expected %s", ref, bareDir, got, newHash)
	}
	return newHash
}

// RefHashOnRemote returns the hash ref points at in the bare repo at bareDir, or
// "" when the ref does not exist.
func (env *TestEnv) RefHashOnRemote(bareDir, ref string) string {
	env.T.Helper()
	return env.refHash(bareDir, ref)
}

// QueuedCheckpointRefs returns the checkpoint refs currently sitting in the
// git-refs push-discovery queue, de-duplicated, in first-seen order. An empty
// result means every write has been confirmed pushed (or nothing was queued).
func (env *TestEnv) QueuedCheckpointRefs() []string {
	env.T.Helper()

	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", pushQueueFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		env.T.Fatalf("failed to read push queue: %v", err)
	}

	seen := make(map[string]bool)
	var refs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			env.T.Fatalf("failed to parse push queue line %q: %v", line, err)
		}
		if entry.Ref == "" || seen[entry.Ref] {
			continue
		}
		seen[entry.Ref] = true
		refs = append(refs, entry.Ref)
	}
	return refs
}

// refHash resolves ref in the repo at dir, returning "" when it does not exist.
func (env *TestEnv) refHash(dir, ref string) string {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// IsAncestor reports whether the commit ancestor is an ancestor of ref in the
// repo at dir.
func (env *TestEnv) IsAncestor(dir, ancestor, ref string) bool {
	env.T.Helper()
	return env.gitSucceeds(dir, "merge-base", "--is-ancestor", ancestor, ref)
}

// gitSucceeds reports whether a git command in dir exits zero. Used for
// predicate-style plumbing calls (e.g. merge-base --is-ancestor) where a
// non-zero exit is an answer, not a failure.
func (env *TestEnv) gitSucceeds(dir string, args ...string) bool {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// gitOutput runs a git command in dir and fails the test on error.
func (env *TestEnv) gitOutput(dir string, args ...string) string {
	env.T.Helper()

	cmd := exec.CommandContext(env.T.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(testutil.GitIsolatedEnv(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.Output()
	if err != nil {
		env.T.Fatalf("git %s in %s failed: %v", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out))
}
