package opencode

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The testdata/opencode2 fixtures are real OpenCode 2.1.1 exports, captured
// with a scripted mock model and the repository path replaced by /repo.

func loadOpenCode2Fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "opencode2", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func normalizeFixture(t *testing.T, name string) (*ExportSession, []byte) {
	t.Helper()
	raw := loadOpenCode2Fixture(t, name)
	if !isOpenCodeV2Export(raw) {
		t.Fatalf("%s: not detected as an OpenCode 2 export", name)
	}
	data, err := normalizeOpenCodeExport(raw)
	if err != nil {
		t.Fatalf("%s: normalize: %v", name, err)
	}
	if isOpenCodeV2Export(data) {
		t.Fatalf("%s: normalized export still looks like OpenCode 2", name)
	}
	session, err := ParseExportSession(data)
	if err != nil || session == nil {
		t.Fatalf("%s: parse normalized export: %v", name, err)
	}
	return session, data
}

func toolNames(session *ExportSession) []string {
	var names []string
	for _, msg := range session.Messages {
		for _, part := range msg.Parts {
			if part.Type == "tool" {
				names = append(names, part.Tool)
			}
		}
	}
	return names
}

func TestNormalizeOpenCodeExport_Tools(t *testing.T) {
	t.Parallel()
	session, data := normalizeFixture(t, "tools")

	if got, want := toolNames(session), []string{"write", "edit", "bash"}; !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v (shell maps back to bash)", got, want)
	}
	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`"SCENARIO:tools go"`}; !slices.Equal(prompts, want) {
		t.Errorf("prompts = %q, want %q", prompts, want)
	}
	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.FromSlash("/repo/a.txt")}; !slices.Equal(files, want) {
		t.Errorf("modified files = %v, want %v (edit/write path maps to an absolute filePath)", files, want)
	}
	usage, err := (&OpenCodeAgent{}).CalculateTokenUsage(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 400 || usage.OutputTokens != 80 || usage.APICallCount != 4 {
		t.Errorf("usage = %+v, want 400 in / 80 out over 4 calls", usage)
	}
	// The idle marker carries no conversation and is dropped.
	if len(session.Messages) != 5 {
		t.Errorf("messages = %d, want 5 (1 user + 4 assistant)", len(session.Messages))
	}
	for _, msg := range session.Messages {
		if msg.Info.ID == "" || msg.Info.Time.Created == 0 {
			t.Errorf("message missing id or time: %+v", msg.Info)
		}
		for _, part := range msg.Parts {
			if part.ID == "" {
				t.Errorf("part without id in %s", msg.Info.ID)
			}
		}
	}
}

func TestNormalizeOpenCodeExport_PatchPathsAreAbsolute(t *testing.T) {
	t.Parallel()
	_, data := normalizeFixture(t, "patch")
	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatal(err)
	}
	// OpenCode 2 reports patched files relative to the session directory;
	// 1.x apply_patch reported absolute paths.
	want := []string{filepath.FromSlash("/repo/c.txt"), filepath.FromSlash("/repo/README")}
	if !slices.Equal(files, want) {
		t.Errorf("modified files = %v, want %v", files, want)
	}
}

func TestNormalizeOpenCodeExport_Subagent(t *testing.T) {
	t.Parallel()
	session, _ := normalizeFixture(t, "subagent")
	var task *Part
	for _, msg := range session.Messages {
		for i := range msg.Parts {
			if msg.Parts[i].Tool == "task" {
				task = &msg.Parts[i]
			}
		}
	}
	if task == nil || task.State == nil {
		t.Fatalf("no task part after normalizing the subagent tool: %v", toolNames(session))
	}
	if task.CallID == "" {
		t.Error("task part has no callID")
	}
	if got := task.State.Input["subagent_type"]; got != "general" {
		t.Errorf("subagent_type = %v, want general (from agent)", got)
	}
	if _, ok := task.State.Input["agent"]; ok {
		t.Error("agent key kept alongside subagent_type")
	}
	if task.State.Output == "" {
		t.Error("tool output not carried from state.content")
	}

	// The child's own export normalizes and scopes to its call like 1.x.
	_, child := normalizeFixture(t, "subagent-child")
	scoped, kept, err := scopeExportToCall(child, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if kept == 0 {
		t.Fatal("scoping the child export kept nothing")
	}
	files, err := ExtractModifiedFiles(scoped)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.FromSlash("/repo/child.txt")}; !slices.Equal(files, want) {
		t.Errorf("child modified files = %v, want %v", files, want)
	}
}

func TestNormalizeOpenCodeExport_BackgroundResultIsNotAPrompt(t *testing.T) {
	t.Parallel()
	session, data := normalizeFixture(t, "background-subagent")
	synthetic := 0
	for _, msg := range session.Messages {
		if msg.Info.Role == roleUser && OnlySyntheticText(msg.Parts) {
			synthetic++
		}
	}
	if synthetic == 0 {
		t.Fatal("background result not converted to a synthetic user message")
	}
	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts {
		if strings.Contains(p, "<subagent ") {
			t.Errorf("background result extracted as a prompt: %q", p)
		}
	}
}

func TestNormalizeOpenCodeExport_Compaction(t *testing.T) {
	t.Parallel()
	session, data := normalizeFixture(t, "compaction")
	var markers, summaries int
	for _, msg := range session.Messages {
		for _, part := range msg.Parts {
			if part.Type == "compaction" {
				markers++
			}
		}
		if strings.HasSuffix(msg.Info.ID, "-summary") {
			summaries++
			if msg.Info.Role != roleAssistant || msg.Info.Tokens == nil {
				t.Errorf("compaction summary = %+v, want an assistant message with tokens", msg.Info)
			}
		}
	}
	if markers != 1 || summaries != 1 {
		t.Errorf("compaction markers = %d, summaries = %d, want 1 and 1", markers, summaries)
	}
	// The compaction marker is not a prompt.
	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts {
		if p == "" {
			t.Error("empty prompt extracted")
		}
	}
}

