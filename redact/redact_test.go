package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// highEntropySecret is a string with Shannon entropy > 4.5 that will trigger redaction.
const highEntropySecret = "sk-ant-api03-xK9mZ2vL8nQ5rT1wY4bC7dF0gH3jE6pA"

var fakeOpenSSHPrivateKey = makeFakeOpenSSHPrivateKey(`b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACB7ZlJ8tkWCKdRJRGF1BngP3bkNbz8bMF6Yl5xLJp9m1QAAAJj2M3UO9jN1
DgAAAAtzc2gtZWQyNTUxOQAAACB7ZlJ8tkWCKdRJRGF1BngP3bkNbz8bMF6Yl5xLJp9m1QA
AAEAGZmFrZS1rZXktZm9yLXJlZGFjdGlvbi10ZXN0LW9ubHkBAgMEBQY=`)

func makeFakeOpenSSHPrivateKey(payload string) string {
	return strings.Join([]string{
		openSSHPrivateKeyMarker("BEGIN"),
		payload,
		openSSHPrivateKeyMarker("END"),
	}, "\n")
}

func openSSHPrivateKeyMarker(kind string) string {
	return "-----" + kind + " " + "OPEN" + "SSH" + " " + "PRIVATE" + " KEY-----"
}

type stringRedactionCase struct {
	name  string
	input string
	want  string
}

func assertStringRedactionCases(t *testing.T, tests []stringRedactionCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func mustJSONLContent(t *testing.T, input string) string {
	t.Helper()
	output, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("JSONLContent(%q): %v", input, err)
	}
	return output
}

func TestBytes_NoSecrets(t *testing.T) {
	input := []byte("hello world, this is normal text")
	result := Bytes(input)
	if string(result) != string(input) {
		t.Errorf("expected unchanged input, got %q", result)
	}
	// Should return the original slice when no changes
	if &result[0] != &input[0] {
		t.Error("expected same underlying slice when no redaction needed")
	}
}

