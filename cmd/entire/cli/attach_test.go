package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"     // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"          // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/cursor"         // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid" // register agent
	_ "github.com/entireio/cli/cmd/entire/cli/agent/geminicli"      // register agent
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	cpkg "github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	cliReview "github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

func TestAttach_MissingSessionID(t *testing.T) {
	t.Parallel()

	cmd := newAttachCmd()
	cmd.SetArgs([]string{})

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	if err != nil {
		t.Fatalf("expected help output, got error: %v", err)
	}
	if !strings.Contains(out.String(), "attach <session-id>") {
		t.Errorf("expected help output containing usage, got: %s", out.String())
	}
}

func TestAttach_TranscriptNotFound(t *testing.T) {
	setupAttachTestRepo(t)

	// Set up a fake Claude project dir that's empty
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", t.TempDir())
	// Redirect HOME so the fallback search doesn't walk real ~/.claude/projects
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, "nonexistent-session-id", agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err == nil {
		t.Fatal("expected error for missing transcript")
	}
}

func TestAttach_Success(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-session-001"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"create a hello world file"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{"file_path":"hello.txt","content":"world"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"wrote file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done! I created hello.txt."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	// Verify session state was created
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.SessionID != sessionID {
		t.Errorf("session ID = %q, want %q", state.SessionID, sessionID)
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after attach")
	}

	// Verify output message
	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}
	if !strings.Contains(output, "Created checkpoint") {
		t.Errorf("expected 'Created checkpoint' in output, got: %s", output)
	}
}

// TestAttach_PopulatesBaseCommitFromHEAD is a regression for
// https://github.com/entireio/cli/issues/411 / PR #1102.
//
// When `entire attach` ran on an existing session whose state had an empty
// BaseCommit (e.g., after a hook initialization failure on session start, or
// for sessions started before `entire enable` ran), saveAttachSessionState
// left BaseCommit empty. The prepare-commit-msg hook then refused to
// recognize the session as active and never wrote Entire-Checkpoint trailers
// onto subsequent commits in that session.
//
// After attach, BaseCommit (and AttributionBaseCommit) must be populated
// from HEAD so the session is recognized as active.
func TestAttach_PopulatesBaseCommitFromHEAD(t *testing.T) {
	setupAttachTestRepo(t)

	repoRoot := mustGetwd(t)
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	headRef, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	headHash := headRef.Hash().String()

	sessionID := "test-attach-empty-base-commit"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"uuid":"a1"}
`)

	// Pre-create a session state with empty BaseCommit — simulates a session
	// that started while hook init failed, or a session that pre-dates `entire
	// enable`. State exists, but BaseCommit was never populated.
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), &session.State{
		SessionID: sessionID,
		AgentType: agent.AgentTypeClaudeCode,
		StartedAt: time.Now(),
		// BaseCommit and AttributionBaseCommit deliberately empty.
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to exist after attach")
	}
	if state.BaseCommit != headHash {
		t.Errorf("BaseCommit = %q, want %q (HEAD); attach did not populate empty BaseCommit",
			state.BaseCommit, headHash)
	}
	if state.AttributionBaseCommit != headHash {
		t.Errorf("AttributionBaseCommit = %q, want %q (HEAD); attach did not populate empty AttributionBaseCommit",
			state.AttributionBaseCommit, headHash)
	}
}

// TestAttach_PreservesActivePhase is a regression for PR #1102.
//
// `entire attach` could be called against a session that is currently active
// (e.g., the user runs attach mid-session to repair a missed checkpoint).
// Previously, saveAttachSessionState unconditionally set Phase to PhaseEnded,
// which broke the running session: the prepare-commit-msg hook then treated
// the session as ended and skipped Entire-Checkpoint trailers on every
// subsequent commit until the agent restarted.
//
// Attach must preserve PhaseActive when the session is already active.
func TestAttach_PreservesActivePhase(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-active-session"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"uuid":"a1"}
`)

	// Pre-create an ACTIVE session — agent is mid-turn when the user runs attach.
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), &session.State{
		SessionID: sessionID,
		AgentType: agent.AgentTypeClaudeCode,
		StartedAt: time.Now(),
		Phase:     session.PhaseActive,
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to exist after attach")
	}
	if state.Phase != session.PhaseActive {
		t.Errorf("Phase = %q, want %q; attach clobbered an active session into PhaseEnded",
			state.Phase, session.PhaseActive)
	}
}

func TestAttach_SessionAlreadyTracked_NoCheckpoint(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-duplicate"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"explain the auth module"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The auth module handles user authentication via JWT tokens."}]},"uuid":"uuid-2"}
`)

	// Pre-create session state without a checkpoint ID (simulates hooks tracking
	// the session but condensation never happening).
	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), &session.State{
		SessionID: sessionID,
		AgentType: agent.AgentTypeClaudeCode,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("expected attach to handle already-tracked session, got error: %v", err)
	}
	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}

	// Verify checkpoint was created
	reloadedState, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedState.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after re-attach")
	}
}

