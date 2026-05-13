package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"
)

func TestReviewCheckpointContext_IncludesSummaryAndPromptFallback(t *testing.T) {
	t.Parallel()

	repoRoot := newReviewContextRepo(t)
	const summaryCheckpointID = "a1b2c3d4e5f6"
	writeReviewContextCheckpoint(t, repoRoot, summaryCheckpointID, reviewContextCheckpointOptions{
		filesTouched: []string{"summary.go"},
		agentType:    agent.AgentTypeClaudeCode,
		summary: &checkpoint.Summary{
			Intent:    "add checkpoint context to review prompts",
			Outcome:   "review prompt sees checkpoint summaries",
			OpenItems: []string{"cover prompt fallback"},
		},
		prompts:    []string{"summary fallback prompt should not appear"},
		transcript: `{"event":"raw summary transcript"}` + "\n",
	})
	commitReviewContextChange(t, repoRoot, "summary.go", "summary\n", "summary change", "Entire-Checkpoint: "+summaryCheckpointID)

	const promptCheckpointID = "b1b2c3d4e5f6"
	writeReviewContextCheckpoint(t, repoRoot, promptCheckpointID, reviewContextCheckpointOptions{
		filesTouched: []string{"prompt.go"},
		agentType:    agent.AgentTypeClaudeCode,
		prompts:      []string{"Implement prompt fallback when summaries are missing"},
		transcript:   `{"event":"raw prompt transcript"}` + "\n",
	})
	commitReviewContextChange(t, repoRoot, "prompt.go", "prompt\n", "prompt change", "Entire-Checkpoint: "+promptCheckpointID)

	got := reviewCheckpointContext(context.Background(), repoRoot, "master")
	for _, want := range []string{
		"Checkpoint context from commits in scope:",
		summaryCheckpointID,
		"summary: add checkpoint context to review prompts; review prompt sees checkpoint summaries; open: cover prompt fallback",
		promptCheckpointID,
		"prompt: Implement prompt fallback when summaries are missing",
		"entire explain <id>",
		"entire explain <id> --raw-transcript",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("review checkpoint context missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{
		"summary fallback prompt should not appear",
		"raw summary transcript",
		"raw prompt transcript",
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("review checkpoint context contains %q:\n%s", unwanted, got)
		}
	}
}

func TestReviewCheckpointContext_CapsCheckpointLines(t *testing.T) {
	t.Parallel()

	repoRoot := newReviewContextRepo(t)
	var oldestCheckpointID string
	for i := range reviewContextMaxCheckpoints + 1 {
		checkpointID := fmt.Sprintf("c%011x", i)
		if i == 0 {
			oldestCheckpointID = checkpointID
		}
		writeReviewContextCheckpoint(t, repoRoot, checkpointID, reviewContextCheckpointOptions{
			filesTouched: []string{fmt.Sprintf("checkpoint-%02d.go", i)},
			agentType:    agent.AgentTypeClaudeCode,
			summary: &checkpoint.Summary{
				Intent: fmt.Sprintf("checkpoint summary %02d", i),
			},
			transcript: `{"event":"test"}` + "\n",
		})
		commitReviewContextChange(
			t,
			repoRoot,
			fmt.Sprintf("checkpoint-%02d.go", i),
			fmt.Sprintf("checkpoint %02d\n", i),
			fmt.Sprintf("checkpoint change %02d", i),
			"Entire-Checkpoint: "+checkpointID,
		)
	}

	got := reviewCheckpointContext(context.Background(), repoRoot, "master")
	if count := strings.Count(got, "summary: checkpoint summary"); count != reviewContextMaxCheckpoints {
		t.Fatalf("checkpoint context summary count = %d, want %d:\n%s", count, reviewContextMaxCheckpoints, got)
	}
	if strings.Contains(got, oldestCheckpointID) {
		t.Fatalf("checkpoint context includes oldest checkpoint %s despite cap:\n%s", oldestCheckpointID, got)
	}
	if !strings.Contains(got, "1 more checkpoint omitted") {
		t.Fatalf("checkpoint context missing truncation notice:\n%s", got)
	}
}

