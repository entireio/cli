package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const attachCommitTranscript = `{"type":"user","message":{"role":"user","content":"make the change"},"uuid":"u1"}
`

// commitAt adds a commit on top of HEAD in the current repo and returns it.
func commitAt(t *testing.T, name string) *object.Commit {
	t.Helper()
	dir := mustGetwd(t)
	testutil.WriteFile(t, dir, name, name)
	testutil.GitAdd(t, dir, name)
	testutil.GitCommit(t, dir, "add "+name)
	return headCommitOf(t)
}

func headCommitOf(t *testing.T) *object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(mustGetwd(t))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func commitByHash(t *testing.T, hash plumbing.Hash) *object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(mustGetwd(t))
	if err != nil {
		t.Fatal(err)
	}
	c, err := repo.CommitObject(hash)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func loadAttachState(t *testing.T, sessionID string) (*session.State, error) {
	t.Helper()
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		return nil, err
	}
	return store.Load(context.Background(), sessionID)
}

func readSummary(t *testing.T, cpID string) *cpkg.CheckpointSummary {
	t.Helper()
	repo, err := git.PlainOpen(mustGetwd(t))
	if err != nil {
		t.Fatal(err)
	}
	parsed, ok := trailers.ParseCheckpoint("Entire-Checkpoint: " + cpID)
	if !ok {
		t.Fatalf("bad checkpoint id %q", cpID)
	}
	summary, err := cpkg.NewGitStore(repo, cpkg.DefaultV1Refs()).Read(context.Background(), parsed)
	if err != nil || summary == nil {
		t.Fatalf("Read(%s) = %v, %v", cpID, summary, err)
	}
	return summary
}

// newBareRemote creates a bare repository whose HEAD names main, as a hosted
// remote's default branch would.
func newBareRemote(t *testing.T) string {
	t.Helper()
	remote := t.TempDir()
	testutil.RunGit(t, remote, "init", "--bare", "-q")
	testutil.RunGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	return remote
}

// pushToOrigin adds a bare "origin" and publishes HEAD to its default branch,
// main, so it contains every commit made so far.
func pushToOrigin(t *testing.T) string {
	t.Helper()
	dir := mustGetwd(t)
	remote := newBareRemote(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	testutil.RunGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	testutil.RunGit(t, dir, "fetch", "-q", "origin")
	return remote
}

// pushFromAnotherClone pushes refspec to remote from a fresh clone of this
// repository, so none of this repository's remote-tracking refs learn of it.
// Pushing by URL from here is not the same: since git 2.56 that updates the
// tracking refs of a configured remote whose URL matches.
func pushFromAnotherClone(t *testing.T, remote, refspec string) {
	t.Helper()
	other := t.TempDir()
	testutil.RunGit(t, other, "clone", "-q", mustGetwd(t), ".")
	testutil.RunGit(t, other, "push", "-q", remote, refspec)
}

func attachHeadless(t *testing.T, sessionID string, opts attachOptions) (string, error) {
	t.Helper()
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)
	var out bytes.Buffer
	opts.Force = true
	err := runAttach(context.Background(), &out, &out, sessionID, agent.AgentNameClaudeCode, opts)
	return out.String(), err
}

// The common attach — hooks missed the commit just made — targets an unpushed
// HEAD. A trailer is the right link there: nobody else has the commit, and the
// trailer survives a later rebase. It is amended in without a prompt, so a
// headless attach no longer leaves the checkpoint unlinked.
func TestAttachCommit_UnpushedHeadIsAmended(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")

	out, err := attachHeadless(t, "attach-unpushed-head", attachOptions{})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	head := headCommitOf(t)
	cpID, ok := trailers.ParseCheckpoint(head.Message)
	if !ok {
		t.Fatalf("unpushed HEAD was not amended with a trailer:\n%s", out)
	}
	if summary := readSummary(t, cpID.String()); len(summary.LinkedCommits) != 0 {
		t.Errorf("LinkedCommits = %v: a trailer-linked checkpoint needs no anchor", summary.LinkedCommits)
	}
}

// A pushed HEAD can't be amended without a force-push, so the link is recorded
// in the checkpoint and the commit is left alone.
func TestAttachCommit_PushedHeadIsLinkedInTheCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	remote := pushToOrigin(t)

	out, err := attachHeadless(t, "attach-pushed-head", attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash || strings.Contains(got.Message, "Entire-Checkpoint") {
		t.Fatalf("pushed HEAD was rewritten: %s %q", got.Hash, got.Message)
	}
	state, err := loadAttachState(t, "attach-pushed-head")
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	if summary := readSummary(t, state.LastCheckpointID.String()); len(summary.LinkedCommits) != 1 || summary.LinkedCommits[0].SHA != head.Hash.String() {
		t.Fatalf("LinkedCommits = %v, want [%s]", summary.LinkedCommits, head.Hash)
	}
	if state.BaseCommit != head.Hash.String() {
		t.Errorf("BaseCommit = %q, want HEAD so a still-running session keeps linking", state.BaseCommit)
	}
	if !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Errorf("expected the checkpoint to be pushed, got:\n%s", out)
	}
	if refs := testutil.RunGit(t, remote, "for-each-ref", "--format=%(refname)"); !strings.Contains(refs, "entire/checkpoints") {
		t.Errorf("remote has no checkpoint refs:\n%s", refs)
	}
}

// Under the git-refs primary the pushed-commit path pushes through the
// pre-push queue, and delivery is confirmed against the checkpoint's own ref.
// Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_PushedHeadIsDeliveredOnGitRefs(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	remote := pushToOrigin(t)

	out, err := attachHeadless(t, "attach-pushed-head-refs", attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("pushed HEAD was rewritten: %s", got.Hash)
	}
	state, err := loadAttachState(t, "attach-pushed-head-refs")
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	if !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Fatalf("expected a confirmed push, got:\n%s", out)
	}
	cpID := state.LastCheckpointID.String()
	local := strings.TrimSpace(testutil.RunGit(t, mustGetwd(t), "for-each-ref", "--format=%(refname) %(objectname)", "refs/entire"))
	remoteRefs := testutil.RunGit(t, remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/entire")
	found := false
	for _, line := range strings.Split(local, "\n") {
		if strings.Contains(line, cpID) {
			found = true
			if !strings.Contains(remoteRefs, line) {
				t.Fatalf("checkpoint ref %q is not on the remote:\n%s", line, remoteRefs)
			}
		}
	}
	if !found {
		t.Fatalf("no local ref names checkpoint %s:\n%s", cpID, local)
	}
}

func setAttachCheckpointPushPolicy(t *testing.T, policy string) {
	t.Helper()
	path := filepath.Join(mustGetwd(t), ".git", filepath.FromSlash(settings.CheckpointPushPolicyFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(policy+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func remoteCheckpointRefs(t *testing.T, remote string) string {
	t.Helper()
	return strings.TrimSpace(testutil.RunGit(t, remote, "for-each-ref", "--format=%(refname)", "refs/entire"))
}

// Under manual, a checkpoint linked to a pushed commit waits for
// `entire checkpoint push` like any other: attach sends nothing, says so, and
// succeeds. A rerun finds it still waiting and has nothing to confirm.
// Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_PushedHeadWaitsUnderManualPolicy(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	remote := pushToOrigin(t)
	setAttachCheckpointPushPolicy(t, "manual")
	const sessionID = "attach-pushed-head-manual"

	out, err := attachHeadless(t, sessionID, attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach under manual: %v\n%s", err, out)
	}
	state, err := loadAttachState(t, sessionID)
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	cpID := state.LastCheckpointID.String()
	if want := "entire checkpoint push " + cpID; !strings.Contains(out, want) || strings.Contains(out, "Pushed checkpoint metadata") {
		t.Fatalf("expected the waiting line naming %q, got:\n%s", want, out)
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("manual attach uploaded checkpoint refs:\n%s", refs)
	}
	outgoing, err := strategy.ListOutgoingCheckpoints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(outgoing.Checkpoints) != 1 || outgoing.Checkpoints[0].CheckpointID.String() != cpID {
		t.Fatalf("checkpoint %s should still be outgoing: %+v", cpID, outgoing.Checkpoints)
	}

	var rerun bytes.Buffer
	if err := runAttach(context.Background(), &rerun, &rerun, sessionID, agent.AgentNameClaudeCode, attachOptions{LinkWithoutTrailer: true}); err != nil {
		t.Fatalf("rerun without confirmation must succeed, since it sends nothing: %v\n%s", err, rerun.String())
	}
	if !strings.Contains(rerun.String(), "entire checkpoint push "+cpID) {
		t.Fatalf("rerun should say the checkpoint is still waiting:\n%s", rerun.String())
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("rerun uploaded checkpoint refs:\n%s", refs)
	}
}

// When queueing the new checkpoint fails, 'entire checkpoint push' would not
// send it, so attach must not say it is waiting.
// Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_FailedEnqueueIsNotReportedAsWaiting(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	if os.Geteuid() == 0 {
		t.Skip("root writes read-only files")
	}
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	setAttachCheckpointPushPolicy(t, "manual")
	queueFile := filepath.Join(mustGetwd(t), ".git", "entire-checkpoint-push-queue.jsonl")
	if err := os.WriteFile(queueFile, nil, 0o400); err != nil {
		t.Fatal(err)
	}

	out, err := attachHeadless(t, "attach-failed-enqueue", attachOptions{LinkWithoutTrailer: true})
	if err == nil || !strings.Contains(err.Error(), "not queued") {
		t.Fatalf("expected an error saying the checkpoint is not queued, got %v\n%s", err, out)
	}
	if strings.Contains(out, "is waiting") {
		t.Fatalf("a checkpoint that is not queued was reported as waiting:\n%s", out)
	}
}

// The confirmation must not promise an upload that manual will not make.
// Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_ManualConfirmationSaysTheCheckpointWaits(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	setAttachCheckpointPushPolicy(t, "manual")
	const sessionID = "attach-manual-unconfirmed"
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)

	var out, errOut bytes.Buffer
	if err := runAttach(context.Background(), &out, &errOut, sessionID, agent.AgentNameClaudeCode, attachOptions{LinkWithoutTrailer: true}); err == nil {
		t.Fatalf("attach without confirmation succeeded:\n%s", out.String())
	}
	if strings.Contains(errOut.String(), "pushed to origin now") || !strings.Contains(errOut.String(), "entire checkpoint push") {
		t.Errorf("expected the warning to say the checkpoint waits, got:\n%s", errOut.String())
	}
}