func TestAttach_OutputContainsCheckpointID(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-checkpoint-output"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"add error handling"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Edit","input":{"file_path":"main.go","old_string":"return nil","new_string":"return fmt.Errorf(\"failed: %w\", err)"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"edited file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Added error wrapping."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	output := out.String()

	// Must contain Entire-Checkpoint trailer with 12-hex-char ID
	re := regexp.MustCompile(`Entire-Checkpoint: [0-9a-f]{12}`)
	if !re.MatchString(output) {
		t.Errorf("expected 'Entire-Checkpoint: <12-hex-id>' in output, got:\n%s", output)
	}
}

func TestAttach_V2DualWriteEnabled(t *testing.T) {
	setupAttachTestRepo(t)

	repoDir := mustGetwd(t)
	setAttachCheckpointsV2Enabled(t, repoDir)

	sessionID := "test-attach-v2-dual-write"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"create hello.txt"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{"file_path":"hello.txt","content":"hello"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"wrote file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.LastCheckpointID.IsEmpty() {
		t.Fatal("expected attach to persist a checkpoint ID")
	}

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cpPath := state.LastCheckpointID.Path()
	mainCompact, found := readFileFromRef(t, repo, paths.V2MainRefName, cpPath+"/0/"+paths.CompactTranscriptFileName)
	if !found {
		t.Fatalf("expected %s on %s", paths.CompactTranscriptFileName, paths.V2MainRefName)
	}
	if !strings.Contains(mainCompact, "create hello.txt") {
		t.Errorf("compact transcript missing prompt, got:\n%s", mainCompact)
	}

	fullTranscript, found := readFileFromRef(t, repo, paths.V2FullCurrentRefName, cpPath+"/0/"+paths.V2RawTranscriptFileName)
	if !found {
		t.Fatalf("expected %s on %s", paths.V2RawTranscriptFileName, paths.V2FullCurrentRefName)
	}
	if !strings.Contains(fullTranscript, "hello.txt") {
		t.Errorf("raw transcript missing file content, got:\n%s", fullTranscript)
	}
}

func TestAttach_CheckpointsVersion2(t *testing.T) {
	setupAttachTestRepo(t)

	repoDir := mustGetwd(t)
	setAttachCheckpointsV2Only(t, repoDir)

	sessionID := "test-attach-v2-only"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"create hello.txt"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{"file_path":"hello.txt","content":"hello"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"wrote file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.LastCheckpointID.IsEmpty() {
		t.Fatal("expected attach to persist a checkpoint ID")
	}

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	cpPath := state.LastCheckpointID.Path()
	if _, found := readFileFromRef(t, repo, paths.MetadataBranchName, cpPath+"/"+paths.MetadataFileName); found {
		t.Fatalf("did not expect %s metadata for %s when checkpoints_version is 2", paths.MetadataBranchName, cpPath)
	}

	mainCompact, found := readFileFromRef(t, repo, paths.V2MainRefName, cpPath+"/0/"+paths.CompactTranscriptFileName)
	if !found {
		t.Fatalf("expected %s on %s", paths.CompactTranscriptFileName, paths.V2MainRefName)
	}
	if !strings.Contains(mainCompact, "create hello.txt") {
		t.Errorf("compact transcript missing prompt, got:\n%s", mainCompact)
	}

	fullTranscript, found := readFileFromRef(t, repo, paths.V2FullCurrentRefName, cpPath+"/0/"+paths.V2RawTranscriptFileName)
	if !found {
		t.Fatalf("expected %s on %s", paths.V2RawTranscriptFileName, paths.V2FullCurrentRefName)
	}
	if !strings.Contains(fullTranscript, "hello.txt") {
		t.Errorf("raw transcript missing file content, got:\n%s", fullTranscript)
	}
}

func TestAttach_V2DualWriteDisabled(t *testing.T) {
	setupAttachTestRepo(t)

	repoDir := mustGetwd(t)

	sessionID := "test-attach-v2-disabled"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"create hello.txt"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{"file_path":"hello.txt","content":"hello"}}]},"uuid":"uuid-2"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"wrote file"}]},"uuid":"uuid-3"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done."}]},"uuid":"uuid-4"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true); err == nil {
		t.Fatalf("did not expect %s when checkpoints_v2 is disabled", paths.V2MainRefName)
	}
	if _, err := repo.Reference(plumbing.ReferenceName(paths.V2FullCurrentRefName), true); err == nil {
		t.Fatalf("did not expect %s when checkpoints_v2 is disabled", paths.V2FullCurrentRefName)
	}
}

func TestAttach_AppendsAsAdditionalSessionWhenIDDiffers(t *testing.T) {
	setupAttachTestRepo(t)

	firstSessionID := "first-session-a-original"
	setupClaudeTranscript(t, firstSessionID, `{"type":"user","message":{"role":"user","content":"first"},"uuid":"u1"}
`)
	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, firstSessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("first attach failed: %v", err)
	}

	repoRoot := mustGetwd(t)
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	headRef, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	headCommit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		t.Fatal(err)
	}
	existingCheckpoints := trailers.ParseAllCheckpoints(headCommit.Message)
	if len(existingCheckpoints) != 1 {
		t.Fatalf("expected one Entire-Checkpoint trailer after first attach; got %v", existingCheckpoints)
	}
	checkpointID := existingCheckpoints[0]

	secondSessionID := "second-session-b-append"
	setupClaudeTranscript(t, secondSessionID, `{"type":"user","message":{"role":"user","content":"second"},"uuid":"u1"}
`)
	out.Reset()
	if err := runAttach(context.Background(), &out, secondSessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("second attach failed: %v", err)
	}

	store := cpkg.NewGitStore(repo)
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted(%s): %v", checkpointID, err)
	}
	if summary == nil {
		t.Fatalf("checkpoint %s summary nil after two attaches", checkpointID)
	}
	if len(summary.Sessions) != 2 {
		t.Fatalf("checkpoint has %d sessions, want 2", len(summary.Sessions))
	}

	idx0, err := store.ReadSessionContent(context.Background(), checkpointID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent(0): %v", err)
	}
	idx1, err := store.ReadSessionContent(context.Background(), checkpointID, 1)
	if err != nil {
		t.Fatalf("ReadSessionContent(1): %v", err)
	}
	haveFirst := idx0.Metadata.SessionID == firstSessionID || idx1.Metadata.SessionID == firstSessionID
	haveSecond := idx0.Metadata.SessionID == secondSessionID || idx1.Metadata.SessionID == secondSessionID
	if !haveFirst {
		t.Errorf("first session %q missing from checkpoint; got [%q, %q]",
			firstSessionID, idx0.Metadata.SessionID, idx1.Metadata.SessionID)
	}
	if !haveSecond {
		t.Errorf("second session %q missing from checkpoint; got [%q, %q]",
			secondSessionID, idx0.Metadata.SessionID, idx1.Metadata.SessionID)
	}
}

