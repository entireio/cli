package redact

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests change process-global redaction config, so none of them run in
// parallel.

const (
	countsEntropySample  = "Xk9mP2qR7vL4nW8jT3hY6bF1cD5gA0sZ"
	countsEntropySample2 = "Qw3eR5tY7uI9oP1aS3dF5gH7jK9lZ2xC"
	countsSupabaseSample = "sb" + "_secret_" + "aaaabbbbccccddddeeeeffffggggh"
)

// alwaysOnLayers run for every transcript whatever the settings say.
var alwaysOnLayers = []string{
	"entropy", "provider_token", "credentialed_uri", "connection_string",
	"shell_stdin", "credential_value", "key_context", "credential_key",
}

// textLine is a transcript line carrying text as a JSON string leaf.
func textLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"text": text})
	require.NoError(t, err)
	return string(b) + "\n"
}

// countsOf redacts content the way checkpoint writes do and returns the counts
// that came with it.
func countsOf(t *testing.T, content string) *Counts {
	t.Helper()
	out, err := JSONLBytes([]byte(content))
	require.NoError(t, err)
	require.NotNil(t, out.Counts(), "JSONLBytes must report what it redacted")
	return out.Counts()
}

// configureCountsRules installs one custom rule plus email and one custom PII
// pattern, restoring the unconfigured state when the test ends.
func configureCountsRules(t *testing.T) {
	t.Helper()
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"project_falcon": `ACME-[0-9]{8}`}})
	ConfigurePII(PIIConfig{
		Enabled:        true,
		Categories:     map[PIICategory]bool{PIIEmail: true},
		CustomPatterns: map[string]string{"employee_id": `EMP-[0-9]{6}`},
	})
	t.Cleanup(resetPIIConfig)
}

func TestJSONLBytesCounts_CreditsEachLayer(t *testing.T) {
	configureCountsRules(t)

	cases := []struct {
		name    string
		content string
		// layers holds the exact matches expected; every other layer must be 0.
		layers map[string]int
		pii    map[string]int
	}{
		{name: "entropy", content: textLine(t, "the value "+countsEntropySample+" appeared"), layers: map[string]int{"entropy": 1}},
		{name: "betterleaks", content: textLine(t, "token "+githubPATFixture+" here"), layers: map[string]int{"betterleaks": 1}},
		{name: "provider token", content: textLine(t, "export SUPABASE="+countsSupabaseSample), layers: map[string]int{"provider_token": 1}},
		{name: "credentialed uri", content: textLine(t, "cache at redis://:hunter2pass@10.0.0.5:6379/0"), layers: map[string]int{"credentialed_uri": 1, "betterleaks": 1}},
		{name: "connection string", content: textLine(t, "Server=db;User Id=sa;Password=hunter2secret;Database=app"), layers: map[string]int{"connection_string": 1, "betterleaks": 1}},
		{name: "credential value", content: textLine(t, "DB_PASSWORD=hunter2secret"), layers: map[string]int{"credential_value": 1, "betterleaks": 1}},
		{name: "custom rule", content: textLine(t, "ticket ACME-12345678 filed"), layers: map[string]int{"custom": 1}},
		{name: "shell stdin", content: textLine(t, "printf 'hunter2secret' | gh secret set DB_PASSWORD"), layers: map[string]int{"shell_stdin": 1}},
		{name: "pii email", content: textLine(t, "mail alice@corp-mail.org now"), layers: map[string]int{"pii": 1}, pii: map[string]int{"EMAIL": 1, "custom": 0}},
		{name: "custom pii pattern", content: textLine(t, "badge EMP-123456 ok"), layers: map[string]int{"pii": 1}, pii: map[string]int{"EMAIL": 0, "custom": 1}},
		// The key-context rule scans the value together with its key to decide
		// whether to fire. Only the placeholder it writes counts, not those scans.
		{name: "key context", content: `{"input":{"api_key":"` + lowEntropyAPIKey + `"}}` + "\n", layers: map[string]int{"key_context": 1}},
		{name: "credential key", content: `{"host":"db","user":"app","pwd":"hunter"}` + "\n", layers: map[string]int{"credential_key": 1}},
		{name: "line that is not JSON", content: "plain log line with " + countsEntropySample + "\n", layers: map[string]int{"entropy": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := countsOf(t, tc.content)
			require.Equal(t, 1, got.Replacements, "one value was replaced")
			for layer, n := range got.Layers {
				require.Equal(t, tc.layers[layer], n, "matches for layer %q", layer)
			}
			for layer := range tc.layers {
				require.Contains(t, got.Layers, layer)
			}
			if tc.pii != nil {
				require.Equal(t, tc.pii, got.PII)
			}
		})
	}
}