func TestBytes_WithSecret(t *testing.T) {
	input := []byte("my key is " + highEntropySecret + " ok")
	result := Bytes(input)
	expected := []byte("my key is REDACTED ok")
	if !bytes.Equal(result, expected) {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestJSONLBytes_NoSecrets(t *testing.T) {
	input := []byte(`{"type":"text","content":"hello"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Bytes()) != string(input) {
		t.Errorf("expected unchanged input, got %q", result.Bytes())
	}
	if &result.Bytes()[0] != &input[0] {
		t.Error("expected same underlying slice when no redaction needed")
	}
}

func TestJSONLBytes_WithSecret(t *testing.T) {
	input := []byte(`{"type":"text","content":"key=` + highEntropySecret + `"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []byte(`{"type":"text","content":"REDACTED"}`)
	if !bytes.Equal(result.Bytes(), expected) {
		t.Errorf("got %q, want %q", result.Bytes(), expected)
	}
}

func TestRedactedBytes_Bytes(t *testing.T) {
	t.Parallel()
	input := []byte(`{"type":"text","content":"hello"}`)
	rb := AlreadyRedacted(input)
	if !bytes.Equal(rb.Bytes(), input) {
		t.Errorf("Bytes() = %q, want %q", rb.Bytes(), input)
	}
}

func TestRedactedBytes_Len(t *testing.T) {
	t.Parallel()
	input := []byte(`some data`)
	rb := AlreadyRedacted(input)
	if rb.Len() != len(input) {
		t.Errorf("Len() = %d, want %d", rb.Len(), len(input))
	}
}

func TestAlreadyRedacted(t *testing.T) {
	t.Parallel()
	input := []byte(`some data`)
	rb := AlreadyRedacted(input)
	if !bytes.Equal(rb.Bytes(), input) {
		t.Errorf("AlreadyRedacted() = %q, want %q", rb.Bytes(), input)
	}
}

// TestApplyRegions_IgnoresZeroLengthRegions pins that a zero-length region
// replaces nothing. Applied, it would insert a token into text no layer
// flagged, whichever layer produced it.
func TestApplyRegions_IgnoresZeroLengthRegions(t *testing.T) {
	t.Parallel()
	const s = "deploy ACME-1 now"
	tests := []struct {
		name    string
		regions []taggedRegion
		want    string
	}{
		{
			name: "only zero-length",
			regions: []taggedRegion{
				{region: region{0, 0}},
				{region: region{6, 6}, label: labelEmail},
				{region: region{len(s), len(s)}},
			},
			want: s,
		},
		{
			name: "zero-length around a real region",
			regions: []taggedRegion{
				{region: region{0, 0}},
				{region: region{7, 7}},
				{region: region{7, 13}},
				{region: region{13, 13}},
				{region: region{len(s), len(s)}},
			},
			want: "deploy REDACTED now",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := applyRegions(s, tt.regions); got != tt.want {
				t.Errorf("applyRegions() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJSONLContent_TopLevelArray(t *testing.T) {
	// Top-level JSON arrays are valid JSONL and should be redacted.
	input := `["` + highEntropySecret + `","normal text"]`
	result := mustJSONLContent(t, input)
	expected := `["REDACTED","normal text"]`
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestJSONLContent_TopLevelArrayNoSecrets(t *testing.T) {
	input := `["hello","world"]`
	result := mustJSONLContent(t, input)
	if result != input {
		t.Errorf("expected unchanged input, got %q", result)
	}
}

func TestJSONLContent_MultipleObjects_AllRedacted(t *testing.T) {
	t.Parallel()
	// Regression test: JSONL with multiple top-level JSON objects must redact
	// secrets in ALL objects, not just the first. The single-JSON fast path must
	// not accidentally consume only the first object and return early.
	input := `{"content":"safe text","id":"abc"}
{"content":"key=` + highEntropySecret + `","id":"def"}
{"content":"also safe","id":"ghi"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The secret in the second line should be redacted.
	if strings.Contains(result, highEntropySecret) {
		t.Error("secret in second JSONL object was not redacted")
	}
	if !strings.Contains(result, "REDACTED") {
		t.Error("expected REDACTED in output")
	}

	// IDs should be preserved (field-aware skip).
	for _, id := range []string{"abc", "def", "ghi"} {
		if !strings.Contains(result, id) {
			t.Errorf("ID %q should be preserved", id)
		}
	}

	// Non-secret content should be preserved.
	if !strings.Contains(result, "safe text") {
		t.Error("safe text in first object was corrupted")
	}
	if !strings.Contains(result, "also safe") {
		t.Error("safe text in third object was corrupted")
	}
}

func TestJSONLContent_InvalidJSONLine(t *testing.T) {
	// Lines that aren't valid JSON should be processed with normal string redaction.
	input := `{"type":"text", "invalid ` + highEntropySecret + " json"
	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := `{"type":"text", "invalid REDACTED json`
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestCollectJSONLReplacements_Succeeds(t *testing.T) {
	obj := map[string]any{
		"content": "token=" + highEntropySecret,
	}
	repls := collectJSONLReplacements(obj, regexLayers)
	// expect one replacement for high-entropy secret
	want := []jsonReplacement{{key: "content", original: "token=" + highEntropySecret, redacted: "REDACTED"}}
	if !slices.Equal(repls, want) {
		t.Errorf("got %+v, want %+v", repls, want)
	}
}

func TestShouldSkipJSONLField(t *testing.T) {
	tests := []struct {
		key  string
		want bool
	}{
		// Fields ending in "id" should be skipped.
		{"id", true},
		{"session_id", true},
		{"sessionId", true},
		{"checkpoint_id", true},
		{"checkpointID", true},
		{"userId", true},
		// Fields ending in "ids" should be skipped.
		{"ids", true},
		{"session_ids", true},
		{"userIds", true},
		// Fields ending in "account" are deliberately NOT skipped: skipping
		// bypasses every content layer and prunes subtrees, and
		// "service_account" is the canonical GCP credential container whose
		// nested private_key must stay scanned. Low-entropy account
		// identifiers survive value-level scanning on their own.
		{"account", false},
		{"google_adsense_account", false},
		{"aws_account", false},
		{"service_account", false},
		// Signature fields should be skipped (any key ending in "signature").
		{"signature", true},
		{"thinkingSignature", true},
		{"thinking_signature", true},
		// Path-related fields should be skipped.
		{"filePath", true},
		{"file_path", true},
		{"cwd", true},
		{"root", true},
		{"directory", true},
		{"dir", true},
		{"path", true},
		// Fields that should NOT be skipped.
		{"content", false},
		{"type", false},
		{"name", false},
		{"text", false},
		{"output", false},
		{"input", false},
		{"command", false},
		{"args", false},
		{"video", false},      // ends in "o", not "id"
		{"identify", false},   // ends in "ify", not "id"
		{"signatures", false}, // does not end in "signature"
		{"signal_data", false},
		{"consideration", false}, // contains "id" but doesn't end with it
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got := shouldSkipJSONLField(tt.key)
			if got != tt.want {
				t.Errorf("shouldSkipJSONLField(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestShouldSkipJSONLField_RedactionBehavior(t *testing.T) {
	// Verify that secrets in skipped fields are preserved (not redacted).
	obj := map[string]any{
		"session_id": highEntropySecret,
		"content":    highEntropySecret,
	}
	repls := collectJSONLReplacements(obj, regexLayers)
	// Only "content" should produce a replacement; "session_id" should be skipped.
	if len(repls) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(repls))
	}
	if repls[0].original != highEntropySecret {
		t.Errorf("expected replacement for secret in content field, got %q", repls[0].original)
	}
}

func TestJSONLContent_SkippedFieldValueCollision(t *testing.T) {
	t.Parallel()
	input := `{"session_id":"` + highEntropySecret + `","content":"` + highEntropySecret + `"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(result, `"session_id":"`+highEntropySecret+`"`) {
		t.Fatalf("expected skipped session_id to be preserved, got: %s", result)
	}
	if !strings.Contains(result, `"content":"REDACTED"`) {
		t.Fatalf("expected content field to be redacted, got: %s", result)
	}
}

func TestJSONLContent_CredentialIdentifierKeysStayScanned(t *testing.T) {
	t.Parallel()
	input := `{"aws_access_key_id":"` + awsDocsKeyFixture + `","api_token_id":"correct-horse-battery","AccessKeyId":"` + awsDocsKeyFixture + `","apiTokenId":"correct-horse-battery","session_id":"ses_37273a1fdffegpYbwUTqEkPsQ0","tenantId":"tenant-123","auth_id":"auth-123","cred_id":"cred-123","cert_id":"cert-123","credential_id":"credential-123"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		`"aws_access_key_id":"REDACTED"`,
		`"api_token_id":"correct-horse-battery"`,
		`"AccessKeyId":"REDACTED"`,
		`"apiTokenId":"correct-horse-battery"`,
		`"session_id":"ses_37273a1fdffegpYbwUTqEkPsQ0"`,
		`"tenantId":"tenant-123"`,
		`"auth_id":"auth-123"`,
		`"cred_id":"cred-123"`,
		`"cert_id":"cert-123"`,
		`"credential_id":"credential-123"`,
	} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %s in output, got: %s", want, result)
		}
	}
}

func TestJSONLContent_PreservesThinkingSignature(t *testing.T) {
	t.Parallel()
	// Oh My Pi stores extended-thinking signatures under "thinkingSignature".
	// Their base64 value is high-entropy; redacting it corrupts the signature and
	// breaks replay with "Invalid `signature` in `thinking` block".
	input := `{"type":"thinking","thinking":"plan","thinkingSignature":"` + highEntropySecret + `"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, `"thinkingSignature":"`+highEntropySecret+`"`) {
		t.Fatalf("expected thinkingSignature to be preserved verbatim, got: %s", result)
	}
}

func TestString_PatternDetection(t *testing.T) {
	// These secrets have entropy below 4.5 so entropy-only detection misses them.
	// Betterleaks pattern matching should catch them.
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "AWS access key (entropy ~3.9, below 4.5 threshold)",
			input: "key=" + awsKeyFixture,
			want:  "key=REDACTED",
		},
		{
			name:  "two AWS keys separated by space produce two REDACTED tokens",
			input: "key=" + awsKeyFixture + " " + awsKeyFixture,
			want:  "key=REDACTED REDACTED",
		},
		{
			name:  "adjacent AWS keys without separator merge into single REDACTED",
			input: "key=" + awsKeyFixture + awsKeyFixture,
			want:  "key=REDACTED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify entropy is below threshold (proving entropy-only would miss this).
			for _, loc := range secretPattern.FindAllStringIndex(tt.input, -1) {
				e := shannonEntropy(tt.input[loc[0]:loc[1]])
				if e > entropyThreshold {
					t.Fatalf("test secret has entropy %.2f > %.1f; this test is meant for low-entropy secrets", e, entropyThreshold)
				}
			}

			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// supabaseSecretPrefix, supabasePersonalPrefix, and supabasePublishablePrefix
// assemble the Supabase credential prefixes from fragments so a complete token
// never appears verbatim in source. This mirrors openSSHPrivateKeyMarker above
// and keeps secret scanners (including GitHub push protection) from flagging
// synthetic test fixtures; the assembled runtime values exercise the redactor
// exactly as a real token would.

// awsKeyFixture is an AWS-Access-Key-ID-shaped value, assembled for the same
// reason as the prefixes above and openSSHPrivateKeyMarker. It matters more
// here than for those: GitHub push protection rejects this pattern outright,
// and because this repo stores its own agent transcripts as checkpoints, a
// literal reaches a transcript whenever someone edits redaction code and wedges
// that checkpoint's push queue permanently. The bytes are unchanged, so the
// AWS rule stays genuinely exercised — the "key=REDACTED" cases below fail if
// it ever stops matching. Do not re-inline it.
const awsKeyFixture = "AKIAYRWQG5" + "EJLPZLBYNP"

// awsDocsKeyFixture is AWS's canonical documentation example key, and
// awsRealisticKeyFixture a mixed-alphabet one; both are assembled for the
// same reason as awsKeyFixture.
const (
	awsDocsKeyFixture      = "AKIAIOSFODNN7" + "EXAMPLE"
	awsRealisticKeyFixture = "AKIAJ4X2QZ" + "KTPBWTNZRQ"
)

func supabaseSecretPrefix() string      { return "sb" + "_secret_" }
func supabasePersonalPrefix() string    { return "sb" + "p_" }
func supabasePublishablePrefix() string { return "sb" + "_publishable_" }

// TestString_SupabaseProviderTokens covers issue #1716: Supabase sb_secret_
// API keys and sbp_ personal access tokens are low-entropy and, captured in
// isolation, are missed by the entropy layer (threshold 4.5). betterleaks
// coverage differs per prefix: its sb_secret_ rule is a composite rule that
// only fires when a *.supabase.co URL is co-present, so a bare sb_secret_
// value never reaches its filter at all; its sbp_ rule fires standalone but
// requires an exact 40-character lowercase body, so bodies of another length
// (like the probe values below) never match its regex regardless of entropy.
// The deterministic provider-prefix layer must catch both regardless of
// entropy, body length, or the surrounding variable name.
func TestString_SupabaseProviderTokens(t *testing.T) {
	t.Parallel()

	secret := supabaseSecretPrefix() + "probe_20260710_7f91c2d8e4a6b3f0" // entropy 4.199
	realSecret := supabaseSecretPrefix() + "9uM4GhB0STF5R4K3HxQtlg_bzWW6DRj"
	sbpToken := supabasePersonalPrefix() + "test_probe_20260710_test_probe_2026071"
	// Real Supabase key bodies are base64url, which includes '-'. No other
	// fixture in this test contains a hyphen, so the charset's '-' member is
	// otherwise unpinned: narrowing [A-Za-z0-9_-] / [a-z0-9_-] to drop the
	// hyphen would still pass every other case here while silently truncating
	// (not merely shrinking) the match at the first hyphen in a real key,
	// leaking the remainder raw — the #1716 failure mode recurring via an
	// innocent charset "tidy-up".
	secretWithHyphen := supabaseSecretPrefix() + "probe-20260710-7f91c2d8e4a6b3f0"
	sbpTokenWithHyphen := supabasePersonalPrefix() + "probe-20260710-7f91c2d8e4a6b3f0"

	// Both probe values sit below the entropy threshold, proving entropy-only
	// detection would miss them (the issue reports entropy 4.199 for sb_secret_).
	for _, low := range []string{secret, sbpToken} {
		if e := shannonEntropy(low); e > entropyThreshold {
			t.Fatalf("value %q has entropy %.3f > %.1f; not a low-entropy regression case", low, e, entropyThreshold)
		}
	}

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "sb_secret_ standalone (issue #1716 repro value)",
			input: secret,
			want:  "REDACTED",
		},
		{
			name:  "sb_secret_ at start of line",
			input: secret + " is the service_role key",
			want:  "REDACTED is the service_role key",
		},
		{
			name:  "sb_secret_ at end of line",
			input: "service_role key: " + secret,
			want:  "service_role key: REDACTED",
		},
		{
			// Canonical .env form. The chosen token value is low-entropy
			// (quoting has no effect on secretPattern matching), so the
			// entropy layer misses it, isolating the deterministic provider
			// layer.
			name:  "sb_secret_ in env-style double-quoted assignment",
			input: `SUPABASE_SERVICE_ROLE_KEY="` + secret + `"`,
			want:  `SUPABASE_SERVICE_ROLE_KEY="REDACTED"`,
		},
		{
			name:  "sb_secret_ single-quoted value",
			input: "key: '" + secret + "'",
			want:  "key: 'REDACTED'",
		},
		{
			name:  "sb_secret_ multiple occurrences",
			input: secret + " then " + secret,
			want:  "REDACTED then REDACTED",
		},
		{
			name:  "sb_secret_ real-shaped mixed-case body",
			input: `SUPABASE_SERVICE_ROLE_KEY="` + realSecret + `"`,
			want:  `SUPABASE_SERVICE_ROLE_KEY="REDACTED"`,
		},
		{
			name:  "sbp_ personal access token (38-char body, betterleaks' rule requires exactly 40)",
			input: "SUPABASE_ACCESS_TOKEN=" + sbpToken,
			want:  "SUPABASE_ACCESS_TOKEN=REDACTED",
		},
		{
			name:  "sb_secret_ body with an early hyphen (real base64url shape)",
			input: secretWithHyphen,
			want:  "REDACTED",
		},
		{
			name:  "sbp_ body with an early hyphen (real base64url shape)",
			input: sbpTokenWithHyphen,
			want:  "REDACTED",
		},
	})
}

// TestString_SupabaseProviderTokenLengthBoundaries pins the {20,} body-length
// floor shared by both provider patterns as an explicit boundary rather than
// an emergent property of an unrelated fixture: a body of exactly 20 chars
// must redact, and a body of exactly 19 chars must be preserved. Before this
// test, the floor was pinned only accidentally — via key_rotation_handler
// (sb_secret_) happening to have a 20-char body, with no equivalent coverage
// for sbp_ at all. Each case fails if either pattern's minimum is tightened
// to {21,}.
func TestString_SupabaseProviderTokenLengthBoundaries(t *testing.T) {
	t.Parallel()

	const (
		body20 = "boundary_probe_2026x" // exactly 20 chars
		body19 = "boundary_probe_2026"  // exactly 19 chars
	)
	if len(body20) != 20 || len(body19) != 19 {
		t.Fatalf("fixture bodies are %d/%d chars, want 20/19", len(body20), len(body19))
	}

	secret20 := supabaseSecretPrefix() + body20
	secret19 := supabaseSecretPrefix() + body19
	sbp20 := supabasePersonalPrefix() + body20
	sbp19 := supabasePersonalPrefix() + body19

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "sb_secret_ with exactly 20-char body redacts",
			input: secret20,
			want:  "REDACTED",
		},
		{
			name:  "sb_secret_ with exactly 19-char body is preserved",
			input: secret19,
			want:  secret19,
		},
		{
			name:  "sbp_ with exactly 20-char body redacts",
			input: sbp20,
			want:  "REDACTED",
		},
		{
			name:  "sbp_ with exactly 19-char body is preserved",
			input: sbp19,
			want:  sbp19,
		},
	})
}

// TestString_SupabaseProviderTokenOverRedactionGuards pins that the
// deterministic provider layer does not over-redact. Publishable keys are
// designed to be embedded in client code and are intentionally not targeted by
// this layer (a low-entropy publishable value therefore passes through it; a
// high-entropy real one would still be caught by the entropy layer). A bare
// prefix or a prefix with a too-short body is not a credential.
func TestString_SupabaseProviderTokenOverRedactionGuards(t *testing.T) {
	t.Parallel()

	publishable := supabasePublishablePrefix() + "probe_20260710_7f91c2d8e4a6b3f0"
	shortSecret := supabaseSecretPrefix() + "short"
	shortToken := supabasePersonalPrefix() + "short"

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			// The publishable fixture is low-entropy (quoting has no effect on
			// secretPattern matching), so the entropy layer does not flag it,
			// proving the provider layer itself does not target publishable
			// keys.
			name:  "publishable key is not targeted by the provider layer",
			input: `NEXT_PUBLIC_SUPABASE_KEY="` + publishable + `"`,
			want:  `NEXT_PUBLIC_SUPABASE_KEY="` + publishable + `"`,
		},
		{
			name:  "sb_secret_ with too-short body is preserved",
			input: shortSecret,
			want:  shortSecret,
		},
		{
			name:  "sbp_ with too-short body is preserved",
			input: shortToken,
			want:  shortToken,
		},
		{
			name:  "bare sb_secret_ prefix in prose is preserved",
			input: "the " + supabaseSecretPrefix() + " prefix identifies Supabase secret keys",
			want:  "the " + supabaseSecretPrefix() + " prefix identifies Supabase secret keys",
		},
	})
}

// TestString_SupabaseProviderTokenLongIdentifierOverRedaction documents a
// known, accepted false-positive class: because the body charset includes
// underscore and the length check is {20,} with no upper bound, sufficiently
// long snake_case identifiers that merely start with a provider prefix are
// redacted even though they are not secrets — including mid-word, since the
// prefix is deliberately not anchored (see the package comment in
// providers.go). This is intentional: over-redaction is the safe direction,
// and reintroducing a \b anchor or a body-length cap to "fix" this would
// reopen the low-entropy under-redaction gap the provider layer exists to
// close. This test pins the tradeoff so it isn't silently reversed.
func TestString_SupabaseProviderTokenLongIdentifierOverRedaction(t *testing.T) {
	t.Parallel()

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "long snake_case identifier starting with sb_secret_ is over-redacted",
			input: "func " + supabaseSecretPrefix() + "key_rotation_handler() {}",
			want:  "func REDACTED() {}",
		},
		{
			name:  "sbp_ mid-word inside a longer identifier is over-redacted",
			input: "call lib" + supabasePersonalPrefix() + "something_long_enough_value()",
			want:  "call libREDACTED()",
		},
	})
}

// TestJSONLContent_SupabaseSecretRedacted drives the secret through the
// field-aware JSONL path used by checkpoint condensation, mirroring a Claude
// Code transcript line where the secret lives in a message-content leaf.
func TestJSONLContent_SupabaseSecretRedacted(t *testing.T) {
	t.Parallel()
	secret := supabaseSecretPrefix() + "probe_20260710_7f91c2d8e4a6b3f0"
	line := `{"type":"user","message":{"role":"user","content":"the service_role key is ` + secret + ` now"}}`
	got, err := JSONLContent(line)
	if err != nil {
		t.Fatalf("JSONLContent error: %v", err)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("secret survived JSONL redaction: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("expected REDACTED placeholder in %q", got)
	}
}

// TestString_SupabaseProviderTokenBoundaries pins that the provider layer
// redacts a Supabase secret even when the prefix abuts a preceding *word*
// character. A \b anchor before the prefix only fires after a non-word
// character, so a secret glued to a preceding letter/digit/underscore — an
// underscore-joined name, or (in the raw redact.Bytes / JSONL fall-back path
// that runs String on undecoded text) a JSON escape whose trailing letter sits
// against the prefix, e.g. "…line1\nsb_secret_…" where the byte before "sb" is
// the literal 'n' — would slip past. These bodies are deliberately low-entropy,
// so no other layer backs the provider layer up: a miss reaches the blob raw.
// Each case fails if the leading \b anchor is reintroduced.
func TestString_SupabaseProviderTokenBoundaries(t *testing.T) {
	t.Parallel()

	secret := supabaseSecretPrefix() + "probe_20260710_7f91c2d8e4a6b3f0"
	sbpToken := supabasePersonalPrefix() + "test_probe_20260710_test_probe_2026071"

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "sb_secret_ glued to a preceding word char",
			input: "x" + secret,
			want:  "xREDACTED",
		},
		{
			// Raw-text fall-back shape: the transcript line failed to parse as
			// JSON, so String runs on the undecoded bytes where "\n" is a literal
			// backslash-n and the 'n' abuts the prefix.
			name:  "sb_secret_ preceded by a literal JSON escape letter",
			input: `first line\n` + secret,
			want:  `first line\nREDACTED`,
		},
		{
			name:  "sbp_ preceded by a literal JSON escape letter",
			input: `first line\n` + sbpToken,
			want:  `first line\nREDACTED`,
		},
	})
}

// TestJSONLContent_SupabaseSecretMalformedLineFallback drives the secret
// through the JSONL fall-back branch (jsonlContentImpl calls the per-leaf
// redactor on the raw line when json.Unmarshal fails), with the secret glued to
// a literal "\n" escape so the byte before the prefix is a word char. This is
// the realistic path by which a malformed/truncated transcript line could leak
// a low-entropy Supabase secret; it must still be redacted.
func TestJSONLContent_SupabaseSecretMalformedLineFallback(t *testing.T) {
	t.Parallel()
	secret := supabaseSecretPrefix() + "probe_20260710_7f91c2d8e4a6b3f0"
	// Trailing garbage after the closing brace makes json.Unmarshal fail, forcing
	// the raw-line fall-back; inside, "\n" is a literal backslash-n before "sb".
	line := `{"content":"line1\n` + secret + `"} <-- truncated`
	got, err := JSONLContent(line)
	if err != nil {
		t.Fatalf("JSONLContent error: %v", err)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("secret survived JSONL fall-back redaction: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("expected REDACTED placeholder in %q", got)
	}
}

func bareSKPrefix() string { return "s" + "k-" }

// TestString_BareSKKeys covers issue #2728: an "sk-" key with a short body
// (LiteLLM proxy virtual keys are sk- + 22 chars) written as a bare value in
// prose was missed by every layer. The entropy layer structurally cannot catch
// it (see providers.go), so the provider layer must, wherever it appears.
func TestString_BareSKKeys(t *testing.T) {
	t.Parallel()

	issueKey := bareSKPrefix() + "Qm4Wn8Rt_bVcXzLkJhGfDs" // issue #2728 fake value
	lowerDigitKey := bareSKPrefix() + "a1b2c3d4e5f6g7h8i9j0k1"
	hyphenKey := bareSKPrefix() + "Qm4Wn8Rt-bVcXzLkJhGfDs"
	body20 := bareSKPrefix() + "Qm4Wn8RtbVcXzLkJhGfD"
	body19 := bareSKPrefix() + "Qm4Wn8RtbVcXzLkJhGf"
	for _, low := range []string{issueKey, lowerDigitKey, body19} {
		if e := shannonEntropy(low); e > entropyThreshold {
			t.Fatalf("value %q has entropy %.3f > %.1f; not an entropy-miss regression case", low, e, entropyThreshold)
		}
	}

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "issue #2728 repro: bare key in prose",
			input: "the first is " + issueKey + " the second",
			want:  "the first is REDACTED the second",
		},
		{
			name:  "lowercase body with digits",
			input: "key " + lowerDigitKey + " here",
			want:  "key REDACTED here",
		},
		{
			name:  "body with a hyphen is redacted whole",
			input: "key " + hyphenKey + " here",
			want:  "key REDACTED here",
		},
		{
			name:  "quoted value",
			input: `"api_key": "` + issueKey + `"`,
			want:  `"api_key": "REDACTED"`,
		},
		{
			name:  "bearer header",
			input: "Authorization: Bearer " + issueKey,
			want:  "Authorization: Bearer REDACTED",
		},
		{
			name:  "exactly 20-char body redacts",
			input: body20,
			want:  "REDACTED",
		},
		{
			name:  "exactly 19-char body is preserved",
			input: body19,
			want:  body19,
		},
		{
			// Raw-text fall-back shape: "\n" is a literal backslash-n, so the
			// byte before the prefix is the word char 'n'.
			name:  "preceded by a literal JSON escape letter",
			input: `first line\n` + issueKey,
			want:  `first line\nREDACTED`,
		},
	})
}

// TestDetectProviderTokens_BareSKKeyBoundaries pins the underscore and escape
// boundaries on the provider layer alone: through String, the entropy layer can
// swallow the preceding name or escape digits together with the key and mask a
// missing boundary.
func TestDetectProviderTokens_BareSKKeyBoundaries(t *testing.T) {
	t.Parallel()

	key := bareSKPrefix() + "Qm4Wn8Rt_bVcXzLkJhGfDs"
	for _, prefix := range []string{`API_KEY_`, `first line\n`, `quote\u0022`} {
		input := prefix + key
		regions := detectProviderTokens(input)
		if len(regions) != 1 || input[regions[0].start:regions[0].end] != key {
			t.Errorf("detectProviderTokens(%q) = %v, want exactly the key", input, regions)
		}
	}
}

// TestString_BareSKKeyOverRedactionGuards pins the two guards that keep the
// bare sk- rule off ordinary text: the prefix must not sit inside a word, and
// an all-lowercase digit-free body is a kebab-case name, not a key.
func TestString_BareSKKeyOverRedactionGuards(t *testing.T) {
	t.Parallel()

	inWord := "risk-assessment-2026-q4-planning-doc"
	kebab := bareSKPrefix() + "learn-model-selection-guide"
	for _, low := range []string{inWord, kebab} {
		if e := shannonEntropy(low); e > entropyThreshold {
			t.Fatalf("value %q has entropy %.3f > %.1f; entropy layer would mask the guard", low, e, entropyThreshold)
		}
	}

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "sk- inside a hyphenated word is preserved",
			input: "see " + inWord + " for details",
			want:  "see " + inWord + " for details",
		},
		{
			name:  "all-lowercase digit-free body is preserved",
			input: "read " + kebab,
			want:  "read " + kebab,
		},
		{
			name:  "bare prefix in prose is preserved",
			input: "OpenAI keys start with " + bareSKPrefix(),
			want:  "OpenAI keys start with " + bareSKPrefix(),
		},
	})
}

// TestJSONLContent_BareSKKeyRedacted drives the issue #2728 value through the
// field-aware JSONL path used for transcript.jsonl and full.jsonl.
func TestJSONLContent_BareSKKeyRedacted(t *testing.T) {
	t.Parallel()
	key := bareSKPrefix() + "Qm4Wn8Rt_bVcXzLkJhGfDs"
	line := `{"type":"user","message":{"role":"user","content":"the first is ` + key + ` the second"}}`
	got, err := JSONLContent(line)
	if err != nil {
		t.Fatalf("JSONLContent error: %v", err)
	}
	if strings.Contains(got, key) {
		t.Fatalf("key survived JSONL redaction: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("expected REDACTED placeholder in %q", got)
	}
}

// awsTempKeyFixture is an STS-shaped (ASIA) key ID, assembled like awsKeyFixture.
const awsTempKeyFixture = "ASIAQ7XK2M" + "ZP4RLW6NTB"

// TestString_BareAWSKeyIDs covers issue #2808: betterleaks' aws-access-token
// rule only fires when an aws-secret-access-key component sits within five
// lines, and a 20-char key ID can never clear the entropy threshold, so a key ID
// alone in prose reached the checkpoint raw.
func TestString_BareAWSKeyIDs(t *testing.T) {
	t.Parallel()

	for _, id := range []string{awsKeyFixture, awsTempKeyFixture} {
		if e := shannonEntropy(id); e > entropyThreshold {
			t.Fatalf("value %q has entropy %.3f > %.1f; not an entropy-miss regression case", id, e, entropyThreshold)
		}
	}

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "issue #2808 repro: bare key ID in prose",
			input: "the access key is " + awsKeyFixture + " for staging",
			want:  "the access key is REDACTED for staging",
		},
		{
			name:  "temporary STS key ID alone on a line",
			input: "creds:\n" + awsTempKeyFixture + "\nend",
			want:  "creds:\nREDACTED\nend",
		},
		{
			name:  "two key IDs separated by one space",
			input: awsKeyFixture + " " + awsTempKeyFixture,
			want:  "REDACTED REDACTED",
		},
		{
			name:  "quoted value",
			input: `"id": "` + awsKeyFixture + `"`,
			want:  `"id": "REDACTED"`,
		},
		{
			name:  "AWS documentation example key is preserved",
			input: "e.g. AKIAIOSFODNN7" + "EXAMPLE",
			want:  "e.g. AKIAIOSFODNN7" + "EXAMPLE",
		},
		{
			name:  "prefix inside a longer uppercase word is preserved",
			input: "TODAYIN" + "ASIAASACKOFRICEFELLOVER",
			want:  "TODAYIN" + "ASIAASACKOFRICEFELLOVER",
		},
	})
}

// TestDetectProviderTokens_AWSKeyIDBoundaries pins the key-ID boundaries on the
// provider layer alone; through String, the entropy layer can swallow a
// preceding name or escape and mask a missing boundary.
func TestDetectProviderTokens_AWSKeyIDBoundaries(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`AWS_ACCESS_KEY_ID_` + awsKeyFixture,
		`first line\n` + awsKeyFixture,
		`quote"` + awsKeyFixture,
		awsKeyFixture + `_suffix`,
		awsKeyFixture + `\n`,
	} {
		regions := detectProviderTokens(input)
		if len(regions) != 1 || input[regions[0].start:regions[0].end] != awsKeyFixture {
			t.Errorf("detectProviderTokens(%q) = %v, want exactly the key ID", input, regions)
		}
	}

	for _, input := range []string{
		"x" + awsKeyFixture,
		awsKeyFixture + "Z",
		awsKeyFixture + "2",
		"AKIA" + strings.Repeat("X", 16),
	} {
		if regions := detectProviderTokens(input); len(regions) != 0 {
			t.Errorf("detectProviderTokens(%q) = %v, want no regions", input, regions)
		}
	}
}

// TestJSONLContent_BareAWSKeyIDRedacted drives a bare key ID through the
// field-aware JSONL path used for transcript.jsonl and full.jsonl.
func TestJSONLContent_BareAWSKeyIDRedacted(t *testing.T) {
	t.Parallel()
	line := `{"type":"user","message":{"role":"user","content":"the access key is ` + awsKeyFixture + ` for staging"}}`
	got, err := JSONLContent(line)
	if err != nil {
		t.Fatalf("JSONLContent error: %v", err)
	}
	if strings.Contains(got, awsKeyFixture) {
		t.Fatalf("key ID survived JSONL redaction: %q", got)
	}
}

// salesforceTokenFixture and tableauPATFixture have the provider shapes the
// bounded token rules catch; assembled like awsKeyFixture.
const (
	salesforceTokenFixture = "00D5g000001" + "2aBc!AQ4AQ" + "Hk2.mP9vLq7sXz3RtY6wNb8cJf1dGh5eKu0iOa4lZ_Wn" + "Ex3Qr7Ty2Ui9Op6As1Df4Gh8Jk5Lz0Xc.Vb3Nm7Qw-Er2Ty6U"
	tableauPATFixture      = "Kq7mZ2xR9vLp4nWb8cJf+/" + "==:" + "aB3dE7gH1jK5mN9pQ2sT6vX0yZ4cF8hL"
)

// TestString_KeywordGatedProviderTokens covers tokens betterleaks scans for
// only when a vendor keyword ("salesforce.com", "tableau") appears in the same
// text, and that the entropy layer misses because "!", ".", "/", "+" and ":"
// split them.
func TestString_KeywordGatedProviderTokens(t *testing.T) {
	t.Parallel()

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "Salesforce access token in prose",
			input: "use " + salesforceTokenFixture + " as the session",
			want:  "use REDACTED as the session",
		},
		{
			name:  "Tableau PAT in prose",
			input: "use " + tableauPATFixture + " as the token",
			want:  "use REDACTED as the token",
		},
	})
}

// TestDetectProviderTokens_JoinedSalesforceTokens covers tokens joined by a
// body character: a greedy body would run into the second token's org ID, stop
// at its "!", and leave the second body unredacted.
func TestDetectProviderTokens_JoinedSalesforceTokens(t *testing.T) {
	t.Parallel()

	for _, sep := range []string{"_", ".", "-"} {
		input := salesforceTokenFixture + sep + salesforceTokenFixture
		want := "REDACTED" + sep + "REDACTED"
		if got := applyRegions(input, detectProviderTokens(input)); got != want {
			t.Errorf("provider layer on tokens joined by %q = %q, want %q", sep, got, want)
		}
	}
}

// TestDetectProviderTokens_KeywordGatedBoundaries pins boundaries and filters
// of the Salesforce and Tableau rules on the provider layer alone.
func TestDetectProviderTokens_KeywordGatedBoundaries(t *testing.T) {
	t.Parallel()

	for _, token := range []string{salesforceTokenFixture, tableauPATFixture} {
		for _, input := range []string{`TOKEN_` + token, `first line\n` + token, token + `"`} {
			regions := detectProviderTokens(input)
			if len(regions) != 1 || input[regions[0].start:regions[0].end] != token {
				t.Errorf("detectProviderTokens(%q) = %v, want exactly the token", input, regions)
			}
		}
	}

	for _, input := range []string{
		"x" + salesforceTokenFixture,
		"00" + strings.Repeat("X", 13) + "!" + strings.Repeat("x", 80),
		salesforceTokenFixture[:len(salesforceTokenFixture)-19],
		tableauPATFixture + "Z",
		strings.Repeat("A", 22) + "==:" + strings.Repeat("a", 32),
		"x" + tableauPATFixture,
	} {
		if regions := detectProviderTokens(input); len(regions) != 0 {
			t.Errorf("detectProviderTokens(%q) = %v, want no regions", input, regions)
		}
	}
}

func TestString_CredentialedURIs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "postgres URI",
			input: "DATABASE_URL=postgres://app:pwd123@db.example.com:5432/app",
			want:  "DATABASE_URL=REDACTED",
		},
		{
			name:  "postgresql URI with query",
			input: `dsn="postgresql://svc:moderatepw@localhost/app?sslmode=require"`,
			want:  `dsn="REDACTED"`,
		},
		{
			name:  "mongodb srv URI",
			input: "mongo=mongodb+srv://user:pass123@cluster0.example.mongodb.net/app?retryWrites=true",
			want:  "mongo=REDACTED",
		},
		{
			name:  "mysql URI",
			input: "mysql://root:p@localhost:3306/app",
			want:  "REDACTED",
		},
		{
			name:  "redis URI with empty username",
			input: "cache redis://:hunter2@localhost:6379/0",
			want:  "cache REDACTED",
		},
		{
			name:  "generic credentialed URL",
			input: "proxy=https://user:pass@example.com/path",
			want:  "proxy=REDACTED",
		},
		{
			name:  "URL without password is preserved",
			input: "repo=ssh://git@github.com/entireio/cli",
			want:  "repo=ssh://git@github.com/entireio/cli",
		},
		{
			name:  "colon and at-sign in path are preserved",
			input: "url=https://example.com/a:b@c",
			want:  "url=https://example.com/a:b@c",
		},
		{
			name:  "placeholder password is preserved",
			input: "DATABASE_URL=postgres://postgres:changeme@localhost:5432/app",
			want:  "DATABASE_URL=postgres://postgres:changeme@localhost:5432/app",
		},
		{
			name:  "shell expansions are preserved",
			input: "postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}/app",
			want:  "postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}/app",
		},
		{
			name:  "printf verbs are preserved",
			input: `fmt.Sprintf("postgres://%s:%s@%s:%d/%s", u, p, h, port, db)`,
			want:  `fmt.Sprintf("postgres://%s:%s@%s:%d/%s", u, p, h, port, db)`,
		},
		{
			name:  "python percent-format names are preserved",
			input: `"postgresql://%(user)s:%(password)s@%(host)s/app" % cfg`,
			want:  `"postgresql://%(user)s:%(password)s@%(host)s/app" % cfg`,
		},
		{
			name:  "format fields are preserved",
			input: `f"postgresql://{user}:{password}@{host}/{db}"`,
			want:  `f"postgresql://{user}:{password}@{host}/{db}"`,
		},
		{
			name:  "empty format fields are preserved",
			input: `"postgresql://{}:{}@{}/{}".format(u, p, h, d)`,
			want:  `"postgresql://{}:{}@{}/{}".format(u, p, h, d)`,
		},
		{
			name:  "uppercase shell variable is preserved",
			input: "postgres://app:$DB_PASSWORD@db:5432/app",
			want:  "postgres://app:$DB_PASSWORD@db:5432/app",
		},
		{
			name:  "mask run is preserved",
			input: "postgres://app:****@db:5432/app",
			want:  "postgres://app:****@db:5432/app",
		},
		{
			name:  "placeholder userinfo with a query but no password is preserved",
			input: "postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}/app?sslmode=require",
			want:  "postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}/app?sslmode=require",
		},
		{
			name:  "placeholder userinfo with a placeholder query password is preserved",
			input: "postgres://app:changeme@db/app?password=${DB_PASSWORD}",
			want:  "postgres://app:changeme@db/app?password=${DB_PASSWORD}",
		},
		{
			name:  "placeholder head with real tail is redacted",
			input: "postgres://postgres:changeme123@localhost:5432/app",
			want:  "REDACTED",
		},
		{
			name:  "format field with digits is redacted",
			input: "postgres://app:{hunter2}@db/app",
			want:  "REDACTED",
		},
		{
			name:  "lowercase dollar value is redacted",
			input: "postgres://app:$hunter2@db/app",
			want:  "REDACTED",
		},
		{
			name:  "placeholder userinfo with real query password is redacted",
			input: "postgres://app:changeme@db/app?password=hunter2",
			want:  "REDACTED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// Over-redaction guards for the keyword-DSN rule: KEY=value lines from env
// output must not be read as a host/user/password connection string.
func TestString_KeywordDSNOverRedactionGuards(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "env output with shell PWD is preserved",
			input: "NODE_ENV=development\nHOST=0.0.0.0\nPORT=3000\nUSER=node\nHOME=/home/node\nPWD=/app\n",
			want:  "NODE_ENV=development\nHOST=0.0.0.0\nPORT=3000\nUSER=node\nHOME=/home/node\nPWD=/app\n",
		},
		{
			name:  "inline env assignments with shell PWD are preserved",
			input: "env HOST=0.0.0.0 USER=node PWD=/app node server.js",
			want:  "env HOST=0.0.0.0 USER=node PWD=/app node server.js",
		},
		{
			name:  "uppercase PWD holding an absolute path is not a keyword DSN password",
			input: "host=0.0.0.0 user=node PWD=/app",
			want:  "host=0.0.0.0 user=node PWD=/app",
		},
		{
			name:  "uppercase PWD holding a home path is not a keyword DSN password",
			input: "host=0.0.0.0 user=node PWD=~/app",
			want:  "host=0.0.0.0 user=node PWD=~/app",
		},
		{
			name:  "env output without HOST is preserved",
			input: "USER=node\nPWD=/app\nSHELL=/bin/bash\nLANG=C.UTF-8\n",
			want:  "USER=node\nPWD=/app\nSHELL=/bin/bash\nLANG=C.UTF-8\n",
		},
	})
}

// Pins that the connection-string, URI, and key/value layers still catch real
// passwords on their own, with no scanner engine to back them up. Not
// parallel: scanner selection is process-global.
func TestString_ConnectionSecretsCaughtWithoutScanners(t *testing.T) {
	t.Cleanup(func() {
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore default scanners: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{}); err != nil {
		t.Fatalf("ConfigureScanners(none): %v", err)
	}
	tests := []stringRedactionCase{
		{
			name:  "keyword DSN is redacted as a unit",
			input: `dsn="host=db.example.com user=svc password=hunter2 dbname=app"`,
			want:  `dsn="REDACTED"`,
		},
		{
			name:  "keyword DSN with a pwd password",
			input: "host=db user=svc pwd=Xk9vq2Lm",
			want:  "REDACTED",
		},
		{
			name:  "keyword DSN with a pwd password starting with a tilde",
			input: "host=db user=svc pwd=~hunter2",
			want:  "REDACTED",
		},
		{
			name:  "keyword DSN with a lowercase pwd password starting with a slash",
			input: "host=db user=app pwd=/s3cretPass",
			want:  "REDACTED",
		},
		{
			name:  "keyword DSN with a lowercase pwd password shaped like a home path",
			input: "host=db user=app pwd=~/s3cretPass",
			want:  "REDACTED",
		},
		{
			name:  "keyword DSN with an uppercase PWD password that is not a path",
			input: "host=db user=svc PWD=Xk9vq2Lm",
			want:  "REDACTED",
		},
		{
			name:  "placeholder userinfo with a real query password",
			input: "https://app:changeme@example.com/app?password=hunter2",
			want:  "REDACTED",
		},
		{
			name:  "templated host with a real query password",
			input: "postgres://app:changeme@${DB_HOST}/app?password=hunter2",
			want:  "REDACTED",
		},
		{
			name:  "credentialed URI with a literal between expansions",
			input: "amqp://app:${P}hunter2${Q}@host/vhost",
			want:  "REDACTED",
		},
		{
			name:  "credentialed URI with a literal inside a nested default",
			input: "amqp://app:${P:-${Q}hunter2${R}}@host/vhost",
			want:  "REDACTED",
		},
		{
			name:  "credential value with a literal between expansions",
			input: "DB_PASSWORD=${P}hunter2${Q}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "credentialed URI with a literal in a pattern-removal expansion",
			input: "amqp://app:${VAR%hunter2}@host/vhost",
			want:  "REDACTED",
		},
		{
			name:  "credential value with a literal in a substitution expansion",
			input: "DB_PASSWORD=${VAR/x/hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "credential value with a literal in a prefix-removal expansion",
			input: "DB_PASSWORD=${VAR#hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "env block with a bare password",
			input: "HOST=db\nUSER=app\nPASSWORD=postgres\n",
			want:  "REDACTED\n",
		},
		{
			name:  "option file block",
			input: "[client]\nhost=localhost\nuser=root\npassword=root\n",
			want:  "[client]\nREDACTED\n",
		},
		{
			name:  "odbc PWD still counts in semicolon strings",
			input: "conn=Driver={ODBC Driver 18 for SQL Server};Server=db;UID=svc;PWD=hunter2;Database=app",
			want:  "conn=REDACTED",
		},
		{
			name:  "credentialed URI",
			input: "postgres://app:hunter2@db:5432/app",
			want:  "REDACTED",
		},
		{
			name:  "credentialed URI with a real shell default",
			input: "postgres://app:${DB_PASSWORD:-hunter2}@db/app",
			want:  "REDACTED",
		},
		{
			name:  "credential value with a real shell default",
			input: "DB_PASSWORD=${DB_PASSWORD:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.input); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// Content digests (go.sum h1: hashes, Subresource Integrity values) are hashes
// of public content. The entropy layer must leave them alone, but only when the
// whole match is the digest.
func TestString_ContentDigestsPreserved(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "go.sum module hash",
			input: "github.com/stretchr/testify v1.9.0 h1:kcj+ClqXiFC1clI9bJ9DPCmcSF0OopwqDYOTocnBi0U=",
			want:  "github.com/stretchr/testify v1.9.0 h1:kcj+ClqXiFC1clI9bJ9DPCmcSF0OopwqDYOTocnBi0U=",
		},
		{
			name:  "go.sum go.mod hash",
			input: "github.com/stretchr/testify v1.9.0/go.mod h1:OP+PZHBRu1YXkVur4Jzyvl73RrGGmWvsWAbEWCFKYGg=",
			want:  "github.com/stretchr/testify v1.9.0/go.mod h1:OP+PZHBRu1YXkVur4Jzyvl73RrGGmWvsWAbEWCFKYGg=",
		},
		{
			name:  "package-lock sha512 integrity",
			input: `"integrity": "sha512-V+Cu1AWSsxpqjQMOTazGG8sOGKWCyRowpjMMscqVu8Szrs1RcmQH0p3H+rPrcJdV6cwNweumkNyN+MUrRW32lg=="`,
			want:  `"integrity": "sha512-V+Cu1AWSsxpqjQMOTazGG8sOGKWCyRowpjMMscqVu8Szrs1RcmQH0p3H+rPrcJdV6cwNweumkNyN+MUrRW32lg=="`,
		},
		{
			name:  "package-lock sha512 integrity containing a slash",
			input: `"integrity": "sha512-XRcglhh3p2lHAu4gFg75i5owZ3/uttIZh11iKj+W2fqc4Iu8OvwIHkDSftaw4gURo0WA2fT2oguwTa4HChLwJw=="`,
			want:  `"integrity": "sha512-XRcglhh3p2lHAu4gFg75i5owZ3/uttIZh11iKj+W2fqc4Iu8OvwIHkDSftaw4gURo0WA2fT2oguwTa4HChLwJw=="`,
		},
		{
			name:  "script sha384 integrity containing a slash",
			input: `<script src="b.js" integrity="sha384-u0KDATKxW9gkYb7bXw2KbKqvfS5Om9AbuB51MpH1J/GU9BxXwQ7fE8gD0sKuq4eE"></script>`,
			want:  `<script src="b.js" integrity="sha384-u0KDATKxW9gkYb7bXw2KbKqvfS5Om9AbuB51MpH1J/GU9BxXwQ7fE8gD0sKuq4eE"></script>`,
		},
		{
			name:  "csp sha256 source",
			input: "script-src 'sha256-ZJunmw1Iu57ltG3ua5ahB3jnP7Xk2Y7Ctmgu6YyLHSA='",
			want:  "script-src 'sha256-ZJunmw1Iu57ltG3ua5ahB3jnP7Xk2Y7Ctmgu6YyLHSA='",
		},
		{
			name:  "secret glued onto a digest is redacted",
			input: "h1:kcj+ClqXiFC1clI9bJ9DPCmcSF0OopwqDYOTocnBi0U=Xk9vq2LmPq7Rw3ZtB8",
			want:  "h1:REDACTED",
		},
		{
			name:  "digest prefix with the wrong length is redacted",
			input: `"integrity": "sha256-V+Cu1AWSsxpqjQMOTazGG8sOGKWCyRowpjMMscqVu8Sz"`,
			want:  `"integrity": "REDACTED"`,
		},
		{
			name:  "digest prefix with the wrong length containing a slash is redacted",
			input: `"integrity": "sha512-XRcglhh3p2lHAu4gFg75i5owZ3/uttIZh11iKj+W2fqc4Iu8Ovw"`,
			want:  `"integrity": "sha512-REDACTED"`,
		},
		{
			// The first 64 characters have a sha384 digest's length, and the
			// slash splits the entropy match inside them.
			name:  "overlong digest is redacted",
			input: `integrity="sha384-ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/A"`,
			want:  `integrity="REDACTED"`,
		},
		{
			name:  "secret between two digests is redacted",
			input: "github.com/a/b v1.0.0 h1:kcj+ClqXiFC1clI9bJ9DPCmcSF0OopwqDYOTocnBi0U=\nnote aB3dE5fG7hJ9kL2mN4pQ6rS8tU1vW0xY\ngithub.com/c/d v1.0.0 h1:OP+PZHBRu1YXkVur4Jzyvl73RrGGmWvsWAbEWCFKYGg=",
			want:  "github.com/a/b v1.0.0 h1:kcj+ClqXiFC1clI9bJ9DPCmcSF0OopwqDYOTocnBi0U=\nnote REDACTED\ngithub.com/c/d v1.0.0 h1:OP+PZHBRu1YXkVur4Jzyvl73RrGGmWvsWAbEWCFKYGg=",
		},
	})
}

// The over-redaction shapes above, through the JSONL path checkpoints use.
func TestJSONLContent_OverRedactionShapesPreserved(t *testing.T) {
	t.Parallel()
	values := []string{
		"NODE_ENV=development\nHOST=0.0.0.0\nPORT=3000\nUSER=node\nHOME=/home/node\nPWD=/app\n",
		"DB_PASSWORD=\nDB_HOST=localhost\n",
		"postgres://postgres:changeme@localhost:5432/app",
		"github.com/stretchr/testify v1.9.0 h1:kcj+ClqXiFC1clI9bJ9DPCmcSF0OopwqDYOTocnBi0U=",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			line, err := json.Marshal(map[string]string{"type": "tool_result", "content": value})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := JSONLContent(string(line))
			if err != nil {
				t.Fatalf("JSONLContent: %v", err)
			}
			if got != string(line) {
				t.Errorf("JSONLContent(%s) = %s, want unchanged", line, got)
			}
		})
	}
}

func TestString_DatabaseConnectionStringRedaction(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "postgres keyword DSN",
			input: `dsn="host=db.example.com port=5432 user=svc password=secret dbname=app sslmode=require"`,
			want:  `dsn="REDACTED"`,
		},
		{
			name:  "postgres keyword DSN different order",
			input: "password=secret sslmode=require user=svc host=db.example.com dbname=app",
			want:  "REDACTED",
		},
		{
			name:  "sql server connection string",
			input: "conn=Server=tcp:db.example.com,1433;Database=app;User Id=svc;Password=secret;Encrypt=true",
			want:  "conn=REDACTED",
		},
		{
			name:  "odbc connection string",
			input: "conn=Driver={ODBC Driver 18 for SQL Server};Server=db;UID=svc;PWD=secret;Database=app",
			want:  "conn=REDACTED",
		},
		{
			name:  "jdbc query password",
			input: "jdbc:postgresql://db.example.com:5432/app?user=svc&password=secret&ssl=true",
			want:  "REDACTED",
		},
		{
			name:  "postgres URL query password without userinfo",
			input: "DATABASE_URL=postgresql://db.example.com:5432/app?user=svc&password=secret&sslmode=require",
			want:  "DATABASE_URL=REDACTED",
		},
		{
			name:  "postgres URL query password is case-insensitive",
			input: "DATABASE_URL=postgresql://db.example.com:5432/app?user=svc&Password=secret&sslmode=require",
			want:  "DATABASE_URL=REDACTED",
		},
		{
			name:  "mongodb URL query password without userinfo",
			input: "MONGO_URL=mongodb://cluster0.example.mongodb.net/app?authSource=admin&username=svc&password=secret",
			want:  "MONGO_URL=REDACTED",
		},
		{
			name:  "mongodb srv URL query password without userinfo",
			input: "MONGO_URL=mongodb+srv://cluster0.example.mongodb.net/app?authSource=admin&username=svc&password=secret",
			want:  "MONGO_URL=REDACTED",
		},
		{
			name:  "placeholder password in database URL query is preserved",
			input: "DATABASE_URL=postgresql://db.example.com/app?user=svc&password=${DB_PASSWORD}",
			want:  "DATABASE_URL=postgresql://db.example.com/app?user=svc&password=${DB_PASSWORD}",
		},
		{
			name:  "jdbc semicolon password",
			input: "jdbc:sqlserver://db.example.com:1433;databaseName=app;user=svc;password=secret;encrypt=true",
			want:  "REDACTED",
		},
		{
			name:  "ado.net quoted password with embedded semicolons",
			input: `conn=Server=db.example.com;User ID=svc;Password="se;cret;here";Encrypt=true`,
			want:  "conn=REDACTED",
		},
		{
			name:  "ado.net single-quoted password with embedded semicolons",
			input: `conn=Server=db.example.com;User ID=svc;Password='se;cret;here';Encrypt=true`,
			want:  "conn=REDACTED",
		},
	})
}

