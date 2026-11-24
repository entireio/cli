package redact

import (
	"strings"
	"testing"
)

// Opaque credentials whose entropy is below entropyThreshold, so only the
// Authorization header layer can catch them outside a curl -H argument.
const (
	authUUIDToken   = "3f9c2a7e-8b41-4d6a-9e05-c1b7d2f48a63"
	authHexToken    = "9a41c07e5bd28f63e1a4097cbd5e2f8136a7c0d4"
	authBase62Token = "Qm4Wn8RtbVcXzLkJhGf2Qm4W"
	authBasicCred   = "YWRtaW46czNjcjN0LXBhc3M="
	authLinearKey   = "lin_api_4f2a8c1e9b7d3a6c5e0f"
)

func TestAuthorizationHeaderTokensAreBelowEntropyThreshold(t *testing.T) {
	t.Parallel()
	for _, tok := range []string{authUUIDToken, authHexToken, authBase62Token, authBasicCred, authLinearKey} {
		if e := shannonEntropy(tok); e > entropyThreshold {
			t.Fatalf("token %q has entropy %.3f > %.1f; not an entropy-miss regression case", tok, e, entropyThreshold)
		}
	}
}

func TestString_AuthorizationHeaderValues(t *testing.T) {
	t.Parallel()

	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "raw HTTP request",
			input: "GET /v1/me HTTP/1.1\nHost: api.example.com\nAuthorization: Bearer " + authUUIDToken + "\nAccept: */*",
			want:  "GET /v1/me HTTP/1.1\nHost: api.example.com\nAuthorization: Bearer REDACTED\nAccept: */*",
		},
		{
			name:  "JSON headers object",
			input: `{"headers": {"Authorization": "Bearer ` + authHexToken + `"}}`,
			want:  `{"headers": {"Authorization": "Bearer REDACTED"}}`,
		},
		{
			name:  "Python f-string in a dict",
			input: `requests.get(url, headers={'Authorization': f'Bearer ` + authBase62Token + `'})`,
			want:  `requests.get(url, headers={'Authorization': f'Bearer REDACTED'})`,
		},
		{
			name:  "Python subscript assignment",
			input: `headers['Authorization'] = 'Bearer ` + authUUIDToken + `'`,
			want:  `headers['Authorization'] = 'Bearer REDACTED'`,
		},
		{
			name:  "Ruby hash rocket",
			input: `{ 'Authorization' => "Bearer ` + authUUIDToken + `" }`,
			want:  `{ 'Authorization' => "Bearer REDACTED" }`,
		},
		{
			name:  "JS object literal with unquoted key",
			input: `fetch(url, { headers: { Authorization: "Bearer ` + authUUIDToken + `" } })`,
			want:  `fetch(url, { headers: { Authorization: "Bearer REDACTED" } })`,
		},
		{
			name:  "Go header set call",
			input: `req.Header.Set("Authorization", "token ` + authHexToken + `")`,
			want:  `req.Header.Set("Authorization", "token REDACTED")`,
		},
		{
			name:  "Go header set call wrapped onto the next line",
			input: "req.Header.Set(\"Authorization\",\n\t\"Bearer " + authUUIDToken + "\")",
			want:  "req.Header.Set(\"Authorization\",\n\t\"Bearer REDACTED\")",
		},
		{
			name:  "Go http.Header literal wrapped onto the next line",
			input: "http.Header{\"Authorization\": []string{\n\t\"Bearer " + authUUIDToken + "\",\n}}",
			want:  "http.Header{\"Authorization\": []string{\n\t\"Bearer REDACTED\",\n}}",
		},
		{
			name:  "JSON array value as json.Marshal writes an http.Header",
			input: `{"Authorization":["Bearer ` + authUUIDToken + `"]}`,
			want:  `{"Authorization":["Bearer REDACTED"]}`,
		},
		{
			name:  "Go http.Header literal",
			input: `http.Header{"Authorization": []string{"Bearer ` + authUUIDToken + `"}}`,
			want:  `http.Header{"Authorization": []string{"Bearer REDACTED"}}`,
		},
		{
			name:  "Proxy-Authorization with Basic credentials",
			input: "Proxy-Authorization: Basic " + authBasicCred,
			want:  "Proxy-Authorization: Basic REDACTED",
		},
		{
			name:  "lowercase header name and scheme",
			input: "authorization: bearer " + authUUIDToken,
			want:  "authorization: bearer REDACTED",
		},
		{
			name:  "Discord bot scheme",
			input: "Authorization: Bot " + authBase62Token + " (rotated weekly)",
			want:  "Authorization: Bot REDACTED (rotated weekly)",
		},
		{
			name:  "unlisted scheme as the whole value",
			input: `-H 'Authorization: GenieKey ` + authUUIDToken + `'`,
			want:  `-H 'Authorization: GenieKey REDACTED'`,
		},
		{
			name:  "raw key without a scheme",
			input: "Authorization: " + authLinearKey + "\nContent-Type: application/json",
			want:  "Authorization: REDACTED\nContent-Type: application/json",
		},
		{
			name:  "JSON-escaped header in raw text after a literal newline escape",
			input: `"text":"line1\nAuthorization: Bearer ` + authUUIDToken + `\nline3"`,
			want:  `"text":"line1\nAuthorization: Bearer REDACTED\nline3"`,
		},
		{
			name:  "JSON-escaped quoted key in raw text",
			input: `{\"Authorization\": \"Bearer ` + authHexToken + `\"}`,
			want:  `{\"Authorization\": \"Bearer REDACTED\"}`,
		},
	})
}

