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
	done := time.Now()

	tests := []struct {
		name        string
		record      session.TaskRecord
		ag          func(dir string) agent.Agent
		wantContent string
		wantFetched bool
		wantErr     bool
	}{
		{
			name:        "completed record without a declared path is fetched",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", CompletedAt: done},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
			wantContent: "fetched",
			wantFetched: true,
		},
		{
			// Its transcript is partial; it is stored again once it completes.
			name:   "in-flight record is not fetched",
			record: session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child"},
			ag:     func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
		},
		{
			name:        "transcript-unavailable record is fetched",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", TranscriptUnavailable: true, CompletedAt: done},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, content: "fetched"} },
			wantContent: "fetched",
			wantFetched: true,
		},
		{
			name:        "unreadable declared path falls through to the fetch",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", DeclaredTranscriptPath: missing, CompletedAt: done},
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
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", CompletedAt: done},
			ag:          func(dir string) agent.Agent { return &fetchingAgent{dir: dir, err: errors.New("export failed")} },
			wantFetched: true,
		},
		{
			name:        "failed fetch keeps the declared path's read error",
			record:      session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", DeclaredTranscriptPath: missing, CompletedAt: done},
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

// TestReadTaskTranscript_FetchBudgetIsShared pins that once a pass's fetch
// budget is spent, later records are not exported at all: each export is a
// separate agent process with its own timeout, run inside post-commit.
func TestReadTaskTranscript_FetchBudgetIsShared(t *testing.T) {
	t.Parallel()
	ag := &fetchingAgent{dir: t.TempDir(), content: "fetched"}
	state := &SessionState{SessionID: "ses_parent"}
	record := session.TaskRecord{ToolUseID: "call_a", AgentID: "ses_child", CompletedAt: time.Now()}
	spent := agent.WithSubagentFetchBudget(context.Background(), 0)
	raw, path, err := readTaskTranscript(spent, context.Background(), ag, state, record, "")
	require.NoError(t, err)
	require.Empty(t, raw)
	require.Empty(t, path)
	require.Empty(t, ag.fetched)
}

// reportingAgent is an agent.Agent whose only behaviour is
// LiveSubagentFileReporter.
type reportingAgent struct {
	agent.Agent

	files     []string
	err       error
	reported  []string
	deadlines []time.Time
}

func (r *reportingAgent) LiveSubagentModifiedFiles(ctx context.Context, agentID, _ string, _ time.Time) ([]string, error) {
	r.reported = append(r.reported, agentID)
	deadline, _ := ctx.Deadline()
	r.deadlines = append(r.deadlines, deadline)
	return r.files, r.err
}

// TestLiveTaskFilesInCommit_AsksTheAgentForARunningSubagent pins that a live
// record with no transcript on disk (OpenCode's, until its stop hook runs) is
// reported by the agent for the co-authorship check, and that a failed or
// out-of-budget report is no evidence rather than an error.
func TestLiveTaskFilesInCommit_AsksTheAgentForARunningSubagent(t *testing.T) {
	t.Parallel()
	committed := map[string]struct{}{"docs/red.md": {}}
	spent := withLiveTaskFetchDeadline(withCommitFetchBudgets(context.Background(), time.Now().Add(-time.Hour)))

	tests := []struct {
		name         string
		ctx          context.Context
		files        []string
		err          error
		want         bool
		wantReported bool
	}{
		{name: "subagent wrote a committed file", ctx: context.Background(), files: []string{"docs/red.md"}, want: true, wantReported: true},
		{name: "subagent wrote other files", ctx: context.Background(), files: []string{"docs/blue.md"}, wantReported: true},
		{name: "report failed", ctx: context.Background(), err: errors.New("opencode export: boom"), wantReported: true},
		{name: "co-authorship budget spent", ctx: spent, files: []string{"docs/red.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			ag := &reportingAgent{files: tt.files, err: tt.err}
			state := &SessionState{
				SessionID:    "ses_parent",
				WorktreePath: dir,
				TaskRecords:  []session.TaskRecord{{ToolUseID: "call_1", AgentID: "ses_child", StartedAt: time.Now()}},
			}
			require.Equal(t, tt.want, liveTaskFilesInCommitFor(tt.ctx, ag, state, committed))
			require.Equal(t, tt.wantReported, len(ag.reported) == 1)
		})
	}
}

// TestCommitFetchBudgets_CoauthorshipGetsAShare pins that the co-authorship
// checks are cut off at their share of the commit's fetch budget, so a hung
// export there cannot spend what condensation's re-exports need.
func TestCommitFetchBudgets_CoauthorshipGetsAShare(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ag := &reportingAgent{}
	state := &SessionState{TaskRecords: []session.TaskRecord{{ToolUseID: "call_1", AgentID: "ses_child", StartedAt: now}}}
	liveTaskFilesInCommitFor(withLiveTaskFetchDeadline(withCommitFetchBudgets(context.Background(), now)), ag, state, nil)
	require.Len(t, ag.deadlines, 1)
	require.WithinDuration(t, now.Add(liveTaskFetchBudget), ag.deadlines[0], time.Second)
	require.Less(t, liveTaskFetchBudget, agent.SubagentFetchBudget)
}