func TestReviewCheckpointDetail_ReadsSessionMetadataOnceForPromptFallback(t *testing.T) {
	t.Parallel()

	cpID := checkpointid.MustCheckpointID("d1b2c3d4e5f6")
	reader := &countingReviewContextReader{
		metadata: checkpoint.CommittedMetadata{
			CheckpointID: cpID,
			SessionID:    "session-1",
		},
		prompts: "Fallback prompt from checkpoint",
	}
	summary := &checkpoint.CheckpointSummary{
		Sessions: []checkpoint.SessionFilePaths{{}},
	}

	got := reviewCheckpointDetail(context.Background(), reader, cpID, summary)
	if got != "prompt: Fallback prompt from checkpoint" {
		t.Fatalf("reviewCheckpointDetail() = %q", got)
	}
	if reader.metadataCalls != 1 {
		t.Fatalf("metadata calls = %d, want 1", reader.metadataCalls)
	}
	if reader.promptCalls != 1 {
		t.Fatalf("prompt calls = %d, want 1", reader.promptCalls)
	}
}

func TestReviewCommandSmoke_IncludesCheckpointContextInPrompt(t *testing.T) {
	repoRoot := newReviewContextRepo(t)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	installReviewContextClaudeHooks(t)
	writeReviewContextSettings(t, repoRoot)

	stubDir := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	writeReviewContextClaudeStub(t, stubDir)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ENTIRE_SMOKE_PROMPT_FILE", promptPath)

	const checkpointID = "f1b2c3d4e5f6"
	writeReviewContextCheckpoint(t, repoRoot, checkpointID, reviewContextCheckpointOptions{
		filesTouched: []string{"checkpointed.go"},
		agentType:    agent.AgentTypeClaudeCode,
		summary: &checkpoint.Summary{
			Intent:  "smoke checkpoint summary",
			Outcome: "review smoke receives checkpoint summary",
		},
		transcript: `{"event":"test"}` + "\n",
	})
	commitReviewContextChange(t, repoRoot, "checkpointed.go", "checkpointed\n", "implement checkpointed change", "Entire-Checkpoint: "+checkpointID)

	cmd := NewRootCmd()
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"review", "--agent", string(agent.AgentNameClaudeCode)})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("entire review failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}

	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read captured prompt: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	prompt := string(promptBytes)
	for _, want := range []string{
		"/review",
		"Scope: review the commits unique to this branch vs master, plus any uncommitted changes in the working tree. Ignore code outside this scope.",
		"Checkpoint context from commits in scope:",
		checkpointID,
		"summary: smoke checkpoint summary; review smoke receives checkpoint summary",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("captured review prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestReviewCommandSmoke_IncludesInProgressSessionContextInPrompt verifies
// that an active session whose state file matches the current worktree +
// base commit produces an "In-progress session context (uncommitted):"
// block in the captured agent prompt. This is the end-to-end analog of
// TestReviewSessionContext_IncludesActiveSessionWithLatestPrompt — it
// catches wiring regressions between reviewSessionContext, the deps bridge,
// and ComposeReviewPrompt that the unit-level tests cannot.
func TestReviewCommandSmoke_IncludesInProgressSessionContextInPrompt(t *testing.T) {
	repoRoot := newReviewContextRepo(t)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	installReviewContextClaudeHooks(t)
	writeReviewContextSettings(t, repoRoot)

	stubDir := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	writeReviewContextClaudeStub(t, stubDir)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ENTIRE_SMOKE_PROMPT_FILE", promptPath)

	// Active session state matching the current worktree + HEAD.
	headSHA := testutil.GetHeadHash(t, repoRoot)
	const sessionID = "019e0c0c-aaaa-7000-bbbb-ccccdddd0001"
	writeReviewContextSessionState(t, repoRoot, session.State{
		SessionID:    sessionID,
		WorktreePath: repoRoot,
		BaseCommit:   headSHA,
		AgentType:    agent.AgentTypeClaudeCode,
	})
	writeReviewContextSessionPrompt(t, repoRoot, sessionID,
		"Refactor the auth flow.\n\n---\n\nAlso add retry tests for token refresh.")

	cmd := NewRootCmd()
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"review", "--agent", string(agent.AgentNameClaudeCode)})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("entire review failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}

	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read captured prompt: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	prompt := string(promptBytes)
	for _, want := range []string{
		"In-progress session context (uncommitted):",
		sessionID[:8],
		// Latest prompt of the session — same convention as committed-fallback.
		"prompt: Also add retry tests for token refresh.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("captured review prompt missing %q:\n%s", want, prompt)
		}
	}
}

// TestReviewCommandSmoke_BaseFlagThreadsThroughToPromptAndBanner verifies
// that the `--base <ref>` flag survives the full cobra → runReview →
// runSingleAgentPath → detectScope → ComputeScopeStats → ComposeReviewPrompt
// chain. Without a command-level test, regressions in the flag wiring (like
// the silentErr suppression bug caught in smoke) wouldn't be caught by the
// unit tests that exercise ComputeScopeStats in isolation.
func TestReviewCommandSmoke_BaseFlagThreadsThroughToPromptAndBanner(t *testing.T) {
	repoRoot := newReviewContextRepo(t)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	// Create feat/parent at the current HEAD (which is feat/review's branch
	// point). --base feat/parent will then be a valid override.
	//nolint:noctx // test helper
	branchCmd := exec.Command("git", "branch", "feat/parent")
	branchCmd.Dir = repoRoot
	if out, err := branchCmd.CombinedOutput(); err != nil {
		t.Fatalf("create feat/parent: %v\n%s", err, out)
	}

	// Add a commit on feat/review so the scope is non-empty.
	commitReviewContextChange(t, repoRoot, "feature.go", "feat\n", "add feature", "")

	installReviewContextClaudeHooks(t)
	writeReviewContextSettings(t, repoRoot)

	stubDir := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	writeReviewContextClaudeStub(t, stubDir)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ENTIRE_SMOKE_PROMPT_FILE", promptPath)

	cmd := NewRootCmd()
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"review", "--agent", string(agent.AgentNameClaudeCode), "--base", "feat/parent"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("entire review failed: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}

	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("read captured prompt: %v\nstdout:\n%s\nstderr:\n%s", err, out.String(), errOut.String())
	}
	prompt := string(promptBytes)
	if !strings.Contains(prompt, "vs feat/parent") {
		t.Errorf("agent prompt must include scope clause referencing the --base override; got:\n%s", prompt)
	}
	if !strings.Contains(out.String(), "vs feat/parent") {
		t.Errorf("scope banner must reflect --base override; got stdout:\n%s", out.String())
	}
}

// TestReviewCommandSmoke_BadBaseRefErrorsBeforeAgentSpawn verifies that a
// non-existent --base ref aborts the run before the agent is invoked, with
// an error message that names the bad ref so the user can fix it.
// Regression guard for the silentErr-suppression bug where the validation
// error existed but was swallowed by main.go's SilentError handling.
func TestReviewCommandSmoke_BadBaseRefErrorsBeforeAgentSpawn(t *testing.T) {
	repoRoot := newReviewContextRepo(t)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	installReviewContextClaudeHooks(t)
	writeReviewContextSettings(t, repoRoot)

	stubDir := t.TempDir()
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	writeReviewContextClaudeStub(t, stubDir)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ENTIRE_SMOKE_PROMPT_FILE", promptPath)

	cmd := NewRootCmd()
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"review", "--agent", string(agent.AgentNameClaudeCode), "--base", "no-such-ref"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("expected non-nil error for invalid --base ref; stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
	if !strings.Contains(err.Error(), "no-such-ref") {
		t.Errorf("error must name the bad ref so the user knows what to fix; got: %v", err)
	}
	// Agent stub must NOT have been invoked.
	if _, statErr := os.Stat(promptPath); statErr == nil {
		captured, _ := os.ReadFile(promptPath) //nolint:errcheck // best-effort debug read
		t.Errorf("agent stub was invoked despite invalid --base; captured prompt:\n%s", string(captured))
	}
}

func newReviewContextRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	testutil.WriteFile(t, tmp, "base.txt", "base\n")
	testutil.GitAdd(t, tmp, "base.txt")
	testutil.GitCommit(t, tmp, "base")
	testutil.GitCheckoutNewBranch(t, tmp, "feat/review")
	return tmp
}

func commitReviewContextChange(t *testing.T, repoRoot, path, content, subject, body string) {
	t.Helper()
	testutil.WriteFile(t, repoRoot, path, content)
	testutil.GitAdd(t, repoRoot, path)
	message := subject
	if body != "" {
		message += "\n\n" + body
	}
	testutil.GitCommit(t, repoRoot, message)
}

type reviewContextCheckpointOptions struct {
	filesTouched []string
	agentType    types.AgentType
	summary      *checkpoint.Summary
	prompts      []string
	transcript   string
}

func writeReviewContextCheckpoint(t *testing.T, repoRoot string, checkpointID string, opts reviewContextCheckpointOptions) {
	t.Helper()
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	cpID := checkpointid.MustCheckpointID(checkpointID)
	err = checkpoint.NewGitStore(repo).WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID:     cpID,
		SessionID:        checkpointID,
		Strategy:         "manual-commit",
		Branch:           "feat/review",
		Transcript:       redact.AlreadyRedacted([]byte(opts.transcript)),
		Prompts:          opts.prompts,
		FilesTouched:     opts.filesTouched,
		CheckpointsCount: 1,
		Agent:            opts.agentType,
		Summary:          opts.summary,
	})
	if err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
}