func TestString_AuthorizationHeaderNonSecretsPreserved(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		`Authorization: Bearer $GITHUB_TOKEN`,
		`Authorization: Bearer ${API_TOKEN}`,
		`headers = {"Authorization": "Bearer " + token}`,
		"headers: { Authorization: `Bearer ${token}` }",
		`Authorization: Bearer {{access_token}}`,
		`Authorization: Bearer <your-token>`,
		`Authorization: Bearer YOUR_ACCESS_TOKEN_HERE`,
		`Authorization: Bearer REDACTED`,
		`Authorization: Bearer abc123`,
		`The authorization token is sent as a bearer credential.`,
		`We need authorization, token rotation_policy_v2_enabled first.`,
		`Authorization: requires v2_scoped_tokens_only for writes`,
		`Authorization: Digest username="admin", realm="v2_restricted_area"`,
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			if got := String(input); got != input {
				t.Errorf("String(%q) = %q, want unchanged", input, got)
			}
		})
	}
}

// TestDetectAuthorizationHeaders_Boundary checks the layer directly: through
// String, the entropy layer can swallow the characters around a credential and
// hide a boundary this layer gets wrong.
func TestDetectAuthorizationHeaders_Boundary(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		prefix string
		found  bool
	}{
		{"start of text", "", true},
		{"underscore-joined name", "X_", true},
		{"literal JSON newline escape", `line1\n`, true},
		{"literal JSON unicode escape", `\u000a`, true},
		{"glued to a word", "Pre", false},
		{"glued to a digit", "2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.prefix + "Authorization: Bearer " + authUUIDToken
			regions := detectAuthorizationHeaders(s)
			if !tc.found {
				if len(regions) != 0 {
					t.Fatalf("detectAuthorizationHeaders(%q) = %v, want none", s, regions)
				}
				return
			}
			if len(regions) != 1 || s[regions[0].start:regions[0].end] != authUUIDToken {
				t.Fatalf("detectAuthorizationHeaders(%q) = %v, want exactly the token", s, regions)
			}
		})
	}
}

// TestDetectAuthorizationHeaders_WholeCredential checks the layer directly
// that a credential containing token68 punctuation is matched whole: through
// String, betterleaks or the entropy layer may cover the tail and hide a
// credential cut short.
func TestDetectAuthorizationHeaders_WholeCredential(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, input, want string
	}{
		{"underscore after a short prefix", "Authorization: Bearer ab12_" + authUUIDToken, "ab12_" + authUUIDToken},
		{"raw key without a scheme", "Authorization: " + authLinearKey + "\n", authLinearKey},
		{"base64url JWT-style segments", "Authorization: Bearer eyJ0_x-Y.c2Vj_cmV0.c2ln_bmF0dXJl", "eyJ0_x-Y.c2Vj_cmV0.c2ln_bmF0dXJl"},
		{"unlisted scheme before trailing whitespace", "Authorization: GenieKey " + authUUIDToken + " \r\n", authUUIDToken},
		{"raw key before trailing whitespace", "Authorization: " + authLinearKey + " \t\r\n", authLinearKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			regions := detectAuthorizationHeaders(tc.input)
			if len(regions) != 1 || tc.input[regions[0].start:regions[0].end] != tc.want {
				t.Fatalf("detectAuthorizationHeaders(%q) = %v, want exactly %q", tc.input, regions, tc.want)
			}
		})
	}
}

