package openhands

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// realFixture is a genuine OpenHands conversation, serialized from the event
// directory a real headless run produced.
const realFixture = "testdata/real_events_v1.jsonl"

func readFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(realFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// materializeEventDir writes the fixture out as OpenHands would store it: one
// file per event, named by index and id.
func materializeEventDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "events")
	if err := writeEventDir(dir, readFixture(t)); err != nil {
		t.Fatalf("writeEventDir: %v", err)
	}
	return dir
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	a := NewOpenHandsAgent()
	if got := a.Name(); got != agent.AgentNameOpenHands {
		t.Errorf("Name() = %q", got)
	}
	if got := a.Type(); got != agent.AgentTypeOpenHands {
		t.Errorf("Type() = %q", got)
	}
	if dirs := a.ProtectedDirs(); len(dirs) != 1 || dirs[0] != ".openhands" {
		t.Errorf("ProtectedDirs() = %v", dirs)
	}
}

func TestRegisteredInRegistry(t *testing.T) {
	t.Parallel()

	got, err := agent.Get(agent.AgentNameOpenHands)
	if err != nil {
		t.Fatalf("agent.Get(openhands): %v", err)
	}
	if got.Name() != agent.AgentNameOpenHands {
		t.Errorf("registry returned %q", got.Name())
	}
}

// The whole directory-to-JSONL design rests on this: expanding the serialized
// form must reproduce OpenHands' own filenames exactly, or `--resume` sees a
// malformed conversation.
func TestEventDirRoundTrip(t *testing.T) {
	t.Parallel()

	original := readFixture(t)
	dir := filepath.Join(t.TempDir(), "events")

	if err := writeEventDir(dir, original); err != nil {
		t.Fatalf("writeEventDir: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// Filenames must match OpenHands' EVENT_FILE_PATTERN.
	for i, name := range names {
		if !eventFileRe.MatchString(name) {
			t.Errorf("file %q does not match OpenHands' event filename pattern", name)
		}
		if !strings.HasPrefix(name, "event-0000"+string(rune('0'+i))) {
			t.Errorf("file %q is not at index %d", name, i)
		}
	}

	got, err := readEventDir(dir)
	if err != nil {
		t.Fatalf("readEventDir: %v", err)
	}
	if string(got) != string(original) {
		t.Error("round trip through the event directory changed the transcript")
	}
}

// Ordering must come from the numeric index, not directory listing order.
func TestReadEventDir_OrdersByIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Written deliberately out of order.
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	write("event-00002-cccccccc-0000-0000-0000-000000000003.json", `{"id":"c","kind":"MessageEvent"}`)
	write("event-00000-aaaaaaaa-0000-0000-0000-000000000001.json", `{"id":"a","kind":"MessageEvent"}`)
	write("event-00001-bbbbbbbb-0000-0000-0000-000000000002.json", `{"id":"b","kind":"MessageEvent"}`)
	// Files OpenHands keeps alongside the events must be ignored.
	write("base_state.json", `{"not":"an event"}`)
	write(".eventlog.lock", "")

	data, err := readEventDir(dir)
	if err != nil {
		t.Fatalf("readEventDir: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (base_state.json and the lock file must be skipped)", len(lines))
	}
	for i, want := range []string{`"id":"a"`, `"id":"b"`, `"id":"c"`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %s, want it to contain %s", i, lines[i], want)
		}
	}
}

func TestReadEventDir_MissingDirIsEmpty(t *testing.T) {
	t.Parallel()

	data, err := readEventDir(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("a missing conversation dir should not error: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected no data, got %d bytes", len(data))
	}
}