func installReviewContextClaudeHooks(t *testing.T) {
	t.Helper()
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		t.Fatalf("agent.Get(%q): %v", agent.AgentNameClaudeCode, err)
	}
	hs, ok := agent.AsHookSupport(ag)
	if !ok {
		t.Fatalf("agent %q does not support hooks", agent.AgentNameClaudeCode)
	}
	if _, err := hs.InstallHooks(context.Background(), false, false); err != nil {
		t.Fatalf("InstallHooks(%q): %v", agent.AgentNameClaudeCode, err)
	}
}

func writeReviewContextSettings(t *testing.T, repoRoot string) {
	t.Helper()
	entireDir := filepath.Join(repoRoot, ".entire")
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("create .entire dir: %v", err)
	}
	settingsJSON := `{"enabled":true,"review":{"claude-code":{"skills":["/review"]}}}` + "\n"
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(settingsJSON), 0o600); err != nil {
		t.Fatalf("write review settings: %v", err)
	}
}

func writeReviewContextClaudeStub(t *testing.T, stubDir string) {
	t.Helper()
	script := `#!/bin/sh
printf '%s' "$2" > "$ENTIRE_SMOKE_PROMPT_FILE"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"usage":{"input_tokens":0,"output_tokens":0}}'
`
	if err := os.WriteFile(filepath.Join(stubDir, "claude"), []byte(script), 0o700); err != nil {
		t.Fatalf("write claude stub: %v", err)
	}
}

