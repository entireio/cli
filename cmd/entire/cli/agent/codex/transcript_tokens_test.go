package codex

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests pin CalculateTokenUsage's observable behaviour so the byte-prefilter
// rewrite can be proven to change only how much work happens, never the numbers.
// They were written against the pre-rewrite implementation and pass unchanged
// against both.

const (
	tokenCountA = `{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5000,"cached_input_tokens":4000,"output_tokens":100,"total_tokens":5100}}}}`
	tokenCountB = `{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10000,"cached_input_tokens":8000,"output_tokens":200,"total_tokens":10200}}}}`
	sessionMeta = `{"timestamp":"t","type":"session_meta","payload":{"id":"x","cwd":"/tmp/repo"}}`
	userMessage = `{"timestamp":"t","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`

	// A real event_msg of another type whose text merely mentions token_count.
	// Any session discussing token accounting emits one of these.
	decoyMentioningTokenCount = `{"timestamp":"t","type":"event_msg","payload":{"type":"agent_message","message":"the token_count event carries cumulative totals"}}`

	// A reasoning blob. Codex writes encrypted_content as URL-safe base64, whose
	// alphabet includes '_', so the literal token_count can appear inside one.
	blobMentioningTokenCount = `{"timestamp":"t","type":"response_item","payload":{"type":"reasoning","encrypted_content":"QUJDRA_token_count_RUZHSA"}}`
)

