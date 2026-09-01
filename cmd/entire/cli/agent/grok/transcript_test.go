package grok

import (
	"os"
	"path/filepath"
	"testing"
)

// realTranscript is a complete updates.jsonl from a real Grok 1.0.5 session
// (one turn, two model calls, one file write).
func realTranscript(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "updates.jsonl"))
	if err != nil {
		t.Fatalf("read transcript fixture: %v", err)
	}
	return data
}

// TestCalculateTokenUsage_DerivesFreshInput is the important one: Grok reports
// inputTokens cache-inclusive, so the fresh figure has to be derived. The
// expected values come from the same session's headless JSON summary, which
// reports the fresh input directly — so this cross-checks the derivation
// against Grok's own arithmetic rather than against itself.
func TestCalculateTokenUsage_DerivesFreshInput(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	usage, err := g.CalculateTokenUsage(realTranscript(t), 0)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage == nil {
		t.Fatal("usage is nil; the turn_completed line was not found")
	}

	// inputTokens 31658 - cachedReadTokens 15744 - cacheCreationTokens 0
	if got, want := usage.InputTokens, 15914; got != want {
		t.Errorf("InputTokens = %d, want %d (fresh input, cache excluded)", got, want)
	}
	if got, want := usage.CacheReadTokens, 15744; got != want {
		t.Errorf("CacheReadTokens = %d, want %d", got, want)
	}
	if got, want := usage.CacheCreationTokens, 0; got != want {
		t.Errorf("CacheCreationTokens = %d, want %d", got, want)
	}
	if got, want := usage.OutputTokens, 399; got != want {
		t.Errorf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := usage.APICallCount, 2; got != want {
		t.Errorf("APICallCount = %d, want %d", got, want)
	}
}

// TestCalculateTokenUsage_NilWhenNoUsage distinguishes "no data" from "zero" —
// the framework treats a nil result as unknown and a zero-valued struct as a
// genuine zero.
func TestCalculateTokenUsage_NilWhenNoUsage(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	usage, err := g.CalculateTokenUsage([]byte(`{"params":{"update":{"sessionUpdate":"agent_message_chunk"}}}`), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage != nil {
		t.Errorf("got %+v, want nil for a transcript with no usage data", usage)
	}
}

// TestCalculateTokenUsage_RespectsOffset covers the checkpoint-scoping
// contract: usage before the offset belongs to an earlier checkpoint.
func TestCalculateTokenUsage_RespectsOffset(t *testing.T) {
	t.Parallel()

	data := realTranscript(t)
	g := &GrokAgent{}

	lines, err := g.GetTranscriptPosition(filepath.Join("testdata", "updates.jsonl"))
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	usage, err := g.CalculateTokenUsage(data, lines)
	if err != nil {
		t.Fatalf("CalculateTokenUsage: %v", err)
	}
	if usage != nil {
		t.Errorf("got %+v, want nil when the offset is past every usage line", usage)
	}
}

// TestModifiedFiles_FromDiffBlocks pins file extraction to the diff content
// blocks, which carry the absolute path, rather than to locations[], which is
// relative and also present on read-only tools.
func TestModifiedFiles_FromDiffBlocks(t *testing.T) {
	t.Parallel()

	files, lines := modifiedFilesFrom(realTranscript(t), 0)
	if lines == 0 {
		t.Fatal("no lines counted")
	}
	if len(files) != 1 {
		t.Fatalf("files = %v, want exactly one", files)
	}
	if !filepath.IsAbs(files[0]) {
		t.Errorf("files[0] = %q, want an absolute path from the diff block", files[0])
	}
	if filepath.Base(files[0]) != "hello.txt" {
		t.Errorf("files[0] = %q, want basename hello.txt", files[0])
	}
}

// TestModifiedFiles_LocationsFallback covers a tool_call_update that has no
// diff block — then locations[] is the only signal available.
func TestModifiedFiles_LocationsFallback(t *testing.T) {
	t.Parallel()

	line := `{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"tool_call_update",` +
		`"toolCallId":"c1","locations":[{"path":"src/main.go"}]}}}`
	files, _ := modifiedFilesFrom([]byte(line), 0)
	if len(files) != 1 || files[0] != "src/main.go" {
		t.Errorf("files = %v, want [src/main.go]", files)
	}
}

// TestModifiedFiles_DiffWinsOverLocations ensures the same tool call is not
// reported twice under two different spellings of the same file.
func TestModifiedFiles_DiffWinsOverLocations(t *testing.T) {
	t.Parallel()

	line := `{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"tool_call_update",` +
		`"toolCallId":"c1","content":[{"type":"diff","path":"/abs/src/main.go"}],` +
		`"locations":[{"path":"src/main.go"}]}}}`
	files, _ := modifiedFilesFrom([]byte(line), 0)
	if len(files) != 1 {
		t.Fatalf("files = %v, want exactly one (diff should win over locations)", files)
	}
	if files[0] != "/abs/src/main.go" {
		t.Errorf("files[0] = %q, want the absolute diff path", files[0])
	}
}

// TestModifiedFiles_IgnoresNonToolUpdates guards against counting reads or
// message chunks as edits.
func TestModifiedFiles_IgnoresNonToolUpdates(t *testing.T) {
	t.Parallel()

	line := `{"method":"_x.ai/session/update","params":{"update":{"sessionUpdate":"agent_message_chunk",` +
		`"content":[{"type":"diff","path":"/abs/nope.go"}]}}}`
	files, _ := modifiedFilesFrom([]byte(line), 0)
	if len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
}

// TestTranscriptEnvelopeIsNested is a regression guard for the shape mistake
// that costs the most: reading sessionUpdate off the top level of a line
// instead of params.update yields an empty result on every real transcript.
func TestTranscriptEnvelopeIsNested(t *testing.T) {
	t.Parallel()

	topLevel := `{"sessionUpdate":"tool_call_update","toolCallId":"c1","content":[{"type":"diff","path":"/abs/x.go"}]}`
	if files, _ := modifiedFilesFrom([]byte(topLevel), 0); len(files) != 0 {
		t.Errorf("files = %v, want none: sessionUpdate lives at params.update, not the top level", files)
	}
}

func TestGetTranscriptPosition(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}

	lines, err := g.GetTranscriptPosition(filepath.Join("testdata", "updates.jsonl"))
	if err != nil {
		t.Fatalf("GetTranscriptPosition: %v", err)
	}
	if lines != 16 {
		t.Errorf("lines = %d, want 16", lines)
	}

	// A missing file is not an error — sessions are created before their
	// transcript exists.
	missing, err := g.GetTranscriptPosition(filepath.Join("testdata", "does-not-exist.jsonl"))
	if err != nil {
		t.Errorf("missing file: unexpected error %v", err)
	}
	if missing != 0 {
		t.Errorf("missing file: got %d, want 0", missing)
	}

	if empty, err := g.GetTranscriptPosition(""); err != nil || empty != 0 {
		t.Errorf(`empty path: got (%d, %v), want (0, nil)`, empty, err)
	}
}

func TestExtractModel(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	model, err := g.ExtractModel(realTranscript(t))
	if err != nil {
		t.Fatalf("ExtractModel: %v", err)
	}
	if model != "grok-4.6" {
		t.Errorf("model = %q, want grok-4.6", model)
	}

	none, err := g.ExtractModel([]byte(`{"params":{"update":{"sessionUpdate":"tool_call"}}}`))
	if err != nil {
		t.Fatalf("ExtractModel (no model): %v", err)
	}
	if none != "" {
		t.Errorf("model = %q, want empty", none)
	}
}

func TestChunkAndReassembleRoundTrip(t *testing.T) {
	t.Parallel()

	g := &GrokAgent{}
	original := realTranscript(t)

	chunks, err := g.ChunkTranscript(t.Context(), original, 2048)
	if err != nil {
		t.Fatalf("ChunkTranscript: %v", err)
	}
	if len(chunks) < 2 {
		t.Fatalf("got %d chunk(s), want the fixture to actually split", len(chunks))
	}

	rejoined, err := g.ReassembleTranscript(chunks)
	if err != nil {
		t.Fatalf("ReassembleTranscript: %v", err)
	}

	// Line count is what must survive; a trailing-newline difference is not a
	// data loss.
	if got, want := len(splitJSONL(rejoined)), len(splitJSONL(original)); got != want {
		t.Errorf("round trip produced %d lines, want %d", got, want)
	}
}

func TestSplitJSONL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"one line no newline", `{"a":1}`, 1},
		{"trailing newline", "{\"a\":1}\n", 1},
		{"blank lines skipped", "{\"a\":1}\n\n{\"b\":2}\n", 2},
		{"crlf", "{\"a\":1}\r\n{\"b\":2}\r\n", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := len(splitJSONL([]byte(tt.in))); got != tt.want {
				t.Errorf("splitJSONL(%q) = %d lines, want %d", tt.in, got, tt.want)
			}
		})
	}
}