func TestDatabaseConnectionStringRuleScope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		candidate string
		hasSecret func(string) bool
		want      bool
	}{
		{
			name:      "database URL query password is in scope",
			candidate: "postgresql://db.example.com:5432/app?user=svc&password=secret&sslmode=require",
			hasSecret: hasDatabaseURLSecret,
			want:      true,
		},
		{
			name:      "database URL userinfo password is handled by credentialed URI detection",
			candidate: "postgresql://svc:secret@db.example.com:5432/app",
			hasSecret: hasDatabaseURLSecret,
			want:      false,
		},
		{
			name:      "JDBC query password is in scope",
			candidate: "jdbc:postgresql://db.example.com:5432/app?user=svc&password=secret",
			hasSecret: hasJDBCPassword,
			want:      true,
		},
		{
			name:      "JDBC userinfo password is handled by credentialed URI detection",
			candidate: "jdbc:postgresql://svc:secret@db.example.com:5432/app",
			hasSecret: hasJDBCPassword,
			want:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.hasSecret(tt.candidate)
			if got != tt.want {
				t.Errorf("hasSecret(%q) = %v, want %v", tt.candidate, got, tt.want)
			}
		})
	}
}

func TestString_BoundedCredentialValueRedaction(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "db password env var",
			input: "DB_PASSWORD=secret123",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "postgres password env var",
			input: "PGPASSWORD='secret123'",
			want:  "PGPASSWORD='REDACTED'",
		},
		{
			name:  "redis password env var",
			input: `REDIS_PASSWORD="secret123"`,
			want:  `REDIS_PASSWORD="REDACTED"`,
		},
		{
			name:  "lowercase database password",
			input: "database_password=secret123",
			want:  "database_password=REDACTED",
		},
		{
			name:  "prefixed db password env var",
			input: "APP_DB_PASSWORD=secret123",
			want:  "APP_DB_PASSWORD=REDACTED",
		},
		{
			name:  "prefixed mysql password env var",
			input: "PROD_MYSQL_PWD=secret123",
			want:  "PROD_MYSQL_PWD=REDACTED",
		},
		{
			name:  "mysql root password env var",
			input: "MYSQL_ROOT_PASSWORD=secret123",
			want:  "MYSQL_ROOT_PASSWORD=REDACTED",
		},
		{
			name:  "mariadb root password env var",
			input: "MARIADB_ROOT_PASSWORD=secret123",
			want:  "MARIADB_ROOT_PASSWORD=REDACTED",
		},
		{
			name:  "mongo initdb root password env var",
			input: "MONGO_INITDB_ROOT_PASSWORD=secret123",
			want:  "MONGO_INITDB_ROOT_PASSWORD=REDACTED",
		},
		{
			name:  "mssql sa password env var",
			input: "MSSQL_SA_PASSWORD=secret123",
			want:  "MSSQL_SA_PASSWORD=REDACTED",
		},
		{
			name:  "double underscore separator",
			input: "DB__PASSWORD=secret123",
			want:  "DB__PASSWORD=REDACTED",
		},
		{
			name:  "github client secret env var",
			input: "GITHUB_CLIENT_SECRET=correct-horse-client",
			want:  "GITHUB_CLIENT_SECRET=REDACTED",
		},
		{
			name:  "spaces around equals",
			input: "DB_PASSWORD = secret123",
			want:  "DB_PASSWORD = REDACTED",
		},
		{
			name:  "only the assigned line is redacted",
			input: "DB_PASSWORD=secret123\nDB_HOST=localhost\n",
			want:  "DB_PASSWORD=REDACTED\nDB_HOST=localhost\n",
		},
		{
			name:  "shell default with a real value",
			input: "DB_PASSWORD=${DB_PASSWORD:-hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "nested shell default with a real value",
			input: "DB_PASSWORD=${DB_PASSWORD:-${FALLBACK_PASSWORD:-hunter2}}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "format field with digits",
			input: "DB_PASSWORD={hunter2}",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "printf verb with a real tail",
			input: "DB_PASSWORD=%s3cret",
			want:  "DB_PASSWORD=REDACTED",
		},
	})
}