func TestJSONLBytesCounts_OverlapsCountInEachLayerButReplaceOnce(t *testing.T) {
	resetCustomRulesForTest(t)

	// An AWS key ID is matched by betterleaks and by the provider-token layer.
	got := countsOf(t, textLine(t, "aws key "+awsKeyFixture+" here"))
	require.Equal(t, 1, got.Replacements)
	require.Equal(t, 1, got.Layers["betterleaks"])
	require.Equal(t, 1, got.Layers["provider_token"])

	// Two secrets in one value, and one secret in two values of one line, are
	// each a placeholder in the stored line.
	got = countsOf(t, textLine(t, "two "+countsEntropySample+" and "+countsEntropySample2))
	require.Equal(t, 2, got.Replacements)
	require.Equal(t, 2, got.Layers["entropy"])

	got = countsOf(t, `{"a":"x `+countsEntropySample+`","b":"y `+countsEntropySample+`"}`+"\n")
	require.Equal(t, 2, got.Replacements)
	require.Equal(t, 2, got.Layers["entropy"])
}

// A layer that did not run is absent, which is different from a layer that ran
// and found nothing.
func TestJSONLBytesCounts_AbsentInactiveAndZeroAreDistinct(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		resetCustomRulesForTest(t)
		resetPIIConfig()

		got := countsOf(t, textLine(t, "plain words only"))
		require.Zero(t, got.Replacements)
		want := map[string]int{"betterleaks": 0}
		for _, layer := range alwaysOnLayers {
			want[layer] = 0
		}
		require.Equal(t, want, got.Layers)
		require.Zero(t, got.CustomRulesActive)
		require.Nil(t, got.PII)
	})

	t.Run("configured layers that matched nothing report zero", func(t *testing.T) {
		resetCustomRulesForTest(t)
		ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"one": `ONE-[0-9]{4}`, "two": `TWO-[0-9]{4}`}})
		ConfigurePII(PIIConfig{
			Enabled:        true,
			Categories:     map[PIICategory]bool{PIIEmail: true, PIIPhone: true},
			CustomPatterns: map[string]string{"employee_id": `EMP-[0-9]{6}`},
		})
		t.Cleanup(resetPIIConfig)

		got := countsOf(t, textLine(t, "plain words only"))
		require.Contains(t, got.Layers, "custom")
		require.Zero(t, got.Layers["custom"])
		require.Equal(t, 2, got.CustomRulesActive)
		require.Contains(t, got.Layers, "pii")
		require.Equal(t, map[string]int{"EMAIL": 0, "PHONE": 0, "custom": 0}, got.PII)
	})

	t.Run("goredact only", func(t *testing.T) {
		resetCustomRulesForTest(t)
		require.NoError(t, ConfigureScanners(ScannersConfig{Goredact: true}))
		t.Cleanup(func() {
			if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
				t.Errorf("restore scanners: %v", err)
			}
		})

		got := countsOf(t, textLine(t, "token "+githubPATFixture+" here"))
		require.NotContains(t, got.Layers, "betterleaks")
		require.Equal(t, 1, got.Layers["goredact"])
		require.Equal(t, 1, got.Replacements)
	})

	t.Run("empty content", func(t *testing.T) {
		resetCustomRulesForTest(t)
		got := countsOf(t, "")
		require.Zero(t, got.Replacements)
		require.Contains(t, got.Layers, "entropy")
	})
}

// Counts are stored in shared checkpoint metadata, so they must carry neither
// the values they counted nor what a team named or wrote its rules.
func TestCounts_JSONHoldsNoValuesOrRuleText(t *testing.T) {
	configureCountsRules(t)

	content := textLine(t, "the value "+countsEntropySample+" appeared") +
		textLine(t, "token "+githubPATFixture) +
		textLine(t, "ticket ACME-12345678 filed") +
		textLine(t, "mail alice@corp-mail.org badge EMP-123456")
	got := countsOf(t, content)
	require.Equal(t, 5, got.Replacements)

	data, err := json.Marshal(got)
	require.NoError(t, err)
	lower := strings.ToLower(string(data))
	for _, leaked := range []string{
		strings.ToLower(countsEntropySample), strings.ToLower(githubPATFixture),
		"acme", "falcon", "emp-", "employee", "alice", "corp-mail", "[0-9]",
	} {
		require.NotContains(t, lower, leaked)
	}

	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &keys))
	require.Subset(t, []string{"replacements", "layers", "custom_rules_active", "pii"}, slices.Collect(maps.Keys(keys)))
}

