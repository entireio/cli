package redact

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the post-splice verification. applyJSONReplacements matches
// the Go re-encoding of each leaf against the producer's raw bytes, and JSON
// admits more than one encoding of the same string, so a producer's encoding
// choice used to leave a flagged leaf unredacted with no signal. Each case
// here asserts that redaction lands regardless of which path (splice or
// structural re-encode) produced the output.

func TestJSONLContent_RedactsLeafWithRawLineSeparator(t *testing.T) {
	t.Parallel()

	// The leaf contains a raw U+2028. Go's encoder escapes U+2028 even with
	// HTML escaping off, so the splice's search key never matches the raw byte.
	line := "{\"text\":\"before \u2028 " + highEntropySecret + "\"}"

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, RedactedPlaceholder)
}

func TestJSONLContent_RedactsLeafWithEscapedSolidus(t *testing.T) {
	t.Parallel()

	// \/ decodes to / but Go never re-encodes / as \/, so the search key
	// cannot match the producer's spelling.
	line := `{"text":"path\/to ` + highEntropySecret + `"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, RedactedPlaceholder)
}

func TestJSONLContent_RedactsLeafWithEscapedASCII(t *testing.T) {
	t.Parallel()

	// The secret's leading byte is spelled as a unicode escape of a plain
	// ASCII letter, the shape a strict ensure_ascii-style producer can emit
	// for any character.
	line := `{"text":"\u0073` + highEntropySecret[1:] + `"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, RedactedPlaceholder)
}

func TestJSONLContent_RedactsLeafWithEscapedNonASCII(t *testing.T) {
	t.Parallel()

	// The escaped non-ASCII rune decodes to a character Go re-encodes as raw
	// UTF-8 bytes, so the search key never matches the escaped spelling.
	line := `{"text":"caf\u00e9 ` + highEntropySecret + `"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, RedactedPlaceholder)
}

func TestJSONLContent_RedactsDuplicatedLeafWhenOneCopyIsVariantEncoded(t *testing.T) {
	t.Parallel()

	// Both leaves decode to the same value, but only one copy is spelled the
	// way Go would re-encode it. The splice lands on that one and misses the
	// other, so the verification must send the whole line to the structural
	// fallback.
	line := `{"a":"x/y ` + highEntropySecret + `","b":"x\/y ` + highEntropySecret + `"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
}

func TestJSONLContent_RedactsKeyedCredentialWithVariantEncoding(t *testing.T) {
	t.Parallel()

	// The password decodes to a value the credential-context rule maps to the
	// placeholder under a password key. The keyed splice misses the producer's
	// escaped spelling, so the fallback must apply the same rule.
	line := `{"host":"db.example.com","username":"svc","password":"hunter\u0032"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, "hunter")
	assert.Contains(t, got, `"password":"REDACTED"`)
}

func TestJSONLContent_RedactsSecretShadowedByDuplicateKey(t *testing.T) {
	t.Parallel()

	// encoding/json keeps only the LAST duplicate key, so the secret in the
	// first "text" is invisible to the decoded tree: nothing is collected and
	// the verification has nothing to check. The line must be rebuilt
	// structurally, which drops the shadowed value outright.
	line := `{"text":"` + highEntropySecret + `","text":"safe"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, `"text":"safe"`)
}

func TestJSONLContent_RedactsSecretShadowedByNestedDuplicateKey(t *testing.T) {
	t.Parallel()

	line := `{"outer":[{"k":"` + highEntropySecret + `","k":"safe"}],"n":1}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
}

func TestJSONLContent_SingleJSONValueDuplicateKeyIsRebuilt(t *testing.T) {
	t.Parallel()

	content := "{\n  \"text\": \"" + highEntropySecret + "\",\n  \"text\": \"safe\"\n}"

	got, err := JSONLContent(content)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
}

func TestJSONLContent_RedactsNULAmbiguousReplacementPairs(t *testing.T) {
	t.Parallel()

	// Under a delimited-string dedup key, ("a", NUL+secret) and ("a"+NUL,
	// secret) collide: only one pair is collected, the other leaf is never
	// spliced, and the verification's key scoping cannot see it either. The
	// struct dedup key keeps both pairs distinct.
	line := `{"a":"\u0000` + highEntropySecret + `","a\u0000":"` + highEntropySecret + `"}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
}

func TestJSONLContent_FastPathStaysBytePreserving(t *testing.T) {
	t.Parallel()

	// No encoding variance: the splice lands, and the verification must not
	// disturb the line's formatting, whitespace, or number literals.
	line := `{ "text" : "` + highEntropySecret + `" , "n" : 12345678901234567890 }`
	want := `{ "text" : "REDACTED" , "n" : 12345678901234567890 }`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestJSONLContent_FallbackPreservesLargeIntegers(t *testing.T) {
	t.Parallel()

	// The structural fallback re-parses with UseNumber, so an integer beyond
	// float64 precision must survive the round-trip byte for byte.
	line := `{"text":"a\/b ` + highEntropySecret + `","n":12345678901234567890}`

	got, err := JSONLContent(line)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, "12345678901234567890")
}

func TestJSONLContent_FallbackPreservesSurroundingWhitespaceAcrossLines(t *testing.T) {
	t.Parallel()

	missLine := `{"text":"a\/b ` + highEntropySecret + `"}`
	content := "{\"k\":\"v\"}\n" + missLine + "\n{\"k2\":\"v2\"}"

	got, err := JSONLContent(content)
	require.NoError(t, err)
	lines := strings.Split(got, "\n")
	require.Len(t, lines, 3)
	// Byte equality on purpose: the neighbouring lines took the fast path and
	// must come through untouched, not merely JSON-equivalent.
	if lines[0] != `{"k":"v"}` {
		t.Fatalf("first line changed: %q", lines[0])
	}
	if lines[2] != `{"k2":"v2"}` {
		t.Fatalf("last line changed: %q", lines[2])
	}
	assert.NotContains(t, lines[1], highEntropySecret)
}

func TestJSONLContent_SingleJSONValueVerifiesSplice(t *testing.T) {
	t.Parallel()

	// Whole-document content (the redactSingleJSONValue path) shares the
	// splice and must share its verification. The trailing newline is
	// surrounding whitespace and must survive the re-encode.
	content := "{\n  \"text\": \"x\\/y " + highEntropySecret + "\"\n}\n"

	got, err := JSONLContent(content)
	require.NoError(t, err)
	assert.NotContains(t, got, highEntropySecret)
	assert.Contains(t, got, RedactedPlaceholder)
	assert.True(t, strings.HasSuffix(got, "\n"))
}
