package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/object"
)

// linkWithoutTrailerFlag records a link to a commit in the checkpoint instead
// of adding the Entire-Checkpoint trailer to it.
const linkWithoutTrailerFlag = "link-without-trailer"

// remoteHEADRef is the ref a remote's default branch is read through.
const remoteHEADRef = "HEAD"

// recordedLinkCaveats explains what a link recorded in the checkpoint gives up
// against a trailer, for the refusal, the confirmation and the help text.
func recordedLinkCaveats() []string {
	return []string{
		"  - It names this exact commit: a rebase, amend or cherry-pick of it loses the link.",
		"  - The commit itself doesn't point at the checkpoint; only Entire's checkpoint data does.",
		"  - It counts only when the commit's author attaches it: anyone who can push checkpoints can record a link to any commit.",
	}
}

// defaultBranchRefusal is the error for a pushed commit that is on a remote's
// default branch: rewriting it is never right, and a recorded link must be
// chosen explicitly.
func defaultBranchRefusal(target *object.Commit, branch string) error {
	lines := []string{
		fmt.Sprintf("commit %s is on %s, a default branch, so attach won't rewrite it to add the Entire-Checkpoint trailer; nothing was changed.", describeCommit(target), branch),
		"",
		fmt.Sprintf("To link the session anyway, rerun with --%s. That records the commit's SHA in the checkpoint instead of a trailer in the commit, which is weaker:", linkWithoutTrailerFlag),
	}
	return errors.New(strings.Join(append(lines, recordedLinkCaveats()...), "\n"))
}

// defaultBranchUnknownRefusal is the error when attach can't tell whether a
// pushed commit is on a default branch: rewriting it might rewrite one.
func defaultBranchUnknownRefusal(target *object.Commit, cause error) error {
	return fmt.Errorf("commit %s is pushed, and attach couldn't tell whether it is on a default branch (%w), so it won't be rewritten; nothing was changed. Retry once the remote's default branch can be read, or rerun with --%s to record a link to it instead of a trailer",
		target.Hash.String()[:12], cause, linkWithoutTrailerFlag)
}

// defaultBranchContaining returns the first remote default branch, as
// "<remote>/<branch>", whose tip contains target, or "" when none does. Every
// remote counts, as for remoteHoldingCommit: in a fork, a commit on upstream's
// default branch is as shared as one on origin's. checked are the remotes the
// commit's branch pushes to; they are asked directly, since their tracking refs
// may be stale, and when one of them can't answer the result is an error rather
// than "none". Other remotes are judged from their tracking refs alone.
func defaultBranchContaining(ctx context.Context, target *object.Commit, checked []string) (string, error) {
	var unknown []string
	for _, remote := range attachRemotes(ctx) {
		ask := slices.Contains(checked, remote)
		branch, tip, ok := remoteDefaultTip(ctx, remote, ask)
		if !ok {
			if ask {
				unknown = append(unknown, remote)
			}
			continue
		}
		if tip == "" {
			// The default branch has no commits yet, so it contains nothing.
			continue
		}
		on, err := reachableFromAny(ctx, target, []string{tip})
		if err != nil {
			if ask {
				unknown = append(unknown, remote)
			}
			continue
		}
		if on {
			return remote + "/" + branch, nil
		}
	}
	if len(unknown) > 0 {
		return "", fmt.Errorf("couldn't read the default branch of %s", strings.Join(unknown, ", "))
	}
	return "", nil
}

// remoteDefaultTip returns remote's default branch and the commit at its tip,
// with the tip's objects present locally. ask asks the remote first; its
// tracking refs are the fallback: origin/HEAD, then a main or master branch.
// tip is "" for a default branch with no commits. ok is false when the default
// branch can't be determined.
func remoteDefaultTip(ctx context.Context, remote string, ask bool) (branch, tip string, ok bool) {
	if ask {
		if branch, tip, ok = askRemoteDefaultTip(ctx, remote); ok {
			return branch, tip, true
		}
	}
	return trackedDefaultTip(ctx, remote)
}

// askRemoteDefaultTip reads remote's HEAD with ls-remote --symref, then fetches
// its tip's objects, writing no ref, when they aren't local.
func askRemoteDefaultTip(ctx context.Context, remote string) (branch, tip string, ok bool) {
	lsCtx, cancel := context.WithTimeout(ctx, attachFetchTimeout)
	defer cancel()
	out, err := attachGitCommand(lsCtx, "ls-remote", "--symref", "--", remote, remoteHEADRef).Output()
	if err != nil {
		return "", "", false
	}
	branch, ok = parseSymrefHEAD(string(out))
	if !ok {
		return "", "", false
	}
	tip = symrefHEADTip(string(out))
	if tip == "" || commitPresent(ctx, tip) {
		return branch, tip, true
	}
	fetchCtx, cancel := context.WithTimeout(ctx, attachFetchTimeout)
	defer cancel()
	if attachGitCommand(fetchCtx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--refmap=", "--", remote, "refs/heads/"+branch).Run() != nil {
		return "", "", false
	}
	return branch, tip, true
}

// symrefHEADTip reads the `<sha>\tHEAD` line from ls-remote --symref output.
func symrefHEADTip(out string) string {
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && ref == remoteHEADRef && !strings.HasPrefix(sha, "ref:") {
			return sha
		}
	}
	return ""
}

// trackedDefaultTip finds remote's default branch from its tracking refs:
// refs/remotes/<remote>/HEAD, else a main or master branch.
func trackedDefaultTip(ctx context.Context, remote string) (branch, tip string, ok bool) {
	prefix := "refs/remotes/" + remote + "/"
	if out, err := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", prefix+remoteHEADRef).Output(); err == nil {
		if name, found := strings.CutPrefix(strings.TrimSpace(string(out)), prefix); found && name != "" {
			if sha := attachRevParse(ctx, prefix+name); sha != "" {
				return name, sha, true
			}
		}
	}
	for _, name := range []string{defaultBaseBranch, masterBaseBranch} {
		if sha := attachRevParse(ctx, prefix+name); sha != "" {
			return name, sha, true
		}
	}
	return "", "", false
}

// attachRevParse resolves ref to a commit hash, or "" when it doesn't exist.
func attachRevParse(ctx context.Context, ref string) string {
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// commitPresent reports whether sha's commit object is in the local store.
func commitPresent(ctx context.Context, sha string) bool {
	return exec.CommandContext(ctx, "git", "cat-file", "-e", sha+"^{commit}").Run() == nil
}

// remoteBranchesContaining lists the remote-tracking branches that contain
// target, for the confirmation before rewriting a pushed commit.
func remoteBranchesContaining(ctx context.Context, target *object.Commit) []string {
	out, err := exec.CommandContext(ctx, "git", "branch", "-r", "--contains", target.Hash.String(), "--format=%(refname:short)").Output()
	if err != nil {
		return nil
	}
	return slices.DeleteFunc(strings.Fields(string(out)), func(ref string) bool { return strings.HasSuffix(ref, "/HEAD") })
}