func TestString_BoundedCredentialValueOverRedactionGuards(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "placeholder env var is preserved",
			input: "DB_PASSWORD=${DB_PASSWORD}",
			want:  "DB_PASSWORD=${DB_PASSWORD}",
		},
		{
			name:  "already redacted value is preserved",
			input: "DB_PASSWORD=REDACTED",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "prose about password is preserved",
			input: "the password field should be rotated regularly",
			want:  "the password field should be rotated regularly",
		},
		{
			name:  "generic key is preserved",
			input: "key=not-a-secret-setting",
			want:  "key=not-a-secret-setting",
		},
		{
			name:  "shell pwd is preserved",
			input: "PWD=/workspace/project",
			want:  "PWD=/workspace/project",
		},
		{
			// betterleaks >= 1.8.0 detects generic password assignments.
			name:  "standalone password assignment is redacted by betterleaks",
			input: "password=not-a-secret-setting",
			want:  "password=REDACTED",
		},
		{
			name:  "password reset query parameter is preserved",
			input: "https://example.com/?password_reset=true",
			want:  "https://example.com/?password_reset=true",
		},
		{
			// The greedy match consumes the trailing query param too.
			name:  "generic https password query is redacted by betterleaks",
			input: "https://example.com/callback?user=svc&password=not-a-db-credential&debug=true",
			want:  "https://example.com/callback?user=svc&password=REDACTED",
		},
		{
			name:  "db password hash field is preserved",
			input: "DB_PASSWORD_HASH=abcdef",
			want:  "DB_PASSWORD_HASH=abcdef",
		},
		{
			name:  "non-credential mysql field is preserved",
			input: "MYSQL_USER_ID=alice",
			want:  "MYSQL_USER_ID=alice",
		},
		{
			name:  "angle bracket placeholder is preserved",
			input: "DB_PASSWORD=<password>",
			want:  "DB_PASSWORD=<password>",
		},
		{
			name:  "your_password placeholder is preserved",
			input: "DB_PASSWORD=your_password",
			want:  "DB_PASSWORD=your_password",
		},
		{
			name:  "your-db-password placeholder is preserved",
			input: "DB_PASSWORD=<your-db-password>",
			want:  "DB_PASSWORD=<your-db-password>",
		},
		{
			name:  "asterisk mask placeholder is preserved",
			input: "DB_PASSWORD=*****",
			want:  "DB_PASSWORD=*****",
		},
		{
			name:  "dot mask placeholder is preserved",
			input: "DB_PASSWORD=......",
			want:  "DB_PASSWORD=......",
		},
		{
			name:  "secret_here placeholder is preserved",
			input: "DB_PASSWORD=secret_here",
			want:  "DB_PASSWORD=secret_here",
		},
		{
			// Exact placeholder match suppresses the betterleaks finding.
			name:  "bare placeholder password assignment is preserved",
			input: "password=changeme",
			want:  "password=changeme",
		},
		{
			// Semicolon DSNs terminate the unquoted capture, so the finding
			// is exactly the placeholder.
			name:  "dsn placeholder password is preserved",
			input: "Server=db.internal;Database=prod;User Id=app;Password=changeme;TrustCert=true",
			want:  "Server=db.internal;Database=prod;User Id=app;Password=changeme;TrustCert=true",
		},
		{
			// Greedy findings are redacted whole, never split at a
			// placeholder head — the tail can hold a real secret.
			name:  "placeholder head with real secret in greedy tail is redacted",
			input: "password=changeme&db_pass=hunter2hunter2",
			want:  "password=REDACTED",
		},
		{
			name:  "quoted secret with placeholder first token is redacted",
			input: `password="changeme;realSecret42"`,
			want:  `password="REDACTED"`,
		},
		{
			name:  "placeholder with trailing query params over-redacts",
			input: "password=changeme&sslmode=require",
			want:  "password=REDACTED",
		},
		{
			name:  "real value with trailing query params is still redacted",
			input: "password=hunter2secret&sslmode=require",
			want:  "password=REDACTED",
		},
		{
			name:  "placeholder literal is preserved",
			input: "DB_PASSWORD=placeholder",
			want:  "DB_PASSWORD=placeholder",
		},
		{
			name:  "google adsense account is preserved as identifier",
			input: "GOOGLE_ADSENSE_ACCOUNT=pub-1234567890123456",
			want:  "GOOGLE_ADSENSE_ACCOUNT=pub-1234567890123456",
		},
		{
			name:  "prose with token colon does not over-redact next word",
			input: "Got the token: hunter2 don't merge yet",
			want:  "Got the token: hunter2 don't merge yet",
		},
		{
			name:  "prose with secret colon does not over-redact next word",
			input: "secret: don't merge yet",
			want:  "secret: don't merge yet",
		},
		{
			name:  "prose with api_key colon does not over-redact next word",
			input: "The api_key: was wrong, retry the request",
			want:  "The api_key: was wrong, retry the request",
		},
		{
			name:  "blank value does not take the next line",
			input: "DB_PASSWORD=\nDB_HOST=localhost\n",
			want:  "DB_PASSWORD=\nDB_HOST=localhost\n",
		},
		{
			name:  "blank value with trailing space does not take the next line",
			input: "DB_PASSWORD= \nDB_HOST=localhost\n",
			want:  "DB_PASSWORD= \nDB_HOST=localhost\n",
		},
		{
			name:  "printf verb is preserved",
			input: "DB_PASSWORD=%s",
			want:  "DB_PASSWORD=%s",
		},
		{
			name:  "format field is preserved",
			input: "DB_PASSWORD={db_password}",
			want:  "DB_PASSWORD={db_password}",
		},
		{
			name:  "uppercase shell variable is preserved",
			input: "DB_PASSWORD=$DB_PASSWORD",
			want:  "DB_PASSWORD=$DB_PASSWORD",
		},
		{
			name:  "shell default with a placeholder value is preserved",
			input: "DB_PASSWORD=${DB_PASSWORD:-changeme}",
			want:  "DB_PASSWORD=${DB_PASSWORD:-changeme}",
		},
		{
			name:  "shell default with an empty value is preserved",
			input: "DB_PASSWORD=${DB_PASSWORD:-}",
			want:  "DB_PASSWORD=${DB_PASSWORD:-}",
		},
		{
			// The word after :? is an error message, never a value.
			name:  "shell required-variable check is preserved",
			input: "DB_PASSWORD=${DB_PASSWORD:?required}",
			want:  "DB_PASSWORD=${DB_PASSWORD:?required}",
		},
		{
			name:  "shell required-variable check without colon is preserved",
			input: "DB_PASSWORD=${DB_PASSWORD?required}",
			want:  "DB_PASSWORD=${DB_PASSWORD?required}",
		},
		{
			name:  "shell default naming another variable is preserved",
			input: "DB_PASSWORD=${DB_PASSWORD:-${FALLBACK_PASSWORD}}",
			want:  "DB_PASSWORD=${DB_PASSWORD:-${FALLBACK_PASSWORD}}",
		},
		{
			name:  "github actions expression is preserved",
			input: "DB_PASSWORD=${{secrets.DB_PASSWORD}}",
			want:  "DB_PASSWORD=${{secrets.DB_PASSWORD}}",
		},
		{
			name:  "terraform variable reference is preserved",
			input: "DB_PASSWORD=${var.db_password}",
			want:  "DB_PASSWORD=${var.db_password}",
		},
	})
}

