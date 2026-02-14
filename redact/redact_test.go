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
const redactedPlaceholder = "REDACTED"

func TestBytes_NoSecrets(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	input := []byte("my key is " + highEntropySecret + " ok")
	result := Bytes(input)
	expected := []byte("my key is REDACTED ok")
	if !bytes.Equal(result, expected) {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestJSONLBytes_NoSecrets(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"text","content":"hello"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result) != string(input) {
		t.Errorf("expected unchanged input, got %q", result)
	}
	if &result[0] != &input[0] {
		t.Error("expected same underlying slice when no redaction needed")
	}
}

func TestJSONLBytes_WithSecret(t *testing.T) {
	t.Parallel()

	input := []byte(`{"type":"text","content":"key=` + highEntropySecret + `"}`)
	result, err := JSONLBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	expected := []byte(`{"type":"text","content":"REDACTED"}`)
	if !bytes.Equal(result, expected) {
		t.Errorf("got %q, want %q", result, expected)
	}
}

func TestJSONLContent_TopLevelArray(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	input := `["hello","world"]`
	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != input {
		t.Errorf("expected unchanged input, got %q", result)
	}
}

func TestJSONLContent_InvalidJSONLine(t *testing.T) {
	t.Parallel()

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

func TestJSONBytes_PreservesIDFields(t *testing.T) {
	t.Parallel()

	idFieldSecret := highEntropySecret + "-session-id"
	intentSecret := highEntropySecret
	input := `{"session_id":"` + idFieldSecret + `","intent":"` + intentSecret + `","agent_id":"` + idFieldSecret + `"}`
	result, err := JSONBytes([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// If this field were not considered ID-like, it would be redacted.
	if String(idFieldSecret) == idFieldSecret {
		t.Fatal("id field secret should be redacted when not excluded")
	}

	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if got["session_id"] != idFieldSecret {
		t.Errorf("session_id should be preserved, got %q", got["session_id"])
	}
	if got["intent"] != redactedPlaceholder {
		t.Errorf("intent should be redacted, got %q", got["intent"])
	}
	if got["agent_id"] != idFieldSecret {
		t.Errorf("agent_id should be preserved, got %q", got["agent_id"])
	}
}

func TestJSONBytes_DuplicateIDAndPayloadValues(t *testing.T) {
	t.Parallel()

	dup := highEntropySecret
	input := `{"session_id":"` + dup + `","intent":"` + dup + `","tool":"` + dup + `"}`
	result, err := JSONBytes([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if got["session_id"] != dup {
		t.Errorf("session_id should be preserved, got %q", got["session_id"])
	}
	if got["intent"] != redactedPlaceholder {
		t.Errorf("intent should be redacted, got %q", got["intent"])
	}
	if got["tool"] != redactedPlaceholder {
		t.Errorf("tool should be redacted, got %q", got["tool"])
	}
}

func TestJSONBytes_InvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := JSONBytes([]byte(`{"session":"bad"`))
	if err == nil {
		t.Fatal("expected error for invalid JSON content")
	}
}

func TestJSONBytes_PreservesIndentedFormatting(t *testing.T) {
	t.Parallel()

	input := []byte("{\n  \"session_id\": \"sess-123\",\n  \"intent\": \"" + highEntropySecret + "\"\n}\n")

	result, err := JSONBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(string(result), "\n") {
		t.Fatalf("expected trailing newline to be preserved, got %q", result)
	}
	if !strings.Contains(string(result), "\n  \"") {
		t.Fatalf("expected indented JSON formatting to be preserved, got %q", result)
	}

	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if got["session_id"] != "sess-123" {
		t.Errorf("session_id should be preserved, got %q", got["session_id"])
	}
	if got["intent"] != redactedPlaceholder {
		t.Errorf("intent should be redacted, got %q", got["intent"])
	}
}

func TestJSONBytes_PreservesCompactFormatting(t *testing.T) {
	t.Parallel()

	input := []byte("{\"intent\":\"" + highEntropySecret + "\"}")

	result, err := JSONBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(string(result), "\n") {
		t.Fatalf("expected compact JSON output without newline, got %q", result)
	}
	if !strings.Contains(string(result), "\"intent\":\"REDACTED\"") {
		t.Fatalf("expected compact redacted output, got %q", result)
	}
}

func TestJSONBytes_PreservesCompactWithTrailingNewline(t *testing.T) {
	t.Parallel()

	input := []byte("{\"intent\":\"" + highEntropySecret + "\"}\n")
	result, err := JSONBytes(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasSuffix(string(result), "\n") {
		t.Fatalf("expected trailing newline to be preserved, got %q", result)
	}
	if strings.Count(string(result), "\n") != 1 {
		t.Fatalf("expected compact single-line output with trailing newline, got %q", result)
	}
	if !strings.Contains(string(result), "\"intent\":\"REDACTED\"") {
		t.Fatalf("expected compact redacted output, got %q", result)
	}
}

func TestCollectJSONLReplacements_Succeeds(t *testing.T) {
	t.Parallel()

	obj := map[string]any{
		"content": "token=" + highEntropySecret,
	}
	repls := collectJSONLReplacements(obj)
	// expect one replacement for high-entropy secret
	want := [][2]string{{"token=" + highEntropySecret, "REDACTED"}}
	if !slices.Equal(repls, want) {
		t.Errorf("got %q, want %q", repls, want)
	}
}

func TestShouldSkipJSONLField(t *testing.T) {
	t.Parallel()

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
		// Exact match "signature" should be skipped.
		{"signature", true},
		// Fields that should NOT be skipped.
		{"content", false},
		{"type", false},
		{"name", false},
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
	t.Parallel()

	// Verify that secrets in skipped fields are preserved (not redacted).
	obj := map[string]any{
		"session_id": highEntropySecret,
		"content":    highEntropySecret,
	}
	repls := collectJSONLReplacements(obj)
	// Only "content" should produce a replacement; "session_id" should be skipped.
	if len(repls) != 1 {
		t.Fatalf("expected 1 replacement, got %d", len(repls))
	}
	if repls[0][0] != highEntropySecret {
		t.Errorf("expected replacement for secret in content field, got %q", repls[0][0])
	}
}

func TestString_PatternDetection(t *testing.T) {
	t.Parallel()

	// These secrets have entropy below 4.5 so entropy-only detection misses them.
	// Gitleaks pattern matching should catch them.
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

func TestShouldSkipJSONLObject(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

	// Verify that secrets inside image objects are NOT redacted.
	obj := map[string]any{
		"type": "image",
		"data": highEntropySecret,
	}
	repls := collectJSONLReplacements(obj)

	// expect no replacements, it's an image which is skipped.
	var wantRepls [][2]string
	if !slices.Equal(repls, wantRepls) {
		t.Errorf("got %q, want %q", repls, wantRepls)
	}

	// Verify that secrets inside non-image objects ARE redacted.
	obj2 := map[string]any{
		"type":    "text",
		"content": highEntropySecret,
	}
	repls2 := collectJSONLReplacements(obj2)
	wantRepls2 := [][2]string{{highEntropySecret, "REDACTED"}}
	if !slices.Equal(repls2, wantRepls2) {
		t.Errorf("got %q, want %q", repls2, wantRepls2)
	}
}
