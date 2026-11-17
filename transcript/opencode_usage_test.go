package transcript

import (
	"bytes"
	"fmt"
	"testing"
)

func TestCompact_OpenCodeUsageLayouts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tokens string
		output int64
	}{
		{"separate reasoning", `"total":137,`, 120},
		{"included reasoning", `"total":97,`, 80},
		{"missing total", "", 120},
		{"unrecognized total", `"total":999,`, 120},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := fmt.Appendf(nil, `{"info":{"id":"s"},"messages":[{"info":{"id":"a","role":"assistant","modelID":"model-a","tokens":{%s"input":10,"output":80,"reasoning":40,"cache":{"read":5,"write":2}}},"parts":[{"type":"text","text":"reply"}]}]}`, tt.tokens)
			got, err := Convert(input, agentOpts("opencode"))
			if err != nil {
				t.Fatal(err)
			}
			lines, err := Decode(got)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1", len(lines))
			}
			line := lines[0]
			if line.Model != "model-a" || line.InputTokens != 10 || line.OutputTokens != tt.output || line.ReasoningTokens != 40 || line.CacheReadTokens != 5 || line.CacheWriteTokens != 2 {
				t.Fatalf("usage = %+v, want input=10 output=%d reasoning=40 cache=5/2 model-a", line, tt.output)
			}
			if line.CacheWrite5mTokens != nil || line.CacheWrite1hTokens != nil {
				t.Fatal("invented an unreported cache TTL split")
			}
		})
	}
}

func TestCompact_OpenCodeUsageWithoutContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, tokens           string
		read, write, reasoning int64
		visible                bool
	}{
		{"cache read", `{"cache":{"read":50}}`, 50, 0, 0, true},
		{"cache write", `{"cache":{"write":20}}`, 0, 20, 0, true},
		{"reasoning", `{"reasoning":40}`, 0, 0, 40, true},
		{"zero usage", `{}`, 0, 0, 0, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			input := fmt.Appendf(nil, `{"info":{"id":"s"},"messages":[{"info":{"id":"a","role":"assistant","modelID":"model-a","tokens":%s},"parts":[]}]}`, tt.tokens)
			got, err := Convert(input, agentOpts("opencode"))
			if err != nil {
				t.Fatal(err)
			}
			if !tt.visible {
				if len(got) != 0 {
					t.Fatalf("zero-usage message visible: %s", got)
				}
				return
			}
			if !bytes.Contains(got, []byte(`"content":null`)) {
				t.Fatalf("usage-only line missing: %s", got)
			}
			lines, err := Decode(got)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 {
				t.Fatalf("got %d lines, want 1", len(lines))
			}
			line := lines[0]
			if line.Model != "model-a" || line.CacheReadTokens != tt.read || line.CacheWriteTokens != tt.write || line.ReasoningTokens != tt.reasoning || line.OutputTokens != tt.reasoning {
				t.Fatalf("usage lost: %+v", line)
			}
			adapter := NewOpenCodeAdapter()
			records, err := adapter.FeedExport(input)
			if err != nil {
				t.Fatal(err)
			}
			folded, err := Fold(records, agentOpts("opencode"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(folded, got) {
				t.Fatalf("stream and conversion differ:\n%s\n%s", folded, got)
			}
		})
	}
}

func TestOpenCodeAdapter_UsageSnapshotReplacementV2(t *testing.T) {
	t.Parallel()
	adapter := NewOpenCodeAdapter()
	folder := NewFolder(agentOpts("opencode"))
	// The first snapshot has no visible content. Usage then arrives, a
	// duplicate export changes nothing, and the next snapshot clears cache
	// counts while changing the model. These are snapshots, not deltas.
	steps := []struct {
		model, tokens                  string
		output, read, write, reasoning int64
		visible, changed               bool
	}{
		{"model-a", `{}`, 0, 0, 0, 0, false, true},
		{"model-a", `{"input":10,"output":80,"reasoning":40,"cache":{"read":5,"write":2}}`, 120, 5, 2, 40, true, true},
		{"model-a", `{"input":10,"output":80,"reasoning":40,"cache":{"read":5,"write":2}}`, 120, 5, 2, 40, true, false},
		{"model-b", `{"input":10,"output":5}`, 5, 0, 0, 0, true, true},
	}
	for i, step := range steps {
		export := fmt.Appendf(nil, `{"info":{"id":"s","location":{"directory":"/repo"}},"messages":[{"id":"a","type":"assistant","model":{"id":%q,"providerID":"test"},"tokens":%s,"content":[]}]}`, step.model, step.tokens)
		records, err := adapter.FeedExport(export)
		if err != nil {
			t.Fatal(err)
		}
		if (len(records) > 0) != step.changed {
			t.Fatalf("step %d: unexpected records: %+v", i, records)
		}
		if _, err := folder.Apply(records); err != nil {
			t.Fatal(err)
		}
		raw, visible := folder.Line("a")
		if visible != step.visible {
			t.Fatalf("step %d visible=%v, want %v", i, visible, step.visible)
		}
		converted, err := Convert(export, agentOpts("opencode"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(converted)) {
			t.Fatalf("step %d differs from Convert", i)
		}
		if !visible {
			continue
		}
		lines, err := Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 1 {
			t.Fatalf("step %d: got %d lines", i, len(lines))
		}
		line := lines[0]
		if line.Model != step.model || line.InputTokens != 10 || line.OutputTokens != step.output || line.CacheReadTokens != step.read || line.CacheWriteTokens != step.write || line.ReasoningTokens != step.reasoning {
			t.Fatalf("step %d: incorrect usage snapshot: %+v", i, line)
		}
	}
}
