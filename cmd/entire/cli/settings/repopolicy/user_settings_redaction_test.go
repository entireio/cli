package repopolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

// The redaction block names an executable (its command becomes argv[0] at
// pre-push), so it is strict like `global`: an unknown key fails the load
// closed. That includes the team-policy keys `enabled` and `categories`, which
// belong in the repository's .entire/settings.json — what gets redacted is the
// team's call, how the binary is found is this machine's.
func TestLoadUserSettings_RedactionBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	writeUserSettingsForTest(t, dir, `{
		"global": {"enabled": true},
		"redaction": {"openai_privacy_filter": {"command": "/opt/opf/bin/opf", "timeout_seconds": 90, "prompt_default": "always"}}
	}`)
	settings, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	opf := settings.OPFConfig()
	if opf == nil || opf.Command != "/opt/opf/bin/opf" || opf.TimeoutSeconds != 90 || opf.PromptDefault != OPFPromptAlways {
		t.Fatalf("redaction block = %+v, want the three machine-local OPF fields", opf)
	}
	if !settings.GlobalEnabled() {
		t.Fatal("the global block must still decode alongside redaction")
	}

	for _, tt := range []struct{ name, body string }{
		{"unknown key in openai_privacy_filter", `{"redaction":{"openai_privacy_filter":{"command":"opf","comand":"x"}}}`},
		{"team-policy key enabled", `{"redaction":{"openai_privacy_filter":{"enabled":true}}}`},
		{"team-policy key categories", `{"redaction":{"openai_privacy_filter":{"categories":{"private_person":true}}}}`},
		{"unknown key in redaction", `{"redaction":{"pii":{}}}`},
		{"invalid prompt_default", `{"redaction":{"openai_privacy_filter":{"prompt_default":"sometimes"}}}`},
		{"negative timeout", `{"redaction":{"openai_privacy_filter":{"timeout_seconds":-1}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", dir)
			writeUserSettingsForTest(t, dir, tt.body)
			_, err := LoadUserSettings(t.Context())
			if err == nil || !strings.Contains(err.Error(), "redaction") {
				t.Fatalf("err = %v, want a redaction-block error that fails closed", err)
			}
		})
	}
}

// null means "unset" for every known block, the way an absent block does.
func TestLoadUserSettings_NullRedactionBlockIsUnset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	writeUserSettingsForTest(t, dir, `{"redaction": null, "global": null}`)
	settings, err := LoadUserSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if settings.Redaction != nil || settings.Global != nil || settings.OPFConfig() != nil {
		t.Fatalf("settings = %+v, want both blocks unset", settings)
	}
}

// json.Marshal uses the method set of the value it is handed. A pointer-only
// MarshalJSON would make json.Marshal(UserSettings{...}) fall back to the
// default encoder, which cannot see the unexported preserved blocks — so a
// caller passing the struct by value would silently drop a newer binary's
// settings on write. Marshaling by value must therefore round-trip everything.
func TestUserSettings_MarshalByValuePreservesEveryBlock(t *testing.T) {
	t.Parallel()
	const body = `{"global":{"enabled":true},"redaction":{"openai_privacy_filter":{"command":"opf"}},"future_feature":{"knob":1}}`
	var us UserSettings
	if err := json.Unmarshal([]byte(body), &us); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"by value":   mustMarshal(t, us),
		"by pointer": mustMarshal(t, &us),
	} {
		var round map[string]json.RawMessage
		if err := json.Unmarshal(data, &round); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, key := range []string{"global", "redaction", "future_feature"} {
			if _, ok := round[key]; !ok {
				t.Fatalf("%s: block %q dropped: %s", name, key, data)
			}
		}
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
