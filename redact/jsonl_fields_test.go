package redact

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Structured values carry their meaning in their key. These tests pin two
// rules of the JSONL walk: a value is scanned together with the key it sits
// under, and a skip key preserves its own values without hiding the objects
// nested beneath it.

const (
	// lowEntropyAPIKey is hex, so its entropy stays under entropyThreshold and
	// no pattern matches it alone. Only the key in front of it marks it.
	lowEntropyAPIKey = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	githubPATFixture = "ghp_a1b2c1d2e1f2g1h2a1b2c1d2e1f2g1h2a1b2"
)

type jsonlFieldCase struct {
	name string
	line string
	want string
}

func assertJSONLFieldCases(t *testing.T, tests []jsonlFieldCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLContent(tt.line)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLowEntropyAPIKeyFixtureNeedsItsKey(t *testing.T) {
	t.Parallel()
	// Guards the fixture: if the value were flagged on its own, the key-context
	// tests below would pass without exercising the key.
	require.Equal(t, lowEntropyAPIKey, String(lowEntropyAPIKey))
	require.Equal(t, "api_key=REDACTED", String("api_key="+lowEntropyAPIKey))
}

func TestJSONLContent_ScansValueWithItsKey(t *testing.T) {
	t.Parallel()
	assertJSONLFieldCases(t, []jsonlFieldCase{
		{
			name: "tool input api_key",
			line: `{"type":"tool_use","input":{"api_key":"` + lowEntropyAPIKey + `"}}`,
			want: `{"type":"tool_use","input":{"api_key":"REDACTED"}}`,
		},
		{
			name: "tool input token",
			line: `{"type":"tool_use","input":{"token":"8f14e45fceea167a5a36dedd4bea2543"}}`,
			want: `{"type":"tool_use","input":{"token":"REDACTED"}}`,
		},
		{
			name: "camelCase apiKey",
			line: `{"config":{"apiKey":"` + lowEntropyAPIKey + `"}}`,
			want: `{"config":{"apiKey":"REDACTED"}}`,
		},
		{
			name: "client_secret",
			line: `{"oauth":{"client_secret":"` + lowEntropyAPIKey + `","client_name":"cli"}}`,
			want: `{"oauth":{"client_secret":"REDACTED","client_name":"cli"}}`,
		},
		{
			name: "password outside a connection object",
			line: `{"input":{"username":"svc","password":"S3cr3tP4ssw0rdXyz"}}`,
			want: `{"input":{"username":"svc","password":"REDACTED"}}`,
		},
		{
			name: "a finding only the key reveals replaces the whole value",
			line: `{"api_key_note":"` + lowEntropyAPIKey + ` is the staging key"}`,
			want: `{"api_key_note":"REDACTED"}`,
		},
		{
			name: "a finding the value reveals on its own keeps the rest",
			line: `{"token":"` + githubPATFixture + ` and a note"}`,
			want: `{"token":"REDACTED and a note"}`,
		},
	})
}

// Key context must only add to what the value alone redacts. Rewriting the
// value before the other layers scan it would stop a rule that needs the
// unchanged value from matching.
func TestJSONLContent_KeyContextKeepsValueAloneMatches(t *testing.T) {
	// Not parallel: custom rules are process-global.
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{
		"prefix":   `ACME-[a-z]+`,
		"anchored": `^ACME-[a-z]+ [0-9]{4}$`,
	}})

	got, err := JSONLContent(`{"author":"ACME-aaaaaa 1234"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"author":"REDACTED"}`, got)
}

func TestJSONLContent_KeyContextKeepsNonSecrets(t *testing.T) {
	t.Parallel()
	unchanged := []string{
		`{"primary_key":"user_account_identifier"}`,
		`{"token_type":"bearer","apiKeySource":"none","auth_mode":"chatgpt"}`,
		`{"author":"Ada Lovelace","access":"read-write","credentials":"include"}`,
		`{"key":"Enter"}`,
		`{"key":"src/components/Button.tsx"}`,
		`{"cacheKey":"v1-linux-go-mod-3f2a1b9c"}`,
		`{"idempotency_key":"order-2024-10-19-abc"}`,
		`{"key":"550e8400-e29b-41d4-a716-446655440000"}`,
		`{"key":"` + claudeToolUseID + `"}`,
		`{"apiKey":"${OPENAI_API_KEY}","api_key":"your_api_key_here","token":"REDACTED"}`,
		`{"secretRef":"projects/acme/secrets/db-password/versions/latest"}`,
		// Skip keys win over credential words: these are identifiers.
		`{"tokenId":"` + lowEntropyAPIKey + `","api_key_id":"` + lowEntropyAPIKey + `"}`,
		// A finding inside the key itself must not spill into the value.
		`{"secret_Zk3pQ9wL2mX7vR4tY8nB1cF6hJ0sD5gA":"hello world"}`,
	}
	tests := make([]jsonlFieldCase, 0, len(unchanged))
	for _, line := range unchanged {
		tests = append(tests, jsonlFieldCase{name: line, line: line, want: line})
	}
	assertJSONLFieldCases(t, tests)
}

