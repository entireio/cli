package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
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
	proceed, err := askAttachConfirmation()
	if err != nil {
		return err
	}
	if !proceed {
		fmt.Fprintln(w, "Nothing was changed.")
		return errAttachDeclined
	}
	return nil
}

// askAttachConfirmation asks the user whether to go ahead, default No. A
// variable so tests can answer, and act while the question is open.
var askAttachConfirmation = func() (bool, error) {
	proceed := false
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Continue?").
			Affirmative("Yes").
			Negative("No").
			Value(&proceed),
	))
	if err := form.Run(); err != nil {
		return false, fmt.Errorf("prompt failed: %w", err)
	}
	return proceed, nil
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
	chain, err := commitsFromTargetToHead(ctx, repo, target, head)
	if err != nil {
		return nil, err
	}
	for _, c := range chain {
		if !replayable(c) {
			return nil, fmt.Errorf("commit %s has a non-UTF-8 encoding or extra headers that attach can't carry over when it rewrites it; push %s first, or add the trailer yourself", c.Hash.String()[:12], target.Hash.String()[:12])
		}
	}
	return chain, nil
}

// commitsFromTargetToHead lists target and the commits after it up to head,
// oldest first, refusing a target off the current branch and merges after it.
func commitsFromTargetToHead(ctx context.Context, repo *git.Repository, target, head *object.Commit) ([]*object.Commit, error) {
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

// replayable reports whether rewriteWithTrailer reproduces c faithfully apart
// from its signature: git commit-tree writes a UTF-8 commit with no extra
// headers.
func replayable(c *object.Commit) bool {
	return (c.Encoding == "" || strings.EqualFold(string(c.Encoding), "UTF-8")) && len(c.ExtraHeaders) == 0
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
	yourEmail := yourGitEmail(ctx)
	yoursSigned := false
	var theirsSigned []string
	for _, c := range chain {
		switch {
		case c.Signature == "":
		case committedBy(c, yourEmail):
			yoursSigned = true
		default:
			theirsSigned = append(theirsSigned, "  "+describeCommit(c))
		}
	}
	if yoursSigned {
		lines = append(lines, "Commits you signed are signed again with your signing key; one signed with another key loses its signature.")
	}
	if len(theirsSigned) > 0 {
		lines = append(lines, "These signed commits were committed by someone else and lose their signatures:")
		lines = append(lines, theirsSigned...)
	}
	return lines
}

// yourGitEmail is the email git commits as for the user, from user.email,
// committer.email or GIT_COMMITTER_EMAIL, or "" when it can't be read.
func yourGitEmail(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "var", "GIT_COMMITTER_IDENT").Output()
	if err != nil {
		return ""
	}
	_, rest, _ := strings.Cut(string(out), "<")
	email, _, ok := strings.Cut(rest, ">")
	if !ok {
		return ""
	}
	return email
}

