package transcript

import (
	"fmt"
	"strings"
	"testing"
)

// benchFragments is one assistant message streamed as n fragments, the shape
// Claude Code writes for a long answer.
func benchFragments(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"user","timestamp":"t0","message":{"content":"go"}}` + "\n")
	text := strings.Repeat("lorem ipsum ", 16)
	for i := range n {
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":"t%d","message":{"id":"m1","content":[{"type":"text","text":"%s %d"}],"usage":{"input_tokens":5,"output_tokens":%d}}}`+"\n", i, text, i, i)
	}
	return []byte(b.String())
}

// benchToolResults is one assistant message with n tool_use fragments followed
// by a single user entry carrying all n results.
func benchToolResults(n int) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[{"type":"tool_use","id":"tu%d","name":"Bash","input":{"command":"echo %d"}}]}}`+"\n", i, i)
	}
	b.WriteString(`{"type":"user","timestamp":"t2","message":{"content":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"tool_result","tool_use_id":"tu%d","content":"%s"}`, i, strings.Repeat("x", 200))
	}
	b.WriteString("]}}\n")
	return []byte(b.String())
}

// benchUnmatchedResults is one assistant entry with n tool_use blocks followed
// by a user entry carrying n results that match none of them.
func benchUnmatchedResults(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"assistant","timestamp":"t1","message":{"id":"m1","content":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"tool_use","id":"tu%d","name":"Bash","input":{"command":"echo %d"}}`, i, i)
	}
	b.WriteString("]}}\n")
	b.WriteString(`{"type":"user","timestamp":"t2","message":{"content":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"type":"tool_result","tool_use_id":"other%d","content":"x"}`, i)
	}
	b.WriteString("]}}\n")
	return []byte(b.String())
}

func BenchmarkJSONLConversion(b *testing.B) {
	inputs := []struct {
		name  string
		input []byte
	}{
		{"fragments-200", benchFragments(200)},
		{"tool-results-100", benchToolResults(100)},
		{"unmatched-results-500", benchUnmatchedResults(500)},
	}
	for _, in := range inputs {
		b.Run(in.name+"/legacy", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := legacyCompactJSONLWith(in.input, defaultOpts, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(in.name+"/records", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := compactJSONLWith(in.input, defaultOpts, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