func TestJSONLContent_WalksObjectsUnderSkippedKeys(t *testing.T) {
	t.Parallel()
	assertJSONLFieldCases(t, []jsonlFieldCase{
		{
			name: "key ending in id that is not an id",
			line: `{"input":{"android":{"apiKey":"` + githubPATFixture + `"}}}`,
			want: `{"input":{"android":{"apiKey":"REDACTED"}}}`,
		},
		{
			name: "valid",
			line: `{"valid":{"token":"` + githubPATFixture + `"}}`,
			want: `{"valid":{"token":"REDACTED"}}`,
		},
		{
			name: "path object",
			line: `{"path":{"env":"GITHUB_TOKEN=` + githubPATFixture + `"}}`,
			want: `{"path":{"env":"GITHUB_TOKEN=REDACTED"}}`,
		},
		{
			name: "map keyed by id",
			line: `{"byId":{"abc":{"secret":"` + lowEntropyAPIKey + `"}}}`,
			want: `{"byId":{"abc":{"secret":"REDACTED"}}}`,
		},
		{
			name: "objects inside an id array",
			line: `{"ids":[{"label":"` + githubPATFixture + `"}]}`,
			want: `{"ids":[{"label":"REDACTED"}]}`,
		},
	})
}

func TestJSONLContent_SkippedKeysKeepTheirValues(t *testing.T) {
	t.Parallel()
	unchanged := []string{
		`{"id":"` + highEntropySecret + `","sessionId":"` + highEntropySecret + `"}`,
		`{"type":"tool_use","id":"` + claudeToolUseID + `","name":"Bash","input":{"command":"ls"}}`,
		`{"type":"tool_result","tool_use_id":"` + claudeToolUseID + `","content":"done"}`,
		`{"type":"thinking","thinking":"plan","signature":"` + highEntropySecret + `"}`,
		`{"thinkingSignature":"` + highEntropySecret + `"}`,
		`{"file_path":"/Users/alice/` + highEntropySecret + `.go","cwd":"/Users/alice/` + highEntropySecret + `"}`,
		// Arrays under a skip key hold that key's values.
		`{"tool_use_ids":["` + highEntropySecret + `","` + claudeToolUseID + `"]}`,
		`{"sessionIds":[["` + highEntropySecret + `"]]}`,
		`{"path":["/Users/alice/` + highEntropySecret + `"]}`,
		// OpenCode's path object: its own fields are skip keys too.
		`{"info":{"path":{"cwd":"/Users/alice/` + highEntropySecret + `","root":"/Users/alice"}}}`,
	}
	tests := make([]jsonlFieldCase, 0, len(unchanged))
	for _, line := range unchanged {
		tests = append(tests, jsonlFieldCase{name: line, line: line, want: line})
	}
	assertJSONLFieldCases(t, tests)
}

// The splice, the single-JSON-value path and the duplicate-key re-encode all
// share one walk; each must apply both rules.
func TestJSONLContent_FieldRulesApplyOnEveryJSONPath(t *testing.T) {
	t.Parallel()

	t.Run("single pretty-printed value", func(t *testing.T) {
		t.Parallel()
		content := "{\n  \"input\": {\"api_key\": \"" + lowEntropyAPIKey + "\"},\n  \"valid\": {\"token\": \"" + githubPATFixture + "\"}\n}"
		got, err := JSONLContent(content)
		require.NoError(t, err)
		assert.NotContains(t, got, lowEntropyAPIKey)
		assert.NotContains(t, got, githubPATFixture)
	})

	t.Run("line rebuilt for a duplicate key", func(t *testing.T) {
		t.Parallel()
		line := `{"text":"safe","text":"safe","input":{"api_key":"` + lowEntropyAPIKey + `"},"valid":{"token":"` + githubPATFixture + `"}}`
		got, err := JSONLContent(line)
		require.NoError(t, err)
		assert.NotContains(t, got, lowEntropyAPIKey)
		assert.NotContains(t, got, githubPATFixture)
	})
}

func TestJSONLContentWithPrivacyFilter_ScansValueWithItsKey(t *testing.T) {
	// Not parallel: the privacy filter config is process-global.
	resetOPFConfig()
	t.Cleanup(resetOPFConfig)
	ConfigurePrivacyFilterWithRuntime(OPFConfig{
		Enabled:    true,
		Categories: map[string]bool{"private_person": true},
	}, &fakeRuntime{})

	line := `{"note":"Alice met Bob","input":{"api_key":"` + lowEntropyAPIKey + `"}}`
	got, err := JSONLContentWithPrivacyFilter(context.Background(), line)
	require.NoError(t, err)
	assert.JSONEq(t, `{"note":"Alice met Bob","input":{"api_key":"REDACTED"}}`, got)
}