// committedBy reports whether email, the user's, is c's committer. Empty never
// matches.
func committedBy(c *object.Commit, email string) bool {
	return email != "" && strings.EqualFold(c.Committer.Email, email)
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
// rest of chain on top, keeping every tree, author, committer and message (as
// git filter-branch does), then moves the
// checked-out branch (or a detached HEAD) to the new tip if it is still at the
// old one. Trees are reused, so the worktree and index are untouched, and no
// commit hooks run. A signed commit is signed again only as replayCommit
// allows; the others are reported. Session state that names the old commits is
// remapped as git's post-rewrite hook would.
func rewriteWithTrailer(ctx context.Context, w io.Writer, chain []*object.Commit, checkpointID id.CheckpointID) error {
	oldHead := chain[len(chain)-1].Hash.String()
	yourEmail := yourGitEmail(ctx)
	keys := signingProbe{}
	var pairs strings.Builder
	var unsigned []string
	newSHA, targetSHA := "", ""
	for i, c := range chain {
		message := c.Message
		if i == 0 {
			message = trailers.AppendCheckpointTrailer(message, checkpointID.String())
		}
		parents := []string{newSHA}
		if i == 0 {
			parents = make([]string, 0, len(c.ParentHashes))
			for _, p := range c.ParentHashes {
				parents = append(parents, p.String())
			}
		}
		sha, lost, err := replayCommit(ctx, c, message, parents, yourEmail, keys)
		if err != nil {
			return fmt.Errorf("rewrite commit %s: %w", c.Hash.String()[:12], err)
		}
		if lost != "" {
			unsigned = append(unsigned, fmt.Sprintf("commit %s (was %s) is no longer signed: %s", sha[:12], c.Hash.String()[:12], lost))
		}
		newSHA = sha
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
	for _, line := range unsigned {
		fmt.Fprintf(w, "  warning: %s\n", line)
	}
	return nil
}

// replayCommit writes c again with message on parents, keeping its tree,
// author and committer. A signed c is signed again, by the user's own git
// signing setup, only when the user committed it, its signature is good, and
// it was made by the key the user signs with now. Anything else is replayed
// unsigned, rather than with the user's signature on someone else's commit or
// on content their key never signed, and lost says why.
func replayCommit(ctx context.Context, c *object.Commit, message string, parents []string, yourEmail string, keys signingProbe) (sha, lost string, err error) {
	if c.Signature == "" {
		sha, err = commitTree(ctx, c, message, parents, false)
		return sha, "", err
	}
	if !committedBy(c, yourEmail) {
		lost = "it was committed by " + c.Committer.Email + ", not you"
	} else if key, good := signingKey(ctx, c.Hash.String()); !good {
		lost = "its signature can't be verified"
	} else if yours, err := keys.key(ctx, c.Committer); err != nil {
		lost = "signing failed: " + strings.Join(strings.Fields(err.Error()), " ")
	} else if key != yours {
		lost = "it was signed with a key other than your signing key"
	} else if signed, err := commitTree(ctx, c, message, parents, true); err != nil {
		lost = "signing failed: " + strings.Join(strings.Fields(err.Error()), " ")
	} else {
		return signed, "", nil
	}
	sha, err = commitTree(ctx, c, message, parents, false)
	return sha, lost, err
}

// commitTree runs git commit-tree for c's tree on parents with message and c's
// author and committer, signed when sign is set (commit-tree ignores
// commit.gpgsign), and returns the new commit's hash.
func commitTree(ctx context.Context, c *object.Commit, message string, parents []string, sign bool) (string, error) {
	args := []string{"commit-tree", c.TreeHash.String()}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	if sign {
		args = append(args, "-S")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = strings.NewReader(message)
	cmd.Env = append(withoutIdentityEnv(os.Environ()),
		"GIT_AUTHOR_NAME="+c.Author.Name,
		"GIT_AUTHOR_EMAIL="+c.Author.Email,
		fmt.Sprintf("GIT_AUTHOR_DATE=@%d %s", c.Author.When.Unix(), c.Author.When.Format("-0700")),
		"GIT_COMMITTER_NAME="+c.Committer.Name,
		"GIT_COMMITTER_EMAIL="+c.Committer.Email,
		fmt.Sprintf("GIT_COMMITTER_DATE=@%d %s", c.Committer.When.Unix(), c.Committer.When.Format("-0700")),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

// signingProbe finds the key the user's git signing setup signs with, by
// signing a commit of the empty tree, so that a commit is signed only once its
// own key is known to match. Without user.signingkey, OpenPGP picks the key by
// the committer, so the key is found once per committer.
type signingProbe map[string]probedKey

type probedKey struct {
	key string
	err error
}

func (p signingProbe) key(ctx context.Context, committer object.Signature) (string, error) {
	who := committer.Name + " <" + committer.Email + ">"
	if k, ok := p[who]; ok {
		return k.key, k.err
	}
	key, err := probeSigningKey(ctx, committer)
	p[who] = probedKey{key, err}
	return key, err
}

func probeSigningKey(ctx context.Context, committer object.Signature) (string, error) {
	tree, err := exec.CommandContext(ctx, "git", "mktree").Output()
	if err != nil {
		return "", fmt.Errorf("write the empty tree: %w", err)
	}
	probe := &object.Commit{TreeHash: plumbing.NewHash(strings.TrimSpace(string(tree))), Author: committer, Committer: committer}
	sha, err := commitTree(ctx, probe, "Entire signing key probe\n", nil, true)
	if err != nil {
		return "", err
	}
	key, _ := signingKey(ctx, sha)
	return key, nil
}

// signingKey is the key git reports for commit sha's signature (%GK): a key ID
// for OpenPGP and X.509, a fingerprint for SSH. Empty when the commit is
// unsigned or the signature can't be read. good is false unless the signature
// checks out: git reports the key of a bad signature too. git reports an SSH
// signature's key only with an allowed-signers file configured, so it is given
// an empty one and reports the key as untrusted (U), which is all comparing
// two signatures needs. The literal /dev/null, not os.DevNull: ssh-keygen
// opens it, and both Git for Windows' and Windows OpenSSH's ssh-keygen map
// that path. --no-show-signature keeps log.showSignature's verification text
// out of the output.
func signingKey(ctx context.Context, sha string) (key string, good bool) {
	out, err := exec.CommandContext(ctx, "git", "-c", "gpg.ssh.allowedSignersFile=/dev/null", "show", "-s", "--no-show-signature", "--format=%G?%n%GK", sha).Output()
	if err != nil {
		return "", false
	}
	status, key, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return key, key != "" && (status == "G" || status == "U")
}

// withoutIdentityEnv drops GIT_AUTHOR_* and GIT_COMMITTER_* from env, so the
// identity commitTree sets is the only one: with a duplicate, which
// one git sees depends on the platform's getenv.
func withoutIdentityEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		return strings.HasPrefix(kv, "GIT_AUTHOR_") || strings.HasPrefix(kv, "GIT_COMMITTER_")
	})
}
