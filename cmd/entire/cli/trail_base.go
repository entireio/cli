package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// trailDefaultBranchTimeout bounds asking origin for its default branch. The
// answer only refines the trail's base; without it the server uses the repo's
// default branch, so a slow remote must not hold up trail creation.
var trailDefaultBranchTimeout = 10 * time.Second

// trailRemoteDefaultBranchMaxOutput caps what ls-remote may return; the
// answer is one short line, and anything larger is not a default branch.
const trailRemoteDefaultBranchMaxOutput = 64 << 10

// trailRemoteDefaultBranch is a seam so tests never ask a real remote.
var trailRemoteDefaultBranch = remoteDefaultBranch

// resolveTrailCreateBase returns the base to send with a new trail and the
// branch to compare against when deciding whether to derive a new branch name.
//
// send is an explicit --base, else origin's default branch when it is known
// (origin/HEAD, else asked of origin), else "": the server then applies the
// repo's default branch. It never guesses: a guessed "main" in a repo whose
// default is develop gave the trail the wrong base, so the user's PR into the
// real default never settled it and Entire kept a draft open on merged work.
//
// naming falls back to the local main/master guess when send is unknown, which
// keeps branch naming as it was; it is never sent.
func resolveTrailCreateBase(ctx context.Context, repo *git.Repository, base string) (send, naming string) {
	if base = strings.TrimSpace(base); base != "" {
		return base, base
	}
	if name := localOriginHEADBranch(repo); name != "" && ValidateBranchName(ctx, name) == nil {
		return name, name
	}
	if name, ok := trailRemoteDefaultBranch(ctx, "origin"); ok && ValidateBranchName(ctx, name) == nil {
		return name, name
	}
	if guess := strategy.GetDefaultBranchName(repo); guess != "" {
		return "", guess
	}
	return "", defaultBaseBranch
}

// localOriginHEADBranch returns the branch refs/remotes/origin/HEAD points at,
// or "" when the clone has no such symbolic ref.
func localOriginHEADBranch(repo *git.Repository) string {
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "HEAD"), false)
	if err != nil || ref == nil || ref.Type() != plumbing.SymbolicReference {
		return ""
	}
	name, _ := strings.CutPrefix(ref.Target().String(), "refs/remotes/origin/")
	return name
}

// remoteDefaultBranch asks remote which branch its HEAD names. Any failure
// reports unknown rather than an error: the caller then lets the server pick.
// It runs against the user's repository as invoked, like the other trail
// ls-remote checks, so exported repo selectors still apply.
func remoteDefaultBranch(ctx context.Context, remote string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, trailDefaultBranchTimeout)
	defer cancel()
	cmd := execx.NonInteractive(ctx, "git", "ls-remote", "--symref", remote, "HEAD")
	cmd.WaitDelay = trailBranchCheckWaitDelay
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		logging.Debug(ctx, "trail create: could not read the remote's default branch",
			"remote", remote, "error", err.Error(), "stderr", redactGitStderr(stderr.String()))
		return "", false
	}
	if stdout.Len() > trailRemoteDefaultBranchMaxOutput {
		return "", false
	}
	return parseSymrefHEAD(stdout.String())
}

// parseSymrefHEAD reads `ref: refs/heads/<name>\tHEAD` from ls-remote --symref
// output. The remote controls this text, so only that exact line counts.
func parseSymrefHEAD(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		target, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || ref != "HEAD" {
			continue
		}
		name, ok := strings.CutPrefix(target, "ref: refs/heads/")
		if ok && name != "" {
			return name, true
		}
	}
	return "", false
}
