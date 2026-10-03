package strategy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
}

func (f *fetchingAgent) FetchSubagentTranscript(_ context.Context, agentID, _ string, _, _ time.Time) (string, error) {
	f.fetched = append(f.fetched, agentID)
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
			raw, path, err := readTaskTranscript(context.Background(), context.Background(), ag, state, tt.record)
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