// An unreadable policy is not automatic: attach uploads nothing and names the
// file to fix. Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_UnreadablePolicyIsAnError(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	remote := pushToOrigin(t)
	setAttachCheckpointPushPolicy(t, "approved")

	out, err := attachHeadless(t, "attach-unreadable-policy", attachOptions{LinkWithoutTrailer: true})
	if err == nil || !strings.Contains(err.Error(), "checkpoint-push-policy") {
		t.Fatalf("expected an error naming the policy file, got %v\n%s", err, out)
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("an unreadable policy uploaded checkpoint refs:\n%s", refs)
	}
}

// A retry that finds the policy unreadable reports the file to fix, not the
// confirmation it cannot use: nothing would upload anyway.
// Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_RetryWithUnreadablePolicyNamesTheFile(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	setAttachCheckpointPushPolicy(t, "manual")
	const sessionID = "attach-retry-unreadable-policy"
	if out, err := attachHeadless(t, sessionID, attachOptions{LinkWithoutTrailer: true}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	setAttachCheckpointPushPolicy(t, "approved")

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{LinkWithoutTrailer: true})
	if err == nil || !strings.Contains(err.Error(), "checkpoint-push-policy") {
		t.Fatalf("expected an error naming the policy file, got %v\n%s", err, out.String())
	}
}

// Delivery follows the policy attach warned about, not a later reread: a
// policy changed by another worktree mid-confirmation must not upload a
// session the user was told stays local.
// Not parallel: sets ENTIRE_CHECKPOINTS_PRIMARY.
func TestAttachCommit_DeliveryUsesThePolicyAttachWarnedAbout(t *testing.T) {
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	remote := pushToOrigin(t)
	setAttachCheckpointPushPolicy(t, "manual")
	const sessionID = "attach-policy-changes-mid-attach"
	if out, err := attachHeadless(t, sessionID, attachOptions{LinkWithoutTrailer: true}); err != nil {
		t.Fatalf("attach: %v\n%s", err, out)
	}
	state, err := loadAttachState(t, sessionID)
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	setAttachCheckpointPushPolicy(t, "automatic")

	plan := attachLinkPlan{target: headCommitOf(t), mode: attachRecordLink, remote: "origin", holder: "origin", heldForManualPush: true}
	var out bytes.Buffer
	if err := pushAttachedCheckpoint(context.Background(), &out, plan, state.LastCheckpointID); err != nil {
		t.Fatalf("pushAttachedCheckpoint: %v\n%s", err, out.String())
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("uploaded after warning that the checkpoint stays local:\n%s", refs)
	}
}

// attachLegacyCheckpoint makes a commit with a legacy hex checkpoint under the
// git-branch backend, publishes both to a new origin, and switches the primary
// to git-refs, as a repo looks after the switch. Callers must not use
// t.Parallel(): it sets ENTIRE_CHECKPOINTS_PRIMARY.
func attachLegacyCheckpoint(t *testing.T) (target *object.Commit, cpID checkpointid.CheckpointID, remote string) {
	t.Helper()
	setupAttachTestRepo(t)
	if out, err := attachHeadless(t, "attach-legacy-first", attachOptions{}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	target = headCommitOf(t)
	cpID, ok := trailers.ParseCheckpoint(target.Message)
	if !ok || cpID.Kind() == checkpointid.KindULID {
		t.Fatalf("expected a legacy checkpoint trailer, got %q", target.Message)
	}
	remote = pushToOrigin(t)
	testutil.RunGit(t, mustGetwd(t), "push", "-q", "origin", paths.MetadataBranchName+":"+paths.MetadataBranchName)
	t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", "git-refs")
	return target, cpID, remote
}

// remoteCheckpointRef returns the commit the remote's ref for cpID points at,
// or "" when the remote has no such ref.
func remoteCheckpointRef(t *testing.T, remote string, cpID checkpointid.CheckpointID) string {
	t.Helper()
	ref, err := cpkg.RefName(cpID)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(testutil.RunGit(t, remote, "for-each-ref", "--format=%(objectname)", ref.String()))
}

// checkpointRefSessionIDs lists the session IDs stored in the checkpoint ref
// for cpID in the repository at dir, in session-index order.
func checkpointRefSessionIDs(t *testing.T, dir string, cpID checkpointid.CheckpointID) []string {
	t.Helper()
	ref, err := cpkg.RefName(cpID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; ; i++ {
		if strings.TrimSpace(testutil.RunGit(t, dir, "ls-tree", "--name-only", ref.String(), strconv.Itoa(i))) == "" {
			return ids
		}
		var meta struct {
			SessionID string `json:"session_id"`
		}
		raw := testutil.RunGit(t, dir, "show", fmt.Sprintf("%s:%d/metadata.json", ref, i))
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatalf("session %d metadata: %v", i, err)
		}
		ids = append(ids, meta.SessionID)
	}
}

// After the switch to git-refs, attaching to a pre-switch commit copies its
// legacy checkpoint into its own ref and pushes that ref; v1 is never pushed
// under git-refs, so writing the session there would leave it local.
func TestAttachCommit_LegacyCheckpointIsDeliveredOnGitRefs(t *testing.T) {
	target, cpID, remote := attachLegacyCheckpoint(t)

	out, err := attachHeadless(t, "attach-legacy-second", attachOptions{Commit: target.Hash.String()})
	if err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Fatalf("expected a confirmed push, got:\n%s", out)
	}
	ref, err := cpkg.RefName(cpID)
	if err != nil {
		t.Fatal(err)
	}
	local := strings.TrimSpace(testutil.RunGit(t, mustGetwd(t), "rev-parse", ref.String()))
	if got := remoteCheckpointRef(t, remote, cpID); got != local {
		t.Fatalf("remote checkpoint ref = %q, want the local %s", got, local)
	}
	if got := checkpointRefSessionIDs(t, remote, cpID); !slices.Equal(got, []string{"attach-legacy-first", "attach-legacy-second"}) {
		t.Fatalf("remote checkpoint sessions = %v, want both", got)
	}
}

// Under manual, the copied ref waits for 'entire checkpoint push' like any
// other checkpoint, and nothing is uploaded.
func TestAttachCommit_LegacyCheckpointUnderManualWaits(t *testing.T) {
	target, cpID, remote := attachLegacyCheckpoint(t)
	setAttachCheckpointPushPolicy(t, "manual")

	out, err := attachHeadless(t, "attach-legacy-second", attachOptions{Commit: target.Hash.String()})
	if err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	if want := "entire checkpoint push " + cpID.String(); !strings.Contains(out, want) || !strings.Contains(out, cpID.String()+" is waiting") {
		t.Fatalf("expected the waiting line naming %q, got:\n%s", want, out)
	}
	if strings.Contains(out, "migrate-checkpoints") {
		t.Fatalf("attach must not point to migration, got:\n%s", out)
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("manual attach uploaded checkpoint refs:\n%s", refs)
	}
}

