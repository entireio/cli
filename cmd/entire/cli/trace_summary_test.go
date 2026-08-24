package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func slowEntry(op string, dur int64, steps ...traceStep) traceEntry {
	return traceEntry{Op: op, DurationMs: dur, Slow: true, Steps: steps}
}

func step(name string, ms int64, subs ...traceStep) traceStep {
	return traceStep{Name: name, DurationMs: ms, SubSteps: subs}
}

// TestSummarizeTraces_DominanceIsTimeWeighted is the property that makes the
// summary trustworthy. A frequency count ties a 3900ms offender with a 150ms one
// and then resolves alphabetically, which names the wrong step on the row whose
// max is 4200ms.
func TestSummarizeTraces_DominanceIsTimeWeighted(t *testing.T) {
	t.Parallel()

	entries := []traceEntry{
		slowEntry("stop", 4200, step("write_temporary_checkpoint", 3900), step("detect_file_changes", 120)),
		{Op: "stop", DurationMs: 320, Steps: []traceStep{step("write_temporary_checkpoint", 90), step("detect_file_changes", 150)}},
	}

	s := summarizeTraces(entries)
	require.Len(t, s.Ops, 1)
	require.Equal(t, "write_temporary_checkpoint", s.Ops[0].Dominant,
		"per-hook dominance must follow time, not how many traces name a step")
	require.Equal(t, 2, s.Ops[0].Count)
	require.Equal(t, 1, s.Ops[0].Slow)
	require.Equal(t, int64(4200), s.Ops[0].MaxMs)

	// The global table ranks by total time, so the heaviest step leads. TotalMs
	// counts time accrued *while dominant*, which is what keeps it coherent with
	// Count (= number of traces where the step was the culprit): the fast trace's
	// 90ms is attributed to detect_file_changes, which dominated there.
	require.Equal(t, "write_temporary_checkpoint", s.StepCounts[0].Step)
	require.Equal(t, int64(3900), s.StepCounts[0].TotalMs)
	byStep := map[string]traceStepCount{}
	for _, sc := range s.StepCounts {
		byStep[sc.Step] = sc
	}
	require.Equal(t, int64(150), byStep["detect_file_changes"].TotalMs,
		"the fast trace's dominant step carries that trace's time")
}

// TestSummarizeTraces_DominanceLooksInsideNestedSteps guards the tree walk: the
// top-level step is often a wrapper, so the culprit can be a child.
func TestSummarizeTraces_DominanceLooksInsideNestedSteps(t *testing.T) {
	t.Parallel()

	entries := []traceEntry{
		slowEntry("post-commit", 5000,
			step("process_sessions", 4800,
				step("process_sessions.0", 200),
				step("process_sessions.1", 4500),
			),
		),
	}
	s := summarizeTraces(entries)
	require.Equal(t, "process_sessions", s.Ops[0].Dominant,
		"the parent owns 4800ms, more than any single child")

	nested := []traceEntry{
		slowEntry("stop", 5000, step("wrapper", 100), step("outer", 900, step("inner", 850))),
	}
	require.Equal(t, "outer", summarizeTraces(nested).Ops[0].Dominant)
}

// TestSummarizeTraces_StableOrderAcrossRuns pins tie-breaking so output does not
// depend on Go's map iteration order.
func TestSummarizeTraces_StableOrderAcrossRuns(t *testing.T) {
	t.Parallel()

	entries := []traceEntry{
		slowEntry("aaa", 100, step("s_b", 50)),
		slowEntry("bbb", 100, step("s_a", 50)),
	}
	first := summarizeTraces(entries)
	for range 12 {
		got := summarizeTraces(entries)
		require.Equal(t, first, got, "equal counts and totals must break ties deterministically")
	}
	require.Equal(t, "aaa", first.Ops[0].Op, "equal counts tie-break by hook name")
	require.Equal(t, "s_a", first.StepCounts[0].Step, "equal totals tie-break by step name")
}

// TestSummarizeTraces_EmptyAndStepless covers shapes that must not panic.
func TestSummarizeTraces_EmptyAndStepless(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, summarizeTraces(nil).Total)

	s := summarizeTraces([]traceEntry{{Op: "stop", DurationMs: 10}})
	require.Equal(t, 1, s.Total)
	require.Empty(t, s.StepCounts, "an entry with no steps contributes no dominant step")
	require.Empty(t, s.Ops[0].Dominant)
}

// TestRenderTraceSummary_EmptyExplainsHowTracesAreEmitted keeps the empty state
// useful rather than silent, and shares its wording with the per-entry renderer.
func TestRenderTraceSummary_EmptyExplainsHowTracesAreEmitted(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	renderTraceSummary(&b, summarizeTraces(nil))
	out := b.String()
	require.Contains(t, out, "No trace entries found.")
	require.Contains(t, out, "traced at WARN")
	require.Contains(t, out, "ENTIRE_LOG_LEVEL=DEBUG")
}

