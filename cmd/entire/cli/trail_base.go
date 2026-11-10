package cli

import (
	"context"
	"fmt"
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
// (asked of origin, else the local origin/HEAD), else "": the server then
// applies the repo's default branch. It never guesses: a guessed "main" in a
// repo whose default is develop gave the trail the wrong base, so the user's PR
// into the real default never settled it and Entire kept a draft open on
// merged work. Origin is asked first because fetch does not move an existing
// origin/HEAD when the upstream default changes while the old branch survives;
// the local ref only answers when origin cannot.
//
// naming falls back to the local main/master guess when send is unknown, which
// keeps branch naming as it was; it is never sent.
func resolveTrailCreateBase(ctx context.Context, repo *git.Repository, base string) (send, naming string) {
	if base = strings.TrimSpace(base); base != "" {
		return base, base
	}
	if name, ok := trailRemoteDefaultBranch(ctx, "origin"); ok && ValidateBranchName(ctx, name) == nil {
		return name, name
	}
	if name := localOriginHEADBranch(repo); name != "" && ValidateBranchName(ctx, name) == nil {
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
	// A hand-set origin/HEAD can point anywhere; only a branch of origin
	// names origin's default.
	name, ok := strings.CutPrefix(ref.Target().String(), "refs/remotes/origin/")
	if !ok {
		return ""
	}
	return name
}

// remoteDefaultBranch asks remote which branch its HEAD names. Any failure
// reports unknown rather than an error: the caller then lets the server pick.
// It runs against the user's repository as invoked, like the other trail
// ls-remote checks, so exported repo selectors still apply.
func remoteDefaultBranch(ctx context.Context, remote string) (string, bool) {
	out, err := lsRemoteSymrefHEAD(ctx, remote, trailDefaultBranchTimeout)
	if err != nil {
		logging.Debug(ctx, "trail create: could not read the remote's default branch",
			"remote", remote, "error", err.Error())
		return "", false
	}
	return parseSymrefHEAD(out)
}

// lsRemoteSymrefHEAD runs `git ls-remote --symref <remote> HEAD` without
// prompting, bounded by timeout and in output size, and returns its stdout.
// The remote and its transport helper control that output, and a timeout
// limits time, not bytes.
func lsRemoteSymrefHEAD(ctx context.Context, remote string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := execx.NonInteractive(ctx, "git", "ls-remote", "--symref", "--", remote, "HEAD")
	cmd.WaitDelay = trailBranchCheckWaitDelay
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	stdout := &limitedWriter{limit: trailRemoteDefaultBranchMaxOutput, failOnOverflow: true}
	stderr := &limitedWriter{limit: maxGitStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ls-remote --symref %s HEAD: %w: %s", remote, err, redactGitStderr(stderr.buf.String()))
	}
	if stdout.exceeded {
		return "", fmt.Errorf("ls-remote --symref %s HEAD: output over %d bytes", remote, trailRemoteDefaultBranchMaxOutput)
	}
	return stdout.buf.String(), nil
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