// Pins that single-char "masks" and arbitrary <…> wrappers do NOT count as
// placeholders, so credentials that happen to be short or bracket-wrapped
// still get redacted. The opposite cases (`***`, `<password>`, etc.) are
// covered above in TestString_BoundedCredentialValueOverRedactionGuards.
func TestString_ShortAndOpaquePlaceholdersFallThrough(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "single x is not a mask",
			input: "DB_PASSWORD=x",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "single dash is not a mask",
			input: "DB_PASSWORD=-",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "single asterisk is not a mask",
			input: "DB_PASSWORD=*",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "two-char repeat is not a mask",
			input: "DB_PASSWORD=xx",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "bracketed value with digits is not a placeholder",
			input: "DB_PASSWORD=<hunter2>",
			want:  "DB_PASSWORD=REDACTED",
		},
		{
			name:  "bracketed mixed-case value is not a placeholder",
			input: "DB_PASSWORD=<RealPassword>",
			want:  "DB_PASSWORD=REDACTED",
		},
	})
}

func TestString_OpenSSHPrivateKeyBlock(t *testing.T) {
	input := "key:\n" + fakeOpenSSHPrivateKey + "\nend"
	want := "key:\nREDACTED\nend"

	got := String(input)
	if got != want {
		t.Errorf("String(private key block) = %q, want %q", got, want)
	}
	if strings.Contains(got, openSSHPrivateKeyMarker("BEGIN")) || strings.Contains(got, openSSHPrivateKeyMarker("END")) {
		t.Errorf("private key block markers should be fully redacted, got %q", got)
	}
}

func TestJSONLContent_CredentialedURI(t *testing.T) {
	input := `{"type":"text","content":"DATABASE_URL=postgres://app:pwd123@db.example.com:5432/app"}`
	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(result, "postgres://app:pwd123@db.example.com:5432/app") {
		t.Error("credentialed database URI was not redacted")
	}
	if !strings.Contains(result, "DATABASE_URL=REDACTED") {
		t.Errorf("expected credentialed URI replacement, got %q", result)
	}
}

func TestJSONLContent_OpenSSHPrivateKeyBlock(t *testing.T) {
	content, err := json.Marshal("key:\n" + fakeOpenSSHPrivateKey + "\nend")
	if err != nil {
		t.Fatalf("marshal content: %v", err)
	}
	input := `{"type":"text","content":` + string(content) + `}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, openSSHPrivateKeyMarker("BEGIN")) || strings.Contains(result, openSSHPrivateKeyMarker("END")) {
		t.Errorf("private key block markers should be fully redacted, got %q", result)
	}
	if !strings.Contains(result, `key:\nREDACTED\nend`) {
		t.Errorf("expected whole private key block replacement, got %q", result)
	}
}

func TestJSONLContent_DatabaseCredentialRedaction(t *testing.T) {
	t.Parallel()
	input := `{"type":"assistant","message":"dsn host=db.example.com user=svc password=secret dbname=app and env DB_PASSWORD=secret123","session_id":"ses_37273a1fdffegpYbwUTqEkPsQ0","file_path":"/tmp/TestE2E_ExistingFiles/controller.go"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, leaked := range []string{"password=secret", "DB_PASSWORD=secret123"} {
		if strings.Contains(result, leaked) {
			t.Fatalf("expected %q to be redacted, got: %s", leaked, result)
		}
	}
	for _, preserved := range []string{"ses_37273a1fdffegpYbwUTqEkPsQ0", "/tmp/TestE2E_ExistingFiles/controller.go"} {
		if !strings.Contains(result, preserved) {
			t.Fatalf("expected structural value %q to be preserved, got: %s", preserved, result)
		}
	}
}

func TestJSONLContent_StructuredCredentialFieldsRedacted(t *testing.T) {
	t.Parallel()
	input := `{"type":"assistant","env":{"DB_PASSWORD":"correct-horse-db","GITHUB_CLIENT_SECRET":"correct-horse-client","REDIS_PASSWORD":"${REDIS_PASSWORD}","note":"correct-horse-db"},"db":{"password":"correct-horse-db","host":"db.example.com","user":"svc"},"session_id":"ses_37273a1fdffegpYbwUTqEkPsQ0"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, leaked := range []string{`"DB_PASSWORD":"correct-horse-db"`, `"GITHUB_CLIENT_SECRET":"correct-horse-client"`, `"password":"correct-horse-db"`} {
		if strings.Contains(result, leaked) {
			t.Fatalf("expected structured credential field %q to be redacted, got: %s", leaked, result)
		}
	}
	for _, preserved := range []string{
		`"DB_PASSWORD":"REDACTED"`,
		`"GITHUB_CLIENT_SECRET":"REDACTED"`,
		`"REDIS_PASSWORD":"${REDIS_PASSWORD}"`,
		`"password":"REDACTED"`,
		`"host":"db.example.com"`,
		`"user":"svc"`,
		`"note":"correct-horse-db"`,
		"ses_37273a1fdffegpYbwUTqEkPsQ0",
	} {
		if !strings.Contains(result, preserved) {
			t.Fatalf("expected %q to be preserved, got: %s", preserved, result)
		}
	}
}

func TestJSONLContent_NormalizedCredentialKeysRedacted(t *testing.T) {
	t.Parallel()
	input := `{"type":"assistant","env":{"DB Password":"correct-horse-db","note":"correct-horse-db"},"session_id":"ses_37273a1fdffegpYbwUTqEkPsQ0"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, preserved := range []string{
		`"DB Password":"REDACTED"`,
		`"note":"correct-horse-db"`,
		"ses_37273a1fdffegpYbwUTqEkPsQ0",
	} {
		if !strings.Contains(result, preserved) {
			t.Fatalf("expected %q to be preserved, got: %s", preserved, result)
		}
	}
	if strings.Contains(result, `"DB Password":"correct-horse-db"`) {
		t.Fatalf("expected normalized credential key to be redacted, got: %s", result)
	}
}

func TestJSONLContent_ShellStdinSecretCommandRedactsPrintfLiteral(t *testing.T) {
	t.Parallel()
	keyName := strings.Join([]string{"EXAMPLE", "API", "KEY"}, "_")
	identifierKeyName := strings.Join([]string{"AWS", "ACCESS", "KEY", "ID"}, "_")
	secret := "correct-horse-client"
	identifierSecret := "correct-horse-identifier"
	input := `{"type":"user","message":{"content":[{"type":"tool_result","content":"tail -2 && printf '` + secret + `' | examplectl secret put ` + keyName + ` 2>&1 | tail -2\nprintf '` + identifierSecret + `' | examplectl secrets put ` + identifierKeyName + `"}]}}`

	result := mustJSONLContent(t, input)
	if strings.Contains(result, secret) || strings.Contains(result, identifierSecret) {
		t.Fatalf("expected shell stdin secret literal to be redacted, got: %s", result)
	}
	if !strings.Contains(result, "printf 'REDACTED' | examplectl secret put "+keyName) {
		t.Fatalf("expected command context to be preserved, got: %s", result)
	}
	if !strings.Contains(result, "printf 'REDACTED' | examplectl secrets put "+identifierKeyName) {
		t.Fatalf("expected credential identifier command context to be preserved, got: %s", result)
	}
}

func TestJSONLContent_ShellStdinSecretCommandOverRedactionGuards(t *testing.T) {
	t.Parallel()
	keyName := strings.Join([]string{"EXAMPLE", "API", "KEY"}, "_")
	clientIDName := strings.Join([]string{"EXAMPLE", "CLIENT", "ID"}, "_")
	input := `{"type":"user","message":{"content":[{"type":"tool_result","content":"printf 'example-client-id.apps.example.test' | examplectl secret put ` + clientIDName + `\nprintf \"$` + keyName + `\" | examplectl secret put ` + keyName + `"}]}}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, preserved := range []string{
		"printf 'example-client-id.apps.example.test' | examplectl secret put " + clientIDName,
		`printf \"$` + keyName + `\" | examplectl secret put ` + keyName,
	} {
		if !strings.Contains(result, preserved) {
			t.Fatalf("expected %q to be preserved, got: %s", preserved, result)
		}
	}
}

func TestJSONLContent_DottedCredentialKeysRedacted(t *testing.T) {
	t.Parallel()
	input := `{"config":{"db.password":"correct-horse-db","mysql.root.password":"correct-horse-mysql","note":"correct-horse-db"}}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, redacted := range []string{
		`"db.password":"REDACTED"`,
		`"mysql.root.password":"REDACTED"`,
	} {
		if !strings.Contains(result, redacted) {
			t.Fatalf("expected %q in output, got: %s", redacted, result)
		}
	}
	if !strings.Contains(result, `"note":"correct-horse-db"`) {
		t.Fatalf("expected unrelated note field to be preserved, got: %s", result)
	}
}

func TestJSONLContent_RootPasswordJSONKeysRedacted(t *testing.T) {
	t.Parallel()
	input := `{"env":{"MYSQL_ROOT_PASSWORD":"correct-horse-mysql","MONGO_INITDB_ROOT_PASSWORD":"correct-horse-mongo","MSSQL_SA_PASSWORD":"correct-horse-mssql"}}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, redacted := range []string{
		`"MYSQL_ROOT_PASSWORD":"REDACTED"`,
		`"MONGO_INITDB_ROOT_PASSWORD":"REDACTED"`,
		`"MSSQL_SA_PASSWORD":"REDACTED"`,
	} {
		if !strings.Contains(result, redacted) {
			t.Fatalf("expected %q in output, got: %s", redacted, result)
		}
	}
	for _, leaked := range []string{"correct-horse-mysql", "correct-horse-mongo", "correct-horse-mssql"} {
		if strings.Contains(result, leaked) {
			t.Fatalf("expected %q to be redacted, got: %s", leaked, result)
		}
	}
}

func TestShouldSkipJSONLObject(t *testing.T) {
	tests := []struct {
		name string
		obj  map[string]any
		want bool
	}{
		{
			name: "image type is skipped",
			obj:  map[string]any{"type": "image", "data": "base64data"},
			want: true,
		},
		{
			name: "text type is not skipped",
			obj:  map[string]any{"type": "text", "content": "hello"},
			want: false,
		},
		{
			name: "no type field is not skipped",
			obj:  map[string]any{"content": "hello"},
			want: false,
		},
		{
			name: "non-string type is not skipped",
			obj:  map[string]any{"type": 42},
			want: false,
		},
		{
			name: "image_url type is skipped",
			obj:  map[string]any{"type": "image_url"},
			want: true,
		},
		{
			name: "base64 type is skipped",
			obj:  map[string]any{"type": "base64"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipJSONLObject(tt.obj)
			if got != tt.want {
				t.Errorf("shouldSkipJSONLObject(%v) = %v, want %v", tt.obj, got, tt.want)
			}
		})
	}
}

func TestShouldSkipJSONLObject_RedactionBehavior(t *testing.T) {
	// Verify that secrets inside image objects are NOT redacted.
	obj := map[string]any{
		"type": "image",
		"data": highEntropySecret,
	}
	repls := collectJSONLReplacements(obj, regexLayers)

	// expect no replacements, it's an image which is skipped.
	var wantRepls []jsonReplacement
	if !slices.Equal(repls, wantRepls) {
		t.Errorf("got %+v, want %+v", repls, wantRepls)
	}

	// Verify that secrets inside non-image objects ARE redacted.
	obj2 := map[string]any{
		"type":    "text",
		"content": highEntropySecret,
	}
	repls2 := collectJSONLReplacements(obj2, regexLayers)
	wantRepls2 := []jsonReplacement{{key: "content", original: highEntropySecret, redacted: "REDACTED"}}
	if !slices.Equal(repls2, wantRepls2) {
		t.Errorf("got %+v, want %+v", repls2, wantRepls2)
	}
}

func TestString_FilePaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "temp directory path preserves filenames",
			input: "/tmp/TestE2E_Something3407889464/001/controller.go",
			want:  "/tmp/TestE2E_Something3407889464/001/controller.go",
		},
		{
			name:  "macOS private var folders path",
			input: "/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/TestE2E_Something/controller",
			want:  "/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/TestE2E_Something/controller",
		},
		{
			name:  "simple Go file path",
			input: "Reading file: /tmp/test/model.go",
			want:  "Reading file: /tmp/test/model.go",
		},
		{
			name:  "user home directory path",
			input: "/Users/peytonmontei/.claude/projects/something.jsonl",
			want:  "/Users/peytonmontei/.claude/projects/something.jsonl",
		},
		{
			name:  "multiple paths separated by newlines",
			input: "/tmp/test/controller.go\n/tmp/test/model.go\n/tmp/test/view.go",
			want:  "/tmp/test/controller.go\n/tmp/test/model.go\n/tmp/test/view.go",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestString_JSONEscapeSequences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "newline escape not corrupted",
			input: `controller.go\nmodel.go\nview.go`,
			want:  `controller.go\nmodel.go\nview.go`,
		},
		{
			name:  "tab escape not corrupted",
			input: `something.go\tanother.go`,
			want:  `something.go\tanother.go`,
		},
		{
			name:  "backslash escape not corrupted",
			input: `C:\\Users\\test\\file.go`,
			want:  `C:\\Users\\test\\file.go`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := String(tt.input)
			if got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestString_RealSecretsStillCaught(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "high entropy API key",
			input: "api_key=" + highEntropySecret,
		},
		{
			name:  "AWS access key (pattern-based)",
			input: "key=" + awsKeyFixture,
		},
		{
			name:  "GitHub personal access token",
			input: "token=ghp_1234567890abcdefghijklmnopqrstuvwxyzAB",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := String(tt.input)
			if !strings.Contains(got, "REDACTED") {
				t.Errorf("String(%q) = %q, expected REDACTED somewhere", tt.input, got)
			}
		})
	}
}

