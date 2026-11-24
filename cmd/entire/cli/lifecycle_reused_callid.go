package cli

import (
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// completionRecordKey picks the task record a completion-without-launch stop
// completes. Its tool_use_id alone may not identify the call: some providers
// number tool calls per conversation, so OpenCode can report a later,
// different task call under a callID an earlier call's record already holds.
//
// Records keyed by the callID, or by a key minted from it ("<callID>-..."),
// are matched to the stop by child (AgentID), then by call start at
// millisecond precision:
//   - the same child and start, or a stop with no start, is that record's
//     repeated stop, and the exactly-once guard skips it;
//   - a live record of the same child is completed, as without reuse;
//   - a completed record of the same child that started no earlier than this
//     stop is treated as its duplicate rather than risking double attribution.
//
// Otherwise the stop is a new call and gets a key of its own: "<callID>-<child>"
// for a different child, "<callID>-<child>-<startMs>" for a later call that
// resumed a child already recorded under this callID. A stop with no record
// under its callID keeps the callID unchanged.
func completionRecordKey(state *strategy.SessionState, event *agent.Event) string {
	callID := event.ToolUseID
	if callID == "" || event.SubagentID == "" || state.FindTaskRecord(callID) == nil {
		return callID
	}
	startMs := event.SubagentStartedAt.UnixMilli()
	sameChild := false
	var live, notEarlier *session.TaskRecord
	for i := range state.TaskRecords {
		rec := &state.TaskRecords[i]
		if rec.AgentID != event.SubagentID || (rec.ToolUseID != callID && !strings.HasPrefix(rec.ToolUseID, callID+"-")) {
			continue
		}
		sameChild = true
		switch {
		// No start (a plugin restarted mid-call) cannot tell a new call on this child from a duplicate; skip rather than double-count.
		case event.SubagentStartedAt.IsZero() || rec.StartedAt.UnixMilli() == startMs:
			return rec.ToolUseID
		case rec.CompletedAt.IsZero():
			// Whatever its start: the plugin's background queue stops a child's calls first in, first out.
			live = rec
		case rec.StartedAt.UnixMilli() > startMs:
			notEarlier = rec
		}
	}
	if live != nil {
		return live.ToolUseID
	}
	if notEarlier != nil {
		return notEarlier.ToolUseID
	}
	return reusedCallKey(callID, event.SubagentID, event.SubagentStartedAt, sameChild)
}

// reusedCallKey mints the record key for a call that reused callID. Both
// parts are already path-safe: dispatch validates SubagentID and the adapter
// maps an unsafe callID before it reaches here.
func reusedCallKey(callID, childID string, startedAt time.Time, sameChild bool) string {
	key := callID + "-" + childID
	if sameChild {
		key += "-" + strconv.FormatInt(startedAt.UnixMilli(), 10)
	}
	return key
}
