package codex

// perf_validation_test.go — SCENARIOS validating the hypotheses in issue #1836
// (Codex checkpoint hooks O(N²) transcript reparse).
//
// These tests/benchmarks are the empirical contract that the root-cause
// analysis is correct BEFORE any fix lands. They are written to PASS against
// the current (slow) implementation: each asserts the buggy behavior exists.
// After the fix, the behavioral guards (H1, H1b) should be UPDATED to assert
// the incremental behavior; the benchmarks let you measure the win.
//
// Hypotheses:
//   H1  — CalculateTokenUsage parses the whole file every hook, regardless of
//         fromOffset (per-hook O(N), whole-session O(N²)).
//   H1b — A naive Claude-style SliceFromLine(offset) breaks Codex numbers,
//         because Codex counts are CUMULATIVE and the delta needs the
//         pre-offset baseline. So the fix must persist the baseline, not just
//         slice.
//   H3  — Codex does NOT implement SubagentAwareExtractor, so turn-end file
//         extraction falls back to the disk-reread path.
//   H4  — encrypted_content reasoning blobs inflate the per-line byte cost of
//         every size-linear step.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/stretchr/testify/require"
)

// tokenCountLine builds an event_msg/token_count line with the given cumulative totals.
func tokenCountLine(input, cached, output int) string {
	return fmt.Sprintf(
		`{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}}}}`,
		input, cached, output, input+output)
}

// fatReasoningLine is a response_item carrying a large encrypted_content blob,
// mirroring real Codex rollouts. It is not a token_count line, so it never
// contributes to the token delta — but the token calc still splits and
// top-level-unmarshals it on every hook.
func fatReasoningLine(blobBytes int) string {
	blob := strings.Repeat("A", blobBytes)
	return `{"timestamp":"t","type":"response_item","payload":{"type":"reasoning","summary":[{"text":"x"}],"encrypted_content":"` + blob + `"}}`
}

// buildRollout returns a synthetic rollout of nTurns turns. Each turn is one
// token_count line with monotonically increasing cumulative counts, optionally
// preceded by a fat reasoning line. Returns the bytes and the total line count.
func buildRollout(nTurns int, withBlobs bool, blobBytes int) ([]byte, int) {
	var b strings.Builder
	b.WriteString(`{"timestamp":"t","type":"session_meta","payload":{"id":"x"}}`)
	b.WriteByte('\n')
	lines := 1
	for i := 1; i <= nTurns; i++ {
		if withBlobs {
			b.WriteString(fatReasoningLine(blobBytes))
			b.WriteByte('\n')
			lines++
		}
		// cumulative: grows every turn
		b.WriteString(tokenCountLine(1000*i, 800*i, 100*i))
		b.WriteByte('\n')
		lines++
	}
	return []byte(b.String()), lines
}

// ---------------------------------------------------------------------------
// H1 — the token calc consults pre-offset lines, proving whole-file reparse.
// ---------------------------------------------------------------------------

// TestValidate_H1_TokenCalcReadsPreOffsetBaseline proves the calc scans lines
// BEFORE fromOffset. We set fromOffset so only the final token_count line is
// "after" it; a correct delta can only be produced by having read the earlier
// (pre-offset) cumulative baseline. If the implementation sliced at the offset
// (touched only new lines), baseline would be nil and the delta would equal the
// full final cumulative value instead of the per-turn delta.
func TestValidate_H1_TokenCalcReadsPreOffsetBaseline(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	// 3 token_count lines at file lines 2, 3, 4 (line 1 is session_meta).
	data, total := buildRollout(3, false, 0)
	require.Equal(t, 4, total)

	// fromOffset=3 → only the line-4 token_count (cumulative {3000,2400,300})
	// is "after"; the line-3 baseline is {2000,1600,200}.
	usage, err := ag.CalculateTokenUsage(data, 3)
	require.NoError(t, err)
	require.NotNil(t, usage)

	// baseline line3 {2000,1600,200}, last line4 {3000,2400,300}.
	// delta = 1000 input, 800 cache, 100 output. fresh input = 1000-800 = 200.
	require.Equal(t, 100, usage.OutputTokens, "output delta uses pre-offset baseline")
	require.Equal(t, 800, usage.CacheReadTokens, "cache delta uses pre-offset baseline")
	require.Equal(t, 200, usage.InputTokens, "fresh input delta uses pre-offset baseline")
	require.Equal(t, 1, usage.APICallCount, "only one token_count line is after the offset")

	// The smoking gun: baseline came from a line BEFORE the offset. A sliced
	// (delta-only) implementation cannot produce these numbers — see H1b.
}

// ---------------------------------------------------------------------------
// H1b — naive Claude-style slicing breaks Codex numbers (baseline lost).
// This validates the fix CONSTRAINT: persist the baseline, don't just slice.
// ---------------------------------------------------------------------------