type countingReviewContextReader struct {
	metadata      checkpoint.CommittedMetadata
	prompts       string
	metadataErr   error
	promptErr     error
	metadataCalls int
	promptCalls   int
}

func (r *countingReviewContextReader) ReadCommitted(
	context.Context,
	checkpointid.CheckpointID,
) (*checkpoint.CheckpointSummary, error) {
	return nil, checkpoint.ErrCheckpointNotFound
}

func (r *countingReviewContextReader) ReadSessionContent(
	context.Context,
	checkpointid.CheckpointID,
	int,
) (*checkpoint.SessionContent, error) {
	return &checkpoint.SessionContent{
		Metadata: r.metadata,
		Prompts:  r.prompts,
	}, nil
}

func (r *countingReviewContextReader) ReadSessionMetadata(
	context.Context,
	checkpointid.CheckpointID,
	int,
) (*checkpoint.CommittedMetadata, error) {
	r.metadataCalls++
	return &r.metadata, r.metadataErr
}

func (r *countingReviewContextReader) ReadSessionMetadataAndPrompts(
	context.Context,
	checkpointid.CheckpointID,
	int,
) (*checkpoint.SessionContent, error) {
	r.promptCalls++
	return &checkpoint.SessionContent{
		Metadata: r.metadata,
		Prompts:  r.prompts,
	}, r.promptErr
}

