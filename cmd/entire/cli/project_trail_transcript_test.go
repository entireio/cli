package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/redact"
)

const liveUser = `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"content":"Fix the failing test"}}`
const liveAssistant = `{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg-1","content":[{"type":"text","text":"Running the tests."},{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"go test ./..."}}]}}`
const liveResult = `{"type":"user","timestamp":"2026-01-01T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"All tests passed"}]}}`

func TestLocalTranscriptLiveUpdatesBeforeEOF(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	emitted := make(chan localLogRecord, 32)
	done := make(chan error, 1)
	go func() {
		done <- publishLocalTranscript(ctx, input, io.Discard, "claude-code", nil, func(_ context.Context, batch []localLogRecord) error {
			for _, r := range batch {
				emitted <- r
			}
			return nil
		})
	}()
	write := func(text string) {
		t.Helper()
		if _, err := io.WriteString(writer, text); err != nil {
			t.Fatal(err)
		}
	}
	next := func() localLogRecord {
		t.Helper()
		select {
		case r := <-emitted:
			return r
		case <-time.After(5 * time.Second):
			t.Fatal("live record did not arrive before EOF")
			return localLogRecord{}
		}
	}
	write(liveUser[:20])
	select {
	case r := <-emitted:
		t.Fatalf("published partial JSON: %+v", r)
	default:
	}
	write(liveUser[20:] + "\n")
	prompt := next()
	if prompt.Kind != "message" || !strings.Contains(prompt.Text, "Fix the failing test") {
		t.Fatalf("prompt: %+v", prompt)
	}
	write(liveAssistant + "\n")
	started := next()
	if strings.Contains(started.Text, `"result"`) {
		t.Fatal("invented a tool result")
	}
	write(liveResult + "\n")
	finished := next()
	var before, after compactLogMessage
	if err := json.Unmarshal([]byte(started.Text), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(finished.Text), &after); err != nil {
		t.Fatal(err)
	}
	if before.ID != after.ID || string(before.TS) != string(after.TS) || !strings.Contains(finished.Text, "All tests passed") {
		t.Fatalf("not a stable-ID replacement: %+v %+v", before, after)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if r := next(); r.Kind != "complete" || r.Sequence != 4 {
		t.Fatalf("completion: %+v", r)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLocalTranscriptModelMetadata(t *testing.T) {
	t.Parallel()
	ag, err := agent.Get(agenttypes.AgentName("claude-code"))
	if err != nil {
		t.Fatal(err)
	}
	c := localTranscriptConverter{name: "claude-code", sanitizer: ag, started: time.Unix(1, 0), filters: []*regexp.Regexp{regexp.MustCompile("private")}}
	first := strings.Replace(liveAssistant, `"id":"msg-1"`, `"id":"msg-1","model":"claude-private"`, 1)
	for i, line := range []string{first, liveResult, `{"type":"assistant","timestamp":"2026-01-01T00:00:03Z","message":{"id":"msg-2","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Done"}]}}`} {
		if err := c.append([]byte(line)); err != nil {
			t.Fatal(err)
		}
		updates, err := c.updates()
		if err != nil || len(updates) != 1 {
			t.Fatalf("update %d: %+v %v", i, updates, err)
		}
		var message compactLogMessage
		if err := json.Unmarshal([]byte(updates[0].Text), &message); err != nil {
			t.Fatal(err)
		}
		want := "claude-[REDACTED]"
		if i == 2 {
			want = "claude-sonnet-4-6"
		}
		if message.Model != want {
			t.Fatalf("model = %q, want %q", message.Model, want)
		}
		c.accept(updates[0])
	}
}

func TestLocalTranscriptConverterRedactsAndOmitsImages(t *testing.T) {
	t.Parallel()
	c := localTranscriptConverter{name: "claude-code", started: time.Unix(1, 0), filters: []*regexp.Regexp{regexp.MustCompile("private-project")}}
	raw := `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"private-project postgres://alice:plainpass123@host/db"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"image-data"}},{"type":"thinking","thinking":"hidden reasoning"}]}}`
	if err := c.append([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	updates, err := c.updates()
	if err != nil || len(updates) != 1 {
		t.Fatalf("updates: %+v %v", updates, err)
	}
	for _, s := range []string{"private-project", "plainpass123", "image-data", "hidden reasoning"} {
		if strings.Contains(updates[0].Text, s) {
			t.Fatalf("unshared content escaped: %s", s)
		}
	}
	c.accept(updates[0])
	unchanged, err := c.updates()
	if err != nil || len(unchanged) != 0 {
		t.Fatalf("unchanged messages re-emitted: %+v %v", unchanged, err)
	}
}

func TestLocalTranscriptBroadRedactionPreservesProtocol(t *testing.T) {
	t.Parallel()
	c := localTranscriptConverter{name: "claude-code", started: time.Unix(1, 0), filters: []*regexp.Regexp{regexp.MustCompile(".+")}}
	assistant := strings.Replace(liveAssistant, `{"command":"go test ./..."}`, `{"private-a":"private-value","private-b":{"status":"private-status"}}`, 1)
	for _, line := range []string{assistant, liveResult} {
		if err := c.append([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	updates, err := c.updates()
	if err != nil || len(updates) != 1 {
		t.Fatalf("updates: %+v %v", updates, err)
	}
	if strings.Contains(updates[0].Text, "private-") || strings.Contains(updates[0].Text, "tool-1") {
		t.Fatal("unredacted content or source identity escaped")
	}
	var result struct {
		Content []struct {
			Type   string `json:"type"`
			ID     string `json:"id"`
			Result struct {
				Status string `json:"status"`
				Output string `json:"output"`
			} `json:"result"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(updates[0].Text), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 2 || result.Content[0].Type != "text" || result.Content[1].Type != "tool_use" || result.Content[1].Result.Status != "success" || result.Content[1].Result.Output != "[REDACTED]" || !strings.HasPrefix(result.Content[1].ID, "tool:") {
		t.Fatalf("redaction corrupted the compact protocol: %+v", result)
	}
	c.accept(updates[0])
	for range 10 {
		unchanged, err := c.updates()
		if err != nil || len(unchanged) != 0 {
			t.Fatalf("redacted updates are not stable: %+v %v", unchanged, err)
		}
	}
}

func TestLocalTranscriptScannerFailsClosed(t *testing.T) {
	redact.WithScannerDegradedSole(t)
	c := localTranscriptConverter{name: "claude-code"}
	if err := c.append([]byte(liveUser)); err == nil || len(c.content) != 0 {
		t.Fatalf("scanner failure did not withhold input: %v", err)
	}
}

func TestLocalTranscriptDrainsInvalidInput(t *testing.T) {
	t.Parallel()
	input := strings.NewReader("not json\n" + strings.Repeat(liveUser+"\n", 100))
	var records []localLogRecord
	err := publishLocalTranscript(t.Context(), input, io.Discard, "claude-code", nil, func(_ context.Context, b []localLogRecord) error { records = append(records, b...); return nil })
	if err == nil || input.Len() != 0 {
		t.Fatalf("error/drain: %v %d", err, input.Len())
	}
	gap := false
	for _, r := range records {
		gap = gap || r.Kind == "gap"
		if r.Kind == "message" || r.Kind == "complete" {
			t.Fatalf("published after capture failure: %+v", r)
		}
	}
	if !gap {
		t.Fatal("missing capture gap")
	}
}

func TestLocalTranscriptLargeEOFReplayAndBatchBounds(t *testing.T) {
	t.Parallel()
	var input strings.Builder
	for i := range 100 {
		fmt.Fprintf(&input, "{\"type\":\"user\",\"message\":{\"content\":\"%03d %s\"}}\n", i, strings.Repeat("x", 1000))
	}
	count := 0
	err := publishLocalTranscript(t.Context(), strings.NewReader(input.String()), io.Discard, "claude-code", nil, func(_ context.Context, b []localLogRecord) error {
		size := 0
		for _, r := range b {
			size += len(r.Text) + 32
			if r.Kind == "message" {
				count++
			}
		}
		if size > 65536 {
			t.Errorf("charged batch too large: %d", size)
		}
		return nil
	})
	if err != nil || count != 100 {
		t.Fatalf("EOF lost messages: %d %v", count, err)
	}
}

func TestLocalTranscriptUploadFailureStillDrains(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(liveUser + "\n" + liveAssistant + "\n" + liveResult + "\n")
	failure := errors.New("offline")
	err := publishLocalTranscript(t.Context(), input, io.Discard, "claude-code", nil, func(context.Context, []localLogRecord) error { return failure })
	if !errors.Is(err, failure) || input.Len() != 0 {
		t.Fatalf("upload failure/drain: %v %d", err, input.Len())
	}
}

func TestLocalTranscriptNativeFormats(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pi": `{"type":"session","version":3,"id":"session-1"}
{"type":"message","id":"m1","parentId":null,"timestamp":"2026-03-27T21:00:01Z","message":{"role":"user","content":[{"type":"text","text":"Run tests"}]}}
{"type":"message","id":"m2","parentId":"m1","timestamp":"2026-03-27T21:00:02Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"go test"}}]}}
{"type":"message","id":"m3","parentId":"m2","timestamp":"2026-03-27T21:00:03Z","message":{"role":"toolResult","toolCallId":"t1","toolName":"bash","content":[{"type":"text","text":"Tests passed"}],"isError":false}}
`,
		"codex": `{"timestamp":"2026-03-27T21:00:00Z","type":"session_meta","payload":{"id":"s1"}}
{"timestamp":"2026-03-27T21:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Run tests"}]}}
{"timestamp":"2026-03-27T21:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Running tests"}]}}
{"timestamp":"2026-03-27T21:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test\"}","call_id":"t1"}}
{"timestamp":"2026-03-27T21:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"t1","output":"Tests passed"}}
`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var records []localLogRecord
			err := publishLocalTranscript(t.Context(), strings.NewReader(input), io.Discard, name, nil, func(_ context.Context, b []localLogRecord) error { records = append(records, b...); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 3 || !strings.Contains(records[1].Text, "Tests passed") || records[2].Kind != "complete" {
				t.Fatalf("native conversion: %+v", records)
			}
		})
	}
}

func TestLocalTranscriptRejectsProjectionRewind(t *testing.T) {
	t.Parallel()
	c := localTranscriptConverter{name: "claude-code", started: time.Unix(1, 0)}
	if err := c.append([]byte(liveAssistant)); err != nil {
		t.Fatal(err)
	}
	updates, err := c.updates()
	if err != nil {
		t.Fatal(err)
	}
	c.accept(updates[0])
	c.content = nil
	if _, err := c.updates(); err == nil {
		t.Fatal("a removed message would leave a stale live projection")
	}
}

func TestLocalTranscriptStringContentAndLimits(t *testing.T) {
	t.Parallel()
	c := localTranscriptConverter{name: "claude-code", started: time.Unix(1, 0)}
	if err := c.append([]byte(`{"type":"assistant","message":{"id":"a","content":"plain assistant message"}}`)); err != nil {
		t.Fatal(err)
	}
	updates, err := c.updates()
	if err != nil || len(updates) != 1 || !strings.Contains(updates[0].Text, "plain assistant message") {
		t.Fatalf("string content: %+v %v", updates, err)
	}
	c.content = make([]byte, localTranscriptMaxBytes)
	if c.append([]byte(liveUser)) == nil {
		t.Fatal("capture limit ignored")
	}
}
