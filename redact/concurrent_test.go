package redact

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildJSONLFixture produces roughly targetBytes of transcript-shaped JSONL
// mixing high-entropy base64 (the pathological case for the entropy layer),
// prose, and embedded credentials, so sharded and sequential passes are compared
// on content that actually exercises every layer.
func buildJSONLFixture(t *testing.T, targetBytes int) string {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	var b strings.Builder
	blob := make([]byte, 4096)
	i := 0
	for b.Len() < targetBytes {
		var line []byte
		var err error
		switch i % 4 {
		case 0:
			rng.Read(blob)
			line, err = json.Marshal(map[string]any{
				"type": "response_item",
				"payload": map[string]any{
					"type":              "reasoning",
					"encrypted_content": base64.StdEncoding.EncodeToString(blob),
				},
			})
		case 1:
			line, err = json.Marshal(map[string]any{
				"type":   "function_call_output",
				"output": strings.Repeat("the quick brown fox jumps over the lazy dog ", 60),
			})
		case 2:
			line, err = json.Marshal(map[string]any{
				"type": "message",
				"text": fmt.Sprintf("connect with postgres://user:hunter2@db%d.internal/app and DB_PASSWORD=s3cr3t%d", i, i),
			})
		default:
			// Deliberately not valid JSON, to exercise the redactor fallback path.
			line = []byte(fmt.Sprintf("not json at all, token sk-live-%d abcdefghijklmnop", i))
		}
		require.NoError(t, err)
		b.Write(line)
		b.WriteByte('\n')
		i++
	}
	return b.String()
}

// TestJSONLContentSharded_MatchesSequential is the core guarantee: sharding
// must not change a single byte of output.
func TestJSONLContentSharded_MatchesSequential(t *testing.T) {
	t.Parallel()

	// Comfortably above jsonlConcurrentMinBytes so sharding actually engages.
	content := buildJSONLFixture(t, 4<<20)

	sequential, err := jsonlContentImpl(content, String, concurrencyUnsafeRedactor)
	require.NoError(t, err)

	concurrent, err := jsonlContentImpl(content, String, concurrencySafeRedactor)
	require.NoError(t, err)

	require.Equal(t, sequential, concurrent,
		"sharded redaction must be byte-identical to the sequential pass")
	require.NotEqual(t, content, concurrent, "fixture should have had something redacted")
}

// TestJSONLContentSharded_ShardBoundariesPreserveLines guards the rejoin: an
// off-by-one in the shard split would drop or duplicate a newline.
func TestJSONLContentSharded_ShardBoundariesPreserveLines(t *testing.T) {
	t.Parallel()

	content := buildJSONLFixture(t, 3<<20)
	out, err := jsonlContentImpl(content, String, concurrencySafeRedactor)
	require.NoError(t, err)

	require.Equal(t, strings.Count(content, "\n"), strings.Count(out, "\n"),
		"redaction must preserve the line count exactly")
	require.Equal(t, strings.HasSuffix(content, "\n"), strings.HasSuffix(out, "\n"),
		"trailing-newline state must be preserved")
}

// TestJSONLContentSharded_SmallContentStaysSequential documents that the
// sharding threshold leaves small inputs on the simple path.
func TestJSONLContentSharded_SmallContentStaysSequential(t *testing.T) {
	t.Parallel()

	content := "{\"a\":\"hello\"}\n{\"b\":\"world\"}\n"
	got, err := jsonlContentImpl(content, String, concurrencySafeRedactor)
	require.NoError(t, err)
	want, err := jsonlContentImpl(content, String, concurrencyUnsafeRedactor)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestJSONLContentSharded_EdgeShapes covers inputs where the shard maths or
// the single-JSON-value fast path could misbehave.
func TestJSONLContentSharded_EdgeShapes(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"empty":                "",
		"whitespace only":      "   \n\t\n",
		"single line no eol":   `{"k":"v"}`,
		"blank lines between":  "{\"a\":1}\n\n\n{\"b\":2}\n",
		"pretty single object": "{\n  \"a\": \"value\",\n  \"b\": 2\n}",
		"trailing newlines":    "{\"a\":1}\n\n",
		"no trailing newline":  "{\"a\":1}\n{\"b\":2}",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, err := jsonlContentImpl(content, String, concurrencyUnsafeRedactor)
			require.NoError(t, err)
			got, err := jsonlContentImpl(content, String, concurrencySafeRedactor)
			require.NoError(t, err)
			require.Equal(t, want, got)
		})
	}
}