// A checkpoint on this clone's deleted list can still be visible, through a
// remote the delete could not reach or a copy another clone pushed back.
// Attaching to it would write it again: to v1, which the next push sends on
// git-branch, undoing the delete. So attach refuses before writing anything.
func TestAttachCommit_RefusesACheckpointDeletedFromThisClone(t *testing.T) {
	for _, backend := range []string{"git-branch", "git-refs"} {
		t.Run(backend, func(t *testing.T) {
			target, cpID, remote := attachLegacyCheckpoint(t)
			t.Setenv("ENTIRE_CHECKPOINTS_PRIMARY", backend)
			dir := mustGetwd(t)
			repo, err := git.PlainOpen(dir)
			if err != nil {
				t.Fatal(err)
			}
			deleted, err := cpkg.DeletedCheckpointsForRepo(repo)
			if err != nil {
				t.Fatal(err)
			}
			if err := deleted.Record(cpID); err != nil {
				t.Fatal(err)
			}
			checkpointRefs := func(repoDir string) string {
				return testutil.RunGit(t, repoDir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/entire", "refs/entire")
			}
			localBefore, remoteBefore := checkpointRefs(dir), checkpointRefs(remote)

			out, err := attachHeadless(t, "attach-deleted", attachOptions{Commit: target.Hash.String()})
			if err == nil || !strings.Contains(err.Error(), "was deleted from this clone") {
				t.Fatalf("expected attach to refuse the deleted checkpoint, got %v\n%s", err, out)
			}
			if got := checkpointRefs(dir); got != localBefore {
				t.Errorf("local checkpoint refs changed:\nbefore:\n%s\nafter:\n%s", localBefore, got)
			}
			if got := checkpointRefs(remote); got != remoteBefore {
				t.Errorf("remote checkpoint refs changed:\nbefore:\n%s\nafter:\n%s", remoteBefore, got)
			}
		})
	}
}

// Two clones that each attach a session to the same legacy checkpoint both
// deliver, and neither session is lost: the second clone builds on the ref the
// first one pushed instead of copying v1 again.
func TestAttachCommit_LegacyCheckpointFromTwoClonesKeepsBothSessions(t *testing.T) {
	target, cpID, remote := attachLegacyCheckpoint(t)

	other := t.TempDir()
	testutil.RunGit(t, other, "clone", "-q", "-b", "main", remote, ".")
	testutil.RunGit(t, other, "fetch", "-q", "origin", paths.MetadataBranchName+":"+paths.MetadataBranchName)

	out, err := attachHeadless(t, "attach-legacy-clone-a", attachOptions{Commit: target.Hash.String()})
	if err != nil {
		t.Fatalf("attach from the first clone: %v\n%s", err, out)
	}

	t.Chdir(other)
	enableEntire(t, other)
	out, err = attachHeadless(t, "attach-legacy-clone-b", attachOptions{Commit: target.Hash.String()})
	if err != nil {
		t.Fatalf("attach from the second clone: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Fatalf("expected a confirmed push from the second clone, got:\n%s", out)
	}
	want := []string{"attach-legacy-first", "attach-legacy-clone-a", "attach-legacy-clone-b"}
	if got := checkpointRefSessionIDs(t, remote, cpID); !slices.Equal(got, want) {
		t.Fatalf("remote checkpoint sessions = %v, want %v", got, want)
	}
}

// An older pushed commit is linked in the checkpoint, never rewritten.
func TestAttachCommit_PushedOlderCommitIsLinkedInTheCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	target := commitAt(t, "work.txt")
	head := commitAt(t, "later.txt")
	pushToOrigin(t)

	out, err := attachHeadless(t, "attach-pushed-older", attachOptions{Commit: target.Hash.String()[:10], LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if got := headCommitOf(t).Hash; got != head.Hash {
		t.Fatalf("HEAD moved from %s to %s", head.Hash, got)
	}
	if msg := commitByHash(t, target.Hash).Message; strings.Contains(msg, "Entire-Checkpoint") {
		t.Fatalf("target commit gained a trailer: %q", msg)
	}
	state, err := loadAttachState(t, "attach-pushed-older")
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	summary := readSummary(t, state.LastCheckpointID.String())
	if len(summary.LinkedCommits) != 1 || summary.LinkedCommits[0].SHA != target.Hash.String() {
		t.Fatalf("LinkedCommits = %v, want [%s]", summary.LinkedCommits, target.Hash)
	}
	if state.BaseCommit != "" {
		t.Errorf("BaseCommit = %q: an older commit must not make the session link future HEAD commits", state.BaseCommit)
	}
	if !strings.Contains(out, target.Hash.String()[:7]) {
		t.Errorf("output should name the linked commit, got:\n%s", out)
	}
}

// An older unpushed commit is treated like HEAD: it gets the trailer, and the
// commits after it are replayed on top with the same trees, so the worktree is
// untouched and only SHAs change.
func TestAttachCommit_UnpushedOlderCommitGetsTheTrailer(t *testing.T) {
	setupAttachTestRepo(t)
	target := commitAt(t, "work.txt")
	// A committer other than whoever runs attach, which the rewrite must keep.
	dir := mustGetwd(t)
	testutil.WriteFile(t, dir, "later.txt", "later.txt")
	testutil.GitAdd(t, dir, "later.txt")
	testutil.RunGit(t, dir, "-c", "user.name=Original Committer", "-c", "user.email=original@example.com", "commit", "-q", "-m", "add later.txt")
	later := headCommitOf(t)
	out, err := attachHeadless(t, "attach-unpushed-older", attachOptions{Commit: target.Hash.String()})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	head := headCommitOf(t)
	if head.Hash == later.Hash || head.TreeHash != later.TreeHash || head.Message != later.Message {
		t.Fatalf("HEAD should be the replayed later commit with the same tree and message: %s %q", head.Hash, head.Message)
	}
	if head.Author.String() != later.Author.String() || head.Committer.String() != later.Committer.String() || !head.Committer.When.Equal(later.Committer.When) {
		t.Fatalf("replay changed author or committer: %v / %v, want %v / %v", head.Author, head.Committer, later.Author, later.Committer)
	}
	parent, err := head.Parent(0)
	if err != nil {
		t.Fatal(err)
	}
	cpID, ok := trailers.ParseCheckpoint(parent.Message)
	if !ok || parent.TreeHash != target.TreeHash {
		t.Fatalf("the target should be rewritten with the trailer and its tree: %q", parent.Message)
	}
	if _, ok := trailers.ParseCheckpoint(head.Message); ok {
		t.Errorf("only the target should get the trailer: %q", head.Message)
	}
	if summary := readSummary(t, cpID.String()); len(summary.LinkedCommits) != 0 {
		t.Errorf("LinkedCommits = %v: a trailer-linked checkpoint needs no anchor", summary.LinkedCommits)
	}
}

// A merge after the target can't be replayed by a message-only rewrite, so it
// is refused before anything is written.
func TestAttachCommit_RefusesToRewriteAcrossAMerge(t *testing.T) {
	setupAttachTestRepo(t)
	dir := mustGetwd(t)
	target := commitAt(t, "work.txt")
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "side")
	commitAt(t, "side.txt")
	testutil.RunGit(t, dir, "checkout", "-q", "-")
	commitAt(t, "main.txt")
	testutil.RunGit(t, dir, "merge", "-q", "--no-edit", "side")
	head := headCommitOf(t)

	out, err := attachHeadless(t, "attach-across-merge", attachOptions{Commit: target.Hash.String()})
	if err == nil || !strings.Contains(err.Error(), "is a merge") {
		t.Fatalf("err = %v, want a refusal naming the merge\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("HEAD changed: %s", got.Hash)
	}
}

// A commit that already carries a checkpoint is already linked: the session
// joins that checkpoint and nothing else is written.
func TestAttachCommit_JoinsTheCommitsExistingCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	if out, err := attachHeadless(t, "attach-join-first", attachOptions{}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	target := headCommitOf(t)
	cpID, ok := trailers.ParseCheckpoint(target.Message)
	if !ok {
		t.Fatalf("first attach left no trailer: %q", target.Message)
	}
	commitAt(t, "later.txt")

	if out, err := attachHeadless(t, "attach-join-second", attachOptions{Commit: target.Hash.String()}); err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	summary := readSummary(t, cpID.String())
	if len(summary.Sessions) != 2 {
		t.Fatalf("checkpoint has %d sessions, want 2", len(summary.Sessions))
	}
	if len(summary.LinkedCommits) != 0 {
		t.Errorf("LinkedCommits = %v: a trailer-linked checkpoint needs no anchor", summary.LinkedCommits)
	}
}

// A link recorded in the checkpoint only counts when the commit's author
// attaches it; say so up front rather than let it fail quietly on the server.
func TestAttachCommit_WarnsWhenNotTheCommitsAuthor(t *testing.T) {
	setupAttachTestRepo(t)
	dir := mustGetwd(t)
	testutil.WriteFile(t, dir, "theirs.txt", "theirs")
	testutil.GitAdd(t, dir, "theirs.txt")
	testutil.RunGit(t, dir, "-c", "user.name=Someone Else", "-c", "user.email=someone@example.com", "commit", "-q", "-m", "their commit")
	pushToOrigin(t)

	out, err := attachHeadless(t, "attach-not-author", attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if !strings.Contains(out, "someone@example.com") {
		t.Errorf("expected a warning naming the commit's author, got:\n%s", out)
	}
}

func TestAttachCommit_RejectsWhatIsNotACommit(t *testing.T) {
	setupAttachTestRepo(t)
	_, err := attachHeadless(t, "attach-bad-rev", attachOptions{Commit: "no-such-revision"})
	if err == nil || !strings.Contains(err.Error(), "no-such-revision") {
		t.Fatalf("err = %v, want an error naming the revision", err)
	}
}

// explain <commit> finds a checkpoint linked without a trailer.
func TestAttachCommit_ExplainFindsTheLinkedCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	target := commitAt(t, "work.txt")
	commitAt(t, "later.txt")
	pushToOrigin(t)
	sessionID := "attach-commit-explain"
	if out, err := attachHeadless(t, sessionID, attachOptions{Commit: target.Hash.String(), LinkWithoutTrailer: true}); err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	state, err := loadAttachState(t, sessionID)
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}

	var explainOut, explainErr bytes.Buffer
	if err := runExplainAuto(context.Background(), &explainOut, &explainErr, target.Hash.String(), true, false, false, false, false, false, false, 0); err != nil {
		t.Fatalf("explain: %v\n%s", err, explainErr.String())
	}
	got := explainOut.String()
	if strings.Contains(got, "no Entire-Checkpoint trailer") || !strings.Contains(got, state.LastCheckpointID.String()) {
		t.Fatalf("explain did not resolve the linked checkpoint %s:\n%s", state.LastCheckpointID, got)
	}
	// The linked commit has no trailer, but the checkpoint lists it.
	if strings.Contains(got, "(none on this branch)") || !strings.Contains(got, target.Hash.String()[:7]) {
		t.Errorf("explain should list the linked commit %s:\n%s", target.Hash.String()[:7], got)
	}
	// A recorded link can't be verified locally, so it is labelled as one.
	if !strings.Contains(got, "(recorded link, unverified)") {
		t.Errorf("the linked commit should be labelled as an unverified recorded link:\n%s", got)
	}
	if !strings.Contains(explainErr.String(), "can't verify who recorded the link") {
		t.Errorf("explain <commit> should note the link is unverified:\n%s", explainErr.String())
	}
}

func TestCheckpointsLinkedTo(t *testing.T) {
	t.Parallel()
	a, b := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	infos := []cpkg.CheckpointInfo{
		{CheckpointID: "111111111111", LinkedCommits: []cpkg.LinkedCommit{{SHA: a}}},
		{CheckpointID: "222222222222"},
		{CheckpointID: "333333333333", LinkedCommits: []cpkg.LinkedCommit{{SHA: b}, {SHA: a}}},
	}
	got := cpkg.CheckpointsLinkedTo(infos, a)
	if len(got) != 2 || got[0] != "111111111111" || got[1] != "333333333333" {
		t.Fatalf("CheckpointsLinkedTo(a) = %v", got)
	}
	if got := cpkg.CheckpointsLinkedTo(infos, "cccccccccccccccccccccccccccccccccccccccc"); len(got) != 0 {
		t.Fatalf("CheckpointsLinkedTo(unlinked) = %v", got)
	}
}

type listingAttributionStub struct {
	attributionCheckpointReaderStub

	infos   []cpkg.CheckpointInfo
	listErr error
	lists   int
}

func (s *listingAttributionStub) List(context.Context) ([]cpkg.CheckpointInfo, error) {
	s.lists++
	return s.infos, s.listErr
}

// blame/why treat a commit linked by attach --commit like a trailer-linked one,
// listing the store once per run.
func TestAttributionResolver_LinkedCheckpoints(t *testing.T) {
	t.Parallel()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store := &listingAttributionStub{infos: []cpkg.CheckpointInfo{
		{CheckpointID: "111111111111", LinkedCommits: []cpkg.LinkedCommit{{SHA: sha}}},
	}}
	commit := &object.Commit{Hash: plumbing.NewHash(sha)}
	r := &attributionResolver{ctx: context.Background(), store: store}
	for range 2 {
		if got := r.linkedCheckpoints(commit); len(got) != 1 || got[0] != "111111111111" {
			t.Fatalf("linkedCheckpoints = %v", got)
		}
	}
	if store.lists != 1 {
		t.Fatalf("store listed %d times, want once per run", store.lists)
	}

	plain := &attributionResolver{ctx: context.Background(), store: &attributionCheckpointReaderStub{}}
	if got := plain.linkedCheckpoints(commit); len(got) != 0 {
		t.Fatalf("a store that cannot list links nothing, got %v", got)
	}
	failing := &attributionResolver{ctx: context.Background(), store: &listingAttributionStub{listErr: context.Canceled}}
	if got := failing.linkedCheckpoints(commit); len(got) != 0 {
		t.Fatalf("a failed listing links nothing, got %v", got)
	}
}

type countingStubReader struct {
	listingAttributionStub

	reads int
}

func (s *countingStubReader) Read(context.Context, checkpointid.CheckpointID) (*cpkg.CheckpointSummary, error) {
	s.reads++
	return &cpkg.CheckpointSummary{}, nil
}

// Blame reads a remote-discovered stub for its links once per run, not once per
// trailer-less commit.
func TestAttributionResolver_ReadsEachStubOncePerRun(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := &countingStubReader{listingAttributionStub: listingAttributionStub{infos: []cpkg.CheckpointInfo{
		{CheckpointID: "111111111111", ListedStub: true, CreatedAt: now},
	}}}
	r := &attributionResolver{ctx: context.Background(), store: store}
	for _, sha := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		r.linkedCheckpoints(&object.Commit{Hash: plumbing.NewHash(sha), Committer: object.Signature{When: now}})
	}
	if store.reads != 1 {
		t.Fatalf("stub read %d times, want once per run", store.reads)
	}
}

// A pushed commit already linked by an earlier attach is linked: a second
// session joins that checkpoint, like the trailer paths, rather than starting a
// separate checkpoint for the same commit.
func TestAttachCommit_SecondSessionJoinsTheRecordedLinkCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	pushToOrigin(t)

	if out, err := attachHeadless(t, "attach-recorded-first", attachOptions{LinkWithoutTrailer: true}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	first, err := loadAttachState(t, "attach-recorded-first")
	if err != nil || first == nil {
		t.Fatalf("load first state: %v, %v", first, err)
	}
	out, err := attachHeadless(t, "attach-recorded-second", attachOptions{})
	if err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	second, err := loadAttachState(t, "attach-recorded-second")
	if err != nil || second == nil {
		t.Fatalf("load second state: %v, %v", second, err)
	}
	if second.LastCheckpointID != first.LastCheckpointID {
		t.Fatalf("second session got checkpoint %s, want the commit's existing %s", second.LastCheckpointID, first.LastCheckpointID)
	}
	summary := readSummary(t, first.LastCheckpointID.String())
	if len(summary.Sessions) != 2 {
		t.Fatalf("checkpoint has %d sessions, want 2", len(summary.Sessions))
	}
	if len(summary.LinkedCommits) != 1 || summary.LinkedCommits[0].SHA != head.Hash.String() {
		t.Errorf("LinkedCommits = %v, want just [%s]", summary.LinkedCommits, head.Hash)
	}
}

// A pushed commit whose linked checkpoint isn't in the local store yet (a fresh
// clone, a deleted local metadata ref) is joined, not duplicated: attach finds
// it through the remote-tracking copy, fetches it into the local store through
// the availability guard, and appends — never rebuilding it from scratch.
func TestAttachCommit_JoinsARemoteOnlyLinkedCheckpointAfterFetchingIt(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	if out, err := attachHeadless(t, "attach-remote-only-first", attachOptions{LinkWithoutTrailer: true}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	first, err := loadAttachState(t, "attach-remote-only-first")
	if err != nil || first == nil {
		t.Fatalf("load first state: %v, %v", first, err)
	}
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "update-ref", "-d", "refs/heads/entire/checkpoints/v1")

	if out, err := attachHeadless(t, "attach-remote-only-second", attachOptions{}); err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	second, err := loadAttachState(t, "attach-remote-only-second")
	if err != nil || second == nil {
		t.Fatalf("load second state: %v, %v", second, err)
	}
	if second.LastCheckpointID != first.LastCheckpointID {
		t.Fatalf("second attach started checkpoint %s; want it to join the commit's existing %s", second.LastCheckpointID, first.LastCheckpointID)
	}
	if summary := readSummary(t, first.LastCheckpointID.String()); len(summary.Sessions) != 2 {
		t.Fatalf("checkpoint has %d sessions, want both (the original kept, the new one appended)", len(summary.Sessions))
	}
}

// Remote-tracking refs can be stale: a commit someone already pushed from
// another clone reads as unpushed here. attach fetches before deciding, so it
// sees the commit on the default branch and refuses instead of amending it.
func TestAttachCommit_FetchesBeforeDecidingAHeadIsUnpushed(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	remote := newBareRemote(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	pushFromAnotherClone(t, remote, "HEAD:refs/heads/main")
	if refs := testutil.RunGit(t, dir, "branch", "-r"); strings.TrimSpace(refs) != "" {
		t.Fatalf("expected no remote-tracking refs before attach, got %q", refs)
	}

	out, err := attachHeadless(t, "attach-stale-tracking", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "origin/main, a default branch") {
		t.Fatalf("err = %v, want a refusal naming origin/main\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash || strings.Contains(got.Message, "Entire-Checkpoint") {
		t.Fatalf("a commit the remote already holds was amended: %s %q\n%s", got.Hash, got.Message, out)
	}
}

// Finding 1: a failed fetch is not evidence the commit is unpushed. attach
// refuses to amend rather than rewrite a commit it couldn't check.
func TestAttachCommit_RefusesToAmendWhenAFetchFails(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", filepath.Join(t.TempDir(), "missing.git"))

	out, err := attachHeadless(t, "attach-fetch-fails", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "couldn't confirm") {
		t.Fatalf("err = %v, want a refusal explaining the commit's push state couldn't be confirmed\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("HEAD was rewritten after a failed fetch: %s", got.Hash)
	}
}

// A session that already has a checkpoint can be attached to another commit:
// it goes into that commit's checkpoint, which records only the turns since
// its previous one.
func TestAttachCommit_SessionCanBeAttachedToASecondCommit(t *testing.T) {
	setupAttachTestRepo(t)
	const sessionID = "attach-second-commit"
	if out, err := attachHeadless(t, sessionID, attachOptions{}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	first, ok := trailers.ParseCheckpoint(headCommitOf(t).Message)
	if !ok {
		t.Fatal("first attach left no trailer")
	}
	transcript := attachCommitTranscript + `{"type":"user","message":{"role":"user","content":"one more change"},"uuid":"u2"}
`
	setupClaudeTranscript(t, sessionID, transcript)
	commitAt(t, "second.txt")

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("second attach: %v\n%s", err, out.String())
	}
	second, ok := trailers.ParseCheckpoint(headCommitOf(t).Message)
	if !ok || second == first {
		t.Fatalf("second commit should get its own checkpoint, got %q (first %s)", headCommitOf(t).Message, first)
	}
	if !strings.Contains(out.String(), "1 turn") {
		t.Errorf("second checkpoint should record only the new turn:\n%s", out.String())
	}
}

// A pushed HEAD that already carries a checkpoint is joined, never rewritten.
func TestAttachCommit_PushedHeadWithACheckpointIsJoined(t *testing.T) {
	setupAttachTestRepo(t)
	if out, err := attachHeadless(t, "attach-pushed-join-a", attachOptions{}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	head := headCommitOf(t)
	cpID, _ := trailers.ParseCheckpoint(head.Message)
	pushToOrigin(t)

	out, err := attachHeadless(t, "attach-pushed-join-b", attachOptions{})
	if err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("pushed HEAD was rewritten: %s", got.Hash)
	}
	if summary := readSummary(t, cpID.String()); len(summary.Sessions) != 2 {
		t.Fatalf("checkpoint has %d sessions, want 2", len(summary.Sessions))
	}
	// The commit is pushed, so no later push of it may carry the checkpoint.
	if !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Fatalf("joined checkpoint of a pushed commit was not pushed:\n%s", out)
	}
}

// Joining the checkpoint of an unpushed commit leaves it for the next git push.
func TestAttachCommit_UnpushedHeadWithACheckpointWaitsForPush(t *testing.T) {
	setupAttachTestRepo(t)
	if out, err := attachHeadless(t, "attach-unpushed-join-a", attachOptions{}); err != nil {
		t.Fatalf("first attach: %v\n%s", err, out)
	}
	out, err := attachHeadless(t, "attach-unpushed-join-b", attachOptions{})
	if err != nil {
		t.Fatalf("second attach: %v\n%s", err, out)
	}
	if strings.Contains(out, "Pushed checkpoint metadata") {
		t.Fatalf("pushed the checkpoint of an unpushed commit:\n%s", out)
	}
	if !strings.Contains(out, "pushed with your next git push") {
		t.Fatalf("warning doesn't say when the checkpoint is pushed:\n%s", out)
	}
}

// Only the remotes the branch pushes to are checked, so an unrelated remote
// that can't be reached (an old fork, say) doesn't block amending an unpushed
// HEAD.
func TestAttachCommit_IgnoresAnUnreachableUnrelatedRemote(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	testutil.RunGit(t, mustGetwd(t), "remote", "add", "oldfork", filepath.Join(t.TempDir(), "missing.git"))
	commitAt(t, "more.txt")

	out, err := attachHeadless(t, "attach-unrelated-remote", attachOptions{})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if _, ok := trailers.ParseCheckpoint(headCommitOf(t).Message); !ok {
		t.Fatalf("unpushed HEAD was not amended:\n%s", out)
	}
}

// Any remote's tracking ref holding the commit means it is shared — upstream
// in a fork, say — so it is never rewritten: on upstream's default branch it
// is refused, and with --link-without-trailer the link's checkpoint still goes
// to the branch's own remote, not to the unrelated one.
func TestAttachCommit_TrackingRefOfAnotherRemoteBlocksTheRewrite(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	dir := mustGetwd(t)
	head := commitAt(t, "more.txt")
	testutil.RunGit(t, dir, "remote", "add", "upstream", filepath.Join(t.TempDir(), "missing.git"))
	testutil.RunGit(t, dir, "update-ref", "refs/remotes/upstream/main", "HEAD")

	out, err := attachHeadless(t, "attach-upstream-tracking-ref", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "upstream/main, a default branch") {
		t.Fatalf("err = %v, want a refusal naming upstream/main\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("a commit upstream holds was rewritten: %s\n%s", got.Hash, out)
	}
	out, err = attachHeadless(t, "attach-upstream-tracking-ref", attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach --link-without-trailer: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("a commit upstream holds was rewritten: %s\n%s", got.Hash, out)
	}
	if !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Fatalf("want the link's checkpoint pushed to origin:\n%s", out)
	}
}

// commit-tree writes UTF-8 with no extra headers, so a commit it can't
// reproduce is refused rather than silently changed.
// The same holds when the commit is HEAD, the common case.
func TestAttachCommit_RefusesToRewriteANonUTF8Head(t *testing.T) {
	setupAttachTestRepo(t)
	dir := mustGetwd(t)
	testutil.WriteFile(t, dir, "work.txt", "work")
	testutil.GitAdd(t, dir, "work.txt")
	testutil.RunGit(t, dir, "-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "-m", "add work.txt")
	head := headCommitOf(t)

	out, err := attachHeadless(t, "attach-non-utf8-head", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "non-UTF-8") {
		t.Fatalf("err = %v, want a refusal naming the encoding\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("HEAD changed: %s", got.Hash)
	}
}

func TestAttachCommit_RefusesToRewriteANonUTF8Commit(t *testing.T) {
	setupAttachTestRepo(t)
	dir := mustGetwd(t)
	testutil.WriteFile(t, dir, "work.txt", "work")
	testutil.GitAdd(t, dir, "work.txt")
	testutil.RunGit(t, dir, "-c", "i18n.commitEncoding=ISO-8859-1", "commit", "-q", "-m", "add work.txt")
	target := headCommitOf(t)
	head := commitAt(t, "later.txt")

	out, err := attachHeadless(t, "attach-non-utf8", attachOptions{Commit: target.Hash.String()})
	if err == nil || !strings.Contains(err.Error(), "non-UTF-8") {
		t.Fatalf("err = %v, want a refusal naming the encoding\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("HEAD changed: %s", got.Hash)
	}
}

// Finding 1: a remote's branch tip is checked directly, so a commit pushed to a
// branch this clone's fetch refspec doesn't track still reads as pushed. Attach
// can't name the branch holding it, so it can't tell whether it is this
// branch's own copy, and refuses rather than rewrite a shared commit.
func TestAttachCommit_RefusesACommitPushedToAnUntrackedBranch(t *testing.T) {
	setupAttachTestRepo(t)
	pushed := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	remote := newBareRemote(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	testutil.RunGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	testutil.RunGit(t, dir, "push", "-q", remote, "HEAD~1:refs/heads/main")
	testutil.RunGit(t, dir, "push", "-q", remote, "HEAD:refs/heads/someone-elses-branch")

	out, err := attachHeadless(t, "attach-untracked-branch", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "couldn't tell which remote branches hold it") || !strings.Contains(err.Error(), "--link-without-trailer") {
		t.Fatalf("err = %v, want a refusal naming the flag\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("a commit on a branch attach can't name was rewritten: %s", got.Hash)
	}
}

// A pushed commit that another branch also holds (a teammate's branch this one
// was built on, a release branch) is shared: rewriting it would leave their
// copy without the trailer, so attach refuses and names the branch and the
// flag, even though no default branch holds it.
func TestAttachCommit_RefusesACommitAnotherBranchHolds(t *testing.T) {
	setupAttachTestRepo(t)
	_, remote := pushFeatureBranch(t)
	dir := mustGetwd(t)
	head := headCommitOf(t)
	testutil.RunGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/teammate")
	testutil.RunGit(t, dir, "fetch", "-q", "origin")

	out, err := attachHeadless(t, "attach-shared-branch", attachOptions{})
	if err == nil {
		t.Fatalf("attach rewrote a commit another branch holds:\n%s", out)
	}
	for _, want := range []string{"is also on origin/teammate", "--link-without-trailer", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "origin/feature") {
		t.Errorf("the branch's own pushed copy isn't another branch:\n%v", err)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("a shared commit was rewritten: %s", got.Hash)
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("a checkpoint was pushed after the refusal:\n%s", refs)
	}
}

// Finding 2: a recorded link only exists in the checkpoint, so attach must not
// report success when the checkpoint never reached the remote.
func TestAttachCommit_FailsWhenTheCheckpointIsNotDelivered(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	writeAttachTestSettings(t, `{"enabled": true, "strategy_options": {"push_sessions": false}}`)

	out, err := attachHeadless(t, "attach-not-delivered", attachOptions{LinkWithoutTrailer: true})
	if err == nil || !strings.Contains(err.Error(), "push_sessions") {
		t.Fatalf("err = %v, want an error naming push_sessions as the reason nothing was pushed\n%s", err, out)
	}
	if strings.Contains(out, "Pushed checkpoint metadata") {
		t.Fatalf("reported a push that didn't happen:\n%s", out)
	}
}

// Rerunning attach after a push that didn't land retries it rather than
// reporting the session as already done.
func TestAttachCommit_RerunRetriesAnUndeliveredCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	writeAttachTestSettings(t, `{"enabled": true, "strategy_options": {"push_sessions": false}}`)
	if out, err := attachHeadless(t, "attach-retry-delivery", attachOptions{LinkWithoutTrailer: true}); err == nil {
		t.Fatalf("first attach should fail to deliver:\n%s", out)
	}

	writeAttachTestSettings(t, `{"enabled": true}`)
	// Without a terminal or --force the rerun pushes nothing: it sends the
	// transcript, so it needs the same go-ahead as any attach.
	var unconfirmed bytes.Buffer
	if err := runAttach(context.Background(), &unconfirmed, &unconfirmed, "attach-retry-delivery", agent.AgentNameClaudeCode, attachOptions{}); err == nil {
		t.Fatalf("rerun without confirmation succeeded:\n%s", unconfirmed.String())
	}
	if strings.Contains(unconfirmed.String(), "Pushed checkpoint metadata") {
		t.Fatalf("rerun pushed without confirmation:\n%s", unconfirmed.String())
	}
	out, err := attachHeadless(t, "attach-retry-delivery", attachOptions{})
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	if !strings.Contains(out, "is already in checkpoint") || !strings.Contains(out, "Pushed checkpoint metadata to origin") {
		t.Fatalf("rerun should push the checkpoint it already holds:\n%s", out)
	}
}

func writeAttachTestSettings(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(mustGetwd(t), ".entire", "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A commit someone pushed and then built on, on a branch this clone doesn't
// track, is an ancestor of that branch's tip rather than the tip itself. It is
// still pushed: once it reaches the default branch, attach must not amend it.
func TestAttachCommit_SeesACommitBuriedInAnUntrackedBranch(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	remote := newBareRemote(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	testutil.RunGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	testutil.RunGit(t, dir, "push", "-q", remote, "HEAD~1:refs/heads/main")
	testutil.RunGit(t, dir, "push", "-q", remote, "HEAD:refs/heads/shared")
	other := filepath.Join(t.TempDir(), "other")
	testutil.RunGit(t, t.TempDir(), "clone", "-q", "--branch", "shared", remote, other)
	testutil.WriteFile(t, other, "more.txt", "more")
	testutil.GitAdd(t, other, "more.txt")
	testutil.RunGit(t, other, "-c", "user.name=Other", "-c", "user.email=other@example.com", "commit", "-q", "-m", "built on top")
	testutil.RunGit(t, other, "push", "-q", "origin", "shared", "shared:main")

	out, err := attachHeadless(t, "attach-buried", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "origin/main, a default branch") {
		t.Fatalf("err = %v, want a refusal naming origin/main\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash || strings.Contains(got.Message, "Entire-Checkpoint") {
		t.Fatalf("a commit already on the remote (buried under another commit) was amended: %s %q\n%s", got.Hash, got.Message, out)
	}
}

// Recording a link to a pushed commit publishes the transcript and gives up the
// trailer, so it needs the user's go-ahead too: without a terminal or --force
// nothing is written, and the confirmation names what a recorded link loses.
func TestAttachCommit_PushedCommitNeedsConfirmation(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	const sessionID = "attach-pushed-unconfirmed"
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)

	var out, errOut bytes.Buffer
	err := runAttach(context.Background(), &out, &errOut, sessionID, agent.AgentNameClaudeCode, attachOptions{LinkWithoutTrailer: true})
	if err == nil {
		t.Fatalf("attach without confirmation succeeded:\n%s", out.String())
	}
	if !strings.Contains(errOut.String(), "won't be changed") || !strings.Contains(errOut.String(), "rebase, amend or cherry-pick") || !strings.Contains(errOut.String(), "are pushed to origin now") {
		t.Errorf("expected the pushed-commit warning, got:\n%s", errOut.String())
	}
	if state, err := loadAttachState(t, sessionID); err != nil || state != nil {
		t.Fatalf("session state written without confirmation: %+v (%v)", state, err)
	}
}

// A history walk that can't finish is an unknown answer, not "pushed".
func TestReachableFromAny_FailedWalkIsAnError(t *testing.T) {
	setupAttachTestRepo(t)
	head := headCommitOf(t)
	if _, err := reachableFromAny(context.Background(), head, []string{strings.Repeat("ab", 20)}); err == nil {
		t.Fatal("a walk from a missing tip reported an answer")
	}
	reachable, err := reachableFromAny(context.Background(), head, []string{head.Hash.String()})
	if err != nil || !reachable {
		t.Fatalf("reachableFromAny(HEAD from HEAD) = %v, %v", reachable, err)
	}
}

// Branch names a remote advertises become fetch refspecs, so only well-formed
// branch refs are used: a ":" would name a local destination.
func TestIsRemoteBranchName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"refs/heads/main":                  true,
		"refs/heads/feature/x":             true,
		"refs/heads/a:refs/heads/victim":   false,
		"refs/heads/a:refs/entire/cp/x":    false,
		"refs/heads/*":                     false,
		"refs/heads/a^b":                   false,
		"refs/tags/v1":                     false,
		"refs/heads/..":                    false,
		"+refs/heads/main:refs/heads/main": false,
	} {
		if got := isRemoteBranchName(name); got != want {
			t.Errorf("isRemoteBranchName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestTrackingRefRemote(t *testing.T) {
	t.Parallel()
	remotes := []string{"a", "a/b", "origin"}
	for ref, want := range map[string]string{
		"refs/remotes/a/main":     "a",
		"refs/remotes/a/b/main":   "a/b",
		"refs/remotes/origin/x/y": "origin",
		"refs/remotes/other/main": "",
	} {
		if got := trackingRefRemote(ref, remotes); got != want {
			t.Errorf("trackingRefRemote(%q) = %q, want %q", ref, got, want)
		}
	}
}

// Blame marks a line attributed through a recorded link, which nothing
// verified, apart from trailer-linked ones.
func TestAttributionLineMarker_RecordedLink(t *testing.T) {
	t.Parallel()
	line := attributionLine{RecordedLink: true, MetadataMissing: true}
	if got := attributionLineMarker(line); got != "!" {
		t.Fatalf("marker = %q, want !", got)
	}
	var legend bytes.Buffer
	renderAttributionMarkerLegend(&legend, newStatusStyles(&legend), []attributionLine{line})
	if !strings.Contains(legend.String(), "recorded link") {
		t.Fatalf("legend doesn't explain !: %q", legend.String())
	}
}

// An agent that reports no transcript position still ends the window at the
// transcript's line count, so the session's offset advances and a later attach
// doesn't record the same turns again.
func TestAttachTranscriptWindowFor_FallsBackToLineCount(t *testing.T) {
	t.Parallel()
	data := []byte("{\"a\":1}\n{\"b\":2}\n{\"c\":3}\n")
	window := attachTranscriptWindowFor(io.Discard, fakeAgent{typ: "No Position Agent"}, "", data, nil)
	if window.end != 3 {
		t.Fatalf("window.end = %d, want 3", window.end)
	}
}

// Repro: the commit is pushed from another clone while the confirmation is
// open. Attach must ask the remote again after the answer, not trust what it
// saw before the prompt or its own stale tracking refs, and leave the commit
// alone. Not parallel: sets ENTIRE_TEST_TTY and the confirmation seam.
func TestAttachCommit_PushedWhileConfirmationIsOpenIsNotRewritten(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	remote := pushToOrigin(t)
	head := commitAt(t, "more.txt")
	setupClaudeTranscript(t, "attach-pushed-during-prompt", attachCommitTranscript)

	t.Setenv(interactive.EnvTestTTY, "1")
	old := askAttachConfirmation
	t.Cleanup(func() { askAttachConfirmation = old })
	askAttachConfirmation = func() (bool, error) {
		// Another clone pushes the commit, so none of this clone's tracking
		// refs learn about it.
		pushFromAnotherClone(t, remote, head.Hash.String()+":refs/heads/someone-else")
		return true, nil
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, &out, "attach-pushed-during-prompt", agent.AgentNameClaudeCode, attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "while attach was waiting") {
		t.Fatalf("err = %v, want a refusal because the commit was pushed during the prompt\n%s", err, out.String())
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("a commit pushed during the prompt was rewritten: %s", got.Hash)
	}
	if state, loadErr := loadAttachState(t, "attach-pushed-during-prompt"); loadErr != nil || state != nil {
		t.Fatalf("session state was written after the refusal: %+v, %v", state, loadErr)
	}
}

// Repro: a hostile remote advertises a branch whose name is a refspec. Attach
// fetches branches whose tips it lacks by name; the name must not become a
// fetch destination that writes a local ref.
func TestAttachCommit_RemoteBranchNameCannotWriteLocalRefs(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	remote := pushToOrigin(t)

	// Another clone pushes a commit this clone doesn't have.
	other := t.TempDir()
	testutil.RunGit(t, other, "clone", "-q", "-b", "main", remote, ".")
	testutil.WriteFile(t, other, "theirs.txt", "theirs")
	testutil.GitAdd(t, other, "theirs.txt")
	testutil.RunGit(t, other, "-c", "user.name=Other", "-c", "user.email=other@example.com", "commit", "-q", "-m", "theirs")
	testutil.RunGit(t, other, "push", "-q", "origin", "HEAD:refs/heads/a")
	theirs := strings.TrimSpace(testutil.RunGit(t, other, "rev-parse", "HEAD"))

	// The remote advertises it under a name carrying a destination.
	testutil.RunGit(t, remote, "pack-refs", "--all")
	packed := filepath.Join(remote, "packed-refs")
	data, err := os.ReadFile(packed)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(theirs+" refs/heads/a:refs/heads/victim\n")...)
	if err := os.WriteFile(packed, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if ls := testutil.RunGit(t, mustGetwd(t), "ls-remote", "--heads", "origin"); !strings.Contains(ls, "refs/heads/a:refs/heads/victim") {
		t.Skipf("this git doesn't advertise the crafted name, so the attack can't be staged:\n%s", ls)
	}

	commitAt(t, "mine.txt")
	out, err := attachHeadless(t, "attach-hostile-branch-name", attachOptions{})
	if refs := testutil.RunGit(t, mustGetwd(t), "for-each-ref", "--format=%(refname)", "refs/heads/victim"); strings.TrimSpace(refs) != "" {
		t.Fatalf("a remote's branch name wrote a local ref: %s", refs)
	}
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
}

// pushFeatureBranch publishes main to a new origin, then commits on a feature
// branch and pushes it there, as an open pull request looks.
func pushFeatureBranch(t *testing.T) (head *object.Commit, remote string) {
	t.Helper()
	remote = pushToOrigin(t)
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "checkout", "-q", "-b", "feature")
	head = commitAt(t, "feature.txt")
	testutil.RunGit(t, dir, "push", "-q", "-u", "origin", "feature")
	return head, remote
}

// A commit on a remote's default branch is never rewritten, and a recorded
// link is never chosen for the user: attach refuses, names the flag and what a
// recorded link gives up, and writes nothing.
func TestAttachCommit_RefusesToRewriteACommitOnTheDefaultBranch(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	remote := pushToOrigin(t)
	const sessionID = "attach-default-branch"

	out, err := attachHeadless(t, sessionID, attachOptions{})
	if err == nil {
		t.Fatalf("attach to a commit on the default branch succeeded:\n%s", out)
	}
	for _, want := range []string{"origin/main, a default branch", "--link-without-trailer", "rebase, amend or cherry-pick", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q:\n%v", want, err)
		}
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("a commit on the default branch was rewritten: %s", got.Hash)
	}
	if state, loadErr := loadAttachState(t, sessionID); loadErr != nil || state != nil {
		t.Fatalf("session state was written after the refusal: %+v, %v", state, loadErr)
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Fatalf("a checkpoint was pushed after the refusal:\n%s", refs)
	}
}

// A commit pushed only to a feature branch gets the trailer, the stable link,
// as an unpushed one does: attach rewrites it locally and tells the user to
// force-push, and pushes nothing itself.
func TestAttachCommit_PushedFeatureBranchCommitGetsTheTrailer(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, remote := pushFeatureBranch(t)

	out, err := attachHeadless(t, "attach-pushed-feature", attachOptions{})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	head := headCommitOf(t)
	cpID, ok := trailers.ParseCheckpoint(head.Message)
	if !ok || head.Hash == pushed.Hash {
		t.Fatalf("pushed feature-branch HEAD was not rewritten with a trailer:\n%s", out)
	}
	if summary := readSummary(t, cpID.String()); len(summary.LinkedCommits) != 0 {
		t.Errorf("LinkedCommits = %v: a trailer-linked checkpoint needs no recorded link", summary.LinkedCommits)
	}
	if !strings.Contains(out, "already pushed (origin/feature)") || !strings.Contains(out, "replace it there with 'git push --force-with-lease'") {
		t.Errorf("output should warn of and explain the force-push:\n%s", out)
	}
	if got := strings.TrimSpace(testutil.RunGit(t, remote, "rev-parse", "refs/heads/feature")); got != pushed.Hash.String() {
		t.Errorf("attach changed the remote's feature branch to %s; the force-push is the user's", got)
	}
	if refs := remoteCheckpointRefs(t, remote); refs != "" {
		t.Errorf("the checkpoint should go out with the force-push, not before it:\n%s", refs)
	}
}

// The force-push is part of what the user agrees to: without --force or a
// terminal, nothing changes and the confirmation says so.
func TestAttachCommit_PushedFeatureBranchConfirmationNamesTheForcePush(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, _ := pushFeatureBranch(t)
	const sessionID = "attach-pushed-feature-unconfirmed"
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)

	var out, errOut bytes.Buffer
	if err := runAttach(context.Background(), &out, &errOut, sessionID, agent.AgentNameClaudeCode, attachOptions{}); err == nil {
		t.Fatalf("attach without confirmation succeeded:\n%s", out.String())
	}
	for _, want := range []string{"This rewrites git history", "already pushed (origin/feature)", "git push --force-with-lease", "--link-without-trailer"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("confirmation should mention %q:\n%s", want, errOut.String())
		}
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("HEAD was rewritten without confirmation: %s", got.Hash)
	}
}

// --link-without-trailer is the user's explicit choice: on a pushed feature
// branch it records the link and leaves the commit alone.
func TestAttachCommit_LinkWithoutTrailerOnAFeatureBranch(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, _ := pushFeatureBranch(t)

	out, err := attachHeadless(t, "attach-feature-no-trailer", attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("HEAD was rewritten despite --link-without-trailer: %s", got.Hash)
	}
	state, err := loadAttachState(t, "attach-feature-no-trailer")
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	if summary := readSummary(t, state.LastCheckpointID.String()); len(summary.LinkedCommits) != 1 || summary.LinkedCommits[0].SHA != pushed.Hash.String() {
		t.Fatalf("LinkedCommits = %v, want [%s]", summary.LinkedCommits, pushed.Hash)
	}
}

// On an unpushed commit the flag records a link too, without rewriting it;
// with no remote holding the commit, the checkpoint waits for the next push.
func TestAttachCommit_LinkWithoutTrailerOnAnUnpushedCommit(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	// The link names the repository the commit will be pushed to. https is
	// refused locally so the remote check fails fast instead of dialing out.
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	testutil.RunGit(t, dir, "config", "protocol.https.allow", "never")

	out, err := attachHeadless(t, "attach-unpushed-no-trailer", attachOptions{LinkWithoutTrailer: true})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("HEAD was rewritten despite --link-without-trailer: %s", got.Hash)
	}
	state, err := loadAttachState(t, "attach-unpushed-no-trailer")
	if err != nil || state == nil {
		t.Fatalf("load state: %v, %v", state, err)
	}
	if summary := readSummary(t, state.LastCheckpointID.String()); len(summary.LinkedCommits) != 1 || summary.LinkedCommits[0].SHA != head.Hash.String() {
		t.Fatalf("LinkedCommits = %v, want [%s]", summary.LinkedCommits, head.Hash)
	} else if got := summary.LinkedCommits[0].Repo; got != "gh/acme/widgets" {
		t.Errorf("LinkedCommits[0].Repo = %q, want the push remote's repository", got)
	}
	if strings.Contains(out, "Pushed checkpoint metadata") {
		t.Errorf("nothing should be pushed for an unpushed commit:\n%s", out)
	}
}

// A remote whose HEAD names a branch with no commits (a bare repo made with
// 'git init --bare' names master while everyone pushes main) might hold the
// commit on its real default branch, so attach refuses rather than guess, and
// says why.
func TestAttachCommit_UnknownDefaultBranchRefusesTheRewrite(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	remote := t.TempDir()
	testutil.RunGit(t, remote, "init", "--bare", "-q")
	testutil.RunGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/nowhere")
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	testutil.RunGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/feature")

	out, err := attachHeadless(t, "attach-unknown-default", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "couldn't tell whether it is on a default branch") {
		t.Fatalf("err = %v, want a refusal because the default branch is unknown\n%s", err, out)
	}
	if !strings.Contains(err.Error(), "origin's HEAD doesn't name a branch with commits") {
		t.Errorf("refusal should say the remote's HEAD names a branch with no commits:\n%v", err)
	}
	if got := headCommitOf(t); got.Hash != head.Hash {
		t.Fatalf("HEAD was rewritten without knowing the default branch: %s", got.Hash)
	}
}

// The prompt can stay open while a pull request is merged: a feature commit
// that reaches the default branch meanwhile is not rewritten.
func TestAttachCommit_MergedWhileConfirmationIsOpenIsNotRewritten(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, remote := pushFeatureBranch(t)
	const sessionID = "attach-merged-during-prompt"
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)

	t.Setenv(interactive.EnvTestTTY, "1")
	old := askAttachConfirmation
	t.Cleanup(func() { askAttachConfirmation = old })
	askAttachConfirmation = func() (bool, error) {
		pushFromAnotherClone(t, remote, pushed.Hash.String()+":refs/heads/main")
		return true, nil
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "reached origin/main while attach was waiting") {
		t.Fatalf("err = %v, want a refusal because the commit reached main during the prompt\n%s", err, out.String())
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("a commit merged during the prompt was rewritten: %s", got.Hash)
	}
	if state, loadErr := loadAttachState(t, sessionID); loadErr != nil || state != nil {
		t.Fatalf("session state was written after the refusal: %+v, %v", state, loadErr)
	}
}

// A teammate can push a branch on top of the commit while the prompt is open:
// the commit is shared from then on and is not rewritten.
func TestAttachCommit_SharedWhileConfirmationIsOpenIsNotRewritten(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, remote := pushFeatureBranch(t)
	const sessionID = "attach-shared-during-prompt"
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)

	t.Setenv(interactive.EnvTestTTY, "1")
	old := askAttachConfirmation
	t.Cleanup(func() { askAttachConfirmation = old })
	askAttachConfirmation = func() (bool, error) {
		pushFromAnotherClone(t, remote, pushed.Hash.String()+":refs/heads/teammate")
		return true, nil
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "reached origin/teammate while attach was waiting") {
		t.Fatalf("err = %v, want a refusal because another branch took the commit during the prompt\n%s", err, out.String())
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("a commit shared during the prompt was rewritten: %s", got.Hash)
	}
}

// Agents pick flags from agent-help, so it has to say why a recorded link is a
// last resort and that it is never theirs to choose, least of all to dodge a
// force-push on a feature branch.
func TestAttachAgentHelp_SHALinkIsALastResort(t *testing.T) {
	t.Parallel()
	guidance := agentHelpGuidance["session attach"]
	for _, want := range []string{
		"--link-without-trailer is a last resort",
		"silently orphans the session",
		"can't be verified locally",
		"counts only when the commit's author attaches it",
		"Never pass it on your own, and never to avoid a force-push",
		"pass the flag only if they choose it",
	} {
		if !strings.Contains(guidance, want) {
			t.Errorf("session attach agent guidance should say %q:\n%s", want, guidance)
		}
	}
}

// A remote the branch pushes to that can't be asked is unknown, even when its
// tracking refs name a default branch: they may predate a merge. Before, a
// stale origin/main without the commit let attach rewrite it.
func TestAttachCommit_UnreachablePushRemoteRefusesTheRewrite(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, _ := pushFeatureBranch(t)
	testutil.RunGit(t, mustGetwd(t), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	out, err := attachHeadless(t, "attach-unreachable-push-remote", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "couldn't tell whether it is on a default branch") {
		t.Fatalf("err = %v, want a refusal because origin couldn't be asked\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("HEAD was rewritten on stale tracking refs: %s", got.Hash)
	}
}

// Another remote's default branch is read from its tracking refs. One that
// holds the commit but names no default branch (no <remote>/HEAD, no main or
// master) is unknown rather than skipped; with <remote>/HEAD set, its
// default branch counts like origin's.
func TestAttachCommit_UpstreamDefaultBranchFromTrackingRefs(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, _ := pushFeatureBranch(t)
	dir := mustGetwd(t)
	testutil.RunGit(t, dir, "remote", "add", "upstream", filepath.Join(t.TempDir(), "missing.git"))
	testutil.RunGit(t, dir, "update-ref", "refs/remotes/upstream/develop", "HEAD")

	out, err := attachHeadless(t, "attach-upstream-develop", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "upstream has no known default branch") || !strings.Contains(err.Error(), "git remote set-head upstream --auto") {
		t.Fatalf("err = %v, want a refusal naming upstream's unknown default branch\n%s", err, out)
	}

	testutil.RunGit(t, dir, "symbolic-ref", "refs/remotes/upstream/HEAD", "refs/remotes/upstream/develop")
	out, err = attachHeadless(t, "attach-upstream-develop", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "upstream/develop, a default branch") {
		t.Fatalf("err = %v, want a refusal naming upstream/develop\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("a commit on upstream's default branch was rewritten: %s", got.Hash)
	}
}

// A merge made elsewhere puts a commit on the default branch that this clone
// doesn't have. The recheck fetches that tip's objects to see the feature
// commit under it, rather than reading the tip as "not containing".
func TestAttachCommit_MergeCommitMadeElsewhereIsFetchedForTheRecheck(t *testing.T) {
	setupAttachTestRepo(t)
	pushed, remote := pushFeatureBranch(t)
	const sessionID = "attach-merged-elsewhere"
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)

	t.Setenv(interactive.EnvTestTTY, "1")
	old := askAttachConfirmation
	t.Cleanup(func() { askAttachConfirmation = old })
	askAttachConfirmation = func() (bool, error) {
		other := filepath.Join(t.TempDir(), "other")
		testutil.RunGit(t, t.TempDir(), "clone", "-q", remote, other)
		testutil.RunGit(t, other, "-c", "user.name=Other", "-c", "user.email=other@example.com", "merge", "-q", "--no-ff", "-m", "merge feature", "origin/feature")
		testutil.RunGit(t, other, "push", "-q", "origin", "HEAD:main")
		return true, nil
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, &out, sessionID, agent.AgentNameClaudeCode, attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "reached origin/main while attach was waiting") {
		t.Fatalf("err = %v, want a refusal because the commit was merged during the prompt\n%s", err, out.String())
	}
	if got := headCommitOf(t); got.Hash != pushed.Hash {
		t.Fatalf("a commit merged during the prompt was rewritten: %s", got.Hash)
	}
}