func TestContainsFoldASCII(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		s    string
		want bool
	}{
		{"AUTHORIZATION: x", true},
		{"x-AuThOrIzAtIoN", true},
		{"authorizatio", false},
		{"", false},
	} {
		if got := containsFoldASCII(tc.s, "authorization"); got != tc.want {
			t.Errorf("containsFoldASCII(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// TestJSONLContent_AuthorizationHeaderLeaf covers a header that is its own
// JSON leaf, as in an MCP tool call's arguments. The leaf "Bearer <token>" has
// lost its key, so keyContextFindsMore finds it and replaces the whole leaf,
// scheme included, unlike String's "Bearer REDACTED".
func TestJSONLContent_AuthorizationHeaderLeaf(t *testing.T) {
	t.Parallel()

	input := `{"type":"tool_use","input":{"url":"https://api.example.com/v1/me","headers":{"Authorization":"Bearer ` + authUUIDToken + `"}}}`
	got, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("JSONLContent: %v", err)
	}
	if strings.Contains(got, authUUIDToken) {
		t.Fatalf("bearer token survived JSONL redaction: %s", got)
	}
	if !strings.Contains(got, `"Authorization":"`) {
		t.Fatalf("header key should be preserved: %s", got)
	}
}

// TestDetectAuthorizationHeaders_EmptyRawHeader checks that only a quoted
// name's value may continue onto the next line: an empty raw header must not
// take the next header's value as its credential.
func TestDetectAuthorizationHeaders_EmptyRawHeader(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"Authorization:\nX-Request-Id: " + authHexToken,
		"Authorization: \n" + authLinearKey + "\n",
	} {
		if regions := detectAuthorizationHeaders(input); len(regions) != 0 {
			t.Errorf("detectAuthorizationHeaders(%q) = %v, want none", input, regions)
		}
	}
}

// TestJSONLContent_AuthorizationHeaderArray covers an http.Header written by
// json.Marshal, whose values are arrays: each element is walked with the
// array's key, so the key-context rescan still sees "Authorization".
func TestJSONLContent_AuthorizationHeaderArray(t *testing.T) {
	t.Parallel()

	input := `{"type":"tool_result","content":{"headers":{"Authorization":["Bearer ` + authUUIDToken + `"]}}}`
	got, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("JSONLContent: %v", err)
	}
	if strings.Contains(got, authUUIDToken) {
		t.Fatalf("bearer token in a header array survived JSONL redaction: %s", got)
	}
}

// TestJSONLContent_ArrayElementsTakeTheirKey pins that an array element is
// judged like a single value under the same key: a secret in a one-element
// array is redacted exactly when the bare value would be, and non-secret
// arrays under credential-like keys are left alone.
func TestJSONLContent_ArrayElementsTakeTheirKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ input, want string }{
		{`{"api_keys":["` + authHexToken + `"]}`, `{"api_keys":["REDACTED"]}`},
		{`{"db_password":["hunter2pass"]}`, `{"db_password":["REDACTED"]}`},
		{`{"tokens":["read","write","admin"]}`, `{"tokens":["read","write","admin"]}`},
		{`{"auth":{"methods":["oauth2","api_key_v2"]}}`, `{"auth":{"methods":["oauth2","api_key_v2"]}}`},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLContent(tc.input)
			if err != nil {
				t.Fatalf("JSONLContent: %v", err)
			}
			if got != tc.want {
				t.Errorf("JSONLContent(%s) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}

// TestJSONLContent_ArraySecretKeepsLayout checks that a secret in an array is
// spliced in place. Array elements are judged under their array's key, but a
// keyed splice only finds "key":"value", so scoping their replacement to that
// key would miss and fall back to the re-encode, which sorts keys.
func TestJSONLContent_ArraySecretKeepsLayout(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ input, want string }{
		{
			`{"z":1,"headers":{"Authorization":["Bearer ` + authUUIDToken + `"]},"a":2}`,
			`{"z":1,"headers":{"Authorization":["REDACTED"]},"a":2}`,
		},
		{
			`{"z":1,"api_keys":["` + authHexToken + `"],"a":2}`,
			`{"z":1,"api_keys":["REDACTED"],"a":2}`,
		},
	} {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLContent(tc.input)
			if err != nil {
				t.Fatalf("JSONLContent: %v", err)
			}
			if got != tc.want {
				t.Errorf("JSONLContent(%s) = %s, want %s", tc.input, got, tc.want)
			}
		})
	}
}
