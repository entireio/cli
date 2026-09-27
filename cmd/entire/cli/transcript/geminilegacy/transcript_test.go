package geminilegacy

import (
	"encoding/json"
	"testing"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	data := []byte(`{
  "messages": [
    {"type": "user", "content": "hello"},
    {"type": "gemini", "content": "hi there"}
  ]
}`)

	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	if len(transcript.Messages) != 2 {
		t.Fatalf("ParseTranscript() got %d messages, want 2", len(transcript.Messages))
	}
	if transcript.Messages[0].Type != MessageTypeUser {
		t.Errorf("First message type = %q, want %q", transcript.Messages[0].Type, MessageTypeUser)
	}
	if transcript.Messages[1].Type != MessageTypeGemini {
		t.Errorf("Second message type = %q, want %q", transcript.Messages[1].Type, MessageTypeGemini)
	}
}

func TestParseTranscript_Invalid(t *testing.T) {
	t.Parallel()

	if _, err := ParseTranscript([]byte(`not valid json`)); err == nil {
		t.Error("ParseTranscript() should error on invalid JSON")
	}
}

func TestParseTranscript_ContentShapes(t *testing.T) {
	t.Parallel()

	// Real Gemini CLI format: user messages have array content, gemini messages
	// have string content, and content may be null.
	data := []byte(`{
  "messages": [
    {"type": "user", "content": [{"text": "part one"}, {"text": "part two"}]},
    {"type": "gemini", "content": "sure thing"},
    {"type": "user", "content": null}
  ]
}`)

	transcript, err := ParseTranscript(data)
	if err != nil {
		t.Fatalf("ParseTranscript() error = %v", err)
	}
	want := []string{"part one\npart two", "sure thing", ""}
	if len(transcript.Messages) != len(want) {
		t.Fatalf("ParseTranscript() got %d messages, want %d", len(transcript.Messages), len(want))
	}
	for i, w := range want {
		if got := transcript.Messages[i].Content; got != w {
			t.Errorf("Message %d content = %q, want %q", i, got, w)
		}
	}
}

func TestSliceFromMessage(t *testing.T) {
	t.Parallel()

	data := []byte(`{"messages":[{"id":"m1","type":"user","content":"a"},{"id":"m2","type":"gemini","content":"b"},{"id":"m3","type":"user","content":"c"}]}`)

	t.Run("zero offset returns input", func(t *testing.T) {
		t.Parallel()
		got, err := SliceFromMessage(data, 0)
		if err != nil {
			t.Fatalf("SliceFromMessage() error = %v", err)
		}
		if string(got) != string(data) {
			t.Errorf("SliceFromMessage(0) = %s, want input unchanged", got)
		}
	})

	t.Run("offset scopes messages", func(t *testing.T) {
		t.Parallel()
		got, err := SliceFromMessage(data, 1)
		if err != nil {
			t.Fatalf("SliceFromMessage() error = %v", err)
		}
		parsed, err := ParseTranscript(got)
		if err != nil {
			t.Fatalf("ParseTranscript() error = %v", err)
		}
		if len(parsed.Messages) != 2 || parsed.Messages[0].ID != "m2" {
			t.Errorf("SliceFromMessage(1) = %s, want messages m2, m3", got)
		}
	})

	t.Run("offset past end returns nil", func(t *testing.T) {
		t.Parallel()
		got, err := SliceFromMessage(data, 3)
		if err != nil {
			t.Fatalf("SliceFromMessage() error = %v", err)
		}
		if got != nil {
			t.Errorf("SliceFromMessage(3) = %s, want nil", got)
		}
	})
}

func TestReassembleChunks(t *testing.T) {
	t.Parallel()

	chunk1 := []byte(`{"messages":[{"type":"user","content":"hello"}]}`)
	chunk2 := []byte(`{"messages":[{"type":"gemini","content":"hi"}]}`)

	result, err := ReassembleChunks([][]byte{chunk1, chunk2})
	if err != nil {
		t.Fatalf("ReassembleChunks() error = %v", err)
	}

	var parsed Transcript
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}
	if len(parsed.Messages) != 2 {
		t.Fatalf("Expected 2 messages, got %d", len(parsed.Messages))
	}
	if parsed.Messages[0].Content != "hello" || parsed.Messages[1].Content != "hi" {
		t.Errorf("Messages out of order: %s", result)
	}
}

func TestReassembleChunks_InvalidChunk(t *testing.T) {
	t.Parallel()

	chunks := [][]byte{[]byte(`{"messages":[]}`), []byte(`not valid json`)}
	if _, err := ReassembleChunks(chunks); err == nil {
		t.Error("ReassembleChunks() should error on invalid JSON chunk")
	}
}

// Stored chunks may carry fields this package does not model; reassembly must
// hand back the history as stored rather than the condensed read model.
func TestReassembleChunks_PreservesUnmodeledFields(t *testing.T) {
	t.Parallel()

	chunk1 := []byte(`{"messages":[{"id":"m1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":[{"text":"hello"}]}]}`)
	chunk2 := []byte(`{"messages":[{"id":"m2","type":"gemini","content":"hi","tokens":{"input":10},"thoughts":[{"subject":"s"}],"toolCalls":[{"id":"t1","name":"read_file","args":{},"result":[{"output":"x"}]}]}]}`)

	result, err := ReassembleChunks([][]byte{chunk1, chunk2})
	if err != nil {
		t.Fatalf("ReassembleChunks() error = %v", err)
	}
	want := `{"messages":[{"id":"m1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":[{"text":"hello"}]},{"id":"m2","type":"gemini","content":"hi","tokens":{"input":10},"thoughts":[{"subject":"s"}],"toolCalls":[{"id":"t1","name":"read_file","args":{},"result":[{"output":"x"}]}]}]}`
	if string(result) != want {
		t.Errorf("ReassembleChunks() =\n%s\nwant\n%s", result, want)
	}
}
