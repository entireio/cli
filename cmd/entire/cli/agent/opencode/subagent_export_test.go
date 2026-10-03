package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// resumedChildExportFixture is a child resumed through `task_id`: its first
// call (messages at 1–2 ms) wrote red.md, its second (at 5000 ms and later)
// wrote blue.md. An unknown top-level field and an unknown message field must
// survive scoping.
const resumedChildExportFixture = `{"info":{"id":"ses_child","parentID":"ses_parent"},"extra":"keep","messages":[` +
	`{"info":{"id":"m1","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"make red"}]},` +
	`{"info":{"id":"m2","role":"assistant","time":{"created":2},"tokens":{"input":100,"output":20,"reasoning":0,"cache":{"read":0,"write":0}}},` +
	`"parts":[{"type":"tool","tool":"write","callID":"w1","state":{"status":"completed","input":{"filePath":"/repo/docs/red.md"}}}]},` +
	`{"info":{"id":"m3","role":"user","time":{"created":5000}},"custom":1,"parts":[{"type":"text","text":"now blue"}]},` +
	`{"info":{"id":"m4","role":"assistant","time":{"created":5001},"tokens":{"input":7,"output":3,"reasoning":0,"cache":{"read":0,"write":0}}},` +
	`"parts":[{"type":"tool","tool":"write","callID":"w2","state":{"status":"completed","input":{"filePath":"/repo/docs/blue.md"}}}]}]}`

func messageIDs(t *testing.T, data []byte) []string {
	t.Helper()
	session, err := ParseExportSession(data)
	require.NoError(t, err)
	ids := make([]string, 0, len(session.Messages))
	for _, m := range session.Messages {
		ids = append(ids, m.Info.ID)
	}
	return ids
}

func TestScopeExportToCall(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		since   time.Time
		until   time.Time
		wantIDs []string
	}{
		{name: "second call keeps only its messages", since: time.UnixMilli(5000), wantIDs: []string{"m3", "m4"}},
		{name: "first call start keeps everything", since: time.UnixMilli(1), wantIDs: []string{"m1", "m2", "m3", "m4"}},
		{name: "start after every message keeps nothing", since: time.UnixMilli(9000), wantIDs: []string{}},
		{name: "first call's completion drops the later call", since: time.UnixMilli(1), until: time.UnixMilli(100), wantIDs: []string{"m1", "m2"}},
		{name: "end alone still cuts later calls", until: time.UnixMilli(100), wantIDs: []string{"m1", "m2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, kept, err := scopeExportToCall([]byte(resumedChildExportFixture), tt.since, tt.until)
			require.NoError(t, err)
			assert.Equal(t, len(tt.wantIDs), kept)
			assert.Equal(t, tt.wantIDs, messageIDs(t, out))

			var doc map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out, &doc))
			assert.JSONEq(t, `"keep"`, string(doc["extra"]), "unknown top-level fields survive")
			if kept > 0 && tt.wantIDs[0] == "m3" {
				assert.Contains(t, string(doc["messages"]), `"custom":1`, "unknown message fields survive")
			}
		})
	}
}

func TestScopeExportToCall_RejectsInvalidExport(t *testing.T) {
	t.Parallel()
	_, _, err := scopeExportToCall([]byte(`{"messages":"nope"}`), time.UnixMilli(1), time.Time{})
	require.Error(t, err)
}

func TestParseHookEvent_SubagentStart_CarriesCallStart(t *testing.T) {
	t.Parallel()
	input := `{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"ses_child","started_at":5000}`
	event, err := (&OpenCodeAgent{}).ParseHookEvent(context.Background(), HookNameSubagentStart, strings.NewReader(input))
	require.NoError(t, err)
	assert.True(t, event.SubagentStartedAt.Equal(time.UnixMilli(5000)), event.SubagentStartedAt)

	input = `{"session_id":"ses_parent","tool_use_id":"call_red","subagent_id":"ses_child"}`
	event, err = (&OpenCodeAgent{}).ParseHookEvent(context.Background(), HookNameSubagentStart, strings.NewReader(input))
	require.NoError(t, err)
	assert.True(t, event.SubagentStartedAt.IsZero(), "an absent start stays unknown so the framework uses its own clock")
}

func TestFetchSubagentTranscript_ResumedChildDeclaresOnlyThisCall(t *testing.T) {
	// Not parallel: t.Chdir and stubExport.
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, []byte(resumedChildExportFixture), 0o600)
	})

	ag := &OpenCodeAgent{}
	path, err := ag.FetchSubagentTranscript(context.Background(), "ses_child", "call_blue", time.UnixMilli(5000), time.Time{})
	require.NoError(t, err)

	assert.True(t, strings.HasSuffix(path, filepath.Join(paths.EntireTmpDir, "ses_child.call_blue.json")),
		"each call declares its own file, so a later call cannot overwrite it: %s", path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"m3", "m4"}, messageIDs(t, data))

	files, _, err := ag.ExtractModifiedFilesFromOffset(context.Background(), path, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"/repo/docs/blue.md"}, files, "the first call's red.md must not be attributed to this call")

	usage, err := ag.CalculateTokenUsage(data, 0)
	require.NoError(t, err)
	assert.Equal(t, 7, usage.InputTokens)
	assert.Equal(t, 3, usage.OutputTokens)
}

func TestFetchSubagentTranscript_UnknownStartDeclaresFullExport(t *testing.T) {
	// Not parallel: t.Chdir and stubExport.
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, []byte(resumedChildExportFixture), 0o600)
	})

	path, err := (&OpenCodeAgent{}).FetchSubagentTranscript(context.Background(), "ses_child", "call_blue", time.Time{}, time.Time{})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(path, filepath.Join(paths.EntireTmpDir, "ses_child.json")), path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"m1", "m2", "m3", "m4"}, messageIDs(t, data))
}

func TestFetchSubagentTranscript_ScopesLikeTheStopHook(t *testing.T) {
	// Not parallel: t.Chdir and stubExport.
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, []byte(resumedChildExportFixture), 0o600)
	})

	path, err := (&OpenCodeAgent{}).FetchSubagentTranscript(context.Background(), "ses_child", "call_blue", time.UnixMilli(5000), time.Time{})
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"m3", "m4"}, messageIDs(t, data))
}

func TestFetchSubagentTranscript_ReExportOfEarlierCallExcludesLaterCalls(t *testing.T) {
	// Not parallel: t.Chdir and stubExport.
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)
	stubExport(t, func(_ context.Context, root *os.Root, _, outputName string) error {
		return root.WriteFile(outputName, []byte(resumedChildExportFixture), 0o600)
	})

	// Condensation re-exports the first call after the child has served the
	// second: the first call's completion must bound the slice.
	ag := &OpenCodeAgent{}
	path, err := ag.FetchSubagentTranscript(context.Background(), "ses_child", "call_red", time.UnixMilli(1), time.UnixMilli(100))
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"m1", "m2"}, messageIDs(t, data))
	files, _, err := ag.ExtractModifiedFilesFromOffset(context.Background(), path, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"/repo/docs/red.md"}, files, "the later call's blue.md must not be attributed to the first call")
}