// TestRenderTraceJSON_IsValidAndCarriesSlow pins the machine-readable contract
// agents rely on.
func TestRenderTraceJSON_IsValidAndCarriesSlow(t *testing.T) {
	t.Parallel()

	var b bytes.Buffer
	require.NoError(t, renderTraceJSON(&b, []traceEntry{
		slowEntry("stop", 4200, step("write_temporary_checkpoint", 3900)),
	}))

	var got []map[string]any
	require.NoError(t, json.Unmarshal(b.Bytes(), &got))
	require.Len(t, got, 1)
	require.Equal(t, "stop", got[0]["op"])
	require.Equal(t, true, got[0]["slow"])
	require.InDelta(t, 4200, got[0]["duration_ms"], 0)

	// Empty input must still be a JSON array, not "null" — callers pipe this to jq.
	var empty bytes.Buffer
	require.NoError(t, renderTraceJSON(&empty, nil))
	require.Equal(t, "[]", strings.TrimSpace(empty.String()))
}

// TestParseTraceEntry_ReadsSlowMarker ties the parser to what #1984 writes.
func TestParseTraceEntry_ReadsSlowMarker(t *testing.T) {
	t.Parallel()

	e := parseTraceEntry(`{"msg":"perf","op":"stop","duration_ms":4200,"slow":true,"steps.a_ms":10}`)
	require.NotNil(t, e)
	require.True(t, e.Slow)

	e = parseTraceEntry(`{"msg":"perf","op":"stop","duration_ms":42,"steps.a_ms":10}`)
	require.NotNil(t, e)
	require.False(t, e.Slow, "absent slow key means not slow")
}

// TestCollectTraceEntries_SlowFilterAppliesBeforeTruncation pins the ordering of
// the two filters. Filtering slow entries after the last-N cut returns "the slow
// traces among the last N", which under DEBUG logging is usually none — so
// --slow would report no traces while hundreds sat in the log.
func TestCollectTraceEntries_SlowFilterAppliesBeforeTruncation(t *testing.T) {
	t.Parallel()

	// Two slow traces, then three fast ones on top of them.
	lines := []string{
		`{"time":"2026-01-15T10:00:00Z","level":"WARN","msg":"perf","op":"stop","duration_ms":4000,"slow":true}`,
		`{"time":"2026-01-15T10:01:00Z","level":"WARN","msg":"perf","op":"stop","duration_ms":5000,"slow":true}`,
		`{"time":"2026-01-15T10:02:00Z","level":"DEBUG","msg":"perf","op":"stop","duration_ms":10}`,
		`{"time":"2026-01-15T10:03:00Z","level":"DEBUG","msg":"perf","op":"stop","duration_ms":11}`,
		`{"time":"2026-01-15T10:04:00Z","level":"DEBUG","msg":"perf","op":"stop","duration_ms":12}`,
	}
	logFile := filepath.Join(t.TempDir(), "trace.jsonl")
	require.NoError(t, os.WriteFile(logFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	// --last 2 --slow must mean "the last 2 slow traces", not "the slow ones
	// among the last 2 traces" (which would be empty, since both are fast).
	entries, err := collectTraceEntries(logFile, 2, "", true)
	require.NoError(t, err)
	require.Len(t, entries, 2, "slow entries must survive the last-N window")
	require.Equal(t, int64(5000), entries[0].DurationMs, "newest slow trace first")
	require.Equal(t, int64(4000), entries[1].DurationMs)

	// Without --slow the same window is the three most recent, all fast.
	all, err := collectTraceEntries(logFile, 3, "", false)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for _, e := range all {
		require.False(t, e.Slow)
	}
}

// TestPercentileMs_NearestRank guards the round-numbered cases, where truncating
// the rank overshoots by a full sample and makes P90 a duplicate of MAX.
func TestPercentileMs_NearestRank(t *testing.T) {
	t.Parallel()

	ten := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	require.Equal(t, int64(9), percentileMs(ten, 90), "p90 of 10 samples is the 9th, not the max")
	require.Equal(t, int64(5), percentileMs(ten, 50), "p50 of 10 samples is the 5th")
	require.Equal(t, int64(10), percentileMs(ten, 100))
	require.Equal(t, int64(1), percentileMs(ten, 1), "a percentile below one rank still lands on the first sample")

	require.Equal(t, int64(7), percentileMs([]int64{7}, 50), "p50 of one sample is that sample")
	require.Equal(t, int64(7), percentileMs([]int64{7}, 90))
	require.Equal(t, int64(1), percentileMs([]int64{1, 2}, 50), "p50 of an even count takes the lower median")
	require.Equal(t, int64(0), percentileMs(nil, 50))
}

// TestRenderTraceJSON_OmitsUnsetTime keeps the JSON honest: the parser tolerates
// a missing time key, and a consumer should see its absence rather than a
// year-1 timestamp that was never recorded.
func TestRenderTraceJSON_OmitsUnsetTime(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, renderTraceJSON(&buf, []traceEntry{{Op: "stop", DurationMs: 12}}))

	require.NotContains(t, buf.String(), "0001-01-01")

	var decoded []map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded, 1)
	_, hasTime := decoded[0]["time"]
	require.False(t, hasTime, "an unrecorded time must be absent, not zero")
}
