package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"charm.land/huh/v2"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// errAttachDeclined is returned when the user answers no; nothing was changed
// and attach exits successfully.
var errAttachDeclined = errors.New("attach declined")

// confirmAttach prints what attach is about to do and asks before doing it.
// With --force it proceeds after printing. Without a terminal to ask on (an
// agent or a script), it changes nothing and exits non-zero, telling the
// caller to get the user's go-ahead and rerun with --force.
func confirmAttach(w, errW io.Writer, warning []string, force bool) error {
	fmt.Fprintln(errW)
	for _, line := range warning {
		fmt.Fprintln(errW, line)
	}
	if force {
		return nil
	}
	if !interactive.CanPromptInteractively() {
		fmt.Fprintln(errW, "\nNothing was changed. This needs the user's go-ahead: show them the above, and if they agree, rerun with --force.")
		return NewSilentError(errors.New("session attach needs the user's confirmation; rerun with --force once they agree"))
	}
	proceed := false
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Continue?").
			Affirmative("Yes").
			Negative("No").
			Value(&proceed),
	))
	if err := form.Run(); err != nil {
		return fmt.Errorf("prompt failed: %w", err)
	}
	if !proceed {
		fmt.Fprintln(w, "Nothing was changed.")
		return errAttachDeclined
	}
	return nil
}

// describeCommit is a commit's short hash and subject, for warnings.
func describeCommit(c *object.Commit) string {
	subject, _, _ := strings.Cut(c.Message, "\n")
	return fmt.Sprintf("%s %q", c.Hash.String()[:12], subject)
}

// attachRewriteChain returns target and the commits after it up to HEAD,
// oldest first: the commits that adding a trailer to target rewrites. It
// refuses mid-operation (rebase, merge, ...), a target not on the current
// branch, and merges after the target, which a message-only replay can't
// reproduce faithfully.
func attachRewriteChain(ctx context.Context, repo *git.Repository, target, head *object.Commit) ([]*object.Commit, error) {
	if op := strategy.GitOperationInProgress(ctx); op != "" {
		return nil, fmt.Errorf("can't add the Entire-Checkpoint trailer to %s while %s is in progress; finish or abort it first", target.Hash.String()[:12], op)
	}
	if target.Hash.Equal(head.Hash) {
		return []*object.Commit{target}, nil
	}
	if exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", target.Hash.String(), head.Hash.String()).Run() != nil {
		return nil, fmt.Errorf("commit %s is not pushed and is not on the current branch, so attach can't add a trailer to it; check out a branch that contains it, or push it first", target.Hash.String()[:12])
	}
	out, err := exec.CommandContext(ctx, "git", "rev-list", "--reverse", "--parents", target.Hash.String()+".."+head.Hash.String()).Output()
	if err != nil {
		return nil, fmt.Errorf("list the commits after %s: %w", target.Hash.String()[:12], err)
	}
	chain := []*object.Commit{target}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("commit %s after %s is a merge, so attach can't rewrite the commits after it; push %s first, or attach while it is HEAD", fields[0][:12], target.Hash.String()[:12], target.Hash.String()[:12])
		}
		c, err := repo.CommitObject(plumbing.NewHash(fields[0]))
		if err != nil {
			return nil, fmt.Errorf("read commit %s: %w", fields[0][:12], err)
		}
		chain = append(chain, c)
	}
	return chain, nil
}

// rewriteWarning describes adding a trailer to chain[0], for confirmAttach.
func rewriteWarning(ctx context.Context, chain []*object.Commit, checkedRemotes []string) []string {
	target := chain[0]
	lines := []string{"This rewrites git history: commit " + describeCommit(target) + " gets an Entire-Checkpoint trailer."}
	if len(chain) > 1 {
		lines = append(lines, fmt.Sprintf("The %d commit(s) after it are rewritten on top of it:", len(chain)-1))
		for _, c := range chain[1:] {
			lines = append(lines, "  "+describeCommit(c))
		}
	}
	if len(chain) == 1 {
		lines = append(lines, "Its SHA changes; its content doesn't.")
	} else {
		lines = append(lines, "Their SHAs change; their content doesn't.")
	}
	if len(checkedRemotes) > 0 {
		lines = append(lines, fmt.Sprintf("It isn't on %s. If you pushed it somewhere else, rewriting it means a force-push there.", strings.Join(checkedRemotes, ", ")))
	}
	if others := otherRefsContaining(ctx, target); len(others) > 0 {
		lines = append(lines, "These keep the old commits: "+strings.Join(others, ", "))
	}
	for _, c := range chain {
		if c.Signature != "" {
			lines = append(lines, "Signed commits lose their signatures.")
			break
		}
	}
	return lines
}