// A session OpenCode 2 migrated from a 1.x database keeps its 1.x tool names
// and input keys, and gains a system notice about the renames.
func TestNormalizeOpenCodeExport_MigratedFromV1(t *testing.T) {
	t.Parallel()
	session, data := normalizeFixture(t, "migrated-from-v1")
	names := toolNames(session)
	for _, want := range []string{"write", "edit", "bash", "task"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools = %v, missing %s", names, want)
		}
	}
	files, err := ExtractModifiedFiles(data)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(files, "a.txt") {
		t.Errorf("modified files = %v, want a.txt from 1.x filePath input", files)
	}
	prompts, err := ExtractAllUserPrompts(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts {
		if strings.Contains(p, "tools have changed") {
			t.Errorf("migration notice extracted as a prompt: %q", p)
		}
	}
}

func TestNormalizeOpenCodeExport_LeavesV1Unchanged(t *testing.T) {
	t.Parallel()
	for _, v1 := range []string{
		`{"info":{"id":"ses_1"},"messages":[{"info":{"id":"m1","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"hi"}]}]}`,
		`{"info":{"id":"ses_1"},"messages":[]}`,
	} {
		got, err := normalizeOpenCodeExport([]byte(v1))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != v1 {
			t.Errorf("1.x export changed:\n got %s\nwant %s", got, v1)
		}
	}
}

// Resume gives OpenCode 2 back exactly the messages it exported, from the
// stored (converted) transcript.
func TestOpenCodeV2ImportPayload_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"tools", "patch", "subagent", "background-subagent", "compaction", "tool-error", "migrated-from-v1"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := loadOpenCode2Fixture(t, name)
			_, stored := normalizeFixture(t, name)
			payload, err := openCodeV2ImportPayload(stored, sessionIDOf(t, stored), "/elsewhere")
			if err != nil {
				t.Fatal(err)
			}
			var want, got struct {
				Info     map[string]any   `json:"info"`
				Messages []map[string]any `json:"messages"`
			}
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatal(err)
			}
			wantMsgs, err := json.Marshal(want.Messages)
			if err != nil {
				t.Fatal(err)
			}
			gotMsgs, err := json.Marshal(got.Messages)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wantMsgs, gotMsgs) {
				t.Fatalf("messages differ after round trip:\n got %.400s\nwant %.400s", gotMsgs, wantMsgs)
			}
			if _, ok := got.Info["directory"]; ok {
				t.Error("1.x-only info.directory sent to OpenCode 2")
			}
			loc, ok := got.Info["location"].(map[string]any)
			if !ok || loc["directory"] != "/elsewhere" {
				t.Errorf("info.location = %v, want the resuming worktree", got.Info["location"])
			}
			if got.Info["id"] != want.Info["id"] {
				t.Errorf("session id = %v, want %v", got.Info["id"], want.Info["id"])
			}
		})
	}
}

// OpenCode 2 rejects the whole import when one attachment's data is not
// base64, which redaction or image externalization can cause.
func TestOpenCodeV2ImportPayload_DropsChangedAttachments(t *testing.T) {
	t.Parallel()
	user := `{"id":"msg_u","type":"user","text":"see","time":{"created":1},"files":[` +
		`{"data":"aGVsbG8=","mime":"text/plain","source":{"type":"uri","uri":"a"}},` +
		`{"data":"[REDACTED]","mime":"image/png","source":{"type":"uri","uri":"b"}}]}`
	tool := `{"id":"msg_a","type":"assistant","time":{"created":2,"completed":3},"content":[{"type":"tool","id":"c1","name":"read","state":{"status":"completed","content":[` +
		`{"type":"text","text":"ok"},{"type":"media","media":{"source":{"type":"base64","data":"REDACTED_x","mediaType":"image/png"}}}]}}]}`
	export := `{"info":{"id":"ses_x","location":{"directory":"/a"}},"messages":[` + user + `,` + tool + `]}`
	stored, err := normalizeOpenCodeExport([]byte(export))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := openCodeV2ImportPayload(stored, sessionIDOf(t, stored), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	s := string(payload)
	if strings.Contains(s, "REDACTED") {
		t.Errorf("payload keeps an attachment OpenCode would reject: %s", s)
	}
	if !strings.Contains(s, "aGVsbG8=") || !strings.Contains(s, `"text":"ok"`) {
		t.Errorf("payload dropped a valid attachment or text: %s", s)
	}
	if !strings.Contains(s, "[attachment not restored]") {
		t.Errorf("changed tool media not replaced with a placeholder: %s", s)
	}
}

// Chunking a large transcript must keep the OpenCode 2 originals resume
// imports, which the typed export structs do not model.
func TestChunkTranscript_KeepsOpenCode2Originals(t *testing.T) {
	t.Parallel()
	_, stored := normalizeFixture(t, "background-subagent")
	ag := &OpenCodeAgent{}
	chunks, err := ag.ChunkTranscript(t.Context(), stored, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the transcript split", len(chunks))
	}
	joined, err := ag.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatal(err)
	}
	want, err := openCodeV2ImportPayload(stored, sessionIDOf(t, stored), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	got, err := openCodeV2ImportPayload(joined, sessionIDOf(t, joined), "/repo")
	if err != nil {
		t.Fatalf("reassembled transcript lost its OpenCode 2 originals: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("reassembled transcript imports differently from the original")
	}
}
