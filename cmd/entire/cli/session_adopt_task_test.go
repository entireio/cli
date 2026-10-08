package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// adoptTaskStores points each agent with a task layout at its own temporary
// session directory.
type adoptTaskStores struct {
	claude, codex, droid, cursor string
}

func setAdoptTaskStores(t *testing.T) adoptTaskStores {
	t.Helper()

	stores := adoptTaskStores{
		claude: filepath.Join(t.TempDir(), "claude-project"),
		codex:  filepath.Join(t.TempDir(), "codex-sessions"),
		droid:  filepath.Join(t.TempDir(), "droid-project"),
		cursor: filepath.Join(t.TempDir(), "cursor-transcripts"),
	}
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", stores.claude)
	t.Setenv("ENTIRE_TEST_CODEX_SESSION_DIR", stores.codex)
	t.Setenv("ENTIRE_TEST_DROID_PROJECT_DIR", stores.droid)
	t.Setenv("ENTIRE_TEST_CURSOR_PROJECT_DIR", stores.cursor)
	return stores
}

func TestValidateAdoptTaskTranscript(t *testing.T) {
	stores := setAdoptTaskStores(t)
	sourceRepo := t.TempDir()
	const sessionID = "adopt-task-session"
	claudeParent := filepath.Join(stores.claude, sessionID+".jsonl")
	claudeSubagents := paths.SubagentsDir(stores.claude, sessionID)
	droidParent := filepath.Join(stores.droid, sessionID+".jsonl")
	codexChild := filepath.Join(stores.codex, "2026", "10", "05", "rollout-2026-10-05T10-00-00-child.jsonl")

	tests := []struct {
		name      string
		agentType types.AgentType
		parent    string
		agentID   string
		path      string
		wantErr   string // empty when the path is accepted
	}{
		{name: "claude nested subagent", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: filepath.Join(claudeSubagents, "agent-a1.jsonl")},
		{name: "claude legacy subagent beside parent", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: filepath.Join(stores.claude, "agent-a1.jsonl")},
		{name: "claude without parent checks the name", agentType: agent.AgentTypeClaudeCode, agentID: "a1",
			path: filepath.Join(claudeSubagents, "agent-a1.jsonl")},
		{name: "claude without parent rejects other files", agentType: agent.AgentTypeClaudeCode, agentID: "a1",
			path: filepath.Join(stores.claude, "memory", "MEMORY.md"), wantErr: "not the transcript of task"},
		{name: "claude sibling session", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "other-session",
			path: filepath.Join(stores.claude, "other-session.jsonl"), wantErr: "not the transcript of task"},
		{name: "claude another task's file", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: filepath.Join(claudeSubagents, "agent-other.jsonl"), wantErr: "not the transcript of task"},
		{name: "claude another session's subagents", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: filepath.Join(paths.SubagentsDir(stores.claude, "other-session"), "agent-a1.jsonl"), wantErr: "not the transcript of task"},
		{name: "claude task without agent ID", agentType: agent.AgentTypeClaudeCode, parent: claudeParent,
			path: filepath.Join(claudeSubagents, "agent-.jsonl"), wantErr: "not the transcript of task"},
		{name: "droid worker beside parent", agentType: agent.AgentTypeFactoryAIDroid, parent: droidParent, agentID: "worker",
			path: filepath.Join(stores.droid, "worker.jsonl")},
		{name: "droid subagent in subagents dir", agentType: agent.AgentTypeFactoryAIDroid, parent: droidParent, agentID: "sub",
			path: filepath.Join(paths.SubagentsDir(stores.droid, sessionID), "agent-sub.jsonl")},
		{name: "droid worker under another session", agentType: agent.AgentTypeFactoryAIDroid, parent: droidParent, agentID: "worker",
			path: filepath.Join(stores.droid, "nested", "worker.jsonl"), wantErr: "not the transcript of task"},
		{name: "codex child rollout", agentType: agent.AgentTypeCodex, agentID: "child", path: codexChild},
		{name: "codex rollout of another thread", agentType: agent.AgentTypeCodex, agentID: "other", path: codexChild,
			wantErr: "not the transcript of task"},
		{name: "cursor has no layout rule", agentType: agent.AgentTypeCursor, agentID: "sub",
			path: filepath.Join(stores.cursor, "anything.jsonl")},
		{name: "outside every agent store", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: filepath.Join(t.TempDir(), "agent-a1.jsonl"), wantErr: "not owned by a registered agent"},
		{name: "another agent's store", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: filepath.Join(stores.codex, "agent-a1.jsonl"), wantErr: "belongs to Codex"},
		{name: "relative path", agentType: agent.AgentTypeClaudeCode, parent: claudeParent, agentID: "a1",
			path: "agent-a1.jsonl", wantErr: "not absolute"},
		{name: "no agent type", parent: claudeParent, agentID: "a1",
			path: filepath.Join(claudeSubagents, "agent-a1.jsonl"), wantErr: "no agent type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &session.State{SessionID: sessionID, AgentType: tt.agentType, TranscriptPath: tt.parent}

			err := validateAdoptTaskTranscript(state, tt.agentID, tt.path, sourceRepo)

			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("validateAdoptTaskTranscript(%q) = %v, want accepted", tt.path, err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("validateAdoptTaskTranscript(%q) = %v, want error containing %q", tt.path, err, tt.wantErr)
			}
		})
	}
}

