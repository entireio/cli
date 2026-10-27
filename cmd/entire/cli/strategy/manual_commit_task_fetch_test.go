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
	spent := withLiveTaskFetchDeadline(withCoauthorshipFetchDeadline(context.Background(), time.Now().Add(-time.Hour)))

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
	liveTaskFilesInCommitFor(withLiveTaskFetchDeadline(withCoauthorshipFetchDeadline(context.Background(), now)), ag, state, nil)
	require.Len(t, ag.deadlines, 1)
	require.WithinDuration(t, now.Add(liveTaskFetchBudget), ag.deadlines[0], time.Second)
	require.Less(t, liveTaskFetchBudget, agent.SubagentFetchBudget)
}

// TestCommitFetchBudgets_PrepareCommitMsgIsBounded pins that prepare-commit-msg's
// running-subagent check gets the co-authorship deadline when nothing set one,
// and never extends a deadline post-commit already set.
func TestCommitFetchBudgets_PrepareCommitMsgIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Now()
	state := &SessionState{TaskRecords: []session.TaskRecord{{ToolUseID: "call_1", AgentID: "ses_child", StartedAt: now}}}

	ag := &reportingAgent{}
	liveTaskFilesInCommitFor(withLiveTaskFetchDeadline(ensureCoauthorshipFetchDeadline(context.Background(), now)), ag, state, nil)
	require.Len(t, ag.deadlines, 1)
	require.WithinDuration(t, now.Add(liveTaskFetchBudget), ag.deadlines[0], time.Second)

	earlier := now.Add(-time.Minute)
	ctx := ensureCoauthorshipFetchDeadline(withCoauthorshipFetchDeadline(context.Background(), earlier), now)
	deadline, ok := ctx.Value(liveTaskFetchDeadlineKey{}).(time.Time)
	require.True(t, ok)
	require.Equal(t, earlier.Add(liveTaskFetchBudget), deadline, "an existing deadline is kept, not extended")
}

// TestCommitFetchBudgets_CondensationKeepsItsOwnBudget pins that the commit's
// co-authorship deadline does not bound condensation: once it has passed, a
// session's re-exports still get their own agent.SubagentFetchBudget, as
// materializeTaskRecords starts one per session.
func TestCommitFetchBudgets_CondensationKeepsItsOwnBudget(t *testing.T) {
	t.Parallel()
	loopCtx := withCoauthorshipFetchDeadline(context.Background(), time.Now().Add(-time.Hour))
	ag := &fetchingAgent{dir: t.TempDir(), content: "{}"}

	_, err := agent.FetchSubagentTranscript(agent.WithSubagentFetchBudget(loopCtx, agent.SubagentFetchBudget), ag, "ses_child", "call_1", time.Time{}, time.Time{})

	require.NoError(t, err)
	require.Equal(t, []string{"ses_child"}, ag.fetched)
}

// TestShouldCondense_RunningSubagentCreditsAnActiveParent pins the read-only
// gate for any agent: an ACTIVE session with no tracked files of its own,
// while another session claims the committed files, is condensed only when its
// running subagent wrote a committed file.
func TestShouldCondense_RunningSubagentCreditsAnActiveParent(t *testing.T) {
	t.Parallel()
	recent := time.Now()
	for _, tt := range []struct {
		name          string
		evidence      bool
		subagentWrote bool
		want          bool
	}{
		{"subagent wrote a committed file", true, true, true},
		{"subagent wrote other files", true, false, false},
		{"no live task", false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &postCommitActionHandler{
				ctx:                        context.Background(),
				hasNew:                     true,
				sessionsWithCommittedFiles: 1,
				liveTaskClaimsCommit:       func() bool { return tt.subagentWrote },
			}
			require.Equal(t, tt.want, h.shouldCondenseWithOverlapCheck(true, &recent, false, tt.evidence))
		})
	}
}

func TestHasFreshLiveTaskRecord_AnyPhaseButEnded(t *testing.T) {
	t.Parallel()
	now := time.Now()
	live := []session.TaskRecord{{ToolUseID: "call_live", StartedAt: now.Add(-time.Minute)}}
	stale := []session.TaskRecord{{ToolUseID: "call_stale", StartedAt: now.Add(-25 * time.Hour)}}
	done := []session.TaskRecord{{ToolUseID: "call_done", StartedAt: now.Add(-time.Minute), CompletedAt: now}}

	tests := []struct {
		name    string
		phase   session.Phase
		records []session.TaskRecord
		want    bool
	}{
		{"active", session.PhaseActive, live, true},
		{"idle", session.PhaseIdle, live, true},
		{"ended", session.PhaseEnded, live, false},
		{"started over a day ago", session.PhaseActive, stale, false},
		{"completed", session.PhaseActive, done, false},
		{"no records", session.PhaseActive, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := &SessionState{SessionID: "s", Phase: tt.phase, TaskRecords: tt.records}
			require.Equal(t, tt.want, hasFreshLiveTaskRecord(state, now))
			require.Equal(t, tt.want && tt.phase == session.PhaseIdle, idleWithLiveTaskRecord(state, now),
				"the overlap-check bypass stays IDLE-only")
		})
	}
}
