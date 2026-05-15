package redact

import (
	"bytes"
	"encoding/json"
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

func TestJSONLContent_TopLevelArray(t *testing.T) {
	// Top-level JSON arrays are valid JSONL and should be redacted.
	input := `["` + highEntropySecret + `","normal text"]`
	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := `["REDACTED","normal text"]`
	if result != expected {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestJSONLContent_TopLevelArrayNoSecrets(t *testing.T) {
	input := `["hello","world"]`
	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
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
	repls := collectJSONLReplacements(obj, String)
	// expect one replacement for high-entropy secret
	want := []jsonReplacement{{key: "content", original: "token=" + highEntropySecret, redacted: "REDACTED"}}
	if !slices.Equal(repls, want) {
		t.Errorf("got %q, want %q", repls, want)
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
		// Fields ending in "account" should be skipped (identifier shape,
		// not credential): google_adsense_account, aws_account_id, etc.
		{"account", true},
		{"google_adsense_account", true},
		{"aws_account", true},
		{"service_account", true},
		// Exact match "signature" should be skipped.
		{"signature", true},
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
		{"signatures", false}, // not exact match "signature"
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
	repls := collectJSONLReplacements(obj, String)
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
			input: "key=AKIAYRWQG5EJLPZLBYNP",
			want:  "key=REDACTED",
		},
		{
			name:  "two AWS keys separated by space produce two REDACTED tokens",
			input: "key=AKIAYRWQG5EJLPZLBYNP AKIAYRWQG5EJLPZLBYNP",
			want:  "key=REDACTED REDACTED",
		},
		{
			name:  "adjacent AWS keys without separator merge into single REDACTED",
			input: "key=AKIAYRWQG5EJLPZLBYNPAKIAYRWQG5EJLPZLBYNP",
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
func supabaseSecretPrefix() string      { return "sb" + "_secret_" }
func supabasePersonalPrefix() string    { return "sb" + "p_" }
func supabasePublishablePrefix() string { return "sb" + "_publishable_" }

// TestString_SupabaseProviderTokens covers issue #1716: Supabase sb_secret_
// API keys and sbp_ personal access tokens are low-entropy and, captured in
// isolation, are missed by the entropy layer (threshold 4.5) and by the
// betterleaks Supabase rule (a composite rule that only fires when a
// *.supabase.co URL is co-present). The deterministic provider-prefix layer
// must catch them regardless of entropy or the surrounding variable name.
func TestString_SupabaseProviderTokens(t *testing.T) {
	t.Parallel()

	secret := supabaseSecretPrefix() + "probe_20260710_7f91c2d8e4a6b3f0" // entropy 4.199
	realSecret := supabaseSecretPrefix() + "9uM4GhB0STF5R4K3HxQtlg_bzWW6DRj"
	sbpToken := supabasePersonalPrefix() + "test_probe_20260710_test_probe_2026071"

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
			name:  "sbp_ personal access token (low entropy, betterleaks misses)",
			input: "SUPABASE_ACCESS_TOKEN=" + sbpToken,
			want:  "SUPABASE_ACCESS_TOKEN=REDACTED",
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
			name:  "standalone password assignment is preserved",
			input: "password=not-a-secret-setting",
			want:  "password=not-a-secret-setting",
		},
		{
			name:  "password reset query parameter is preserved",
			input: "https://example.com/?password_reset=true",
			want:  "https://example.com/?password_reset=true",
		},
		{
			name:  "generic https password query is preserved",
			input: "https://example.com/callback?user=svc&password=not-a-db-credential&debug=true",
			want:  "https://example.com/callback?user=svc&password=not-a-db-credential&debug=true",
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
	secret := "correct-horse-client"
	input := `{"type":"user","message":{"content":[{"type":"tool_result","content":"tail -2 && printf '` + secret + `' | examplectl secret put ` + keyName + ` 2>&1 | tail -2"}]}}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result, secret) {
		t.Fatalf("expected shell stdin secret literal to be redacted, got: %s", result)
	}
	if !strings.Contains(result, "printf 'REDACTED' | examplectl secret put "+keyName) {
		t.Fatalf("expected command context to be preserved, got: %s", result)
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
	repls := collectJSONLReplacements(obj, String)

	// expect no replacements, it's an image which is skipped.
	var wantRepls []jsonReplacement
	if !slices.Equal(repls, wantRepls) {
		t.Errorf("got %q, want %q", repls, wantRepls)
	}

	// Verify that secrets inside non-image objects ARE redacted.
	obj2 := map[string]any{
		"type":    "text",
		"content": highEntropySecret,
	}
	repls2 := collectJSONLReplacements(obj2, String)
	wantRepls2 := []jsonReplacement{{key: "content", original: highEntropySecret, redacted: "REDACTED"}}
	if !slices.Equal(repls2, wantRepls2) {
		t.Errorf("got %q, want %q", repls2, wantRepls2)
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
			input: "key=AKIAYRWQG5EJLPZLBYNP",
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

// Pins that `*_account` JSON keys are preserved by structure, not by entropy
// coincidence. The value here is intentionally high-entropy (well above the
// 4.5 threshold) so the test would fail if `_account` were not in the skip
// list — proving the protection is design-driven, not luck-driven.
func TestJSONLContent_AccountJSONKeysPreservedDespiteHighEntropy(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"google_adsense_account":"` + highEntropySecret + `"}`,
		`{"aws_account":"` + highEntropySecret + `"}`,
		`{"service_account":"` + highEntropySecret + `"}`,
	}
	for _, in := range cases {
		out, err := JSONLContent(in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", in, err)
		}
		if out != in {
			t.Errorf("expected %q preserved (account is identifier-shape), got: %s", in, out)
		}
	}
}

// Pins that JSON keys with known non-secret prefixes (cancel/pagination/
// continuation/idempotency/cursor/next/previous/page/sync) followed by
// `_token` or `_secret` are NOT flat-redacted. These are routinely safe
// debug/pagination identifiers. Real high-entropy credential-shaped values
// still get caught by entropy + Betterleaks via the per-value String() call.
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
		`{"cancel_secret":"cancel-state-marker"}`,
	}
	for _, in := range cases {
		out, err := JSONLContent(in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", in, err)
		}
		if out != in {
			t.Errorf("expected %q preserved (non-secret prefix), got: %s", in, out)
		}
	}
}

// Pins that prefixed `*_token` keys NOT in the non-secret allowlist still
// redact — e.g. csrf_token, auth_token, oauth_token, refresh_token. Prevents
// the allowlist from expanding to swallow real credential shapes.
func TestJSONLContent_UnknownTokenPrefixesStillRedacted(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, wantSub string }{
		{`{"csrf_token":"abc-csrf-real-secret"}`, `"csrf_token":"REDACTED"`},
		{`{"auth_token":"abc-auth-real-secret"}`, `"auth_token":"REDACTED"`},
		{`{"oauth_token":"abc-oauth-real-secret"}`, `"oauth_token":"REDACTED"`},
		{`{"random_token":"abc-random-real-secret"}`, `"random_token":"REDACTED"`},
	}
	for _, c := range cases {
		out, err := JSONLContent(c.in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", c.in, err)
		}
		if !strings.Contains(out, c.wantSub) {
			t.Errorf("input %q: expected %q in output, got: %s", c.in, c.wantSub, out)
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
		out, err := JSONLContent(in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", in, err)
		}
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
		{`{"api_key":"abc-api-real-secret"}`, `"api_key":"REDACTED"`},
		{`{"client_secret":"abc-client-real-secret"}`, `"client_secret":"REDACTED"`},
		{`{"refresh_token":"abc-refresh-real-secret"}`, `"refresh_token":"REDACTED"`},
	}
	for _, c := range cases {
		out, err := JSONLContent(c.in)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", c.in, err)
		}
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
