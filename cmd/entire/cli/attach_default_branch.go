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

// sharedBranchRefusal is the error for a pushed commit that branches other
// than this one's own pushed copy hold: rewriting it would leave theirs
// without the trailer, and force-pushing them is not this branch's call.
func sharedBranchRefusal(target *object.Commit, others []string) error {
	lines := []string{
		fmt.Sprintf("commit %s is also on %s, not just this branch's own pushed copy, so attach won't rewrite it to add the Entire-Checkpoint trailer; nothing was changed.", describeCommit(target), strings.Join(others, ", ")),
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
// may be stale, and one that can't answer makes the result an error rather than
// "none" (no fallback to those tracking refs). Other remotes are judged from
// their tracking refs alone; one whose tracking refs hold target but name no
// default branch is an error too.
func defaultBranchContaining(ctx context.Context, target *object.Commit, checked []string) (string, error) {
	tracking, err := remoteTrackingRefsContaining(ctx, target)
	if err != nil {
		return "", err
	}
	all := attachRemotes(ctx)
	holds := map[string]bool{}
	for _, ref := range tracking {
		holds[trackingRefRemote(ref, all)] = true
	}
	var unknown []string
	for _, remote := range all {
		ask := slices.Contains(checked, remote)
		if !ask && !holds[remote] {
			// Its tracking refs hold no copy of target, so its tracked
			// default branch can't contain it either.
			continue
		}
		branch, on, err := onRemoteDefaultBranch(ctx, target, remote, ask)
		if err != nil {
			unknown = append(unknown, err.Error())
		} else if on {
			return remote + "/" + branch, nil
		}
	}
	if len(unknown) > 0 {
		return "", errors.New(strings.Join(unknown, "; "))
	}
	return "", nil
}

// otherBranchesHolding lists the remote branches holding target other than the
// current branch's own pushed copies: its upstream, its push branch, and a
// branch of the same name on a remote it pushes to (checked). A commit only
// those hold is this branch's to rewrite; any other branch holding it is
// shared. It is an error when no remote branch is known to hold target, since
// a remote that holds it then does so on a branch attach can't name.
func otherBranchesHolding(ctx context.Context, target *object.Commit, checked []string) ([]string, error) {
	holding := remoteBranchesContaining(ctx, target)
	if len(holding) == 0 {
		return nil, fmt.Errorf("commit %s is pushed, but attach couldn't tell which remote branches hold it, so it won't be rewritten; nothing was changed. Rerun with --%s to record a link to it instead of a trailer",
			target.Hash.String()[:12], linkWithoutTrailerFlag)
	}
	own := currentBranchPushedCopies(ctx, checked)
	var others []string
	for _, branch := range holding {
		if !slices.Contains(own, branch) {
			others = append(others, branch)
		}
	}
	return others, nil
}

// currentBranchPushedCopies names the current branch's own copies on its
// remotes as "<remote>/<branch>": its upstream and push branch, and the same
// name on each of checked. None for a detached HEAD.
func currentBranchPushedCopies(ctx context.Context, checked []string) []string {
	ref, err := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "HEAD").Output()
	if err != nil {
		return nil
	}
	fullName := strings.TrimSpace(string(ref))
	var own []string
	if out, err := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(upstream:short)%0a%(push:short)", "--", fullName).Output(); err == nil {
		own = strings.Fields(string(out))
	}
	name := strings.TrimPrefix(fullName, "refs/heads/")
	for _, remote := range checked {
		own = append(own, remote+"/"+name)
	}
	return own
}

// onRemoteDefaultBranch reports whether remote's default branch contains
// target, naming the branch. ask reads the default branch from the remote
// itself, else from its tracking refs.
func onRemoteDefaultBranch(ctx context.Context, target *object.Commit, remote string, ask bool) (string, bool, error) {
	var branch, tip string
	var err error
	if ask {
		branch, tip, err = askRemoteDefaultTip(ctx, remote)
	} else {
		branch, tip, err = trackedDefaultTip(ctx, remote)
	}
	if err != nil {
		return "", false, err
	}
	on, err := reachableFromAny(ctx, target, []string{tip})
	if err != nil {
		return "", false, fmt.Errorf("couldn't check %s/%s: %w", remote, branch, err)
	}
	return branch, on, nil
}