// TestJSONLContent_ConcurrentCallsAreRaceFree exercises the shared layer config
// and the betterleaks detector from many goroutines. Meaningful under -race.
func TestJSONLContent_ConcurrentCallsAreRaceFree(t *testing.T) {
	t.Parallel()

	content := buildJSONLFixture(t, 2<<20)
	want, err := jsonlContentImpl(content, String, concurrencyUnsafeRedactor)
	require.NoError(t, err)

	const callers = 8
	errs := make(chan error, callers)
	for range callers {
		go func() {
			got, err := JSONLContent(content)
			if err != nil {
				errs <- err
				return
			}
			if got != want {
				errs <- errors.New("concurrent caller produced different output")
				return
			}
			errs <- nil
		}()
	}
	for range callers {
		require.NoError(t, <-errs)
	}
}

// TestJSONLContent_SplitConcatenationEquivalence pins the property the
// checkpoint prefix cache depends on: for newline-terminated A,
// redact(A+B) == redact(A) + redact(B). The cache lives in another package, so
// without this test a refactor here could break it while redact's own suite
// stayed green.
func TestJSONLContent_SplitConcatenationEquivalence(t *testing.T) {
	t.Parallel()

	prefix := buildJSONLFixture(t, 2<<20)
	require.True(t, strings.HasSuffix(prefix, "\n"), "prefix must end on a line boundary")
	suffix := buildJSONLFixture(t, 64<<10)

	whole, err := JSONLContent(prefix + suffix)
	require.NoError(t, err)

	redactedPrefix, err := JSONLContent(prefix)
	require.NoError(t, err)
	redactedSuffix, err := JSONLContent(suffix)
	require.NoError(t, err)

	require.Equal(t, whole, redactedPrefix+redactedSuffix,
		"redact(A+B) must equal redact(A)+redact(B) for newline-terminated A")
}

// TestShardLineBounds_BalancesByBytes covers the shard splitter directly: a few
// huge lines among many small ones must not all land in one shard.
func TestShardLineBounds_BalancesByBytes(t *testing.T) {
	t.Parallel()

	lines := make([]string, 0, 300)
	for i := range 300 {
		if i%100 == 0 {
			lines = append(lines, strings.Repeat("x", 3<<20)) // 3MiB line
			continue
		}
		lines = append(lines, "short")
	}

	bounds := shardLineBounds(lines, 1<<20)
	require.GreaterOrEqual(t, len(bounds), 3, "each oversized line should force a shard cut")

	// Bounds must tile the input exactly, with no gaps or overlaps.
	require.Equal(t, 0, bounds[0].lo)
	require.Equal(t, len(lines), bounds[len(bounds)-1].hi)
	for i := 1; i < len(bounds); i++ {
		require.Equal(t, bounds[i-1].hi, bounds[i].lo, "shard %d must start where %d ended", i, i-1)
	}
}

// TestShardLineBounds_SingleShardForSmallInput keeps small content on the
// sequential path.
func TestShardLineBounds_SingleShardForSmallInput(t *testing.T) {
	t.Parallel()

	bounds := shardLineBounds([]string{"a", "b", "c"}, 1<<20)
	require.Len(t, bounds, 1)
	require.Equal(t, lineRange{lo: 0, hi: 3}, bounds[0])
}

// TestIsLineDelimited covers the predicate callers use to decide whether
// splitting content and redacting the pieces separately is sound.
func TestIsLineDelimited(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		content string
		want    bool
	}{
		"jsonl two objects":       {"{\"a\":1}\n{\"b\":2}\n", true},
		"jsonl single line":       {"{\"a\":1}\n", false},
		"compact single object":   {`{"info":{"id":"x"},"messages":[{"t":"hi"}]}`, false},
		"pretty single object":    {"{\n  \"a\": 1\n}\n", false},
		"single array":            {`[1,2,3]`, false},
		"not json at all":         {"plain text line\nanother line\n", true},
		"jsonl with blank lines":  {"{\"a\":1}\n\n{\"b\":2}\n", true},
		"empty":                   {"", false},
		"whitespace only":         {"  \n\t\n", false},
		"trailing partial object": {"{\"a\":1}\n{\"b\":", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, IsLineDelimited([]byte(tc.content)))
		})
	}
}

// TestIsLineDelimited_AgreesWithRouting pins the predicate to the behaviour it
// describes: whenever it reports false, JSONLContent must have taken the
// single-JSON-value path, and vice versa.
func TestIsLineDelimited_AgreesWithRouting(t *testing.T) {
	t.Parallel()

	for name, content := range map[string]string{
		"jsonl":         "{\"a\":\"x\"}\n{\"b\":\"y\"}\n",
		"single object": "{\n  \"a\": \"x\",\n  \"b\": \"y\"\n}",
		"single line":   "{\"a\":\"x\"}\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, handled, err := redactSingleJSONValue(content, String)
			require.NoError(t, err)
			require.Equal(t, !handled, IsLineDelimited([]byte(content)),
				"IsLineDelimited must mirror the redactor's own routing decision")
		})
	}
}