// TestReviewSessionContext_IncludesActiveSessionWithLatestPrompt verifies
// that an active session whose worktree + base commit match the current
// review context produces a "prompt:" entry in the in-progress section,
// mirroring the committed-checkpoint pipeline's prompt-fallback format.
func TestReviewSessionContext_IncludesActiveSessionWithLatestPrompt(t *testing.T) {
	repoRoot := newReviewContextRepo(t)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	headSHA := testutil.GetHeadHash(t, repoRoot)

	const sessionID = "019e0871-c1b2-7000-aa11-bb22cc33dd44"
	writeReviewContextSessionState(t, repoRoot, session.State{
		SessionID:    sessionID,
		WorktreePath: repoRoot,
		BaseCommit:   headSHA,
		AgentType:    agent.AgentTypeClaudeCode,
	})
	writeReviewContextSessionPrompt(t, repoRoot, sessionID,
		"Implement the auth feature.\n\n---\n\nAlso handle the edge case for refresh tokens.")

	got := reviewSessionContext(context.Background(), repoRoot, headSHA)
	for _, want := range []string{
		"In-progress session context (uncommitted):",
		sessionID[:8],
		// state.AgentType is the display name ("Claude Code"), not the
		// registry slug. Display name is what the user already sees in
		// `entire session list` output, so it stays consistent here.
		"Claude Code",
		// Latest prompt wins per the same convention as the committed
		// fallback path (reviewPromptText loops backwards).
		"prompt: Also handle the edge case for refresh tokens.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected section to contain %q, got:\n%s", want, got)
		}
	}
}

// TestReviewSessionContext_SkipsSessionsOutsideScope verifies the four
// exclusion criteria: condensed, wrong worktree, wrong base commit, review-
// kind sessions. Each is set up in isolation; the helper returns "" when
// no in-scope sessions remain.
func TestReviewSessionContext_SkipsSessionsOutsideScope(t *testing.T) {
	repoRoot := newReviewContextRepo(t)
	t.Chdir(repoRoot)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	headSHA := testutil.GetHeadHash(t, repoRoot)
	otherSHA := strings.Repeat("0", len(headSHA))

	// Each of the four exclusion conditions, plus a prompt for each so we
	// could tell if they leaked into output.
	cases := []struct {
		name  string
		state session.State
	}{
		{"FullyCondensed", session.State{SessionID: "019e0001-1", WorktreePath: repoRoot, BaseCommit: headSHA, AgentType: agent.AgentTypeClaudeCode, FullyCondensed: true}},
		{"WrongWorktree", session.State{SessionID: "019e0002-2", WorktreePath: "/some/other/repo", BaseCommit: headSHA, AgentType: agent.AgentTypeClaudeCode}},
		{"WrongBaseCommit", session.State{SessionID: "019e0003-3", WorktreePath: repoRoot, BaseCommit: otherSHA, AgentType: agent.AgentTypeClaudeCode}},
		{"KindAgentReview", session.State{SessionID: "019e0004-4", WorktreePath: repoRoot, BaseCommit: headSHA, AgentType: agent.AgentTypeClaudeCode, Kind: session.KindAgentReview}},
	}
	for _, c := range cases {
		writeReviewContextSessionState(t, repoRoot, c.state)
		writeReviewContextSessionPrompt(t, repoRoot, c.state.SessionID, "LEAKED-"+c.name)
	}

	got := reviewSessionContext(context.Background(), repoRoot, headSHA)
	if got != "" {
		t.Fatalf("expected empty section when no in-scope sessions match; got:\n%s", got)
	}
	for _, c := range cases {
		if strings.Contains(got, "LEAKED-"+c.name) {
			t.Errorf("%s session leaked into output", c.name)
		}
	}
}

// writeReviewContextSessionState writes a session.State JSON file to
// .git/entire-sessions/<sessionID>.json so StateStore.List picks it up.
// Bypasses StateStore.Save to avoid pulling in lifecycle dependencies.
func writeReviewContextSessionState(t *testing.T, repoRoot string, state session.State) {
	t.Helper()
	if state.StartedAt.IsZero() {
		state.StartedAt = time.Now()
	}
	dir := filepath.Join(repoRoot, ".git", "entire-sessions")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal session state: %v", err)
	}
	path := filepath.Join(dir, state.SessionID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeReviewContextSessionPrompt writes a prompt.txt file to the session's
// metadata directory at the on-filesystem path lifecycle.go uses for
// mid-turn prompt accumulation.
func writeReviewContextSessionPrompt(t *testing.T, repoRoot, sessionID, content string) {
	t.Helper()
	dir := filepath.Join(repoRoot, paths.SessionMetadataDirFromSessionID(sessionID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, paths.PromptFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