// askRemoteDefaultTip reads remote's default branch and the commit at its tip
// with ls-remote --symref, then fetches the tip's objects, writing no ref,
// when they aren't local. A remote that doesn't answer, or names no default
// branch with a commit, is an error: its tracking refs may be stale.
func askRemoteDefaultTip(ctx context.Context, remote string) (branch, tip string, err error) {
	out, err := lsRemoteSymrefHEAD(ctx, remote, attachFetchTimeout)
	if err != nil {
		return "", "", fmt.Errorf("couldn't read the default branch of %s: %w", remote, err)
	}
	branch, ok := parseSymrefHEAD(out)
	tip = symrefHEADTip(out)
	// The name becomes a fetch refspec, so a hostile remote's
	// "x:refs/heads/main" must not write a local ref.
	if !ok || tip == "" || !isRemoteBranchName("refs/heads/"+branch) {
		// Git reports no HEAD at all when it names a branch with no
		// commits. That is not read as "contains nothing": a bare repo made
		// with 'git init --bare' names master while everyone pushes main.
		return "", "", fmt.Errorf("%s's HEAD doesn't name a branch with commits, so its default branch is unknown (on the remote, 'git symbolic-ref HEAD refs/heads/<default>' sets it)", remote)
	}
	if commitPresent(ctx, tip) {
		return branch, tip, nil
	}
	fetchCtx, cancel := context.WithTimeout(ctx, attachFetchTimeout)
	defer cancel()
	if err := attachGitCommand(fetchCtx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--refmap=", "--", remote, "refs/heads/"+branch).Run(); err != nil {
		return "", "", fmt.Errorf("couldn't fetch %s/%s: %w", remote, branch, err)
	}
	return branch, tip, nil
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
func trackedDefaultTip(ctx context.Context, remote string) (branch, tip string, err error) {
	prefix := "refs/remotes/" + remote + "/"
	if out, err := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", prefix+remoteHEADRef).Output(); err == nil {
		if name, found := strings.CutPrefix(strings.TrimSpace(string(out)), prefix); found && name != "" {
			if sha := attachRevParse(ctx, prefix+name); sha != "" {
				return name, sha, nil
			}
		}
	}
	for _, name := range []string{defaultBaseBranch, masterBaseBranch} {
		if sha := attachRevParse(ctx, prefix+name); sha != "" {
			return name, sha, nil
		}
	}
	return "", "", fmt.Errorf("%s has no known default branch ('git remote set-head %s --auto' records it)", remote, remote)
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

// remoteTrackingRefsContaining lists the full names of the remote-tracking
// refs that contain target.
func remoteTrackingRefsContaining(ctx context.Context, target *object.Commit) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "branch", "-r", "--contains", target.Hash.String(), "--format=%(refname)").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to check which remote branches contain %s: %w", target.Hash.String()[:12], err)
	}
	return strings.Fields(string(out)), nil
}

// remoteBranchesContaining lists the remote-tracking branches that contain
// target, short names without <remote>/HEAD, for the confirmation before
// rewriting a pushed commit. A failed query lists none: it only words the
// confirmation.
func remoteBranchesContaining(ctx context.Context, target *object.Commit) []string {
	refs, err := remoteTrackingRefsContaining(ctx, target)
	if err != nil {
		return nil
	}
	var branches []string
	for _, ref := range refs {
		if short := strings.TrimPrefix(ref, "refs/remotes/"); !strings.HasSuffix(short, "/"+remoteHEADRef) {
			branches = append(branches, short)
		}
	}
	return branches
}