func TestJSONLContent_PathFieldsPreserved(t *testing.T) {
	t.Parallel()
	// Simulates a real agent log line with path fields that should NOT be redacted
	input := `{"session_id":"ses_37273a1fdffegpYbwUTqEkPsQ0","file_path":"/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/test/controller.go","cwd":"/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/test","root":"/private/var/folders/v4/31cd3cg52_sfrpb1mbtr7q7r0000gn/T/test","directory":"/tmp/TestE2E_ExistingFiles","content":"normal text here"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Structural fields should be preserved
	mustContain := []string{
		"ses_37273a1fdffegpYbwUTqEkPsQ0", // session_id (skipped by *id rule)
		"/private/var/folders",           // file_path (skipped by path rule)
		"controller.go",                  // filename in file_path
		"/tmp/TestE2E_ExistingFiles",     // directory (skipped by path rule)
	}
	for _, s := range mustContain {
		if !strings.Contains(result, s) {
			t.Errorf("expected %q to be preserved, but result is: %s", s, result)
		}
	}

	// No false positives
	if strings.Contains(result, "REDACTED") {
		t.Errorf("expected no redactions in structural fields, got: %s", result)
	}
}

func TestJSONLContent_PrettyPrintedJSON_IDsPreserved(t *testing.T) {
	t.Parallel()
	// Simulates OpenCode's pretty-printed JSON export format.
	// High-entropy IDs (like msg_cb99a444f001Ftd3kTVmr8XQHZ with entropy > 4.5)
	// must be preserved. Before the fix, line-by-line processing couldn't parse
	// individual lines of pretty-printed JSON and fell back to entropy-based
	// redaction, corrupting these IDs.
	input := `{
  "info": {
    "id": "ses_309461a8bffeQfY7CYDOUHX6VP",
    "slug": "misty-river",
    "directory": "/tmp/test-repo"
  },
  "messages": [
    {
      "info": {
        "id": "msg_cb99a444f001Ftd3kTVmr8XQHZ",
        "sessionID": "ses_309461a8bffeQfY7CYDOUHX6VP",
        "role": "user"
      },
      "parts": [
        {
          "id": "prt_cb99a443b001GE99vjBG60vHbF",
          "type": "text",
          "text": "hello world"
        }
      ]
    },
    {
      "info": {
        "id": "msg_cb99a444f001Ftd3kTVmr8XQHZ",
        "sessionID": "ses_309461a8bffeQfY7CYDOUHX6VP",
        "role": "assistant"
      },
      "parts": [
        {
          "id": "prt_cb99a6f2e0012koCcOJBSwRBwR",
          "type": "text",
          "text": "hello back"
        },
        {
          "id": "prt_cb99a6f2f001e98CKuwDKU3oWr",
          "type": "tool",
          "tool": "write",
          "callID": "call_abc123",
          "state": {
            "status": "completed",
            "input": {"filePath": "/tmp/test/hello.md"},
            "output": "wrote file",
            "metadata": {"files": [{"filePath": "/tmp/test/hello.md", "relativePath": "hello.md"}]}
          }
        }
      ]
    }
  ]
}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the entropy threshold: msg_cb99a444f001Ftd3kTVmr8XQHZ has entropy > 4.5
	// and would be redacted by String() if processed line-by-line.
	entropy := shannonEntropy("msg_cb99a444f001Ftd3kTVmr8XQHZ")
	if entropy <= entropyThreshold {
		t.Fatalf("test assumption broken: msg ID entropy %.2f should be > %.1f", entropy, entropyThreshold)
	}

	// All IDs must be preserved (they're in "id"/"sessionID" fields which are skipped).
	mustContain := []string{
		"ses_309461a8bffeQfY7CYDOUHX6VP",
		"msg_cb99a444f001Ftd3kTVmr8XQHZ",
		"prt_cb99a443b001GE99vjBG60vHbF",
		"prt_cb99a6f2e0012koCcOJBSwRBwR",
		"prt_cb99a6f2f001e98CKuwDKU3oWr",
	}
	for _, s := range mustContain {
		if !strings.Contains(result, s) {
			t.Errorf("expected ID %q to be preserved, but it was corrupted in result", s)
		}
	}

	// No false positives on structural data.
	if strings.Contains(result, "REDACTED") {
		t.Errorf("expected no redactions in OpenCode export, got redacted content")
	}
}

func TestJSONLContent_PrettyPrintedJSON_SecretsStillCaught(t *testing.T) {
	t.Parallel()
	// Even in pretty-printed JSON mode, actual secrets in content fields should
	// still be redacted.
	input := `{
  "info": {
    "id": "ses_test123"
  },
  "messages": [
    {
      "info": {
        "id": "msg_test456",
        "role": "assistant"
      },
      "parts": [
        {
          "id": "prt_test789",
          "type": "text",
          "text": "your api key is ` + highEntropySecret + `"
        }
      ]
    }
  ]
}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Secret in text content should be redacted.
	if strings.Contains(result, highEntropySecret) {
		t.Error("secret in text field was not redacted")
	}
	if !strings.Contains(result, "REDACTED") {
		t.Error("expected REDACTED in output")
	}

	// IDs should still be preserved.
	for _, id := range []string{"ses_test123", "msg_test456", "prt_test789"} {
		if !strings.Contains(result, id) {
			t.Errorf("ID %q should be preserved", id)
		}
	}
}

func TestJSONLContent_SecretsInContentStillCaught(t *testing.T) {
	t.Parallel()
	// Path fields should be preserved, but secrets in content should be caught
	input := `{"file_path":"/tmp/test.go","content":"api_key=` + highEntropySecret + `"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// file_path should be preserved
	if !strings.Contains(result, "/tmp/test.go") {
		t.Error("file_path was incorrectly modified")
	}

	// Secret in content should be redacted
	if strings.Contains(result, highEntropySecret) {
		t.Error("secret in content field was not redacted")
	}
	if !strings.Contains(result, "REDACTED") {
		t.Error("expected REDACTED in output")
	}
}

// Pins a known gap: shell shorthand `--password=...` is not redacted because
// no detector matches `--password=` (no DB-prefix, no DSN structure, no URI).
func TestString_MysqlShellShorthandIsNotRedacted(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "mysql cli flag",
			input: "mysql -u svc --password=hunter2 -h db.example.com app",
			want:  "mysql -u svc --password=hunter2 -h db.example.com app",
		},
		{
			name:  "psql cli flag",
			input: "psql --password=hunter2 -U svc -h db.example.com app",
			want:  "psql --password=hunter2 -U svc -h db.example.com app",
		},
	})
}

// Pins f(f(x)) == f(x): once-redacted output must not match any detector on
// a second pass.
func TestString_RedactionIsIdempotent(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"DATABASE_URL=postgres://svc:hunter2@db.example.com/app",
		"DB_PASSWORD=hunter2",
		`conn=Server=db.example.com;User ID=svc;Password="se;cret;here";Encrypt=true`,
		"jdbc:postgresql://db.example.com:5432/app?user=svc&password=hunter2",
		"my key is " + highEntropySecret + " ok",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			once := String(input)
			twice := String(once)
			if once != twice {
				t.Errorf("not idempotent for %q:\n  once:  %q\n  twice: %q", input, once, twice)
			}
		})
	}
}

// Regression pins for the `*_account` skip-list hazard: account-named fields
// must keep value-level scanning. A high-entropy or provider-shaped secret in
// an account field (including the nested private_key of a GCP
// service_account JSON, the canonical credential container) must redact,
// while low-entropy account identifiers are preserved by the entropy
// threshold on their own — which is what fixes the original
// GOOGLE_ADSENSE_ACCOUNT over-redaction without a key-name bypass.
func TestJSONLContent_AccountJSONKeysStayScanned(t *testing.T) {
	t.Parallel()
	redactedCases := []string{
		`{"google_adsense_account":"` + highEntropySecret + `"}`,
		`{"aws_account":"` + highEntropySecret + `"}`,
		`{"service_account":"` + highEntropySecret + `"}`,
		`{"service_account":{"private_key":"` + highEntropySecret + `"}}`,
		`{"service_account":["` + highEntropySecret + `"]}`,
	}
	for _, in := range redactedCases {
		out := mustJSONLContent(t, in)
		if strings.Contains(out, highEntropySecret) {
			t.Errorf("high-entropy secret in account field must redact, got: %s", out)
		}
	}

	preservedCases := []string{
		`{"google_adsense_account":"pub-1234567890123456"}`,
		`{"aws_account":"123456789012"}`,
	}
	for _, in := range preservedCases {
		out := mustJSONLContent(t, in)
		if out != in {
			t.Errorf("low-entropy account identifier must be preserved, got: %s", out)
		}
	}
}

// Pins that JSON keys with known non-secret prefixes (cancel/pagination/
// continuation/idempotency/cursor/next/previous/page/sync) followed by
// `_token` are NOT flat-redacted. These are routinely safe debug/pagination
// identifiers. Real high-entropy credential-shaped values still get caught
// by entropy + Betterleaks via the per-value String() call. The allowlist
// deliberately does NOT extend to `_secret` keys — see
// TestJSONLContent_RotationSecretKeysRedacted.
func TestJSONLContent_NonSecretTokenPrefixesPreserved(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"cancel_token":"abc-123-xyz-cancel-debug"}`,
		`{"pagination_token":"page_500_offset_10000"}`,
		`{"continuation_token":"foo_bar_baz_continuation_12345"}`,
		`{"idempotency_token":"req-7f2a-deadbeef"}`,
		`{"cursor_token":"opaque-cursor-value"}`,
		`{"next_token":"page-2-marker"}`,
		`{"previous_token":"page-1-marker"}`,
		`{"prev_token":"page-1-marker"}`,
		`{"page_token":"page-2-marker"}`,
		`{"sync_token":"sync-state-marker"}`,
	}
	for _, in := range cases {
		out := mustJSONLContent(t, in)
		if out != in {
			t.Errorf("expected %q preserved (non-secret prefix), got: %s", in, out)
		}
	}
}

// Pins that bare `{"token":"…"}` and `{"secret":"…"}` JSON fields are NOT
// flat-redacted by the structural layer. Standalone `token`/`secret` keys
// appear in many non-credential contexts (debug, request IDs, etc.); real
// high-entropy values still get caught by entropy + Betterleaks.
func TestJSONLContent_BareTokenSecretJSONKeysPreserved(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"token":"abc-def-ghi-not-a-secret-debug-id"}`,
		`{"secret":"abc-def-ghi-not-a-secret-debug-id"}`,
	}
	for _, in := range cases {
		out := mustJSONLContent(t, in)
		if out != in {
			t.Errorf("expected %q preserved (bare token/secret keys are not credentials), got: %s", in, out)
		}
	}
}

