package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// fetchingAgent is an agent.Agent whose only implemented behaviour is
// SubagentTranscriptFetcher; every other method panics through the nil
// embedded interface, which pins that the materializer calls nothing else.
var _ agent.SubagentTranscriptFetcher = (*fetchingAgent)(nil)

type fetchingAgent struct {
	agent.Agent

	dir     string
	content string
	err     error
	fetched []string
	// deadlines records the deadline of each fetch's context.
	deadlines []time.Time
}

func (f *fetchingAgent) FetchSubagentTranscript(ctx context.Context, agentID, _ string, _, _ time.Time) (string, error) {
	f.fetched = append(f.fetched, agentID)
	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)
	if f.err != nil {
		return "", f.err
	}
	p := filepath.Join(f.dir, agentID+".json")
	if err := os.WriteFile(p, []byte(f.content), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

type plainAgent struct{ agent.Agent }

func TestReadTaskTranscript_FetchFallback(t *testing.T) {
	t.Parallel()
	declared := filepath.Join(t.TempDir(), "declared.json")
	require.NoError(t, os.WriteFile(declared, []byte("declared"), 0o600))
	missing := filepath.Join(t.TempDir(), "gone.json")

	tests := []struct {
		name        string
		record      session.TaskRecord
		ag          func(dir string) agent.Agent
		wantContent string
		wantFetched bool
		wantErr     bool
	}{
		{
			name:        "in-flight record without a declared path is fetched",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child"},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
			wantContent: "fetched",
			wantFetched: true,
		},
		{
			name:        "transcript-unavailable record is fetched",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", TranscriptUnavailable: true},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
			wantContent: "fetched",
			wantFetched: true,
		},
		{
			name:        "unreadable declared path falls through to the fetch",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", DeclaredTranscriptPath: missing},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
			wantContent: "fetched",
			wantFetched: true,
		},
		{
			name:        "readable declared path wins and nothing is fetched",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", DeclaredTranscriptPath: declared},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
			wantContent: "declared",
		},
		{
			name:        "failed fetch with no candidates stays unresolvable",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child"},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, err: errors.New("export failed")} },
			wantFetched: true,
		},
		{
			name:        "failed fetch keeps the declared path's read error",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", DeclaredTranscriptPath: missing},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, err: errors.New("export failed")} },
			wantFetched: true,
			wantErr:     true,
		},
		{
			name:   "agent without the capability is never asked",
			record: session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", TranscriptUnavailable: true},
			ag:     func(string) agent.Agent { return &plainAgent{} },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ag := tt.ag(t.TempDir())
			state := &SessionState{SessionID: "ses_parent"}
			raw, path, err := readTaskTranscript(context.Background(), context.Background(), ag, state, tt.record, "")
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.wantContent, string(raw))
			require.Equal(t, tt.wantContent == "", path == "")
			if f, ok := ag.(*fetchingAgent); ok {
				if tt.wantFetched {
					require.Equal(t, []string{"ses_child"}, f.fetched)
				} else {
					require.Empty(t, f.fetched)
				}
			}
		})
	}
}

func TestMaterializeTaskRecords_FetchedTranscriptIsRedactedAndStored(t *testing.T) {
	t.Parallel()
	ag := &fetchingAgent{dir: t.TempDir(), content: `{"info":{"id":"ses_child"},"messages":[{"text":"` + taskTranscriptSecret + `"}]}` + "\n"}
	state := &SessionState{
		SessionID: "ses_parent",
		TaskRecords: []session.TaskRecord{{
			ToolUseID:             "call_red",
			AgentID:               "ses_child",
			StartedAt:             time.Now(),
			CompletedAt:           time.Now(),
			TranscriptUnavailable: true,
		}},
	}

	payloads, _ := (&ManualCommitStrategy{}).materializeTaskRecords(context.Background(), context.Background(), ag, state, nil)
	require.Len(t, payloads, 1)
	require.Empty(t, payloads[0].TranscriptUnavailableReason)
	stored := string(payloads[0].Transcript.Bytes())
	require.NotContains(t, stored, taskTranscriptSecret)
	require.Contains(t, stored, "REDACTED")
}

// analyzingFetchingAgent is a fetchingAgent that is also a transcript
// analyzer: its transcript is one modified file path per line.
type analyzingFetchingAgent struct{ fetchingAgent }

func (a *analyzingFetchingAgent) GetTranscriptPosition(string) (int, error) { return 0, nil }

func (a *analyzingFetchingAgent) ExtractModifiedFilesFromOffset(_ context.Context, path string, _ int) ([]string, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	files := strings.Fields(string(data))
	return files, len(files), nil
}

// TestLiveTaskFilesInCommit_FetchesInFlightTranscript pins that a running
// subagent with no transcript on disk (OpenCode's in-flight record) is
// re-exported for the co-authorship check, and that a failed export is no
// evidence rather than an error.
func TestLiveTaskFilesInCommit_FetchesInFlightTranscript(t *testing.T) {
	t.Parallel()
	committed := map[string]struct{}{"docs/red.md": {}}

	tests := []struct {
		name    string
		content string
		err     error
		want    bool
	}{
		{name: "subagent wrote a committed file", content: "docs/red.md", want: true},
		{name: "subagent wrote other files", content: "docs/blue.md", want: false},
		{name: "export failed", err: errors.New("opencode export: boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ag := &analyzingFetchingAgent{fetchingAgent{dir: dir, content: tt.content, err: tt.err}}
			state := &SessionState{
				SessionID:    "ses_parent",
				WorktreePath: dir,
				TaskRecords: []session.TaskRecord{
					{ToolUseID: "call_1", AgentID: "ses_child", StartedAt: time.Now()},
				},
			}
			got := liveTaskFilesInCommitFor(context.Background(), ag, state, committed, time.Now().Add(liveTaskFetchTimeout))
			require.Equal(t, tt.want, got)
			require.Equal(t, []string{"ses_child"}, ag.fetched, "the in-flight record must be fetched")
		})
	}
}

// TestLiveTaskFilesInCommit_SessionsShareOneDeadline pins that the re-export
// budget is per commit, not per session: post-commit checks sessions one after
// another inside the user's `git commit`, so a fresh timeout per session would
// let N slow exports block the commit for N times the budget.
func TestLiveTaskFilesInCommit_SessionsShareOneDeadline(t *testing.T) {
	t.Parallel()
	committed := map[string]struct{}{"docs/red.md": {}}
	deadline := time.Now().Add(liveTaskFetchTimeout)

	for _, child := range []string{"ses_child_a", "ses_child_b"} {
		dir := t.TempDir()
		ag := &analyzingFetchingAgent{fetchingAgent{dir: dir, content: "docs/blue.md"}}
		state := &SessionState{
			SessionID:    "ses_parent_" + child,
			WorktreePath: dir,
			TaskRecords:  []session.TaskRecord{{ToolUseID: "call_" + child, AgentID: child, StartedAt: time.Now()}},
		}
		time.Sleep(5 * time.Millisecond) // a later session must not get a later deadline
		require.False(t, liveTaskFilesInCommitFor(context.Background(), ag, state, committed, deadline))
		require.Len(t, ag.deadlines, 1)
		require.True(t, ag.deadlines[0].Equal(deadline), "session %s fetched with deadline %v, want the commit's %v", child, ag.deadlines[0], deadline)
	}
}