func TestAttach_RefusesWhenCheckpointMissingFromLocalBranch(t *testing.T) {
	setupAttachTestRepo(t)

	repoRoot := mustGetwd(t)
	runGitInDir(t, repoRoot, "commit", "--amend", "-m", "init\n\nEntire-Checkpoint: ffffffffeeee")

	sessionID := "orphaned-attach-session"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"attach please"},"uuid":"u1"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err == nil {
		t.Fatal("expected error: checkpoint referenced by HEAD is missing locally and attach should refuse")
	}
	if !strings.Contains(err.Error(), "missing from the local entire/checkpoints/v1 branch") {
		t.Errorf("error message should explain the missing-branch situation; got: %v", err)
	}
	if !strings.Contains(err.Error(), "git fetch origin entire/checkpoints/v1") {
		t.Errorf("error message should include the fetch command to fix it; got: %v", err)
	}

	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := cpkg.NewGitStore(repo)
	summary, err := store.ReadCommitted(context.Background(), "ffffffffeeee")
	if err != nil {
		t.Fatalf("ReadCommitted: %v", err)
	}
	if summary != nil {
		t.Errorf("attach should NOT have created checkpoint ffffffffeeee locally; found %+v", summary)
	}
}

// Regression for https://github.com/entireio/cli/pull/1014#pullrequestreview-copilot:
// Bob clones a repo where Alice's checkpoint is on the remote-tracking ref
// (refs/remotes/origin/entire/checkpoints/v1) but the local branch doesn't
// exist yet. ReadCommitted falls back to the remote-tracking tree, so a naive
// "read and check" guard would think all is well. But WriteCommitted would
// then create a *fresh* orphan local branch, and Bob's push would clobber
// Alice's data on origin. Attach must refuse in this shape.
func TestAttach_RefusesWhenCheckpointOnlyInRemoteTrackingRef(t *testing.T) {
	setupAttachTestRepo(t)

	repoRoot := mustGetwd(t)
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Seed the local branch with a checkpoint representing Alice's session.
	alicesCheckpoint := id.MustCheckpointID("abcdef012345")
	store := cpkg.NewGitStore(repo)
	if writeErr := store.WriteCommitted(context.Background(), cpkg.WriteCommittedOptions{
		CheckpointID: alicesCheckpoint,
		SessionID:    "alice-original",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":"hi"}` + "\n")),
		AuthorName:   "Alice",
		AuthorEmail:  "alice@example.com",
	}); writeErr != nil {
		t.Fatalf("WriteCommitted: %v", writeErr)
	}

	// Move the populated branch to the remote-tracking ref, then delete the
	// local ref. This is the shape `git clone` produces for a branch the user
	// never explicitly checked out locally.
	localRef := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	remoteTrackingRef := plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName)
	populated, err := repo.Reference(localRef, true)
	if err != nil {
		t.Fatalf("reading local ref: %v", err)
	}
	if setErr := repo.Storer.SetReference(plumbing.NewHashReference(remoteTrackingRef, populated.Hash())); setErr != nil {
		t.Fatalf("SetReference remote-tracking: %v", setErr)
	}
	if rmErr := repo.Storer.RemoveReference(localRef); rmErr != nil {
		t.Fatalf("RemoveReference local: %v", rmErr)
	}

	// Amend HEAD so attach treats this as an existing-checkpoint case.
	runGitInDir(t, repoRoot, "commit", "--amend", "-m", "init\n\nEntire-Checkpoint: "+alicesCheckpoint.String())

	sessionID := "bob-attempted-attach"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}
`)

	var out bytes.Buffer
	err = runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err == nil {
		t.Fatal("expected attach to refuse when checkpoint is only in the remote-tracking ref")
	}
	if !strings.Contains(err.Error(), "missing from the local entire/checkpoints/v1 branch") {
		t.Errorf("error should explain the local-branch gap; got: %v", err)
	}

	// Local branch must still not exist — attach should not have created a
	// fresh orphan on refuse.
	if _, refErr := repo.Reference(localRef, true); refErr == nil {
		t.Error("local entire/checkpoints/v1 branch was created despite refuse; would clobber remote on push")
	}

	// Remote-tracking ref must still hold Alice's untouched data.
	remoteRef, err := repo.Reference(remoteTrackingRef, true)
	if err != nil {
		t.Fatalf("remote-tracking ref missing: %v", err)
	}
	if remoteRef.Hash() != populated.Hash() {
		t.Errorf("remote-tracking ref moved from %s to %s", populated.Hash(), remoteRef.Hash())
	}
}

// In v2-only mode, the refuse hint must reference the v2 /main ref and
// its fully-qualified refspec (refs/entire/checkpoints/v2/main lives under
// refs/entire/, not refs/heads/, so a short refspec won't resolve).
func TestAttach_RefuseHint_V2Only(t *testing.T) {
	setupAttachTestRepo(t)

	repoRoot := mustGetwd(t)
	setAttachCheckpointsV2Only(t, repoRoot)

	runGitInDir(t, repoRoot, "commit", "--amend", "-m", "init\n\nEntire-Checkpoint: ffffffffeeee")

	sessionID := "v2-orphaned-attach"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true})
	if err == nil {
		t.Fatal("expected v2-only attach to refuse when checkpoint is missing")
	}
	if !strings.Contains(err.Error(), "missing from the local v2 /main ref") {
		t.Errorf("error should describe the v2 /main ref; got: %v", err)
	}
	v2Refspec := paths.V2MainRefName + ":" + paths.V2MainRefName
	if !strings.Contains(err.Error(), v2Refspec) {
		t.Errorf("error should include v2 refspec %q; got: %v", v2Refspec, err)
	}
	// And must NOT suggest the v1 refspec.
	if strings.Contains(err.Error(), "entire/checkpoints/v1:entire/checkpoints/v1") {
		t.Errorf("v2-only hint should not reference the v1 branch; got: %v", err)
	}
}

func TestAttach_PopulatesTokenUsage(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-token-usage"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}},"uuid":"a1"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.TokenUsage == nil {
		t.Fatal("expected TokenUsage to be set")
	}
	if state.TokenUsage.OutputTokens == 0 {
		t.Error("expected OutputTokens > 0")
	}
}

func TestAttach_SetsSessionTurnCount(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-turn-count"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"first prompt"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"response 1"},"uuid":"a1"}
{"type":"user","message":{"role":"user","content":"second prompt"},"uuid":"u2"}
{"type":"assistant","message":{"role":"assistant","content":"response 2"},"uuid":"a2"}
`)

	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionTurnCount != 2 {
		t.Errorf("SessionTurnCount = %d, want 2", state.SessionTurnCount)
	}
}