func TestValidate_H1b_NaiveSliceBreaksCumulativeDelta(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	data, _ := buildRollout(3, false, 0)
	fromOffset := 3

	correct, err := ag.CalculateTokenUsage(data, fromOffset)
	require.NoError(t, err)
	require.NotNil(t, correct)

	// Simulate the Claude-style approach: slice to the checkpoint window, then
	// run the calc from offset 0 on the slice. This is what a naive port would
	// do — and it is WRONG for Codex because the baseline lives before the slice.
	sliced := transcript.SliceFromLine(data, fromOffset)
	naive, err := ag.CalculateTokenUsage(sliced, 0)
	require.NoError(t, err)
	require.NotNil(t, naive)

	// The naive slice has no baseline, so it reports the full cumulative value
	// as this checkpoint's delta — inflated vs correct.
	require.NotEqual(t, correct.OutputTokens, naive.OutputTokens,
		"naive slicing must diverge — proves baseline persistence is required")
	require.Greater(t, naive.OutputTokens, correct.OutputTokens,
		"naive slice over-counts because it lost the cumulative baseline")
	t.Logf("correct delta output=%d, naive-slice output=%d (inflated)",
		correct.OutputTokens, naive.OutputTokens)
}

// ---------------------------------------------------------------------------
// H3 — Codex has no bytes-based subagent-aware extractor → disk-reread path.
// ---------------------------------------------------------------------------

// H3 (post-fix): Codex now exposes a bytes-based file extractor, so lifecycle no
// longer falls back to the disk-reread path. Pre-fix this asserted the opposite;
// see TestCodex_DeclaresBytesCapabilities in incremental_test.go for the guard.
func TestValidate_H3_CodexNowHasBytesExtractor(t *testing.T) {
	t.Parallel()

	_, ok := agent.AsSubagentAwareExtractor(NewCodexAgent())
	require.True(t, ok,
		"Codex must be a SubagentAwareExtractor after the fix — turn-end file "+
			"extraction runs on in-memory bytes instead of a 2nd full-file disk read")
}

// ---------------------------------------------------------------------------
// Benchmarks — empirical proof of the O(N)/O(N²) cost and the blob amplifier.
// Run: go test ./cmd/entire/cli/agent/codex/ -run '^$' -bench BenchmarkValidate -benchmem
// ---------------------------------------------------------------------------

// benchSink keeps benchmark results reachable so the compiler can't elide the
// call under test.
var benchSink *agent.TokenUsage

// H1 per-hook cost: fromOffset pinned to the LAST turn (delta = 1 line). If the
// calc were incremental this would be ~flat across N; today it grows with N.
func BenchmarkValidate_H1_PerHook_LastTurnOnly(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		data, total := buildRollout(n, false, 0)
		offset := total - 1 // only the final token_count is "after"
		b.Run(fmt.Sprintf("turns=%d", n), func(b *testing.B) {
			ag := &CodexAgent{}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				usage, err := ag.CalculateTokenUsage(data, offset)
				if err != nil {
					b.Fatal(err)
				}
				benchSink = usage
			}
		})
	}
}

// H1 whole-session cost: replay every turn-end hook over a growing prefix, as
// the real pipeline does. Total work is the sum of per-hook O(prefix) → O(N²).
func BenchmarkValidate_H1_WholeSession(b *testing.B) {
	for _, n := range []int{200, 1000} {
		data, _ := buildRollout(n, false, 0)
		// Precompute per-turn byte prefixes (line boundaries).
		prefixes := make([][]byte, 0, n)
		nl := 0
		for i, c := range data {
			if c == '\n' {
				nl++
				if nl >= 2 { // after session_meta, snapshot each turn boundary
					prefixes = append(prefixes, data[:i+1])
				}
			}
		}
		b.Run(fmt.Sprintf("turns=%d", n), func(b *testing.B) {
			ag := &CodexAgent{}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				off := 0
				for _, p := range prefixes {
					usage, err := ag.CalculateTokenUsage(p, off)
					if err != nil {
						b.Fatal(err)
					}
					benchSink = usage
					off = strings.Count(string(p), "\n") - 1
				}
			}
		})
	}
}

// H4 constant-factor amplifier: same number of token_count lines, but one
// variant interleaves fat encrypted_content reasoning blobs. Compare ns/op and
// B/op — the blob variant is markedly heavier per hook.
func BenchmarkValidate_H4_EncryptedContentAmplifier(b *testing.B) {
	const turns = 1000
	const blob = 4096 // ~4KB per reasoning line, conservative vs real rollouts

	plain, _ := buildRollout(turns, false, 0)
	fat, _ := buildRollout(turns, true, blob)

	run := func(name string, data []byte) {
		b.Run(name, func(b *testing.B) {
			ag := &CodexAgent{}
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for range b.N {
				usage, err := ag.CalculateTokenUsage(data, 0)
				if err != nil {
					b.Fatal(err)
				}
				benchSink = usage
			}
		})
	}
	run("no_blobs", plain)
	run("with_blobs", fat)
}

// After-fix proof: per-hook incremental cost with a warm baseline stays flat as
// the session grows, vs the full-scan growth in BenchmarkValidate_H1_PerHook.
func BenchmarkValidate_Fixed_IncrementalPerHook(b *testing.B) {
	ag := &CodexAgent{}
	for _, n := range []int{100, 1000, 10000} {
		data, _ := buildRollout(n, false, 0)
		prev, _ := buildRollout(n-1, false, 0)
		fromOffset := lineCount(prev)
		_, prior, err := ag.CalculateTokenUsageIncremental(prev, lineCount(prev)-1, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("turns=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				usage, _, incErr := ag.CalculateTokenUsageIncremental(data, fromOffset, prior)
				if incErr != nil {
					b.Fatal(incErr)
				}
				benchSink = usage
			}
		})
	}
}
