package transcript

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// codexAdapterRollout is codexItemsRollout followed by a second turn that
// switches model, has a call with no item at all (its usage-only line is
// keyed by its closing line), and a counter decrease that stops usage.
var codexAdapterRollout = codexItemsRollout + `{"timestamp":"2030-01-01T00:01:00.000Z","type":"event_msg","payload":{"type":"task_started","turn_id":"t2"}}
{"timestamp":"2030-01-01T00:01:00.000Z","type":"turn_context","payload":{"turn_id":"t2","model":"model-2"}}
{"timestamp":"2030-01-01T00:01:01.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"UserMessage","id":"u2","content":[{"type":"text","text":"think"}]}}}
{"timestamp":"2030-01-01T00:01:02.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":300,"cached_input_tokens":170,"output_tokens":20,"reasoning_output_tokens":6}}}}
{"timestamp":"2030-01-01T00:01:03.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"AgentMessage","id":"m3","content":[{"type":"Text","text":"Thought."}]}}}
{"timestamp":"2030-01-01T00:01:04.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"cached_input_tokens":100,"output_tokens":5,"reasoning_output_tokens":1}}}}
{"timestamp":"2030-01-01T00:01:05.000Z","type":"event_msg","payload":{"type":"item_completed","turn_id":"t2","item":{"type":"AgentMessage","id":"m4","content":[{"type":"Text","text":"Again."}]}}}
{"timestamp":"2030-01-01T00:01:06.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":400,"cached_input_tokens":200,"output_tokens":30,"reasoning_output_tokens":8}}}}
{"timestamp":"2030-01-01T00:01:07.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t2"}}
`

// feedCodexAdapter feeds content to a fresh adapter in chunks of size (the
// whole content when size is 0) and returns every record.
func feedCodexAdapter(content []byte, size int) []Record {
	a := NewCodexRolloutAdapter()
	if size == 0 {
		return a.Feed(content)
	}
	var records []Record
	for len(content) > 0 {
		n := min(size, len(content))
		records = append(records, a.Feed(content[:n])...)
		content = content[n:]
	}
	return records
}

func foldCodex(t *testing.T, records []Record) []byte {
	t.Helper()
	f := NewFolder(codexItemsOpts)
	if _, err := f.Apply(records); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return f.Lines()
}

// usageOnlyLineID is the key of the line-keyed usage-only record, or "".
func usageOnlyLineID(records []Record) string {
	for _, r := range records {
		if r.Op == OpUsage && strings.HasPrefix(r.ID, "codex-call:line-") {
			return r.ID
		}
	}
	return ""
}

func TestCodexRolloutAdapterFoldEqualsConvert(t *testing.T) {
	t.Parallel()
	content := []byte(codexAdapterRollout)
	want, err := Convert(content, codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	// The fixture exercises what it claims: a model switch, a line-keyed
	// usage-only line, and usage stopped by the decrease.
	for _, s := range []string{`"model":"model-2"`, `"content":null`, `"id":"m4","content"`} {
		if !strings.Contains(string(want), s) {
			t.Fatalf("Convert's output lacks %s:\n%s", s, want)
		}
	}
	for _, size := range []int{1, 7, 64, 0} {
		records := feedCodexAdapter(content, size)
		if got := foldCodex(t, records); !bytes.Equal(got, want) {
			t.Errorf("chunks of %d: fold differs from Convert\n got %s\nwant %s", size, got, want)
		}
		if id := usageOnlyLineID(records); id != "codex-call:line-15" {
			t.Errorf("chunks of %d: usage-only key = %q, want codex-call:line-15", size, id)
		}
	}
}

// A chunk that ends mid-line yields nothing for that line until its newline
// arrives.
func TestCodexRolloutAdapterPartialLine(t *testing.T) {
	t.Parallel()
	lines := strings.SplitAfter(codexItemsRollout, "\n")
	head := strings.Join(lines[:4], "")
	user := lines[4] // the UserMessage item
	a := NewCodexRolloutAdapter()
	if recs := a.Feed([]byte(head)); len(recs) != 0 {
		t.Fatalf("head gave %d records, want 0", len(recs))
	}
	cut := len(user) / 2
	if recs := a.Feed([]byte(user[:cut])); len(recs) != 0 {
		t.Fatalf("half a line gave %d records, want 0", len(recs))
	}
	if recs := a.Feed([]byte(strings.TrimSuffix(user[cut:], "\n"))); len(recs) != 0 {
		t.Fatalf("a line without its newline gave %d records, want 0", len(recs))
	}
	recs := a.Feed([]byte("\n"))
	if len(recs) != 1 || recs[0].ID != "u1" {
		t.Fatalf("the newline gave %+v, want the u1 record", recs)
	}
}

// An empty line (sessionsource feeds one in place of an oversized line) still
// counts, so later line-keyed records match Convert's numbering.
func TestCodexRolloutAdapterCountsEmptyLines(t *testing.T) {
	t.Parallel()
	lines := strings.SplitAfter(codexAdapterRollout, "\n")
	// An empty line before the second turn's user message.
	content := []byte(strings.Join(lines[:13], "") + "\n" + strings.Join(lines[13:], ""))
	want, err := Convert(content, codexItemsOpts)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	records := feedCodexAdapter(content, 0)
	if got := foldCodex(t, records); !bytes.Equal(got, want) {
		t.Errorf("fold differs from Convert\n got %s\nwant %s", got, want)
	}
	if id, wantID := usageOnlyLineID(records), fmt.Sprintf("codex-call:line-%d", 16); id != wantID {
		t.Errorf("usage-only key = %q, want %s", id, wantID)
	}
}