func TestCountUserTurns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want int
	}{
		{
			name: "gemini format",
			data: []byte(`{"messages":[{"type":"user","content":"first"},{"type":"gemini","content":"ok"},{"type":"user","content":"second"},{"type":"gemini","content":"done"}]}`),
			want: 2,
		},
		{
			name: "jsonl with tool_result should not double count",
			data: []byte(`{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Write","input":{}}]},"uuid":"a1"}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]},"uuid":"u2"}
{"type":"user","message":{"role":"user","content":"next prompt"},"uuid":"u3"}
`),
			want: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTranscriptMetadata(tt.data).TurnCount
			if got != tt.want {
				t.Errorf("extractTranscriptMetadata().TurnCount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExtractModelFromTranscript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "claude code with model",
			data: []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hello"}]},"uuid":"a1"}
`),
			want: "claude-sonnet-4-20250514",
		},
		{
			name: "no model field",
			data: []byte(`{"type":"user","message":{"role":"user","content":"hi"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"hello"},"uuid":"a1"}
`),
			want: "",
		},
		{
			name: "gemini format (no model in transcript)",
			data: []byte(`{"messages":[{"type":"user","content":"hi"},{"type":"gemini","content":"hello"}]}`),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractTranscriptMetadata(tt.data).Model
			if got != tt.want {
				t.Errorf("extractTranscriptMetadata().Model = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractFirstPromptFromTranscript_GeminiFormat(t *testing.T) {
	t.Parallel()

	data := []byte(`{"messages":[{"type":"user","content":"fix the login bug"},{"type":"gemini","content":"I'll look at that"}]}`)
	got := extractTranscriptMetadata(data).FirstPrompt
	if got != "fix the login bug" {
		t.Errorf("extractTranscriptMetadata(gemini).FirstPrompt = %q, want %q", got, "fix the login bug")
	}
}

func TestExtractFirstPromptFromTranscript_JSONLFormat(t *testing.T) {
	t.Parallel()

	data := []byte(`{"type":"user","message":{"role":"user","content":"hello world"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"hi"},"uuid":"a1"}
`)
	got := extractTranscriptMetadata(data).FirstPrompt
	if got != "hello world" {
		t.Errorf("extractTranscriptMetadata(jsonl).FirstPrompt = %q, want %q", got, "hello world")
	}
}

func TestAttach_GeminiSubdirectorySession(t *testing.T) {
	setupAttachTestRepo(t)

	// Redirect HOME so searchTranscriptInProjectDirs searches our fake Gemini dir
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	// Create a Gemini transcript in a *different* project hash directory,
	// simulating a session started from a subdirectory (different CWD hash).
	differentProjectDir := filepath.Join(fakeHome, ".gemini", "tmp", "different-hash", "chats")
	if err := os.MkdirAll(differentProjectDir, 0o750); err != nil {
		t.Fatal(err)
	}

	sessionID := "abcd1234-gemini-subdir-test"
	transcriptContent := `{"messages":[{"type":"user","content":"hello"},{"type":"gemini","content":"hi"}]}`
	// Gemini names files as session-<date>-<shortid>.json where shortid = sessionID[:8]
	transcriptFile := filepath.Join(differentProjectDir, "session-2026-01-01T10-00-abcd1234.json")
	if err := os.WriteFile(transcriptFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Set the expected project dir to an empty directory so the primary lookup fails
	// and the fallback search kicks in.
	emptyProjectDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", emptyProjectDir)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameGemini, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}

	store, storeErr := session.NewStateStore(context.Background())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	state, loadErr := store.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeGemini)
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after attach")
	}
}

func TestAttach_GeminiSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	// Create Gemini transcript in expected project dir
	geminiDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", geminiDir)

	sessionID := "abcd1234-gemini-success-test"
	transcriptContent := `{"messages":[{"type":"user","content":"fix the login bug"},{"type":"gemini","content":"I will fix the login bug now."}]}`
	transcriptFile := filepath.Join(geminiDir, "session-2026-01-01T10-00-abcd1234.json")
	if err := os.WriteFile(transcriptFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameGemini, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", output)
	}

	// Verify session state
	store, storeErr := session.NewStateStore(context.Background())
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	state, loadErr := store.Load(context.Background(), sessionID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeGemini)
	}
	if state.SessionTurnCount != 1 {
		t.Errorf("SessionTurnCount = %d, want 1", state.SessionTurnCount)
	}
}

func TestAttach_CursorSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)

	sessionID := "test-attach-cursor-session"
	// Cursor uses JSONL format, same as Claude Code
	transcriptContent := `{"type":"user","message":{"role":"user","content":"add dark mode"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"I'll add dark mode support."},"uuid":"a1"}
`
	// Cursor flat layout: <dir>/<id>.jsonl
	if err := os.WriteFile(filepath.Join(cursorDir, sessionID+".jsonl"), []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameCursor, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeCursor {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeCursor)
	}
	if state.SessionTurnCount != 1 {
		t.Errorf("SessionTurnCount = %d, want 1", state.SessionTurnCount)
	}
}

func TestAttach_CodexSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	codexDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", codexDir)

	sessionID := "019d6c43-1537-7343-9691-1f8cee04fe59"
	transcriptContent := `{"timestamp":"2026-04-08T10:43:48.000Z","type":"session_meta","payload":{"id":"019d6c43-1537-7343-9691-1f8cee04fe59","timestamp":"2026-04-08T10:43:48.000Z"}}
{"timestamp":"2026-04-08T10:43:49.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"investigate attach failure"}]}}
{"timestamp":"2026-04-08T10:43:50.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking into it."}]}}
`
	sessionFile := filepath.Join(codexDir, "2026", "04", "08", "rollout-2026-04-08T10-43-48-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(sessionFile), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameCodex, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeCodex {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeCodex)
	}
	if state.TranscriptPath != sessionFile {
		t.Errorf("TranscriptPath = %q, want %q", state.TranscriptPath, sessionFile)
	}
	if state.LastCheckpointID.IsEmpty() {
		t.Error("expected LastCheckpointID to be set after attach")
	}
}

func TestAttach_FactoryAIDroidSuccess(t *testing.T) {
	setupAttachTestRepo(t)

	droidDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_DROID_PROJECT_DIR", droidDir)

	sessionID := "test-attach-droid-session"
	// Factory AI Droid uses JSONL format
	transcriptContent := `{"type":"user","message":{"role":"user","content":"deploy to staging"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":"Deploying to staging now."},"uuid":"a1"}
`
	// Factory AI Droid: flat <dir>/<id>.jsonl
	if err := os.WriteFile(filepath.Join(droidDir, sessionID+".jsonl"), []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameFactoryAIDroid, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
		return
	}
	if state.AgentType != agent.AgentTypeFactoryAIDroid {
		t.Errorf("AgentType = %q, want %q", state.AgentType, agent.AgentTypeFactoryAIDroid)
	}
	if state.SessionTurnCount != 1 {
		t.Errorf("SessionTurnCount = %d, want 1", state.SessionTurnCount)
	}
}

func TestAttach_CursorNestedLayout(t *testing.T) {
	setupAttachTestRepo(t)

	cursorDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", cursorDir)

	sessionID := "test-cursor-nested-layout"
	transcriptContent := `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"u1"}
`
	// Cursor IDE nested layout: <dir>/<id>/<id>.jsonl
	nestedDir := filepath.Join(cursorDir, sessionID)
	if err := os.MkdirAll(nestedDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, sessionID+".jsonl"), []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameCursor, attachOptions{Force: true})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	if !strings.Contains(out.String(), "Attached session") {
		t.Errorf("expected 'Attached session' in output, got: %s", out.String())
	}
}

// TestAttach_WithReviewFlag exercises runAttach with review mode: the
// attached session must be tagged Kind=agent_review with the given skills
// and the transcript's first prompt captured as ReviewPrompt.
func TestAttach_WithReviewFlag(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-review-001"
	firstPrompt := "please review the auth module for security issues"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"`+firstPrompt+`"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Reviewing now."}]},"uuid":"uuid-2"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{
		Force:                true,
		Review:               true,
		ReviewSkillsOverride: []string{"/pr-review-toolkit:review-pr", "/test-auditor"},
	})
	if err != nil {
		t.Fatalf("runAttach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
	}
	if state.Kind != session.KindAgentReview {
		t.Errorf("Kind = %q, want %q", state.Kind, session.KindAgentReview)
	}
	if len(state.ReviewSkills) != 2 {
		t.Errorf("ReviewSkills = %v, want 2 entries", state.ReviewSkills)
	}
	if state.ReviewPrompt != firstPrompt {
		t.Errorf("ReviewPrompt = %q, want %q", state.ReviewPrompt, firstPrompt)
	}
}

func TestReviewAttach_UsesPendingReviewMarkerDefaults(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-review-attach-marker"
	firstPrompt := "manual session prompt"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"`+firstPrompt+`"},"uuid":"uuid-1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Reviewing now."}]},"uuid":"uuid-2"}
`)
	markerPrompt := "marker prompt\nwith scope"
	markerSkills := []string{"/review", "/test-auditor"}
	repoRoot, err := paths.WorktreeRoot(context.Background())
	if err != nil {
		t.Fatalf("WorktreeRoot: %v", err)
	}
	if err := cliReview.WritePendingReviewMarker(context.Background(), cliReview.PendingReviewMarker{
		AgentName:    string(agent.AgentNameClaudeCode),
		Skills:       markerSkills,
		Prompt:       markerPrompt,
		StartingSHA:  "deadbeef",
		StartedAt:    time.Now().UTC(),
		WorktreePath: repoRoot,
	}); err != nil {
		t.Fatalf("WritePendingReviewMarker: %v", err)
	}

	rootCmd := NewRootCmd()
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"review", "attach", sessionID, "--force"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("review attach failed: %v\nstderr: %s", err, errBuf.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
	}
	if state.Kind != session.KindAgentReview {
		t.Errorf("Kind = %q, want %q", state.Kind, session.KindAgentReview)
	}
	if !reflect.DeepEqual(state.ReviewSkills, markerSkills) {
		t.Errorf("ReviewSkills = %v, want %v", state.ReviewSkills, markerSkills)
	}
	if state.ReviewPrompt != markerPrompt {
		t.Errorf("ReviewPrompt = %q, want marker prompt %q", state.ReviewPrompt, markerPrompt)
	}
	if _, ok, err := cliReview.ReadPendingReviewMarker(context.Background()); err != nil || ok {
		t.Fatalf("pending marker should be cleared after attach: ok=%v err=%v", ok, err)
	}
}

// TestAttach_ReviewWithExistingCheckpointErrors: attempting to tag a session
// that already has a checkpoint is refused. Upgrading an existing
// checkpoint's metadata to carry review fields would require rewriting the
// entire/checkpoints/v1 tree — not supported in this first cut.
func TestAttach_ReviewWithExistingCheckpointErrors(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-review-existing"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"uuid-1"}
`)

	// First attach (non-review) creates a checkpoint.
	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("first attach failed: %v", err)
	}

	// Second attach with --review should error rather than silently
	// linking the existing checkpoint.
	out.Reset()
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{
		Force:                true,
		Review:               true,
		ReviewSkillsOverride: []string{"/pr-review-toolkit:review-pr"},
	})
	if err == nil {
		t.Fatal("expected error when review-attaching a session that already has a checkpoint")
	}
	if !strings.Contains(err.Error(), "already has checkpoint") {
		t.Errorf("error should mention 'already has checkpoint'; got: %v", err)
	}
}

// Regression for the second "review-attach overwrote the session on the
// checkpoint" report: a DIFFERENT session ID (not present in the existing
// checkpoint) must APPEND at the next-available index, not overwrite
// session 0. In the wild this happens when a user runs a manual claude
// session, commits (with the checkpoint trailer), then runs
// `entire review attach <new-session-id>` to record a separate review.
// The expected result is two sessions on the same checkpoint.
func TestAttach_ReviewAppendsAsAdditionalSessionWhenIDDiffers(t *testing.T) {
	setupAttachTestRepo(t)

	// First session: a normal claude-code attach creates the checkpoint
	// and session 0. Amend HEAD with the trailer so the next attach sees
	// an existing checkpoint.
	firstSessionID := "first-session-a-original"
	setupClaudeTranscript(t, firstSessionID, `{"type":"user","message":{"role":"user","content":"first"},"uuid":"u1"}
`)
	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, firstSessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("first attach failed: %v", err)
	}

	// Sanity: HEAD now carries the Entire-Checkpoint trailer.
	repoRoot := mustGetwd(t)
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	headRef, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	headCommit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		t.Fatal(err)
	}
	existingCheckpoints := trailers.ParseAllCheckpoints(headCommit.Message)
	if len(existingCheckpoints) != 1 {
		t.Fatalf("expected one Entire-Checkpoint trailer after first attach; got %v", existingCheckpoints)
	}
	checkpointID := existingCheckpoints[0]

	// Second session: a different sessionID tagged as a review.
	secondSessionID := "second-session-b-review"
	setupClaudeTranscript(t, secondSessionID, `{"type":"user","message":{"role":"user","content":"please review"},"uuid":"u1"}
`)
	out.Reset()
	if err := runAttach(context.Background(), &out, secondSessionID, agent.AgentNameClaudeCode, attachOptions{
		Force:                true,
		Review:               true,
		ReviewSkillsOverride: []string{"/review"},
	}); err != nil {
		t.Fatalf("review attach failed: %v", err)
	}

	// Read the checkpoint summary and verify BOTH sessions are present.
	// Pre-fix observation: session 0 is OVERWRITTEN with the review session,
	// losing the original attach. The summary has only one session entry
	// despite two attach calls with different IDs.
	store := cpkg.NewGitStore(repo)
	summary, err := store.ReadCommitted(context.Background(), checkpointID)
	if err != nil {
		t.Fatalf("ReadCommitted(%s): %v", checkpointID, err)
	}
	if summary == nil {
		t.Fatalf("checkpoint %s summary nil after two attaches", checkpointID)
	}
	if len(summary.Sessions) != 2 {
		t.Fatalf("checkpoint has %d sessions, want 2 (original attach + review attach). "+
			"Session-0 overwrite bug: findSessionIndex returned 0 instead of appending.", len(summary.Sessions))
	}

	// Explicitly confirm each session's ID is in the checkpoint.
	var idx0, idx1 *cpkg.SessionContent
	if idx0, err = store.ReadSessionContent(context.Background(), checkpointID, 0); err != nil {
		t.Fatalf("ReadSessionContent(0): %v", err)
	}
	if idx1, err = store.ReadSessionContent(context.Background(), checkpointID, 1); err != nil {
		t.Fatalf("ReadSessionContent(1): %v", err)
	}
	haveFirst := idx0.Metadata.SessionID == firstSessionID || idx1.Metadata.SessionID == firstSessionID
	haveSecond := idx0.Metadata.SessionID == secondSessionID || idx1.Metadata.SessionID == secondSessionID
	if !haveFirst {
		t.Errorf("first session %q missing from checkpoint (overwritten?); got [%q, %q]",
			firstSessionID, idx0.Metadata.SessionID, idx1.Metadata.SessionID)
	}
	if !haveSecond {
		t.Errorf("second session %q missing from checkpoint; got [%q, %q]",
			secondSessionID, idx0.Metadata.SessionID, idx1.Metadata.SessionID)
	}
}

// Reproduces the cross-user "missing checkpoint data" scenario: a
// teammate pushed a branch with commits whose messages carry
// Entire-Checkpoint trailers, but the orphan `entire/checkpoints/v1`
// branch holding the actual session data wasn't fetched (git pull
// doesn't bring it in by default). Running review-attach against the
// trailer used to silently CREATE a fresh checkpoint, orphaning the
// original session on push. Now it must refuse with a clear
// "run `git fetch ...` or ask them to push" message.
func TestAttach_ReviewRefusesWhenCheckpointMissingFromLocalBranch(t *testing.T) {
	setupAttachTestRepo(t)

	// Simulate a teammate's commit: amend HEAD with an Entire-Checkpoint
	// trailer that points at a checkpoint ID the local entire/checkpoints/v1
	// branch doesn't know about. No corresponding checkpoint data is
	// written locally — that's the whole point.
	repoRoot := mustGetwd(t)
	runGitInDir(t, repoRoot, "commit", "--amend", "--no-edit", "-m", "init\n\nEntire-Checkpoint: ffffffffeeee")

	sessionID := "orphaned-review-session"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"review please"},"uuid":"u1"}
`)

	var out bytes.Buffer
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{
		Force:                true,
		Review:               true,
		ReviewSkillsOverride: []string{"/review"},
	})
	if err == nil {
		t.Fatal("expected error: checkpoint referenced by HEAD is missing locally and attach should refuse")
	}
	if !strings.Contains(err.Error(), "missing from the local entire/checkpoints/v1 branch") {
		t.Errorf("error message should explain the missing-branch situation; got: %v", err)
	}
	if !strings.Contains(err.Error(), "git fetch origin entire/checkpoints/v1") {
		t.Errorf("error message should include the fetch command to fix it; got: %v", err)
	}

	// Confirm no fresh checkpoint was created for the orphaned ID.
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	store := cpkg.NewGitStore(repo)
	summary, err := store.ReadCommitted(context.Background(), "ffffffffeeee")
	if err != nil {
		t.Fatalf("ReadCommitted: %v", err)
	}
	if summary != nil {
		t.Errorf("attach should NOT have created checkpoint ffffffffeeee locally; found %+v", summary)
	}
}

