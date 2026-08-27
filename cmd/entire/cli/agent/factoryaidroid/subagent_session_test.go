package factoryaidroid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// workerSessionStart is a verbatim session_start line from a Droid Worker
// transcript. Droid records the invoking session and tool-use ID here, which is
// the only subagent boundary that survives its detached Worker dispatch.
const workerSessionStart = `{"type":"session_start","id":"30b34828-e462-45c9-b63c-bfefb6bd178f","title":"# Task Tool Invocation Subagent type: worker Task description: Create red.md and commit ## Context You are a specialized subagent invoked by another a...","sessionTitle":"worker: Create red.md and commit","isSessionTitleManuallySet":true,"owner":"soph","callingSessionId":"0b34cbcb-108c-4800-b68e-af7093c8cae9","callingToolUseId":"toolu_01SC9sRHSef1vtNFtMrX9w6T","version":2,"cwd":"/tmp/e2e-repo-948994727"}`

// topLevelSessionStart is a verbatim session_start line from the parent session
// that spawned the Worker above. It carries no calling IDs.
const topLevelSessionStart = `{"type":"session_start","id":"0b34cbcb-108c-4800-b68e-af7093c8cae9","title":"use a subagent: create a markdown file at docs/red.md","sessionTitle":"use a subagent: create a markdown file at docs/red.md","owner":"soph","version":2,"cwd":"/tmp/e2e-repo-948994727"}`

const messageLine = `{"type":"message","id":"a7727545-a952-4c56-b581-db72279007c7","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`

// writeTranscript writes a transcript into dir and returns its path.
func writeTranscript(t *testing.T, dir, sessionID string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

func TestResolveSubagentSession_WorkerSessionLinksToParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parent := writeTranscript(t, dir, "0b34cbcb-108c-4800-b68e-af7093c8cae9", topLevelSessionStart, messageLine)
	worker := writeTranscript(t, dir, "30b34828-e462-45c9-b63c-bfefb6bd178f", workerSessionStart, messageLine)

	link, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(worker)

	require.True(t, ok, "a Worker session must be recognized as a subagent session")
	assert.Equal(t, "0b34cbcb-108c-4800-b68e-af7093c8cae9", link.ParentSessionID)
	assert.Equal(t, "toolu_01SC9sRHSef1vtNFtMrX9w6T", link.ToolUseID)
	assert.Equal(t, parent, link.ParentTranscriptPath)
	assert.Equal(t, "worker", link.SubagentType)
	assert.Equal(t, "Create red.md and commit", link.TaskDescription)
}

func TestResolveSubagentSession_TopLevelSessionIsNotSubagent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	parent := writeTranscript(t, dir, "0b34cbcb-108c-4800-b68e-af7093c8cae9", topLevelSessionStart, messageLine)

	_, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(parent)

	assert.False(t, ok, "a session with no calling IDs is a top-level session")
}

func TestResolveSubagentSession_RejectsPartialLink(t *testing.T) {
	t.Parallel()

	// A link naming a parent session but no tool use (or vice versa) cannot key
	// a task checkpoint; attributing it anyway would file the work in the wrong
	// place, so it must fall back to the top-level session path.
	tests := map[string]string{
		"missing tool use id":    `{"type":"session_start","id":"w","callingSessionId":"p","sessionTitle":"worker: x"}`,
		"missing parent id":      `{"type":"session_start","id":"w","callingToolUseId":"toolu_1","sessionTitle":"worker: x"}`,
		"both present but empty": `{"type":"session_start","id":"w","callingSessionId":"","callingToolUseId":"","sessionTitle":"worker: x"}`,
	}

	for name, line := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := writeTranscript(t, dir, "w", line, messageLine)

			_, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(path)

			assert.False(t, ok)
		})
	}
}

func TestResolveSubagentSession_RejectsPathUnsafeIDs(t *testing.T) {
	t.Parallel()

	// Both IDs become path segments of the checkpoint's metadata directory, and
	// the parent ID also names a file we stat. A transcript is a file on disk, so
	// treat its contents as untrusted and fail closed rather than traversing.
	tests := map[string]struct{ parent, toolUse string }{
		"parent traverses":        {"../../etc", "toolu_1"},
		"parent has separator":    {"a/b", "toolu_1"},
		"parent is dot dot":       {"..", "toolu_1"},
		"parent has glob":         {"sess*", "toolu_1"},
		"parent has volume":       {"C:sess", "toolu_1"},
		"parent starts with dash": {"-sess", "toolu_1"},
		"tool use traverses":      {"parent-1", "../../etc"},
		"tool use has separator":  {"parent-1", "a/b"},
		"tool use has glob":       {"parent-1", "toolu_*"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			line, err := json.Marshal(map[string]any{
				"type":             "session_start",
				"id":               "w",
				"sessionTitle":     "worker: x",
				"callingSessionId": tt.parent,
				"callingToolUseId": tt.toolUse,
			})
			require.NoError(t, err)
			path := writeTranscript(t, dir, "w", string(line), messageLine)

			_, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(path)

			assert.False(t, ok, "a path-unsafe ID must not produce a subagent link")
		})
	}
}

func TestResolveSubagentSession_MissingParentTranscriptStillLinks(t *testing.T) {
	t.Parallel()

	// The parent transcript is optional context for the checkpoint. Losing it
	// must not cost us the attribution itself.
	dir := t.TempDir()
	worker := writeTranscript(t, dir, "30b34828-e462-45c9-b63c-bfefb6bd178f", workerSessionStart, messageLine)

	link, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(worker)

	require.True(t, ok)
	assert.Equal(t, "0b34cbcb-108c-4800-b68e-af7093c8cae9", link.ParentSessionID)
	assert.Empty(t, link.ParentTranscriptPath)
}

func TestResolveSubagentSession_LongTitleLineIsParsed(t *testing.T) {
	t.Parallel()

	// Droid embeds the whole task prompt in `title`, which routinely exceeds
	// bufio.Scanner's default 64KB line cap. Reading must not depend on it.
	dir := t.TempDir()
	huge := strings.Repeat("x", 300*1024)
	line := `{"type":"session_start","id":"w","title":"` + huge +
		`","sessionTitle":"worker: big","callingSessionId":"p","callingToolUseId":"toolu_big"}`
	worker := writeTranscript(t, dir, "w", line, messageLine)

	link, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(worker)

	require.True(t, ok, "a multi-hundred-KB session_start line must still parse")
	assert.Equal(t, "p", link.ParentSessionID)
	assert.Equal(t, "toolu_big", link.ToolUseID)
}

func TestResolveSubagentSession_MissingTranscript(t *testing.T) {
	t.Parallel()

	_, ok := (&FactoryAIDroidAgent{}).ResolveSubagentSession(filepath.Join(t.TempDir(), "absent.jsonl"))

	assert.False(t, ok, "an unreadable transcript must not be treated as a subagent session")
}

func TestParseDroidSessionTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		title           string
		wantType        string
		wantDescription string
	}{
		{"type and description", "worker: Create red.md", "worker", "Create red.md"},
		{"empty title", "", "", ""},
		{"no separator", "Create red.md", "", "Create red.md"},
		{"sentence with colon is not a type", "fix the bug: it crashes", "", "fix the bug: it crashes"},
		{"empty description", "worker:", "", "worker:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotType, gotDescription := parseDroidSessionTitle(tt.title)
			assert.Equal(t, tt.wantType, gotType)
			assert.Equal(t, tt.wantDescription, gotDescription)
		})
	}
}