// The id appears in two spellings: undashed for the directory, dashed for
// --resume. Confusing them points the reader at a directory that does not exist.
func TestConversationIDSpellings(t *testing.T) {
	t.Parallel()

	const dashed = "04e2eedb-e2d6-4736-a1a4-436334d9e1e6"
	const undashed = "04e2eedbe2d64736a1a4436334d9e1e6"

	if got := conversationDirID(dashed); got != undashed {
		t.Errorf("conversationDirID(dashed) = %q, want %q", got, undashed)
	}
	if got := conversationDirID(undashed); got != undashed {
		t.Errorf("conversationDirID(undashed) = %q, want it unchanged", got)
	}
	if got := resumeID(undashed); got != dashed {
		t.Errorf("resumeID(undashed) = %q, want %q", got, dashed)
	}
	if got := resumeID(dashed); got != dashed {
		t.Errorf("resumeID(dashed) = %q, want it unchanged", got)
	}
	// A non-UUID id must pass through rather than being mangled.
	if got := resumeID("not-a-uuid"); got != "not-a-uuid" {
		t.Errorf("resumeID passthrough = %q", got)
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	got := a.FormatResumeCommand("04e2eedbe2d64736a1a4436334d9e1e6")
	want := "openhands --resume 04e2eedb-e2d6-4736-a1a4-436334d9e1e6"
	if got != want {
		t.Errorf("FormatResumeCommand = %q, want %q (must use the dashed form)", got, want)
	}
	if got := a.FormatResumeCommand(" "); got != "openhands" {
		t.Errorf("blank session = %q", got)
	}
}

func TestGetTranscriptPositionAndFiles(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	dir := materializeEventDir(t)

	pos, err := a.GetTranscriptPosition(dir)
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	if pos != 5 {
		t.Errorf("position = %d, want 5 (the fixture has 5 events)", pos)
	}

	files, total, err := a.ExtractModifiedFilesFromOffset(dir, 0)
	if err != nil {
		t.Fatalf("ExtractModifiedFilesFromOffset: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	if len(files) != 1 || files[0] != "hello.txt" {
		t.Errorf("files = %v, want [hello.txt]", files)
	}
}

// tool_call.arguments is a JSON-encoded string, so a naive single decode finds
// nothing. This pins the second unmarshal.
func TestExtractModifiedFiles_DecodesNestedArguments(t *testing.T) {
	t.Parallel()

	const line = `{"id":"x","kind":"ActionEvent","tool_name":"file_editor",` +
		`"tool_call":{"id":"c","name":"file_editor","arguments":"{\"path\": \"a.txt\", \"command\": \"create\"}"}}`

	files := ExtractModifiedFiles([]byte(line))
	if len(files) != 1 || files[0] != "a.txt" {
		t.Errorf("files = %v, want [a.txt]", files)
	}
}

// A view is a read, not a modification.
func TestExtractModifiedFiles_SkipsViewCommand(t *testing.T) {
	t.Parallel()

	const line = `{"id":"x","kind":"ActionEvent","tool_name":"file_editor",` +
		`"tool_call":{"id":"c","name":"file_editor","arguments":"{\"path\": \"a.txt\", \"command\": \"view\"}"}}`

	files := ExtractModifiedFiles([]byte(line))
	if len(files) != 0 {
		t.Errorf("files = %v, want empty (view is a read)", files)
	}
}

func TestExtractPrompts_OnlyUserSourced(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	prompts, err := a.ExtractPrompts(materializeEventDir(t), 0)
	if err != nil {
		t.Fatalf("ExtractPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("prompts = %v, want exactly the one user message", prompts)
	}
	if prompts[0] != "create hello.txt" {
		t.Errorf("prompt = %q", prompts[0])
	}
}

func TestChunkAndReassemble_RoundTrip(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	data := readFixture(t)

	// The fixture's SystemPromptEvent is ~86KB on a single line, because it
	// embeds the whole system prompt and every tool schema. A chunk size below
	// that cannot split it, so this picks a bound above the largest line and
	// still small enough to force several chunks.
	chunks, err := a.ChunkTranscript(context.Background(), data, 87000)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	merged, err := a.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript: %v", err)
	}
	if strings.TrimSpace(string(merged)) != strings.TrimSpace(string(data)) {
		t.Error("chunk/reassemble round trip lost data")
	}
}

func TestConversationsRoot_HonoursEnvOverrides(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	t.Setenv(conversationsEnv, "")
	t.Setenv(persistenceEnv, "/custom/persist")
	got, err := conversationsRoot()
	if err != nil {
		t.Fatalf("conversationsRoot: %v", err)
	}
	if got != filepath.Join("/custom/persist", "conversations") {
		t.Errorf("root = %q", got)
	}

	// The conversations override wins outright.
	t.Setenv(conversationsEnv, "/only/here")
	got, err = conversationsRoot()
	if err != nil {
		t.Fatalf("conversationsRoot: %v", err)
	}
	if got != "/only/here" {
		t.Errorf("root = %q, want the conversations override to win", got)
	}
}

func TestWriteSession_Rejects(t *testing.T) {
	t.Parallel()

	a := &OpenHandsAgent{}
	ctx := context.Background()
	if err := a.WriteSession(ctx, nil); err == nil {
		t.Error("expected an error for a nil session")
	}
	if err := a.WriteSession(ctx, &agent.AgentSession{SessionRef: "/x"}); err == nil {
		t.Error("expected an error for empty data")
	}
	if err := a.WriteSession(ctx, &agent.AgentSession{NativeData: []byte("{}")}); err == nil {
		t.Error("expected an error for a missing session ref")
	}
}

// An event with no id cannot have its filename rebuilt, so writing must fail
// loudly rather than silently dropping it.
func TestWriteEventDir_RejectsEventWithoutID(t *testing.T) {
	t.Parallel()

	err := writeEventDir(filepath.Join(t.TempDir(), "events"), []byte(`{"kind":"MessageEvent"}`))
	if err == nil {
		t.Fatal("expected an error for an event with no id")
	}
	if !strings.Contains(err.Error(), "no id") {
		t.Errorf("error = %v, want it to name the missing id", err)
	}
}

func TestSliceFromEvent(t *testing.T) {
	t.Parallel()

	data := readFixture(t)
	out := SliceFromEvent(data, 3)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Errorf("sliced to %d lines, want 2", len(lines))
	}

	past := SliceFromEvent(data, 999)
	if past != nil {
		t.Error("offset past the end should yield nil")
	}
}