func TestDropInvalidAdoptTaskTranscripts(t *testing.T) {
	stores := setAdoptTaskStores(t)
	sourceRepo := t.TempDir()
	const sessionID = "adopt-task-session"
	nested := filepath.Join(paths.SubagentsDir(stores.claude, sessionID), "agent-a1.jsonl")
	codexChild := filepath.Join(stores.codex, "2026", "10", "05", "rollout-2026-10-05T10-00-00-child.jsonl")

	t.Run("task records", func(t *testing.T) {
		state := &session.State{
			SessionID:      sessionID,
			AgentType:      agent.AgentTypeClaudeCode,
			TranscriptPath: filepath.Join(stores.claude, sessionID+".jsonl"),
			TaskRecords: []session.TaskRecord{
				{ToolUseID: "toolu_unclean", AgentID: "a1", DeclaredTranscriptPath: filepath.Dir(nested) + "/x/../agent-a1.jsonl"},
				{ToolUseID: "toolu_outside", AgentID: "a2", DeclaredTranscriptPath: filepath.Join(t.TempDir(), "agent-a2.jsonl")},
				{ToolUseID: "toolu_none", AgentID: "a3"},
			},
		}

		if got := dropInvalidAdoptTaskTranscripts(context.Background(), state, sourceRepo); got != 1 {
			t.Fatalf("cleared = %d, want 1", got)
		}
		want := []string{nested, "", ""}
		for i, record := range state.TaskRecords {
			if record.DeclaredTranscriptPath != want[i] {
				t.Errorf("TaskRecords[%d].DeclaredTranscriptPath = %q, want %q", i, record.DeclaredTranscriptPath, want[i])
			}
		}
	})

	t.Run("subagent inventory", func(t *testing.T) {
		state := &session.State{
			SessionID: sessionID,
			AgentType: agent.AgentTypeCodex,
			SubagentInventory: []session.SubagentInventoryEntry{
				{AgentID: "child", DeclaredTranscriptPath: codexChild, ResolvedTranscriptPath: codexChild},
				{AgentID: "forged", DeclaredTranscriptPath: filepath.Join(t.TempDir(), "id_rsa"), ResolvedTranscriptPath: filepath.Join(t.TempDir(), "id_rsa")},
			},
		}

		if got := dropInvalidAdoptTaskTranscripts(context.Background(), state, sourceRepo); got != 1 {
			t.Fatalf("cleared = %d, want 1 for the forged inventory path", got)
		}
		if got := state.SubagentInventory[0].DeclaredTranscriptPath; got != codexChild {
			t.Errorf("valid declared inventory path = %q, want %q", got, codexChild)
		}
		if got := state.SubagentInventory[1].DeclaredTranscriptPath; got != "" {
			t.Errorf("forged declared inventory path = %q, want cleared", got)
		}
		for i, entry := range state.SubagentInventory {
			if entry.ResolvedTranscriptPath != "" {
				t.Errorf("SubagentInventory[%d].ResolvedTranscriptPath = %q, want cleared until re-verified", i, entry.ResolvedTranscriptPath)
			}
		}
	})
}