// Pins that prefixed `*_token` / `*_secret` JSON fields continue to redact —
// these are the structural credential shapes the layer targets.
func TestJSONLContent_PrefixedTokenSecretJSONKeysRedacted(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, wantSub string }{
		{`{"csrf_token":"abc-csrf-real-secret"}`, `"csrf_token":"REDACTED"`},
		{`{"auth_token":"abc-auth-real-secret"}`, `"auth_token":"REDACTED"`},
		{`{"oauth_token":"abc-oauth-real-secret"}`, `"oauth_token":"REDACTED"`},
		{`{"random_token":"abc-random-real-secret"}`, `"random_token":"REDACTED"`},
		{`{"api_key":"abc-api-real-secret"}`, `"api_key":"REDACTED"`},
		{`{"client_secret":"abc-client-real-secret"}`, `"client_secret":"REDACTED"`},
		{`{"refresh_token":"abc-refresh-real-secret"}`, `"refresh_token":"REDACTED"`},
	}
	for _, c := range cases {
		out := mustJSONLContent(t, c.in)
		if !strings.Contains(out, c.wantSub) {
			t.Errorf("input %q: expected %q in output, got: %s", c.in, c.wantSub, out)
		}
	}
}

// Pins keyed-JSON replacement as (key, value) rather than (path, value): a
// shared value under the same key name redacts in every context, not just
// the credential one. Conservative on purpose — flag if changed.
func TestJSONLContent_CrossContextValueCollision(t *testing.T) {
	t.Parallel()
	input := `{"db":{"host":"db.example.com","user":"svc","password":"shared-secret"},"misc":{"password":"shared-secret"}}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, "shared-secret") {
		t.Errorf("expected shared-secret to be redacted in both contexts, got: %s", result)
	}
	if strings.Count(result, `"password":"REDACTED"`) != 2 {
		t.Errorf("expected both password fields redacted, got: %s", result)
	}
}

// Rotation-secret field names (previous_secret/next_secret/sync_secret) are
// real webhook/HMAC rotation payload keys, and hex values can never trip the
// entropy layer (16 symbols cap Shannon entropy at 4.0 < 4.5), so the
// non-secret prefix allowlist must not extend to the `_secret` suffix.
func TestJSONLContent_RotationSecretKeysRedacted(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"previous_secret":"9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a3928"}`,
		`{"next_secret":"deadbeefcafef00ddeadbeefcafef00d"}`,
		`{"sync_secret":"0123456789abcdef0123456789abcdef"}`,
		`{"cancel_secret":"deadbeefcafef00ddeadbeefcafef00d"}`,
	}
	for _, in := range cases {
		out := mustJSONLContent(t, in)
		if !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("rotation-style secret must redact (hex defeats the entropy fallback), got: %s", out)
		}
	}
}

// A single-quoted shell value is a literal — `'$uperSecret123'` is a real
// password convention ($ as leet S), never a variable expansion. The
// shell-variable placeholder rule must only unredact unquoted or
// double-quoted $-leading values.
func TestString_SingleQuotedDollarPasswordRedacted(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "single-quoted $-leading password is a literal, not a var ref",
			input: `DB_PASSWORD='$uperSecret123'`,
			want:  `DB_PASSWORD='REDACTED'`,
		},
		{
			name:  "single-quoted ${...} shape is also a literal",
			input: `DB_PASSWORD='${notAVar}'`,
			want:  `DB_PASSWORD='REDACTED'`,
		},
		{
			name:  "unquoted $VAR stays a placeholder",
			input: `DB_PASSWORD=$DB_PASS`,
			want:  `DB_PASSWORD=$DB_PASS`,
		},
		{
			name:  "double-quoted $VAR stays a placeholder",
			input: `DB_PASSWORD="$DB_PASS"`,
			want:  `DB_PASSWORD="$DB_PASS"`,
		},
	})
}

// Pins the high-entropy escape hatch inside isNonSecretIdentifierAssignment:
// an identifier-named key carrying a genuinely random value must still
// redact on the raw-text path. Removing the isHighEntropySecretCandidate
// check silently reopens this leak.
func TestString_IdentifierKeyWithHighEntropyValueStillRedacts(t *testing.T) {
	t.Parallel()
	in := "SERVICE_ACCOUNT=" + highEntropySecret
	out := String(in)
	if strings.Contains(out, highEntropySecret) {
		t.Fatalf("high-entropy value under identifier-named key must redact, got: %s", out)
	}
}

// Pins the zero-separator branch of secretValueKeyShape ([_-]*): separator-less
// credential keys are matched by design in both the env-var and JSONL paths.
func TestString_NoSeparatorCredentialKeysRedacted(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "APIKEY without separator",
			input: `APIKEY=correct-horse-battery`,
			want:  `APIKEY=REDACTED`,
		},
		{
			name:  "clientsecret without separator",
			input: `clientsecret=correct-horse-battery`,
			want:  `clientsecret=REDACTED`,
		},
	})
}

func TestJSONLContent_NoSeparatorCredentialJSONKeyRedacted(t *testing.T) {
	t.Parallel()
	out := mustJSONLContent(t, `{"clientsecret":"correct-horse-battery"}`)
	if !strings.Contains(out, RedactedPlaceholder) {
		t.Fatalf("separator-less credential JSON key must redact, got: %s", out)
	}
}

// Pins every verb and both noun forms of the shell-stdin pattern so no
// alternative can be silently dropped.
func TestString_ShellStdinSecretVerbMatrix(t *testing.T) {
	t.Parallel()
	for _, noun := range []string{"secret", "secrets"} {
		for _, verb := range []string{"put", "set", "add", "create"} {
			in := `printf 'literal-secret-value' | examplectl ` + noun + ` ` + verb + ` deploy_token`
			out := String(in)
			if strings.Contains(out, "literal-secret-value") {
				t.Errorf("%s %s: literal not redacted: %s", noun, verb, out)
			}
		}
	}
}

func TestString_GenericPasswordCredentialContexts(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "uppercase password env assignment",
			input: `PASSWORD=hunter2`,
			want:  `PASSWORD=REDACTED`,
		},
		{
			name:  "uppercase password shell-stdin key",
			input: `printf 'hunter2' | examplectl secrets put PASSWORD`,
			want:  `printf 'REDACTED' | examplectl secrets put PASSWORD`,
		},
		{
			name:  "lowercase password shell-stdin key",
			input: `printf 'hunter2' | examplectl secrets put password`,
			want:  `printf 'REDACTED' | examplectl secrets put password`,
		},
	})
}

// Pins the {1,512} literal bound: an oversized "literal" is not eaten by the
// shell-stdin layer — the bound is the layer's only span-size safety valve.
func TestString_ShellStdinSecretLiteralBoundHolds(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("m", 600)
	in := `printf '` + huge + `' | examplectl secrets put deploy_token`
	out := String(in)
	if !strings.Contains(out, huge) {
		t.Fatalf("oversized literal should not be redacted by the bounded shell-stdin layer, got: %s", out)
	}
}

// Corpus-derived over-redaction guards: each case mangled a real transcript
// (Go code, test output, or documentation) before the corresponding
// constraint was added to secretValuePattern / the placeholder rules.
func TestString_CorpusDerivedCodeContextGuards(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "Go equality comparison is not an assignment",
			input: `if apiKey == "" { return nil }`,
			want:  `if apiKey == "" { return nil }`,
		},
		{
			name:  "test banner after a _token word across a newline is not an assignment",
			input: "RUN TestProbe/cross_host_leaks_token\n=== RUN TestNext",
			want:  "RUN TestProbe/cross_host_leaks_token\n=== RUN TestNext",
		},
		{
			name:  "quoted value cannot span lines to a later quote",
			input: "input: \"api_key=\" + highEntropySecretVar,\nname: \"AWS access key\",",
			want:  "input: \"api_key=\" + highEntropySecretVar,\nname: \"AWS access key\",",
		},
		{
			name:  "command substitution is a placeholder recipe, not a secret",
			input: `ENTIRE_TOKEN="$(cat token)"`,
			want:  `ENTIRE_TOKEN="$(cat token)"`,
		},
		{
			name:  "Python credential assignment from method call is code",
			input: `access_token = self.refresh_token()`,
			want:  `access_token = self.refresh_token()`,
		},
		{
			name:  "credential assignment from function call is code",
			input: `client_secret = fetch_secret()`,
			want:  `client_secret = fetch_secret()`,
		},
		{
			name:  "JavaScript credential assignment from function call is code",
			input: `auth_token = getAuthToken()`,
			want:  `auth_token = getAuthToken()`,
		},
		{
			name:  "credential assignment from member reference is code",
			input: `client_secret = config.client_secret`,
			want:  `client_secret = config.client_secret`,
		},
		{
			name:  "dotted credential literal is not a code reference",
			input: `client_secret = alpha.bravo.charlie`,
			want:  `client_secret = REDACTED`,
		},
		{
			name:  "async credential assignment is code",
			input: `installationToken = await getInstallationToken()`,
			want:  `installationToken = await getInstallationToken()`,
		},
		{
			name:  "Go template environment reference is a placeholder",
			input: `telemetry.PostHogAPIKey={{.Env.POSTHOG_API_KEY}}`,
			want:  `telemetry.PostHogAPIKey={{.Env.POSTHOG_API_KEY}}`,
		},
		{
			name:  "printf formatting verb is a placeholder",
			input: `GetToken = %v, from cache=%v`,
			want:  `GetToken = %v, from cache=%v`,
		},
		{
			name:  "spaced credential assignment with a literal still redacts",
			input: `client_secret = correct-horse-client`,
			want:  `client_secret = REDACTED`,
		},
		{
			name:  "uppercase bracketed placeholder preserved",
			input: `subject_token = <ENTIRE_TOKEN>`,
			want:  `subject_token = <ENTIRE_TOKEN>`,
		},
		{
			name:  "base64 padding at value end still redacts",
			input: `api_key=dGVzdHZhbHVlCg==`,
			want:  `api_key=REDACTED`,
		},
	})
}

// Trail-finding regression: Shannon entropy is not monotonic under
// concatenation, so key=value can clear the 4.5 threshold while the value
// alone does not (aws_access_key_id=AKIA… is 4.50 combined, 3.68 alone) —
// and betterleaks' AWS rule filters these moderate-entropy forms, so no
// layer backstops the identifier guard. A credential word inside the key
// must therefore disqualify suppression even under an _id/_account suffix.
func TestString_CredentialWordInIdentifierKeyStillRedacts(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "aws access key id (canonical docs example value)",
			input: "aws_access_key_id=" + awsDocsKeyFixture,
			want:  "REDACTED",
		},
		{
			// The regression window is mixed-case key=value forms: an
			// all-uppercase key shares the value's character set and keeps
			// combined entropy below 4.5 (leaks on main too — out of scope).
			name:  "aws access key id (lowercase key, realistic value; combined 4.58)",
			input: "aws_access_key_id=" + awsRealisticKeyFixture,
			want:  "REDACTED",
		},
		{
			name:  "token-flavored id key",
			input: "api_token_id=Kj8mN3pQ7rXw2Zv5Yb9c",
			want:  "REDACTED",
		},
		{
			// An api_token_id names a token, like main's api_key_id: only
			// the entropy guard applies, and this value is below it.
			name:  "token-flavored id key with low-entropy value is an identifier",
			input: "api_token_id=correct-horse-battery",
			want:  "api_token_id=correct-horse-battery",
		},
		{
			name:  "access-key id with low-entropy value",
			input: "aws_access_key_id=correct-horse-battery",
			want:  "aws_access_key_id=REDACTED",
		},
		{
			name:  "pure identifier keys still suppress the combined-entropy FP",
			input: "GOOGLE_ADSENSE_ACCOUNT=pub-1234567890123456",
			want:  "GOOGLE_ADSENSE_ACCOUNT=pub-1234567890123456",
		},
		{
			name:  "user id with random-looking value stays preserved (identifier semantics)",
			input: "tenant_id=Kj8mN3pQ7rXw2Zv5Yb9c",
			want:  "tenant_id=Kj8mN3pQ7rXw2Zv5Yb9c",
		},
	})
}

func TestString_OrdinaryCredentialNounIdentifiersPreserved(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{name: "authorization id", input: "auth_id=auth-123", want: "auth_id=auth-123"},
		{name: "WebAuthn credential id abbreviation", input: "cred_id=Kj8mN3pQ7rXw2Zv5Yb9c", want: "cred_id=Kj8mN3pQ7rXw2Zv5Yb9c"},
		{name: "certificate id", input: "cert_id=Kj8mN3pQ7rXw2Zv5Yb9c", want: "cert_id=Kj8mN3pQ7rXw2Zv5Yb9c"},
		{name: "WebAuthn credential id", input: "credential_id=Kj8mN3pQ7rXw2Zv5Yb9c", want: "credential_id=Kj8mN3pQ7rXw2Zv5Yb9c"},
	})
}

// Trail-finding regression: compound pagination keys whose every segment is
// allowlisted (next_page_token) must stay readable, while a single
// non-allowlisted segment (auth_page_token) still redacts.
func TestJSONLContent_CompoundNonSecretTokenKeys(t *testing.T) {
	t.Parallel()
	preserved := []string{
		`{"next_page_token":"page-2-opaque-marker"}`,
		`{"prev_page_token":"page-1-opaque-marker"}`,
		`{"next_sync_token":"sync-state-marker"}`,
	}
	for _, in := range preserved {
		out := mustJSONLContent(t, in)
		if out != in {
			t.Errorf("compound allowlisted token key must be preserved, got: %s", out)
		}
	}
	redacted := []string{
		`{"auth_page_token":"opaque-value-here"}`,
		`{"session_next_token":"opaque-value-here"}`,
	}
	for _, in := range redacted {
		out := mustJSONLContent(t, in)
		if !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("token key with non-allowlisted segment must redact, got: %s", out)
		}
	}
}