func rollout(lines ...string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// syntheticRollout builds a rollout of the given number of turns. Each turn is a
// user message, blobsPerTurn reasoning lines carrying a blobKB payload, and one
// cumulative token_count — the shape that made the old whole-file unmarshal
// expensive.
func syntheticRollout(turns, blobsPerTurn, blobKB int) []byte {
	blob := strings.Repeat("QUJDRE_VGdoSWpLbE1uT3BRclN0VXZXeFla", blobKB*1024/35)
	var b strings.Builder
	b.WriteString(sessionMeta + "\n")
	for i := 1; i <= turns; i++ {
		b.WriteString(userMessage + "\n")
		for range blobsPerTurn {
			fmt.Fprintf(&b, `{"timestamp":"t","type":"response_item","payload":{"type":"reasoning","encrypted_content":%q}}`+"\n", blob)
		}
		fmt.Fprintf(&b,
			`{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}}`+"\n",
			i*5000, i*4000, i*100, i*5100)
	}
	return []byte(b.String())
}

// TestCalculateTokenUsage_ParseWorkTracksEventsNotTranscriptSize is the
// regression gate for the performance fix. It counts unmarshals rather than
// wall-clock, so it is deterministic and safe in CI.
//
// The bound has to be tight enough to distinguish "skipped the blob lines" from
// "parsed everything": with several lines per turn, a whole-file parser is also
// linear in turns, so a loose ratio between two session lengths would pass for
// both. Pinning parses at one per token_count event, and asserting the fixture
// holds several times more lines than that, is what makes the gate meaningful.
func TestCalculateTokenUsage_ParseWorkTracksEventsNotTranscriptSize(t *testing.T) {
	t.Parallel()

	for _, turns := range []int{10, 100, 1000} {
		t.Run(fmt.Sprintf("turns_%d", turns), func(t *testing.T) {
			t.Parallel()
			data := syntheticRollout(turns, 3, 1)
			totalLines := len(splitJSONL(data))

			parses := 0
			usage, err := calculateTokenUsage(data, totalLines-3, func() { parses++ })
			require.NoError(t, err)
			require.NotNil(t, usage)

			// One token_count per turn, plus slack for the marker appearing anywhere else.
			require.LessOrEqualf(t, parses, turns+2,
				"parsed %d lines for %d turns — blob lines are reaching the unmarshal", parses, turns)
			// Without this the bound above would also hold for a parser that read
			// every line, since the fixture's line count is itself linear in turns.
			require.Greaterf(t, totalLines, 3*(turns+2),
				"fixture too thin to prove anything: %d lines for a bound of %d", totalLines, turns+2)
		})
	}
}

// BenchmarkCalculateTokenUsage measures a single turn-end hook against sessions
// of growing length — the shape of the original complaint, where late-turn hooks
// felt like a hang. Reported for the record; the assertion above is the gate.
func BenchmarkCalculateTokenUsage(b *testing.B) {
	for _, turns := range []int{50, 200, 500} {
		b.Run(fmt.Sprintf("turns_%d", turns), func(b *testing.B) {
			ag := &CodexAgent{}
			data := syntheticRollout(turns, 4, 8)
			from := len(splitJSONL(data)) - 4
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := ag.CalculateTokenUsage(data, from); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestCalculateTokenUsage_EveryOffset pins the full offset sweep over the shared
// sampleRollout fixture, including both ends and past EOF. sampleRollout has 12
// non-empty lines with token_count events on lines 4, 8 and 11.
func TestCalculateTokenUsage_EveryOffset(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	type want struct {
		fresh, cache, out, calls int
		nilUsage                 bool
	}
	// Offsets 0-3 precede every token_count, so they share the no-baseline result;
	// 4-7 baseline on line 4; 8-10 baseline on line 8; from 11 on there is no
	// token_count after the offset at all, so the nil, nil contract applies.
	cases := map[int]want{
		0:  {fresh: 3000, cache: 12000, out: 300, calls: 3},
		1:  {fresh: 3000, cache: 12000, out: 300, calls: 3},
		3:  {fresh: 3000, cache: 12000, out: 300, calls: 3},
		4:  {fresh: 2000, cache: 8000, out: 200, calls: 2},
		7:  {fresh: 2000, cache: 8000, out: 200, calls: 2},
		8:  {fresh: 1000, cache: 4000, out: 100, calls: 1},
		10: {fresh: 1000, cache: 4000, out: 100, calls: 1},
		11: {nilUsage: true},
		12: {nilUsage: true},
		99: {nilUsage: true}, // past EOF
	}

	for offset, exp := range cases {
		t.Run(fmt.Sprintf("offset_%d", offset), func(t *testing.T) {
			t.Parallel()
			usage, err := ag.CalculateTokenUsage([]byte(sampleRollout), offset)
			require.NoError(t, err)
			if exp.nilUsage {
				require.Nil(t, usage)
				return
			}
			require.NotNil(t, usage)
			require.Equal(t, exp.fresh, usage.InputTokens, "fresh input")
			require.Equal(t, exp.cache, usage.CacheReadTokens, "cache read")
			require.Equal(t, exp.out, usage.OutputTokens, "output")
			require.Equal(t, exp.calls, usage.APICallCount, "api calls")
		})
	}
}

// TestCalculateTokenUsage_BlankLinesDoNotShiftNumbering is the line-numbering
// parity test. Offsets are in splitJSONL coordinates, which count only non-empty
// lines, so padding the transcript with blank and whitespace-only lines must not
// move the boundary. A rewrite that counted physical newlines instead would
// silently pick the wrong baseline.
func TestCalculateTokenUsage_BlankLinesDoNotShiftNumbering(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	dense := rollout(sessionMeta, tokenCountA, userMessage, tokenCountB)
	padded := rollout(sessionMeta, "", tokenCountA, "   ", "\t", userMessage, "", tokenCountB)

	for offset := range 6 {
		denseUsage, err := ag.CalculateTokenUsage(dense, offset)
		require.NoError(t, err)
		paddedUsage, err := ag.CalculateTokenUsage(padded, offset)
		require.NoError(t, err)
		require.Equal(t, denseUsage, paddedUsage, "offset %d: blank lines shifted the boundary", offset)
	}
}

// TestCalculateTokenUsage_DecoyDoesNotDisplaceBaseline guards the byte prefilter.
// Lines that merely contain the literal token_count must be rejected by the
// envelope type check, not mistaken for the baseline. Getting this wrong loses
// the baseline entirely and silently doubles the reported numbers.
func TestCalculateTokenUsage_DecoyDoesNotDisplaceBaseline(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	for name, decoy := range map[string]string{
		"event_msg_of_another_type": decoyMentioningTokenCount,
		"reasoning_blob":            blobMentioningTokenCount,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// Boundary at line 3, so the baseline is tokenCountA on line 2 and the
			// decoy on line 3 sits between it and the boundary.
			data := rollout(sessionMeta, tokenCountA, decoy, tokenCountB)

			usage, err := ag.CalculateTokenUsage(data, 3)
			require.NoError(t, err)
			require.NotNil(t, usage)

			// B - A = {in 5000, cache 4000, out 100} → fresh 1000.
			require.Equal(t, 1000, usage.InputTokens, "baseline was displaced")
			require.Equal(t, 4000, usage.CacheReadTokens)
			require.Equal(t, 100, usage.OutputTokens)
			require.Equal(t, 1, usage.APICallCount, "decoy counted as an API call")
		})
	}
}

// TestCalculateTokenUsage_MalformedLinesIgnored pins tolerance: a truncated or
// non-JSON line is skipped rather than failing the hook.
func TestCalculateTokenUsage_MalformedLinesIgnored(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	data := rollout(
		sessionMeta,
		tokenCountA,
		`{"type":"event_msg","payload":{"type":"token_`, // truncated mid-token
		`not json at all`,
		tokenCountB,
		`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":`, // truncated tail
	)

	usage, err := ag.CalculateTokenUsage(data, 2)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 1000, usage.InputTokens)
	require.Equal(t, 4000, usage.CacheReadTokens)
	require.Equal(t, 100, usage.OutputTokens)
	require.Equal(t, 1, usage.APICallCount)
}

// TestCalculateTokenUsage_BaselineAboveLastStaysUnclamped documents a deliberate
// asymmetry in the current accounting: only fresh input tokens are floored at
// zero. When cumulative totals go backwards — a session reset or compaction —
// cache-read and output deltas stay negative. The rewrite must not start
// clamping them.
func TestCalculateTokenUsage_BaselineAboveLastStaysUnclamped(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}

	// Baseline is the larger B on line 2; the last value is the smaller A.
	data := rollout(sessionMeta, tokenCountB, userMessage, tokenCountA)

	usage, err := ag.CalculateTokenUsage(data, 2)
	require.NoError(t, err)
	require.NotNil(t, usage)

	// in = 5000-10000 = -5000, cache = 4000-8000 = -4000,
	// fresh = -5000 - (-4000) = -1000 → floored to 0. out = 100-200 = -100.
	require.Equal(t, 0, usage.InputTokens, "fresh input must be floored at zero")
	require.Equal(t, -4000, usage.CacheReadTokens, "cache read must stay unclamped")
	require.Equal(t, -100, usage.OutputTokens, "output must stay unclamped")
	require.Equal(t, 1, usage.APICallCount)
}
