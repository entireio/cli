package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestTrailResumeCmdRejectsConflictingSelectors(t *testing.T) {
	t.Parallel()

	cmd := newTrailResumeCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"575", "--trail", "feature/a"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error combining positional trail with --trail, got nil")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Fatalf("error = %q, want it to mention 'not both'", err)
	}
}

func TestValidateTrailResumeOptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		opts    trailResumeOptions
		wantErr string
	}{
		{
			name:    "session and checkpoint conflict",
			opts:    trailResumeOptions{SessionID: "session-1", CheckpointID: "0123456789ab"},
			wantErr: "cannot combine --session and --checkpoint",
		},
		{
			name:    "json requires no resume",
			opts:    trailResumeOptions{JSON: true},
			wantErr: "--json can only be used with --no-resume",
		},
		{
			name: "json no resume accepted",
			opts: trailResumeOptions{JSON: true, NoResume: true},
		},
		{
			name:    "invalid repo assertion",
			opts:    trailResumeOptions{ExpectedRepo: "not a repo"},
			wantErr: "validate --repo",
		},
		{
			name: "repo assertion accepted",
			opts: trailResumeOptions{ExpectedRepo: "entireio/cli"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateTrailResumeOptions(tc.opts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTrailResumeOptions() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTrailResumeOptions() = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateTrailResumeExpectedRepo(t *testing.T) {
	t.Parallel()

	current := trailResumeRepository{Forge: "gh", Owner: "EntireIO", Repo: "CLI"}
	expected := trailResumeRepository{Forge: "gh", Owner: "entireio", Repo: "cli"}
	if err := validateTrailResumeExpectedRepo(current, expected); err != nil {
		t.Fatalf("validateTrailResumeExpectedRepo() matching repo = %v, want nil", err)
	}
	if err := validateTrailResumeExpectedRepo(current, trailResumeRepository{}); err != nil {
		t.Fatalf("validateTrailResumeExpectedRepo() empty expected repo = %v, want nil", err)
	}

	err := validateTrailResumeExpectedRepo(current, trailResumeRepository{Forge: "gh", Owner: "entireio", Repo: "entire.io"})
	if err == nil {
		t.Fatal("validateTrailResumeExpectedRepo() mismatch = nil, want error")
	}
	for _, want := range []string{"targets repository entireio/entire.io", "current checkout is EntireIO/CLI"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestValidateTrailResumeExpectedBranch(t *testing.T) {
	t.Parallel()

	trail := &api.TrailResource{
		Number: 575,
		Title:  "Add trail resume",
		Branch: "feature/trail-resume",
	}
	if err := validateTrailResumeExpectedBranch(trail, " feature/trail-resume "); err != nil {
		t.Fatalf("validateTrailResumeExpectedBranch() matching branch = %v, want nil", err)
	}
	if err := validateTrailResumeExpectedBranch(trail, ""); err != nil {
		t.Fatalf("validateTrailResumeExpectedBranch() empty expected branch = %v, want nil", err)
	}

	err := validateTrailResumeExpectedBranch(trail, "feature/other")
	if err == nil {
		t.Fatal("validateTrailResumeExpectedBranch() mismatch = nil, want error")
	}
	for _, want := range []string{"trail #575", "feature/trail-resume", "feature/other"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestKnownTrailResumeSessionsForContextTreatsDiscoveryErrorAsUnavailable(t *testing.T) {
	t.Parallel()

	sessions := []trailResumeSessionContext{{
		SessionID:    "known-session",
		CheckpointID: "abc123def456",
	}}
	got, skipped, unavailable := knownTrailResumeSessionsForContext(sessions, 2, errors.New("branch not found locally or on origin"))
	if len(got) != 0 {
		t.Fatalf("knownTrailResumeSessionsForContext() len = %d, want 0: %#v", len(got), got)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0 when sessions are unavailable", skipped)
	}
	if unavailable != "branch not found locally or on origin" {
		t.Fatalf("unavailable = %q", unavailable)
	}
}

func TestBuildTrailResumeContextSortsCheckpointSessions(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	const newSessionID = "new-session"
	ctx := buildTrailResumeContext(api.TrailResource{
		ID:     "trl_1",
		Number: 575,
		Title:  "Add trail resume",
		Branch: "feature/trail-resume",
		Status: "open",
		Phase:  "has_code",
	}, []trailResumeSessionContext{
		{
			SessionID:    "old-session",
			Agent:        "claude-code",
			LastPrompt:   "older work",
			LastActive:   now.Add(-time.Hour),
			CheckpointID: "bbbbbbbbbbbb",
		},
		{
			SessionID:    newSessionID,
			Agent:        "codex",
			LastPrompt:   "newer work",
			LastActive:   now,
			CheckpointID: "aaaaaaaaaaaa",
		},
	}, "", trailResumeFindingsContext{})

	if len(ctx.Sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2: %#v", len(ctx.Sessions), ctx.Sessions)
	}
	if ctx.Sessions[0].SessionID != newSessionID || ctx.Sessions[0].CheckpointID != "aaaaaaaaaaaa" {
		t.Fatalf("first session = %#v, want newest trail session", ctx.Sessions[0])
	}
	if ctx.Sessions[1].SessionID != "old-session" {
		t.Fatalf("second session = %#v, want old-session", ctx.Sessions[1])
	}
	if ctx.DefaultResume == nil || ctx.DefaultResume.SessionID != newSessionID {
		t.Fatalf("DefaultResume = %#v, want new-session", ctx.DefaultResume)
	}
	wantCommands := []string{
		"entire trail finding 575 --json",
		"entire trail resume 575 --branch feature/trail-resume",
		"entire trail resume 575 --branch feature/trail-resume --checkpoint aaaaaaaaaaaa",
		"entire trail resume 575 --branch feature/trail-resume --session new-session",
		"entire trail resume 575 --branch feature/trail-resume --session old-session",
	}
	if len(ctx.Commands) != len(wantCommands) {
		t.Fatalf("commands len = %d, want %d: %#v", len(ctx.Commands), len(wantCommands), ctx.Commands)
	}
	for i, want := range wantCommands {
		if ctx.Commands[i] != want {
			t.Fatalf("commands[%d] = %q, want %q", i, ctx.Commands[i], want)
		}
	}
}

func TestBuildTrailResumeContextWithRepoIncludesRepoInResumeCommands(t *testing.T) {
	t.Parallel()

	ctx := buildTrailResumeContextForRepo(api.TrailResource{
		ID:     "trl_1",
		Number: 575,
		Title:  "Add trail resume",
		Branch: "feature/trail-resume",
	}, nil, "", trailResumeFindingsContext{}, "entireio/cli")

	wantCommands := []string{
		"entire trail finding 575 --json",
		"entire trail resume 575 --repo entireio/cli --branch feature/trail-resume",
	}
	if len(ctx.Commands) != len(wantCommands) {
		t.Fatalf("commands len = %d, want %d: %#v", len(ctx.Commands), len(wantCommands), ctx.Commands)
	}
	for i, want := range wantCommands {
		if ctx.Commands[i] != want {
			t.Fatalf("commands[%d] = %q, want %q", i, ctx.Commands[i], want)
		}
	}
}

func newTrailResumeCheckpointTestRepo(t *testing.T) (string, *git.Repository, *git.Worktree) {
	t.Helper()

	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	testutil.InitRepo(t, tmpDir)
	repo, err := git.PlainOpen(tmpDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if _, err := wt.Add("readme.md"); err != nil {
		t.Fatalf("add readme: %v", err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: testTrailResumeSignature(time.Date(2026, 6, 23, 9, 0, 0, 0, time.UTC))}); err != nil {
		t.Fatalf("commit init: %v", err)
	}

	if err := wt.Checkout(&git.CheckoutOptions{Create: true, Branch: "refs/heads/feature/trail"}); err != nil {
		t.Fatalf("checkout feature: %v", err)
	}
	return tmpDir, repo, wt
}

func TestResolveTrailCheckpointSessionsUsesBranchCheckpointMetadata(t *testing.T) {
	tmpDir, repo, wt := newTrailResumeCheckpointTestRepo(t)
	cpID := id.MustCheckpointID("abc123def456")
	firstTime := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	secondTime := firstTime.Add(time.Hour)
	writeTrailResumeCheckpointSession(t, repo, cpID, "session-alice", firstTime, agent.AgentTypeClaudeCode, "alice started this trail")
	writeTrailResumeCheckpointSession(t, repo, cpID, "session-bob", secondTime, agent.AgentTypeCodex, "bob continued from another machine")
	if err := os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	if _, err := wt.Add("readme.md"); err != nil {
		t.Fatalf("add feature: %v", err)
	}
	if _, err := wt.Commit("feature work\n\nEntire-Checkpoint: "+cpID.String(), &git.CommitOptions{Author: testTrailResumeSignature(secondTime)}); err != nil {
		t.Fatalf("commit feature: %v", err)
	}

	sessions, skipped, err := resolveTrailCheckpointSessions(context.Background(), "feature/trail")
	if err != nil {
		t.Fatalf("resolveTrailCheckpointSessions() error = %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2: %#v", len(sessions), sessions)
	}
	if sessions[0].SessionID != "session-bob" || sessions[0].CheckpointID != cpID.String() {
		t.Fatalf("first session = %#v, want newest checkpoint session", sessions[0])
	}
	if sessions[0].Agent != string(agent.AgentTypeCodex) {
		t.Fatalf("first agent = %q, want %q", sessions[0].Agent, agent.AgentTypeCodex)
	}
	if sessions[0].LastPrompt != "bob continued from another machine" {
		t.Fatalf("first prompt = %q", sessions[0].LastPrompt)
	}
	if sessions[1].SessionID != "session-alice" {
		t.Fatalf("second session = %#v, want session-alice", sessions[1])
	}
}

func TestResolveTrailCheckpointSessionsIncludesAllBranchCheckpoints(t *testing.T) {
	tmpDir, repo, wt := newTrailResumeCheckpointTestRepo(t)
	oldCP := id.MustCheckpointID("abc123def456")
	newCP := id.MustCheckpointID("def456abc123")
	oldTime := time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC)
	newTime := oldTime.Add(time.Hour)
	writeTrailResumeCheckpointSession(t, repo, oldCP, "old-session", oldTime, agent.AgentTypeClaudeCode, "older checkpoint work")
	writeTrailResumeCheckpointSession(t, repo, newCP, "new-session", newTime, agent.AgentTypeCodex, "newer checkpoint work")
	if err := os.WriteFile(filepath.Join(tmpDir, "readme.md"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	if _, err := wt.Add("readme.md"); err != nil {
		t.Fatalf("add feature: %v", err)
	}
	message := "feature work\n\nEntire-Checkpoint: " + oldCP.String() + "\nEntire-Checkpoint: " + newCP.String()
	if _, err := wt.Commit(message, &git.CommitOptions{Author: testTrailResumeSignature(newTime)}); err != nil {
		t.Fatalf("commit feature: %v", err)
	}

	sessions, skipped, err := resolveTrailCheckpointSessions(context.Background(), "feature/trail")
	if err != nil {
		t.Fatalf("resolveTrailCheckpointSessions() error = %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(sessions) != 2 {
		t.Fatalf("sessions len = %d, want 2: %#v", len(sessions), sessions)
	}
	if sessions[0].SessionID != "new-session" || sessions[0].CheckpointID != newCP.String() {
		t.Fatalf("first session = %#v, want newest checkpoint session", sessions[0])
	}
	if sessions[1].SessionID != "old-session" || sessions[1].CheckpointID != oldCP.String() {
		t.Fatalf("second session = %#v, want older checkpoint session", sessions[1])
	}
}

func TestReadTrailCheckpointSessionContextsReportsSkippedSessions(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("abc123def456")
	store := fakeTrailResumeCheckpointReader{
		summary: &checkpoint.CheckpointSummary{
			CheckpointID: cpID,
			Sessions:     make([]checkpoint.SessionFilePaths, 2),
		},
		contents: map[int]*checkpoint.SessionContent{
			0: {
				Metadata: checkpoint.Metadata{
					CheckpointID: cpID,
					SessionID:    "kept-session",
					Agent:        agent.AgentTypeCodex,
					CreatedAt:    time.Date(2026, 6, 23, 10, 0, 0, 0, time.UTC),
				},
				Prompts: "continue the trail",
			},
		},
		errs: map[int]error{1: errors.New("missing session blob")},
	}

	sessions, skipped, err := readTrailCheckpointSessionContexts(context.Background(), store, cpID)
	if err != nil {
		t.Fatalf("readTrailCheckpointSessionContexts() error = %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "kept-session" {
		t.Fatalf("sessions = %#v, want kept-session only", sessions)
	}
}

type fakeTrailResumeCheckpointReader struct {
	summary  *checkpoint.CheckpointSummary
	contents map[int]*checkpoint.SessionContent
	errs     map[int]error
}

func (f fakeTrailResumeCheckpointReader) Read(context.Context, id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	return f.summary, nil
}

func (f fakeTrailResumeCheckpointReader) List(context.Context) ([]checkpoint.CheckpointInfo, error) {
	return nil, nil
}

func (f fakeTrailResumeCheckpointReader) ReadSessionMetadata(_ context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, error) {
	content, err := f.sessionContent(checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	return &content.Metadata, nil
}

func (f fakeTrailResumeCheckpointReader) ReadSessionMetadataAndPrompts(_ context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, string, error) {
	content, err := f.sessionContent(checkpointID, sessionIndex)
	if err != nil {
		return nil, "", err
	}
	return &content.Metadata, content.Prompts, nil
}

func (f fakeTrailResumeCheckpointReader) sessionContent(checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error) {
	if err := f.errs[sessionIndex]; err != nil {
		return nil, err
	}
	content := f.contents[sessionIndex]
	if content == nil {
		return nil, errors.New("missing session content")
	}
	if content.Metadata.CheckpointID != checkpointID {
		return nil, errors.New("unexpected checkpoint ID")
	}
	return content, nil
}

func testTrailResumeSignature(when time.Time) *object.Signature {
	return &object.Signature{
		Name:  "Test User",
		Email: "test@example.com",
		When:  when,
	}
}

func writeTrailResumeCheckpointSession(
	t *testing.T,
	repo *git.Repository,
	checkpointID id.CheckpointID,
	sessionID string,
	createdAt time.Time,
	agentType types.AgentType,
	prompt string,
) {
	t.Helper()

	if err := checkpoint.NewGitStore(repo, checkpoint.DefaultV1Refs()).Write(context.Background(), checkpoint.Session{
		CheckpointID: checkpointID,
		SessionID:    sessionID,
		CreatedAt:    createdAt,
		Strategy:     resumeTestStrategy,
		Branch:       "feature/trail",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"user","message":{"content":[{"type":"text","text":"` + prompt + `"}]}}` + "\n")),
		Prompts:      []string{prompt},
		Agent:        agentType,
		AuthorName:   "Test",
		AuthorEmail:  "test@example.com",
	}); err != nil {
		t.Fatalf("Write(%s): %v", sessionID, err)
	}
}

func TestPrintTrailResumeContextIncludesSessionsFindingsAndCommands(t *testing.T) {
	t.Parallel()

	sev := trailReviewSeverityHigh
	line := 42
	file := "cmd/entire/cli/trail_cmd.go"
	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{
			ID:     "trl_1",
			Number: 575,
			Title:  "Add trail resume",
			Branch: "feature/trail-resume",
			Status: "open",
			Phase:  "has_code",
			URL:    "https://entire.io/gh/o/r/trails/575",
		},
		Sessions: []trailResumeSessionContext{{
			SessionID:    "session-1",
			Agent:        "codex",
			LastPrompt:   "implement trail resume",
			LastActive:   time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC),
			CheckpointID: "aaaaaaaaaaaa",
		}},
		Findings: trailResumeFindingsContext{
			Counts: trailReviewCommentCounts{Open: 1, OpenHigh: 1, Resolved: 2},
			Top: []api.TrailReviewComment{{
				ID:       "finding-1",
				Body:     trailReviewStrPtr("Resume output should show context"),
				Severity: &sev,
				Status:   trailReviewStatusOpen,
				Location: api.TrailReviewLocation{
					Granularity: "line",
					FilePath:    &file,
					StartLine:   &line,
				},
			}},
		},
		Commands: []string{
			"entire trail finding 575 --json",
			"entire trail resume 575 --branch feature/trail-resume --session session-1",
		},
	}

	var out strings.Builder
	printTrailResumeContext(&out, ctx)
	text := out.String()
	for _, want := range []string{
		"Trail #575  Add trail resume",
		"Status: open · Phase: has_code · Branch: feature/trail-resume",
		"Checkpoint sessions:",
		"session-1",
		"codex",
		"aaaaaaaaaaaa",
		"Findings: open 1",
		"high 1",
		"finding-1",
		"cmd/entire/cli/trail_cmd.go:42",
		"Resume output should show context",
		"Commands:",
		"entire trail finding 575 --json",
		"entire trail resume 575 --branch feature/trail-resume --session session-1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("context output missing %q:\n%s", want, text)
		}
	}
}

func TestPrintTrailResumeContextShowsSkippedSessions(t *testing.T) {
	t.Parallel()

	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{
			Number: 575,
			Title:  "Add trail resume",
			Branch: "feature/trail-resume",
		},
		Sessions: []trailResumeSessionContext{{
			SessionID:    "session-1",
			Agent:        "codex",
			CheckpointID: "aaaaaaaaaaaa",
		}},
		SessionsSkipped: 2,
	}

	var out strings.Builder
	printTrailResumeContext(&out, ctx)
	text := out.String()
	if !strings.Contains(text, "skipped 2 checkpoint sessions due to read errors") {
		t.Fatalf("context output missing skipped sessions message:\n%s", text)
	}
}

func TestPrintTrailResumeContextShowsUnavailableSessions(t *testing.T) {
	t.Parallel()

	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{
			Number: 575,
			Title:  "Add trail resume",
			Branch: "feature/trail-resume",
		},
		SessionsUnavailable: "fetch checkpoint blob: object not found",
	}

	var out strings.Builder
	printTrailResumeContext(&out, ctx)
	text := out.String()
	for _, want := range []string{
		"Checkpoint sessions:",
		"unavailable before restore: fetch checkpoint blob: object not found",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("context output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "none found before restore") {
		t.Fatalf("context output reported empty sessions instead of unavailable sessions:\n%s", text)
	}
}

func TestPrintTrailResumeContextSuppressesCountsWhenFindingsUnavailable(t *testing.T) {
	t.Parallel()

	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{
			Number: 575,
			Title:  "Add trail resume",
			Branch: "feature/trail-resume",
		},
		Findings: trailResumeFindingsContext{
			Unavailable: "reviews API unavailable",
		},
	}

	var out strings.Builder
	printTrailResumeContext(&out, ctx)
	text := out.String()
	if !strings.Contains(text, "Findings:") || !strings.Contains(text, "unavailable: reviews API unavailable") {
		t.Fatalf("context output missing unavailable findings message:\n%s", text)
	}
	if strings.Contains(text, "open 0") || strings.Contains(text, "high 0") {
		t.Fatalf("context output should not print zero findings counts when findings are unavailable:\n%s", text)
	}
}

func TestEncodeTrailResumeContextJSON(t *testing.T) {
	t.Parallel()

	sev := trailReviewSeverityHigh
	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{ID: "trl_1", Number: 575, Branch: "feature/trail-resume"},
		Sessions: []trailResumeSessionContext{{
			SessionID:    "session-1",
			CheckpointID: "aaaaaaaaaaaa",
		}},
		SessionsUnavailable: "checkpoint store unavailable",
		Findings: trailResumeFindingsContext{
			Counts: trailReviewCommentCounts{Open: 1, OpenHigh: 1},
			Top: []api.TrailReviewComment{{
				ID:       "finding-1",
				Severity: &sev,
				Status:   trailReviewStatusOpen,
			}},
		},
		DefaultResume: &trailResumeDefaultContext{SessionID: "session-1", CheckpointID: "aaaaaaaaaaaa", Branch: "feature/trail-resume"},
		Commands:      []string{"entire trail resume 575 --branch feature/trail-resume --session session-1"},
	}

	var out bytes.Buffer
	if err := encodeTrailResumeContextJSON(&out, ctx); err != nil {
		t.Fatalf("encodeTrailResumeContextJSON: %v", err)
	}
	var decoded struct {
		Trail struct {
			ID     string `json:"id"`
			Number int    `json:"number"`
			Branch string `json:"branch"`
		} `json:"trail"`
		Sessions []struct {
			SessionID string `json:"session_id"`
		} `json:"sessions"`
		SessionsUnavailable string `json:"sessions_unavailable"`
		DefaultResume       struct {
			SessionID string `json:"session_id"`
		} `json:"default_resume"`
		FindingsSummary struct {
			Open     int `json:"open"`
			OpenHigh int `json:"open_high"`
		} `json:"findings_summary"`
		Findings []struct {
			ID string `json:"id"`
		} `json:"findings"`
		Commands []string `json:"commands"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if decoded.Trail.ID != "trl_1" || decoded.Trail.Number != 575 || decoded.Trail.Branch != "feature/trail-resume" {
		t.Fatalf("decoded trail = %#v", decoded.Trail)
	}
	if len(decoded.Sessions) != 1 || decoded.Sessions[0].SessionID != "session-1" {
		t.Fatalf("decoded sessions = %#v", decoded.Sessions)
	}
	if decoded.SessionsUnavailable != "checkpoint store unavailable" {
		t.Fatalf("decoded sessions_unavailable = %q", decoded.SessionsUnavailable)
	}
	if decoded.DefaultResume.SessionID != "session-1" {
		t.Fatalf("decoded default_resume = %#v", decoded.DefaultResume)
	}
	if decoded.FindingsSummary.Open != 1 || decoded.FindingsSummary.OpenHigh != 1 {
		t.Fatalf("decoded findings_summary = %#v", decoded.FindingsSummary)
	}
	if len(decoded.Findings) != 1 || decoded.Findings[0].ID != "finding-1" {
		t.Fatalf("decoded findings = %#v", decoded.Findings)
	}
}

func TestEncodeTrailResumeContextJSONOmitsUnsetLastActive(t *testing.T) {
	t.Parallel()

	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{ID: "trl_1", Number: 575, Branch: "feature/trail-resume"},
		Sessions: []trailResumeSessionContext{{
			SessionID:    "session-1",
			CheckpointID: "aaaaaaaaaaaa",
		}},
	}

	var out bytes.Buffer
	if err := encodeTrailResumeContextJSON(&out, ctx); err != nil {
		t.Fatalf("encodeTrailResumeContextJSON: %v", err)
	}
	var decoded struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if len(decoded.Sessions) != 1 {
		t.Fatalf("decoded sessions len = %d, want 1", len(decoded.Sessions))
	}
	if _, ok := decoded.Sessions[0]["last_active"]; ok {
		t.Fatalf("last_active should be omitted when unset:\n%s", out.String())
	}
}

func TestEncodeTrailResumeContextJSONOmitsFindingsSummaryWhenUnavailable(t *testing.T) {
	t.Parallel()

	ctx := trailResumeContext{
		Trail: trailResumeTrailContext{ID: "trl_1", Number: 575, Branch: "feature/trail-resume"},
		Findings: trailResumeFindingsContext{
			Unavailable: "reviews API unavailable",
		},
	}

	var out bytes.Buffer
	if err := encodeTrailResumeContextJSON(&out, ctx); err != nil {
		t.Fatalf("encodeTrailResumeContextJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, out.String())
	}
	if decoded["findings_unavailable"] != "reviews API unavailable" {
		t.Fatalf("decoded findings_unavailable = %#v", decoded["findings_unavailable"])
	}
	if _, ok := decoded["findings_summary"]; ok {
		t.Fatalf("findings_summary should be omitted when findings are unavailable:\n%s", out.String())
	}
}

func TestBuildTrailResumeRestoredSessionChoicesDefaultsToMostRecent(t *testing.T) {
	t.Parallel()

	oldTime := time.Date(2026, 6, 22, 14, 30, 0, 0, time.UTC)
	newTime := time.Date(2026, 6, 23, 7, 39, 0, 0, time.UTC)
	choices := buildTrailResumeRestoredSessionChoices([]strategy.RestoredSession{
		{
			SessionID: "019eefbd-bb6a-7f51-a909-feb4cd95588d",
			Agent:     types.AgentType("Codex"),
			Prompt:    "set up the persistent checkpoint contract",
			CreatedAt: oldTime,
		},
		{
			SessionID: "019ef36b-a485-7ca2-992b-b4f164266e7f",
			Agent:     types.AgentType("Codex"),
			Prompt:    "finish the api/checkpoint extraction",
			CreatedAt: newTime,
		},
	})

	if len(choices) != 2 {
		t.Fatalf("choices len = %d, want 2", len(choices))
	}
	if choices[0].SessionID != "019ef36b-a485-7ca2-992b-b4f164266e7f" {
		t.Fatalf("first choice = %#v, want most recent restored session", choices[0])
	}
	if !strings.Contains(choices[0].Label, "default") {
		t.Fatalf("first choice label = %q, want default marker", choices[0].Label)
	}
	if choices[1].SessionID != "019eefbd-bb6a-7f51-a909-feb4cd95588d" {
		t.Fatalf("second choice = %#v, want older restored session", choices[1])
	}
}

func TestBuildTrailResumeRestoredSessionChoicesPrefersWorkSessionOverReview(t *testing.T) {
	t.Parallel()

	workTime := time.Date(2026, 6, 23, 7, 30, 0, 0, time.UTC)
	reviewTime := workTime.Add(10 * time.Minute)
	choices := buildTrailResumeRestoredSessionChoices([]strategy.RestoredSession{
		{
			SessionID: "work-session",
			Agent:     types.AgentType("Codex"),
			Prompt:    "extract the persistent contract",
			CreatedAt: workTime,
		},
		{
			SessionID:    "review-session",
			Agent:        types.AgentType("Codex"),
			Kind:         "agent_review",
			ReviewPrompt: "Review the code changes introduced by commit f9000bc1a.",
			CreatedAt:    reviewTime,
		},
	})

	if len(choices) != 2 {
		t.Fatalf("choices len = %d, want 2", len(choices))
	}
	if choices[0].SessionID != "work-session" {
		t.Fatalf("first choice = %#v, want normal work session before newer review session", choices[0])
	}
	if !strings.Contains(choices[0].Label, "default") {
		t.Fatalf("work choice label = %q, want default marker", choices[0].Label)
	}
	if choices[1].SessionID != "review-session" {
		t.Fatalf("second choice = %#v, want review session after work session", choices[1])
	}
	if !strings.Contains(choices[1].Label, "review") {
		t.Fatalf("review choice label = %q, want review marker", choices[1].Label)
	}
	if !strings.Contains(choices[1].Label, "Review the code changes") {
		t.Fatalf("review choice label = %q, want review prompt fallback", choices[1].Label)
	}
}

func TestBuildTrailResumeRestoredSessionChoicesPrefersWorkSessionOverReviewPrompt(t *testing.T) {
	t.Parallel()

	workTime := time.Date(2026, 6, 23, 7, 30, 0, 0, time.UTC)
	reviewTime := workTime.Add(10 * time.Minute)
	choices := buildTrailResumeRestoredSessionChoices([]strategy.RestoredSession{
		{
			SessionID: "work-session",
			Agent:     types.AgentType("Codex"),
			Prompt:    "extract the persistent contract",
			CreatedAt: workTime,
		},
		{
			SessionID: "review-session",
			Agent:     types.AgentType("Codex"),
			Prompt:    "Review the code changes introduced by commit f9000bc1a.",
			CreatedAt: reviewTime,
		},
	})

	if len(choices) != 2 {
		t.Fatalf("choices len = %d, want 2", len(choices))
	}
	if choices[0].SessionID != "work-session" {
		t.Fatalf("first choice = %#v, want work session before newer review prompt", choices[0])
	}
	if choices[1].SessionID != "review-session" {
		t.Fatalf("second choice = %#v, want review prompt after work session", choices[1])
	}
	if !strings.Contains(choices[1].Label, "review") {
		t.Fatalf("review choice label = %q, want review marker", choices[1].Label)
	}
}

func TestPrintTrailRestoredSessionSummaryIdentifiesReviewOnlyCheckpointSessions(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	printTrailRestoredSessionSummary(&out, []strategy.RestoredSession{
		{
			SessionID:    "review-session-1",
			Kind:         string(session.KindAgentReview),
			ReviewPrompt: "Review the code changes introduced by commit abc123.",
		},
		{
			SessionID: "review-session-2",
			Prompt:    "Review this branch for regressions.",
		},
	})

	text := out.String()
	for _, want := range []string{
		"Restored 2 checkpoint sessions",
		"Only review/investigation checkpoint sessions were found",
		"may not appear as trail UI sessions",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("summary missing %q:\n%s", want, text)
		}
	}
}

func TestDisplayTrailRestoredSessionsIncludesReviewWarning(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	err := displayTrailRestoredSessions(&out, []strategy.RestoredSession{
		{
			SessionID:    "019ef36b-a485-7ca2-992b-b4f164266e7f",
			Agent:        types.AgentType("Codex"),
			Kind:         string(session.KindAgentReview),
			ReviewPrompt: "Review the code changes introduced by commit abc123.",
			CreatedAt:    time.Date(2026, 6, 23, 7, 39, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("displayTrailRestoredSessions() error = %v", err)
	}

	text := out.String()
	for _, want := range []string{
		"Restored checkpoint session 019ef36b-a485-7ca2-992b-b4f164266e7f",
		"Only review/investigation checkpoint sessions were found",
		"To continue this checkpoint session:",
		"codex resume 019ef36b-a485-7ca2-992b-b4f164266e7f",
		"Review the code changes introduced by commit abc123.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("display output missing %q:\n%s", want, text)
		}
	}
}

func TestDisplayTrailRestoredSessionsMarksActualMostRecent(t *testing.T) {
	t.Parallel()

	workTime := time.Date(2026, 6, 23, 7, 30, 0, 0, time.UTC)
	reviewTime := workTime.Add(10 * time.Minute)
	var out strings.Builder
	err := displayTrailRestoredSessions(&out, []strategy.RestoredSession{
		{
			SessionID: "work-session",
			Agent:     agent.AgentTypeClaudeCode,
			Prompt:    "continue implementation",
			CreatedAt: workTime,
		},
		{
			SessionID:    "review-session",
			Agent:        types.AgentType("Codex"),
			Kind:         string(session.KindAgentReview),
			ReviewPrompt: "Review this branch for regressions.",
			CreatedAt:    reviewTime,
		},
	})
	if err != nil {
		t.Fatalf("displayTrailRestoredSessions() error = %v", err)
	}

	text := out.String()
	workLine := lineContaining(text, "claude -r work-session")
	if strings.Contains(workLine, "most recent") {
		t.Fatalf("work session command should not be marked most recent:\n%s", text)
	}
	reviewLine := lineContaining(text, "codex resume review-session")
	if !strings.Contains(reviewLine, "most recent") {
		t.Fatalf("newest review session command should be marked most recent:\n%s", text)
	}
}

func TestTrailResumeCanPromptRestoredSessionsHonorsForce(t *testing.T) {
	t.Setenv("ENTIRE_TEST_TTY", "1")

	if trailResumeCanPromptRestoredSessions(true) {
		t.Fatal("force should suppress restored-session prompts even in an interactive terminal")
	}
	if !trailResumeCanPromptRestoredSessions(false) {
		t.Fatal("interactive restored-session prompts should remain enabled without force")
	}
}

func TestContinueRestoredSessionsTTYDeclinePrintsAgentCommands(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{
		{
			SessionID: "codex-session",
			Agent:     types.AgentType("Codex"),
			Prompt:    "continue implementation",
			CreatedAt: time.Date(2026, 6, 23, 7, 30, 0, 0, time.UTC),
		},
		{
			SessionID: "claude-session",
			Agent:     agent.AgentTypeClaudeCode,
			Prompt:    "review the branch",
			CreatedAt: time.Date(2026, 6, 23, 7, 40, 0, 0, time.UTC),
		},
	}
	var out strings.Builder
	launched := false

	err := continueRestoredSessions(context.Background(), &out, sessions, restoredSessionContinueOptions{
		CanPrompt: true,
		PromptStartAgent: func(context.Context, []strategy.RestoredSession) (bool, error) {
			return false, nil
		},
		PromptSession: func(context.Context, io.Writer, []strategy.RestoredSession) (strategy.RestoredSession, bool, error) {
			t.Fatal("session picker should not run when user declines launching")
			return strategy.RestoredSession{}, false, nil
		},
		Launch: func(context.Context, io.Writer, strategy.RestoredSession) error {
			launched = true
			return nil
		},
		Display: displayTrailRestoredSessions,
	})
	if err != nil {
		t.Fatalf("continueRestoredSessions() error = %v", err)
	}
	if launched {
		t.Fatal("agent should not launch when user chooses to show commands")
	}
	text := out.String()
	for _, want := range []string{
		"To continue:",
		"codex resume codex-session",
		"claude -r claude-session",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("output missing %q:\n%s", want, text)
		}
	}
}

func TestContinueRestoredSessionsTTYStartSingleLaunchesSession(t *testing.T) {
	t.Parallel()

	session := strategy.RestoredSession{
		SessionID: "codex-session",
		Agent:     types.AgentType("Codex"),
		Prompt:    "continue implementation",
		CreatedAt: time.Date(2026, 6, 23, 7, 30, 0, 0, time.UTC),
	}
	var out strings.Builder
	var launched string

	err := continueRestoredSessions(context.Background(), &out, []strategy.RestoredSession{session}, restoredSessionContinueOptions{
		CanPrompt: true,
		PromptStartAgent: func(context.Context, []strategy.RestoredSession) (bool, error) {
			return true, nil
		},
		PromptSession: func(context.Context, io.Writer, []strategy.RestoredSession) (strategy.RestoredSession, bool, error) {
			t.Fatal("session picker should not run for a single restored session")
			return strategy.RestoredSession{}, false, nil
		},
		Launch: func(_ context.Context, _ io.Writer, selected strategy.RestoredSession) error {
			launched = selected.SessionID
			return nil
		},
		Display: displayTrailRestoredSessions,
	})
	if err != nil {
		t.Fatalf("continueRestoredSessions() error = %v", err)
	}
	if launched != "codex-session" {
		t.Fatalf("launched session = %q, want codex-session", launched)
	}
	if strings.Contains(out.String(), "To continue") {
		t.Fatalf("should not print manual commands when launching succeeds:\n%s", out.String())
	}
}

func TestContinueRestoredSessionsTTYStartMultipleLaunchesPickerSelection(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{
		{SessionID: "first-session", Agent: types.AgentType("Codex")},
		{SessionID: "second-session", Agent: agent.AgentTypeClaudeCode},
	}
	var out strings.Builder
	pickerCalled := false
	var launched string

	err := continueRestoredSessions(context.Background(), &out, sessions, restoredSessionContinueOptions{
		CanPrompt: true,
		PromptStartAgent: func(context.Context, []strategy.RestoredSession) (bool, error) {
			return true, nil
		},
		PromptSession: func(_ context.Context, _ io.Writer, restored []strategy.RestoredSession) (strategy.RestoredSession, bool, error) {
			pickerCalled = true
			return restored[1], true, nil
		},
		Launch: func(_ context.Context, _ io.Writer, selected strategy.RestoredSession) error {
			launched = selected.SessionID
			return nil
		},
		Display: displayTrailRestoredSessions,
	})
	if err != nil {
		t.Fatalf("continueRestoredSessions() error = %v", err)
	}
	if !pickerCalled {
		t.Fatal("expected picker to run for multiple restored sessions")
	}
	if launched != "second-session" {
		t.Fatalf("launched session = %q, want second-session", launched)
	}
}

func TestContinueRestoredSessionsPreferredDeclinePrintsOnlyPreferredSession(t *testing.T) {
	t.Parallel()

	sessions := []strategy.RestoredSession{
		{SessionID: "first-session", Agent: types.AgentType("Codex")},
		{SessionID: "second-session", Agent: agent.AgentTypeClaudeCode},
	}
	var out strings.Builder

	err := continueRestoredSessions(context.Background(), &out, sessions, restoredSessionContinueOptions{
		CanPrompt:          true,
		PreferredSessionID: "second-session",
		PromptStartAgent: func(context.Context, []strategy.RestoredSession) (bool, error) {
			return false, nil
		},
		Launch:  func(context.Context, io.Writer, strategy.RestoredSession) error { return nil },
		Display: displayTrailRestoredSessions,
	})
	if err != nil {
		t.Fatalf("continueRestoredSessions() error = %v", err)
	}
	text := out.String()
	if strings.Contains(text, "codex resume first-session") {
		t.Fatalf("preferred --session fallback should not print unrelated sessions:\n%s", text)
	}
	if !strings.Contains(text, "claude -r second-session") {
		t.Fatalf("preferred session command missing:\n%s", text)
	}
}

func TestPrintTrailRestoredSessionSummaryIncludesCheckpointID(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	printTrailRestoredSessionSummary(&out, []strategy.RestoredSession{
		{
			SessionID:    "019ef5f3-3472-7f70-82f7-6f0ce46691f4",
			CheckpointID: "8a18ef79cd93",
		},
	})

	text := out.String()
	if !strings.Contains(text, "✓ Restored checkpoint 8a18ef79cd93 (1 session).") {
		t.Fatalf("summary missing checkpoint ID:\n%s", text)
	}
}

func TestStartRestoredAgentPromptUsesYesNoLabels(t *testing.T) {
	t.Parallel()

	startAgent := true
	prompt := newStartRestoredAgentConfirm(&startAgent, "8a18ef79cd93")
	prompt.WithWidth(80)
	view := prompt.View()

	for _, want := range []string{
		"Start the agent now?",
		"Entire restored checkpoint 8a18ef79cd93.",
		"Choose No to print the resume",
		"instead.",
		"Yes",
		"No",
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("prompt view missing %q:\n%s", want, view)
		}
	}
	for _, notWant := range []string{"Start agent", "Show commands"} {
		if strings.Contains(view, notWant) {
			t.Fatalf("prompt view should not contain %q:\n%s", notWant, view)
		}
	}
}

func TestLaunchTrailRestoredSessionTreatsAgentExitAsHandled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell script fake executable")
	}

	binDir := t.TempDir()
	fakeCodex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out strings.Builder
	err := launchTrailRestoredSession(context.Background(), &out, strategy.RestoredSession{
		SessionID: "codex-session",
		Agent:     types.AgentType("Codex"),
	})
	if err != nil {
		t.Fatalf("launchTrailRestoredSession() error = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "Launching: codex resume codex-session") {
		t.Fatalf("launch output missing command:\n%s", out.String())
	}
}

func lineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func TestTrailResumeWorktreeClashMessage(t *testing.T) {
	t.Parallel()

	msg := trailResumeWorktreeClashMessage("feature/work", "/tmp/path with spaces")
	for _, want := range []string{
		`Branch "feature/work" is already checked out in another worktree:`,
		"/tmp/path with spaces",
		"Resume from that worktree with:",
		"cd '/tmp/path with spaces' && entire trail resume feature/work",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}