// otherRefsContaining lists the branches and tags, other than the checked-out
// branch, that contain target.
func otherRefsContaining(ctx context.Context, target *object.Commit) []string {
	current, _ := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "--short", "HEAD").Output() //nolint:errcheck // detached HEAD has none
	out, err := exec.CommandContext(ctx, "git", "for-each-ref", "--contains", target.Hash.String(), "--format=%(refname:short)", "refs/heads", "refs/tags").Output()
	if err != nil {
		return nil
	}
	var refs []string
	for _, ref := range strings.Fields(string(out)) {
		if ref != strings.TrimSpace(string(current)) {
			refs = append(refs, ref)
		}
	}
	return refs
}

// rewriteWithTrailer adds the checkpoint trailer to chain[0] and replays the
// rest of chain on top, keeping every tree, author and message, then moves the
// checked-out branch (or a detached HEAD) to the new tip if it is still at the
// old one. Trees are reused, so the worktree and index are untouched, and no
// commit hooks run. Session state that names the old commits is remapped as
// git's post-rewrite hook would.
func rewriteWithTrailer(ctx context.Context, w io.Writer, chain []*object.Commit, checkpointID id.CheckpointID) error {
	oldHead := chain[len(chain)-1].Hash.String()
	var pairs strings.Builder
	parents := chain[0].ParentHashes
	newSHA, targetSHA := "", ""
	for i, c := range chain {
		message := c.Message
		if i == 0 {
			message = trailers.AppendCheckpointTrailer(message, checkpointID.String())
		}
		args := []string{"commit-tree", c.TreeHash.String()}
		if i == 0 {
			for _, p := range parents {
				args = append(args, "-p", p.String())
			}
		} else {
			args = append(args, "-p", newSHA)
		}
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Stdin = strings.NewReader(message)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME="+c.Author.Name,
			"GIT_AUTHOR_EMAIL="+c.Author.Email,
			fmt.Sprintf("GIT_AUTHOR_DATE=@%d %s", c.Author.When.Unix(), c.Author.When.Format("-0700")),
		)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("rewrite commit %s: %w: %s", c.Hash.String()[:12], err, strings.TrimSpace(stderr.String()))
		}
		newSHA = strings.TrimSpace(string(out))
		if i == 0 {
			targetSHA = newSHA
		}
		fmt.Fprintf(&pairs, "%s %s\n", c.Hash, newSHA)
	}

	reason := "entire session attach: add Entire-Checkpoint " + checkpointID.String() + " to " + chain[0].Hash.String()[:12]
	updateArgs := []string{"update-ref", "-m", reason}
	if branch, err := exec.CommandContext(ctx, "git", "symbolic-ref", "-q", "HEAD").Output(); err == nil {
		updateArgs = append(updateArgs, strings.TrimSpace(string(branch)))
	} else {
		updateArgs = append(updateArgs, "--no-deref", "HEAD")
	}
	updateArgs = append(updateArgs, newSHA, oldHead)
	if out, err := exec.CommandContext(ctx, "git", updateArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("move HEAD to the rewritten commits (HEAD changed meanwhile?): %w: %s", err, strings.TrimSpace(string(out)))
	}

	if err := strategy.NewManualCommitStrategy().PostRewrite(ctx, "rebase", strings.NewReader(pairs.String())); err != nil {
		fmt.Fprintf(w, "warning: couldn't update session state for the rewritten commits: %v\n", err)
	}
	fmt.Fprintf(w, "  Added Entire-Checkpoint: %s to commit %s (was %s", checkpointID, targetSHA[:12], chain[0].Hash.String()[:12])
	if len(chain) > 1 {
		fmt.Fprintf(w, "; HEAD was %s", oldHead[:12])
	}
	fmt.Fprintln(w, ")")
	return nil
}
