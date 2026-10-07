package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
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

// pushToOrigin adds a bare "origin" and publishes HEAD to it, so remote
// branches contain every commit made so far.
func pushToOrigin(t *testing.T) string {
	t.Helper()
	dir := mustGetwd(t)
	remote := t.TempDir()
	testutil.RunGit(t, remote, "init", "--bare", "-q")
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	testutil.RunGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")
	testutil.RunGit(t, dir, "fetch", "-q", "origin")
	return remote
}

func attachHeadless(t *testing.T, sessionID string, opts attachOptions) (string, error) {
	t.Helper()
	setupClaudeTranscript(t, sessionID, attachCommitTranscript)
	var out bytes.Buffer
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

	out, err := attachHeadless(t, "attach-pushed-head", attachOptions{})
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

// An older pushed commit is linked in the checkpoint, never rewritten.
func TestAttachCommit_PushedOlderCommitIsLinkedInTheCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	target := commitAt(t, "work.txt")
	head := commitAt(t, "later.txt")
	pushToOrigin(t)

	out, err := attachHeadless(t, "attach-pushed-older", attachOptions{Commit: target.Hash.String()[:10]})
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

// An older commit no remote holds can be linked neither way: amending it means
// a rebase, and a recorded link wouldn't survive the rebase still likely to
// come. It is refused before anything is written.
func TestAttachCommit_UnpushedOlderCommitIsRefused(t *testing.T) {
	setupAttachTestRepo(t)
	target := commitAt(t, "work.txt")
	commitAt(t, "later.txt")

	out, err := attachHeadless(t, "attach-unpushed-older", attachOptions{Commit: target.Hash.String()})
	if err == nil || !strings.Contains(err.Error(), "not pushed") {
		t.Fatalf("err = %v, want a refusal explaining the commit isn't pushed\n%s", err, out)
	}
	state, err := loadAttachState(t, "attach-unpushed-older")
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if state != nil {
		t.Errorf("a refused attach wrote session state: %+v", state)
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

	out, err := attachHeadless(t, "attach-not-author", attachOptions{})
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
	if out, err := attachHeadless(t, sessionID, attachOptions{Commit: target.Hash.String()}); err != nil {
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
	r := &attributionResolver{ctx: context.Background(), store: store}
	for range 2 {
		if got := r.linkedCheckpoints(sha); len(got) != 1 || got[0] != "111111111111" {
			t.Fatalf("linkedCheckpoints = %v", got)
		}
	}
	if store.lists != 1 {
		t.Fatalf("store listed %d times, want once per run", store.lists)
	}

	plain := &attributionResolver{ctx: context.Background(), store: &attributionCheckpointReaderStub{}}
	if got := plain.linkedCheckpoints(sha); len(got) != 0 {
		t.Fatalf("a store that cannot list links nothing, got %v", got)
	}
	failing := &attributionResolver{ctx: context.Background(), store: &listingAttributionStub{listErr: context.Canceled}}
	if got := failing.linkedCheckpoints(sha); len(got) != 0 {
		t.Fatalf("a failed listing links nothing, got %v", got)
	}
}

// A pushed commit already linked by an earlier attach is linked: a second
// session joins that checkpoint, like the trailer paths, rather than starting a
// separate checkpoint for the same commit.
func TestAttachCommit_SecondSessionJoinsTheRecordedLinkCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	pushToOrigin(t)

	if out, err := attachHeadless(t, "attach-recorded-first", attachOptions{}); err != nil {
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
	if out, err := attachHeadless(t, "attach-remote-only-first", attachOptions{}); err != nil {
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
// records the link instead of amending a shared commit.
func TestAttachCommit_FetchesBeforeDecidingAHeadIsUnpushed(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	remote := t.TempDir()
	testutil.RunGit(t, remote, "init", "--bare", "-q")
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	// Pushed by URL, as another clone would: this repo's origin/* refs don't move.
	testutil.RunGit(t, dir, "push", "-q", remote, "HEAD:refs/heads/main")
	if refs := testutil.RunGit(t, dir, "branch", "-r"); strings.TrimSpace(refs) != "" {
		t.Fatalf("expected no remote-tracking refs before attach, got %q", refs)
	}

	out, err := attachHeadless(t, "attach-stale-tracking", attachOptions{})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
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

// Finding 1: a remote's branch tip is checked directly, so a commit pushed to a
// branch this clone's fetch refspec doesn't track still reads as pushed.
func TestAttachCommit_SeesACommitPushedToAnUntrackedBranch(t *testing.T) {
	setupAttachTestRepo(t)
	head := commitAt(t, "work.txt")
	dir := mustGetwd(t)
	remote := t.TempDir()
	testutil.RunGit(t, remote, "init", "--bare", "-q")
	testutil.RunGit(t, dir, "remote", "add", "origin", remote)
	testutil.RunGit(t, dir, "config", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	testutil.RunGit(t, dir, "push", "-q", remote, "HEAD:refs/heads/someone-elses-branch")

	out, err := attachHeadless(t, "attach-untracked-branch", attachOptions{})
	if err != nil {
		t.Fatalf("runAttach: %v\n%s", err, out)
	}
	if got := headCommitOf(t); got.Hash != head.Hash || strings.Contains(got.Message, "Entire-Checkpoint") {
		t.Fatalf("a commit the remote already holds was amended: %s %q\n%s", got.Hash, got.Message, out)
	}
}

// Finding 2: a recorded link only exists in the checkpoint, so attach must not
// report success when the checkpoint never reached the remote.
func TestAttachCommit_FailsWhenTheCheckpointIsNotDelivered(t *testing.T) {
	setupAttachTestRepo(t)
	commitAt(t, "work.txt")
	pushToOrigin(t)
	writeAttachTestSettings(t, `{"enabled": true, "strategy_options": {"push_sessions": false}}`)

	out, err := attachHeadless(t, "attach-not-delivered", attachOptions{})
	if err == nil || !strings.Contains(err.Error(), "push_sessions") {
		t.Fatalf("err = %v, want an error naming push_sessions as the reason nothing was pushed\n%s", err, out)
	}
	if strings.Contains(out, "Pushed checkpoint metadata") {
		t.Fatalf("reported a push that didn't happen:\n%s", out)
	}
}

func writeAttachTestSettings(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(mustGetwd(t), ".entire", "settings.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