func TestValidateAdoptSourceTranscript_RejectsRelativePath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{filepath.Join("projects", "relative.jsonl"), "   "} {
		source := &session.State{SessionID: "relative", TranscriptPath: path}
		err := validateAdoptSourceTranscript(source, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "not absolute") {
			t.Fatalf("validateAdoptSourceTranscript(%q) = %v, want relative-path refusal", path, err)
		}
	}
}

func TestSessionAdopt_DropsTaskTranscriptsOutsideTheSessionLayout(t *testing.T) {
	sourceRepo := setupAdoptRepo(t)
	targetRepo := setupAdoptRepo(t)

	const sessionID = "test-adopt-task-paths"
	transcriptPath := claudeAdoptTranscriptPath(t, sourceRepo, sessionID)
	testutil.WriteFile(t, filepath.Dir(transcriptPath), filepath.Base(transcriptPath), `{"type":"user"}`+"\n")
	kept := filepath.Join(paths.SubagentsDir(filepath.Dir(transcriptPath), sessionID), "agent-kept.jsonl")
	sibling := filepath.Join(filepath.Dir(transcriptPath), "sibling-session.jsonl")
	secret := filepath.Join(t.TempDir(), "secret.txt")

	sourceStore := session.NewStateStoreWithDir(filepath.Join(sourceRepo, ".git", session.SessionStateDirName))
	lastInteraction := time.Now().Add(-1 * time.Minute)
	// No AgentType: adoption takes it from the agent that owns the transcript
	// and applies that agent's task layout.
	if err := sourceStore.Save(context.Background(), &session.State{
		SessionID:           sessionID,
		StartedAt:           time.Now().Add(-5 * time.Minute),
		LastInteractionTime: &lastInteraction,
		Phase:               session.PhaseActive,
		BaseCommit:          testutil.GetHeadHash(t, sourceRepo),
		WorktreePath:        sourceRepo,
		TranscriptPath:      transcriptPath,
		TaskRecords: []session.TaskRecord{
			{ToolUseID: "toolu_kept", AgentID: "kept", DeclaredTranscriptPath: kept},
			{ToolUseID: "toolu_sibling", AgentID: "sibling-session", DeclaredTranscriptPath: sibling},
			{ToolUseID: "toolu_secret", AgentID: "secret", DeclaredTranscriptPath: secret},
		},
	}); err != nil {
		t.Fatal(err)
	}

	testutil.WriteFile(t, targetRepo, "feature.txt", "agent change\n")
	t.Chdir(targetRepo)

	var out bytes.Buffer
	if err := runAdopt(context.Background(), &out, sessionID, adoptOptions{FromWorktree: sourceRepo, Force: true}); err != nil {
		t.Fatalf("runAdopt failed: %v", err)
	}
	if !strings.Contains(out.String(), "Dropped 2 subagent transcript path(s)") {
		t.Errorf("output = %q, want a notice for the two dropped paths", out.String())
	}

	targetStore, err := session.NewStateStore(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := targetStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if adopted == nil || len(adopted.TaskRecords) != 3 {
		t.Fatalf("adopted state = %#v, want three task records", adopted)
	}
	if adopted.AgentType != agent.AgentTypeClaudeCode {
		t.Errorf("AgentType = %q, want %q from the transcript's owner", adopted.AgentType, agent.AgentTypeClaudeCode)
	}
	for i, want := range []string{kept, "", ""} {
		if got := adopted.TaskRecords[i].DeclaredTranscriptPath; got != want {
			t.Errorf("adopted TaskRecords[%d].DeclaredTranscriptPath = %q, want %q", i, got, want)
		}
	}

	retired, err := sourceStore.Load(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if retired == nil || retired.TaskRecords[2].DeclaredTranscriptPath != secret {
		t.Errorf("retired source state = %#v, want its task records unchanged", retired)
	}
}
