package cli

import (
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/stretchr/testify/assert"
)

// TestCompletionRecordKey pins how a completion-without-launch stop picks its
// record when its tool_use_id may be reused: some providers number tool calls
// per conversation, so OpenCode can send a later, different task call under
// a callID an earlier call's record already holds.
func TestCompletionRecordKey(t *testing.T) {
	t.Parallel()
	t1 := time.UnixMilli(1_000)
	t2 := time.UnixMilli(2_000)
	t3 := time.UnixMilli(3_000)
	done := time.UnixMilli(9_000)
	completed := func(key, child string, startedAt time.Time) session.TaskRecord {
		return session.TaskRecord{ToolUseID: key, AgentID: child, StartedAt: startedAt, CompletedAt: done}
	}

	tests := []struct {
		name      string
		records   []session.TaskRecord
		child     string
		startedAt time.Time
		want      string
	}{
		{
			name:  "no record under the callID keeps it",
			child: "ses_a", startedAt: t1,
			want: "c",
		},
		{
			name:    "duplicate stop without a start time",
			records: []session.TaskRecord{completed("c", "ses_a", t1)},
			child:   "ses_a",
			want:    "c",
		},
		{
			name:    "live record of the same child is completed",
			records: []session.TaskRecord{{ToolUseID: "c", AgentID: "ses_a", StartedAt: t1}},
			child:   "ses_a", startedAt: t2,
			want: "c",
		},
		{
			name:    "a different child reusing the callID gets its own key",
			records: []session.TaskRecord{completed("c", "ses_a", t1)},
			child:   "ses_b", startedAt: t2,
			want: "c-ses_b",
		},
		{
			// The running call's record is left alone, not completed with the wrong child.
			name:    "a different child while the first call is still live",
			records: []session.TaskRecord{{ToolUseID: "c", AgentID: "ses_a", StartedAt: t1}},
			child:   "ses_b", startedAt: t2,
			want: "c-ses_b",
		},
		{
			name:    "duplicate stop of the second call",
			records: []session.TaskRecord{completed("c", "ses_a", t1), completed("c-ses_b", "ses_b", t2)},
			child:   "ses_b", startedAt: t2,
			want: "c-ses_b",
		},
		{
			name:    "a later call resuming the same child gets its own key",
			records: []session.TaskRecord{completed("c", "ses_a", t1)},
			child:   "ses_a", startedAt: t2,
			want: "c-ses_a-2000",
		},
		{
			name:    "a stop starting no later than the completed call is its duplicate",
			records: []session.TaskRecord{completed("c", "ses_a", t2)},
			child:   "ses_a", startedAt: t1,
			want: "c",
		},
		{
			name:    "a third call resuming the second call's child",
			records: []session.TaskRecord{completed("c", "ses_a", t1), completed("c-ses_b", "ses_b", t2)},
			child:   "ses_b", startedAt: t3,
			want: "c-ses_b-3000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			state := &strategy.SessionState{TaskRecords: tt.records}
			event := &agent.Event{ToolUseID: "c", SubagentID: tt.child, SubagentStartedAt: tt.startedAt, CompletionWithoutLaunch: true}
			assert.Equal(t, tt.want, completionRecordKey(state, event))
		})
	}
}