func TestConfigureScanners_GoredactLayer(t *testing.T) {
	t.Cleanup(func() {
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore default scanners: %v", err)
		}
	})

	// Baseline: both scanners off (legal at the package level; the settings
	// layer forbids this combination in production). This LOW-entropy,
	// shape-valid PAT (entropy ≈3.22 full token, ≈2.99 over the suffix — below
	// layer 1's 4.5 threshold) survives
	// unchanged, proving no non-scanner layer catches it — every redaction
	// asserted below is scanner-attributable — and pinning the betterleaks
	// gate-off direction (betterleaks-only DOES redact this PAT).
	if err := ConfigureScanners(ScannersConfig{}); err != nil {
		t.Fatalf("ConfigureScanners(none): %v", err)
	}
	in := "auth with token ghp_a1b2c1d2e1f2g1h2a1b2c1d2e1f2g1h2a1b2"
	if got := String(in); got != in {
		t.Errorf("no-scanners String() = %q, want input preserved", got)
	}

	// goredact-only: with betterleaks gated off, a redaction of the PAT can
	// only come from goredact's shape-only GitHubPAT validator. This is the
	// non-vacuous wiring proof for the OnFinding collector and offset
	// conversion.
	if err := ConfigureScanners(ScannersConfig{Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners: %v", err)
	}
	if got := String(in); strings.Contains(got, "ghp_a1b2c1d2e1f2g1h2a1b2c1d2e1f2g1h2a1b2") {
		t.Errorf("goredact-only String() left a GitHub PAT unredacted: %q", got)
	}

	// Regression pin: placeholders stay preserved under goredact-only.
	// goredact's own validators reject placeholder shapes; the filter loop
	// in detectGoredact is defensive belt-and-braces on top of that.
	if got := String("password=changeme"); got != "password=changeme" {
		t.Errorf("goredact-only String(changeme) = %q, want preserved", got)
	}

	// Non-scanner layers stay on under every configuration: layer 8
	// (bounded credential key/values) is scanner-independent.
	if got := String("database_password=secret123"); got != "database_password=REDACTED" {
		t.Errorf("goredact-only String(db kv) = %q, want layer-8 redaction", got)
	}

	// Gate-off pin: a betterleaks-1.8.0-exclusive detection (the
	// generic-password rule; goredact rejects the low-entropy value and no
	// other layer matches) must NOT fire under goredact-only.
	if got := String("password=not-a-secret-setting"); got != "password=not-a-secret-setting" {
		t.Errorf("goredact-only String() = %q, want unchanged (betterleaks gated off)", got)
	}

	// Both engines: goredact remains active alongside betterleaks.
	if err := ConfigureScanners(ScannersConfig{Betterleaks: true, Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners(both): %v", err)
	}
	if got := String(in); strings.Contains(got, "ghp_a1b2c1d2e1f2g1h2a1b2c1d2e1f2g1h2a1b2") {
		t.Errorf("both-scanners String() left the PAT unredacted: %q", got)
	}
	// ...and betterleaks remains active too (same 1.8.0-exclusive detection).
	if got := String("password=not-a-secret-setting"); got != "password=REDACTED" {
		t.Errorf("both-scanners String() = %q, want betterleaks generic-password detection", got)
	}
}

func TestConfigureScanners_DefaultConfig(t *testing.T) {
	// Explicit default config = betterleaks-only, identical to the
	// pre-feature pipeline; the existing betterleaks-layer tests in this
	// file remain the pin for the unconfigured state.
	if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
		t.Fatalf("ConfigureScanners: %v", err)
	}
	// The low-entropy PAT is invisible to every non-scanner layer (pinned by
	// the all-off baseline in TestConfigureScanners_GoredactLayer), so this
	// redaction is attributable to betterleaks running under the default
	// config.
	if got := String("auth with token ghp_a1b2c1d2e1f2g1h2a1b2c1d2e1f2g1h2a1b2"); strings.Contains(got, "ghp_a1b2c1d2e1f2g1h2a1b2c1d2e1f2g1h2a1b2") {
		t.Errorf("betterleaks-only String() left the PAT unredacted: %q", got)
	}
}

func TestJSONLBytes_ScannerDegradedSentinel(t *testing.T) {
	t.Cleanup(func() {
		SetScannerDegradedForTest(false)
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners: %v", err)
	}
	SetScannerDegradedForTest(true)

	// Secret-free input: redaction leaves it byte-identical, so this pins
	// the degradation check running before the `redacted == s` short-circuit
	// rather than depending on a redaction actually happening.
	if _, err := JSONLBytes([]byte(`{"message":"hello"}`)); !errors.Is(err, ErrScannerDegraded) {
		t.Fatalf("JSONLBytes error = %v, want ErrScannerDegraded", err)
	}

	// Degradation with betterleaks also enabled is logged-only: no error.
	if err := ConfigureScanners(ScannersConfig{Betterleaks: true, Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners(both): %v", err)
	}
	SetScannerDegradedForTest(true)
	if _, err := JSONLBytes([]byte(`{"message":"hello"}`)); err != nil {
		t.Fatalf("JSONLBytes with betterleaks covering = %v, want nil", err)
	}
}

func TestJSONLBytesWithPrivacyFilter_ScannerDegradedSentinel(t *testing.T) {
	t.Cleanup(func() {
		SetScannerDegradedForTest(false)
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners: %v", err)
	}
	SetScannerDegradedForTest(true)

	// OPF is unconfigured, so this falls back to the plain JSONLContent flow;
	// the sentinel must still surface from that path.
	if _, err := JSONLBytesWithPrivacyFilter(context.Background(), []byte(`{"message":"hello"}`)); !errors.Is(err, ErrScannerDegraded) {
		t.Fatalf("JSONLBytesWithPrivacyFilter error = %v, want ErrScannerDegraded", err)
	}
}

// Merge-review regression: scanner findings keep exact placeholder matching.
// The key/value placeholder rules (ellipses, shell references, code literals)
// must not drop a betterleaks finding whose capture ends in prose dots.
func TestString_ScannerFindingsIgnoreKeyValuePlaceholderRules(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`api_key = "Xk3R9pLm2QwZ8vTn5Yb7..."`,
		`curl -H "Authorization: Bearer Xk3R9pLm2QwZ8vTn5Yb7Wq..." https://x`,
		`password = "Tr0ub4dor&3..."`,
		`DB_PASSWORD=hunter2...`,
	} {
		out := String(in)
		if !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("String(%q) = %q, want a redaction", in, out)
		}
	}
	// A bare documentation prefix before the ellipsis is still a placeholder.
	if got := String(`ANTHROPIC_API_KEY=sk-ant-...`); got != `ANTHROPIC_API_KEY=sk-ant-...` {
		t.Errorf("documentation ellipsis placeholder redacted: %q", got)
	}
}

// Merge-review regression: camel-splitting must not lose keys the flat
// normalization matched (hostName -> host_name, MySQLPassword -> my_sql_password).
func TestJSONLContent_VendorCasedCredentialKeysRedacted(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`{"hostName":"db.internal","userName":"admin","password":"hunter2"}`,
		`{"Host":"db.internal","UserName":"admin","Password":"hunter2"}`,
		`{"HostName":"db.internal","UserId":"admin","Password":"hunter2"}`,
		`{"msg":"x","MySQLPassword":"hunter2"}`,
		`{"msg":"x","PostgreSQLPassword":"hunter2"}`,
		`{"msg":"x","SQLServerPassword":"hunter2"}`,
		`{"msg":"x","MariaDBPassword":"hunter2"}`,
		`{"msg":"x","mySqlPassword":"hunter2"}`,
	} {
		if out := mustJSONLContent(t, in); strings.Contains(out, "hunter2") {
			t.Errorf("JSONLContent(%s) leaked the password: %s", in, out)
		}
	}
}

// Code literals and other tools' masks in a credential key/value position are
// not secrets: `access_token = None`, `api_key=hidden`, `--api-key=(value hidden)`.
func TestString_CredentialKeyValueCodeLiteralsPreserved(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`access_token = None`,
		`api_key=null`,
		`private_key=False`,
		`https://mcp.example/sse?api_key=hidden&region=us`,
		`npx mcp-a --api-key=(value hidden) --verbose`,
	} {
		if got := String(in); got != in {
			t.Errorf("String(%q) = %q, want unchanged", in, got)
		}
	}
	// Trail-finding regression: only the exact masks count, and never under a
	// password key, where the "literal" can be the real password.
	for _, in := range []string{
		`api_key=(hunter2)`,
		`api_key=(correcthorse)`,
		`PASSWORD=hidden`,
		`PASSWORD=true`,
		`DB_PASSWORD=none`,
		`DB_PASSWORD=(value hidden)`,
	} {
		if got := String(in); !strings.Contains(got, RedactedPlaceholder) {
			t.Errorf("String(%q) = %q, want a redaction", in, got)
		}
	}
}

// Identifier keys that only mention a generic credential word name a resource
// (an AWS Secrets Manager secret, a KMS key, a record), not key material.
func TestJSONLContent_GenericCredentialWordIdentifiersPreserved(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`{"SecretId":"prod/db/creds"}`,
		`{"keyId":"alias/my-key"}`,
		`{"token_id":"t_123"}`,
	} {
		if out := mustJSONLContent(t, in); out != in {
			t.Errorf("JSONLContent(%s) = %s, want unchanged", in, out)
		}
	}
}

// Trail-finding regression: the call-expression exemption must not preserve a
// secret literal placed in callee or argument position.
func TestString_CredentialCodeCallWithLiteralRedacts(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`PASSWORD = Pa55w0rd(x)`,
		`api_key = decode(c2stbGl2ZS1hYmM)`,
		`client_secret = f(hunter2)`,
	} {
		if out := String(in); !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("String(%q) = %q, want a redaction", in, out)
		}
	}
	for _, in := range []string{
		`client_secret = fetch_secret()`,
		`access_token = self.refresh_token()`,
		`api_key = load_key(cfg.path)`,
	} {
		if out := String(in); out != in {
			t.Errorf("String(%q) = %q, want unchanged", in, out)
		}
	}
}

// Merge-review regression: a $(...) value is a placeholder only when it runs an
// allowlisted secret-fetching command; paths, wrappers and here-strings can
// carry the literal and must redact as they do on main.
func TestString_CommandSubstitutionCarryingTextRedacts(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`DB_PASSWORD="$(/bin/echo hunter2)"`,
		`DB_PASSWORD="$(command echo hunter2)"`,
		`DB_PASSWORD="$(cat <<< hunter2)"`,
		`DB_PASSWORD=$(cat<<<hunter2)`,
		`AUTH_TOKEN="$(echo hunter2)"`,
	} {
		if out := String(in); strings.Contains(out, "hunter2") {
			t.Errorf("String(%q) = %q, want the literal redacted", in, out)
		}
	}
	for _, in := range []string{`{"DB_PASSWORD":"$(/bin/echo hunter2)"}`, `{"password":"$(cat <<< hunter2)","host":"h","user":"u"}`} {
		if out := mustJSONLContent(t, in); strings.Contains(out, "hunter2") {
			t.Errorf("JSONLContent(%s) = %s, want the literal redacted", in, out)
		}
	}
	if got := String(`AUTH_TOKEN="$(gh auth token)"`); got != `AUTH_TOKEN="$(gh auth token)"` {
		t.Errorf("secret-fetching recipe must stay a placeholder, got %q", got)
	}
}

// Merge-review regression: the key/value placeholder shapes must not reach
// main's connection-string layers, which redact a string with a real password
// as a unit.
func TestString_KeyValuePlaceholdersStayOutOfConnectionStrings(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`Server=db.example.com;Database=app;User Id=u;Password=your-pw-value;`: `REDACTED;`,
		`host=db.example.com dbname=app user=u password=your-pw-value`:         `REDACTED`,
		`DB_PASSWORD=<HUNTER>`: `DB_PASSWORD=REDACTED`,
	} {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
	if got := String(`subject_token = <ENTIRE_TOKEN>`); got != `subject_token = <ENTIRE_TOKEN>` {
		t.Errorf("uppercase bracketed key/value placeholder redacted: %q", got)
	}
	if out := mustJSONLContent(t, `{"api_key":"hidden"}`); out != `{"api_key":"hidden"}` {
		t.Errorf("JSON mask under a non-password key redacted: %s", out)
	}
}

// Trail-finding regressions (post-merge review): secrets-store writes redact
// whatever the key, bearer-style ids keep entropy coverage, and the doc
// fill-in and ellipsis placeholders accept only placeholder-shaped values.
func TestString_PostMergeTrailFindings(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`printf 'sk_live_hunter2' | mycli secrets put STRIPE_KEY`,
		`printf 'postgres://u:hunter2@db/app' | mycli secrets put DATABASE_URL`,
		"session_id=" + "Zx9QwErTy7UiOp3AsDfGh",
		`API_KEY=your-dog-name`,
		`client_secret=your_mom_is_nice`,
		`API_KEY=Zk9f3QpL7x-...`,
		`DB_PASSWORD=hunter2-...`,
	} {
		if out := String(in); !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("String(%q) = %q, want a redaction", in, out)
		}
	}
	for _, in := range []string{
		`printf 'example-client-id.apps.example.test' | mycli secrets put CLIENT_ID`,
		`API_KEY=your_api_key_here`,
		`API_KEY=your-token`,
		`ANTHROPIC_API_KEY=sk-ant-...`,
		`GITHUB_TOKEN=ghp_...`,
	} {
		if out := String(in); out != in {
			t.Errorf("String(%q) = %q, want unchanged", in, out)
		}
	}
}

// Trail-finding regression: the AWS secret access key name is a credential
// key shape in text and JSON, even for values below the entropy threshold.
func TestString_AWSSecretAccessKeyNameRedacts(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`AWS_SECRET_ACCESS_KEY=correct-horse-battery`,
		`aws_secret_access_key = correct-horse-battery`,
		`secret_access_key=correct-horse-battery`,
	} {
		if out := String(in); strings.Contains(out, "correct-horse-battery") {
			t.Errorf("String(%q) = %q, want the value redacted", in, out)
		}
	}
	if out := mustJSONLContent(t, `{"aws_secret_access_key":"correct-horse-battery"}`); strings.Contains(out, "correct-horse-battery") {
		t.Errorf("JSON aws_secret_access_key leaked: %s", out)
	}
}

// Trail-finding regression: password keys get only the shared placeholder
// rules, so a human-chosen password shaped like a doc fill-in still redacts.
func TestPasswordKeysSkipKeyValuePlaceholderShapes(t *testing.T) {
	t.Parallel()
	for _, in := range []string{`{"password":"your-secret"}`, `{"db_password":"your_token"}`} {
		if out := mustJSONLContent(t, in); !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("JSONLContent(%s) = %s, want a redaction", in, out)
		}
	}
	for _, in := range []string{`PASSWORD=your-secret`, `DB_PASSWORD=api-token-here`} {
		if out := String(in); !strings.Contains(out, RedactedPlaceholder) {
			t.Errorf("String(%q) = %q, want a redaction", in, out)
		}
	}
	if out := mustJSONLContent(t, `{"api_key":"your_api_key_here"}`); out != `{"api_key":"your_api_key_here"}` {
		t.Errorf("non-password doc fill-in redacted: %s", out)
	}
}

// The default scanner's key-context pass hides the password-key gap above;
// with goredact alone, only the placeholder scope keeps db_password redacted.
func TestPasswordKeysSkipKeyValuePlaceholderShapes_GoredactOnly(t *testing.T) {
	t.Cleanup(func() {
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore scanners: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{Goredact: true}); err != nil {
		t.Fatalf("ConfigureScanners: %v", err)
	}
	if out := mustJSONLContent(t, `{"db_password":"your_token"}`); !strings.Contains(out, RedactedPlaceholder) {
		t.Errorf("goredact-only db_password doc-fill-in value leaked: %s", out)
	}
}