// runGitInDir runs `git <args>` in the given directory, failing the test
// on error. Used to amend commits with synthetic trailers for test setup.

// Regression for the "review-attach overwrote the existing session"
// bug: the LastCheckpointID guard in session state only catches the case
// where the state file tracks the checkpoint. A session that's already
// in a checkpoint on HEAD but whose state file is missing, stale, or
// has an empty LastCheckpointID would bypass that guard — findSessionIndex
// would then match by SessionID and overwrite the existing session's
// metadata. Defense-in-depth check against the on-disk checkpoint must
// catch it too.
func TestAttach_ReviewWithExistingCheckpointErrorsEvenWithoutSessionState(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-review-no-state"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"hello"},"uuid":"uuid-1"}
`)

	// First attach (non-review) creates a checkpoint and writes session state.
	var out bytes.Buffer
	if err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{Force: true}); err != nil {
		t.Fatalf("first attach failed: %v", err)
	}

	// Delete the session state file to simulate the gap: state lost but
	// the checkpoint on disk still has the session. (Real-world triggers:
	// state never written, state file manually removed, condensation path
	// that didn't update LastCheckpointID.)
	repoRoot := mustGetwd(t)
	stateFile := filepath.Join(repoRoot, ".git", "entire-sessions", sessionID+".json")
	if err := os.Remove(stateFile); err != nil {
		t.Fatalf("remove state file: %v", err)
	}

	// Second attach with --review must error. Without the defense-in-depth
	// guard, this call would silently overwrite the existing session's
	// metadata in the checkpoint with review-flavored metadata.
	out.Reset()
	err := runAttach(context.Background(), &out, sessionID, agent.AgentNameClaudeCode, attachOptions{
		Force:                true,
		Review:               true,
		ReviewSkillsOverride: []string{"/pr-review-toolkit:review-pr"},
	})
	if err == nil {
		t.Fatal("expected error when review-attaching a session already recorded in HEAD's checkpoint, even without session state")
	}
	if !strings.Contains(err.Error(), "already recorded in checkpoint") {
		t.Errorf("error should mention 'already recorded in checkpoint'; got: %v", err)
	}
}

// Regression: attach must NOT silently attach skills from the spawn-path
// config. settings.Review[agent] is what the user would run if they used
// `entire review`, not a claim about what ran in a given manual session.
// Only explicit --skills counts as a user assertion; without it,
// ReviewSkills must be empty even when config exists.
func TestAttachCmd_ReviewDoesNotInferSkillsFromConfig(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-review-no-leak"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"review please"},"uuid":"uuid-1"}
`)

	// Seed review config — the spawn-path default. Attach must ignore this.
	if err := cliReview.SaveReviewConfig(context.Background(), map[string]settings.ReviewConfig{
		"claude-code": {Skills: []string{"/pr-review-toolkit:review-pr"}},
	}); err != nil {
		t.Fatal(err)
	}

	rootCmd := NewRootCmd()
	rootCmd.SetArgs([]string{"attach", "--force", "--review", sessionID})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("attach --review failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Kind != session.KindAgentReview {
		t.Errorf("expected session tagged as review; got state=%+v", state)
	}
	if len(state.ReviewSkills) != 0 {
		t.Errorf("ReviewSkills leaked from spawn config: %v; want empty (no --skills passed)", state.ReviewSkills)
	}
}