// Some lines are redacted a second time by the structural fallback; each value
// in the stored line must still be counted once.
func TestJSONLBytesCounts_LineRedactedTwiceCountsOnce(t *testing.T) {
	resetCustomRulesForTest(t)

	// The escaped slash makes the splice miss, so the line is re-encoded.
	escaped := `{"text":"see https:\/\/x.com value ` + countsEntropySample + `"}` + "\n"
	out, err := JSONLBytes([]byte(escaped))
	require.NoError(t, err)
	require.NotContains(t, string(out.Bytes()), `\/`, "the line must have gone through the structural fallback")
	require.NotNil(t, out.Counts())
	require.Equal(t, 1, out.Counts().Replacements)
	require.Equal(t, 1, out.Counts().Layers["entropy"])

	// Duplicate keys are rebuilt structurally and keep only the last value.
	dup := `{"a":"x ` + countsEntropySample + `","a":"y ` + countsEntropySample2 + `"}` + "\n"
	got := countsOf(t, dup)
	require.Equal(t, 1, got.Replacements)
	require.Equal(t, 1, got.Layers["entropy"])
}

// The splice rewrites every copy of a redacted value in the line, including
// copies in fields the walk skips and under the same key outside a credential
// context. Replacements must count the placeholders stored; the layers count
// what each layer found in the values it scanned.
func TestJSONLBytesCounts_ReplacementsMatchWhatTheSpliceWrites(t *testing.T) {
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"acme": `ACME-[0-9]{8}`}})

	cases := []struct {
		name   string
		line   string
		layers map[string]int
	}{
		{
			name:   "array value also under a skipped id field",
			line:   `{"content":["ACME-12345678"],"session_id":"ACME-12345678"}`,
			layers: map[string]int{"custom": 1},
		},
		{
			name:   "credential key value repeated outside its credential context",
			line:   `{"db":{"host":"db","user":"app","pwd":"hunter"},"misc":{"pwd":"hunter"}}`,
			layers: map[string]int{"credential_key": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := JSONLBytes([]byte(tc.line + "\n"))
			require.NoError(t, err)
			stored := strings.Count(string(out.Bytes()), RedactedPlaceholder)
			require.Equal(t, 2, stored, "both copies are replaced in the stored line")
			require.NotNil(t, out.Counts())
			require.Equal(t, stored, out.Counts().Replacements)
			for layer, n := range tc.layers {
				require.Equal(t, n, out.Counts().Layers[layer], "layer %q counts the value it scanned", layer)
			}
		})
	}
}

// Redaction is per line, so the counts of a whole transcript are the sum of the
// counts of its parts. The prefix cache relies on this, and the whole here is
// large enough to be redacted in parallel shards while each half is not.
func TestJSONLBytesCounts_WholeEqualsSumOfParts(t *testing.T) {
	configureCountsRules(t)

	var half strings.Builder
	secrets := 0
	for i := 0; half.Len() < jsonlShardTargetBytes+jsonlShardTargetBytes/10; i++ {
		if i%50 == 0 {
			half.WriteString(textLine(t, "ticket ACME-12345678 for alice@corp-mail.org"))
			secrets += 2
			continue
		}
		half.WriteString(textLine(t, "routine tool output line with nothing secret in it at all"))
	}
	first, second := half.String(), half.String()
	require.Less(t, len(first), 2*jsonlShardTargetBytes, "each part must stay on the sequential path")
	require.GreaterOrEqual(t, len(first+second), 2*jsonlShardTargetBytes, "the whole must be sharded")

	whole := countsOf(t, first+second)
	require.Equal(t, 2*secrets, whole.Replacements)
	require.Equal(t, countsOf(t, first).Add(countsOf(t, second)), whole)
}

func TestCounts_Add(t *testing.T) {
	a := &Counts{Replacements: 3, Layers: map[string]int{"entropy": 2, "custom": 1}, CustomRulesActive: 2, PII: map[string]int{"EMAIL": 1}}
	b := &Counts{Replacements: 1, Layers: map[string]int{"entropy": 1, "goredact": 1}, CustomRulesActive: 1, PII: map[string]int{"PHONE": 2}}

	require.Equal(t, &Counts{
		Replacements:      4,
		Layers:            map[string]int{"entropy": 3, "custom": 1, "goredact": 1},
		CustomRulesActive: 2,
		PII:               map[string]int{"EMAIL": 1, "PHONE": 2},
	}, a.Add(b))
	require.Equal(t, 3, a.Replacements, "Add must not change its operands")

	// Unknown plus anything is unknown.
	require.Nil(t, a.Add(nil))
	require.Nil(t, (*Counts)(nil).Add(b))
}

func TestRedactedBytes_CountsOnlyFromRedaction(t *testing.T) {
	require.Nil(t, AlreadyRedacted([]byte("stored\n")).Counts(), "bytes redacted elsewhere have unknown counts")

	c := &Counts{Replacements: 1, Layers: map[string]int{"entropy": 1}}
	require.Equal(t, c, AlreadyRedacted([]byte("stored\n")).WithCounts(c).Counts())
}