// TestReviewAttachCmd_TagsSession drives `entire review attach <id>`,
// verifying the subcommand reaches runAttach with review options set.
func TestReviewAttachCmd_TagsSession(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-review-attach-cmd-001"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"check this out"},"uuid":"uuid-1"}
`)

	rootCmd := NewRootCmd()
	rootCmd.SetArgs([]string{"review", "attach", "--force", "--skills", "/custom-review", sessionID})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("review attach failed: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Kind != session.KindAgentReview {
		t.Errorf("expected session tagged as review; got state=%+v", state)
	}
	if len(state.ReviewSkills) != 1 || state.ReviewSkills[0] != "/custom-review" {
		t.Errorf("--skills override not applied: %v", state.ReviewSkills)
	}
}

// TestAttachCmd_ReviewWithoutSkillsOrConfigErrors: the --review flag
// requires either a --skills override or configured skills. Otherwise we
// error rather than tagging a review with an empty skills list.
// TestAttachCmd_ReviewWithoutSkillsOrConfigSucceeds: --review must not
// block attach when neither --skills nor configured skills exist. The
// review is still tagged via Kind + ReviewPrompt (the session's first
// user prompt); ReviewSkills is the queryable convenience, not the
// source of truth, and is allowed to be empty.
func TestAttachCmd_ReviewWithoutSkillsOrConfigSucceeds(t *testing.T) {
	setupAttachTestRepo(t)

	sessionID := "test-attach-review-no-skills"
	firstPrompt := "please review this change end-to-end"
	setupClaudeTranscript(t, sessionID, `{"type":"user","message":{"role":"user","content":"`+firstPrompt+`"},"uuid":"uuid-1"}
`)

	rootCmd := NewRootCmd()
	rootCmd.SetArgs([]string{"attach", "--force", "--review", sessionID})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("attach --review without skills config should succeed; got error: %v", err)
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("expected session state to be created")
	}
	if state.Kind != session.KindAgentReview {
		t.Errorf("Kind = %q, want %q", state.Kind, session.KindAgentReview)
	}
	if state.ReviewPrompt != firstPrompt {
		t.Errorf("ReviewPrompt = %q, want %q", state.ReviewPrompt, firstPrompt)
	}
	if len(state.ReviewSkills) != 0 {
		t.Errorf("ReviewSkills = %v, want empty (no --skills, no config)", state.ReviewSkills)
	}
}

// Regression: `entire attach --review <gemini-session-id>` without
// --agent must attach successfully. The plain attach flow already
// auto-detects Gemini from the transcript; the review path must not
// add a blocking pre-check against the --agent flag's default
// (claude-code), which would have failed when claude-code had no
// matching transcript/config.
func TestAttachCmd_ReviewAutoDetectsAgent(t *testing.T) {
	setupAttachTestRepo(t)

	// Force claude-code transcript lookup to fail so auto-detect kicks in.
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Create a valid Gemini transcript in the expected project dir.
	geminiDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_GEMINI_PROJECT_DIR", geminiDir)
	sessionID := "abcd1234-review-gemini-autodetect"
	transcriptContent := `{"messages":[{"type":"user","content":"review this"},{"type":"gemini","content":"reviewing"}]}`
	transcriptFile := filepath.Join(geminiDir, "session-2026-01-01T10-00-abcd1234.json")
	if err := os.WriteFile(transcriptFile, []byte(transcriptContent), 0o600); err != nil {
		t.Fatal(err)
	}

	// Invoke without --agent (flag falls through to DefaultAgentName =
	// claude-code). runAttach's auto-detect should find Gemini.
	rootCmd := NewRootCmd()
	var errBuf, outBuf bytes.Buffer
	rootCmd.SetErr(&errBuf)
	rootCmd.SetOut(&outBuf)
	rootCmd.SetArgs([]string{"attach", "--force", "--review", sessionID})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("attach --review with auto-detect failed: %v\nstderr: %s", err, errBuf.String())
	}

	store, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.Kind != session.KindAgentReview {
		t.Fatalf("expected session tagged as review; got state=%+v", state)
	}
	if state.AgentType != agent.AgentTypeGemini {
		t.Errorf("AgentType = %q, want %q (auto-detect should have found Gemini)", state.AgentType, agent.AgentTypeGemini)
	}
}

// setupAttachTestRepo creates a temp git repo with one commit and enables Entire.
// Returns the repo directory. Caller must not use t.Parallel() (uses t.Chdir).
func setupAttachTestRepo(t *testing.T) {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "init.txt", "init")
	testutil.GitAdd(t, tmpDir, "init.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)
	enableEntire(t, tmpDir)
}

// setupClaudeTranscript creates a fake Claude transcript file.
// The file's mtime is backdated so that waitForTranscriptFlush treats it as
// stale and skips the 3-second poll loop.
func setupClaudeTranscript(t *testing.T, sessionID, content string) {
	t.Helper()
	claudeDir := t.TempDir()
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)
	fpath := filepath.Join(claudeDir, sessionID+".jsonl")
	if err := os.WriteFile(fpath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(fpath, stale, stale); err != nil {
		t.Fatal(err)
	}
}

// enableEntire creates the .entire/settings.json file to mark Entire as enabled.
func enableEntire(t *testing.T, repoDir string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	settingsContent := `{"enabled": true}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsContent), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setAttachCheckpointsV2Enabled(t *testing.T, repoDir string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	settingsContent := `{"enabled": true, "strategy_options": {"checkpoints_v2": true}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsContent), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setAttachCheckpointsV2Only(t *testing.T, repoDir string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatal(err)
	}
	settingsContent := `{"enabled": true, "strategy_options": {"checkpoints_version": 2}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsContent), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func readFileFromRef(t *testing.T, repo *git.Repository, refName, filePath string) (string, bool) {
	t.Helper()

	ref, err := repo.Reference(plumbing.ReferenceName(refName), true)
	if err != nil {
		return "", false
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}
	file, err := tree.File(filePath)
	if err != nil {
		return "", false
	}
	content, err := file.Contents()
	if err != nil {
		return "", false
	}
	return content, true
}

// TestAttach_DiscoversExternalAgents verifies that `entire attach --agent <external>`
// gets past the agent registry check when external_agents is enabled and a
// matching binary is on PATH. Without the DiscoverAndRegister call in the
// attach command, this would fail with "unknown agent: <name>".
//
// This test does not verify end-to-end attach behavior — it asserts only
// that discovery ran. The command is expected to fail later (transcript
// resolution) because we don't stand up a real session.
func TestAttach_DiscoversExternalAgents(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	setupAttachTestRepo(t)

	// Overwrite settings to enable external_agents (enableEntire writes the
	// file without it).
	cwd := mustGetwd(t)
	settingsPath := filepath.Join(cwd, ".entire", "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"enabled":true,"external_agents":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Use a unique name so concurrent test runs can't collide in the global
	// agent registry.
	agentName := types.AgentName("attachtest-discovery-agent")

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "entire-agent-"+string(agentName))
	infoJSON := `{
  "protocol_version": 1,
  "name": "` + string(agentName) + `",
  "type": "Attach Test Agent",
  "description": "Agent for attach discovery test",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {}
}`
	script := "#!/bin/sh\nif [ \"$1\" = \"info\" ]; then\n  echo '" + infoJSON + "'\nfi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock agent binary: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cmd := newAttachCmd()
	// Pass a bogus session ID — the point is to exercise the registry check,
	// not full attach flow.
	cmd.SetArgs([]string{"--agent", string(agentName), "-f", "fake-session-id"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	err := cmd.Execute()
	// We expect an error (no transcript), but it must not be the
	// registry-lookup error. A regression (removing DiscoverAndRegister)
	// would produce "unknown agent: attachtest-discovery-agent".
	if err == nil {
		t.Fatalf("expected attach to fail on missing transcript, got success\noutput: %s", out.String())
	}
	if strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("attach did not discover external agent — got registry miss: %v", err)
	}

	// Also confirm the agent actually landed in the registry, so the check
	// above is meaningful (not merely passing because some other error
	// short-circuited before the registry lookup).
	if _, lookupErr := agent.Get(agentName); lookupErr != nil {
		t.Errorf("expected external agent %q in registry after attach, got: %v", agentName, lookupErr)
	}
}

func runGitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
