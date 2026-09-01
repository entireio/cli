package repopolicy

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The redaction block is exec-bearing (its command becomes argv[0] at
// pre-push), so it is strict: an unknown key is an error. That includes the
// team-policy keys `enabled` and `categories`, which belong in the
// repository's .entire/settings.json — what gets redacted is the team's call,
// how the binary is found is this machine's.
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
	if settings.RedactionError() != "" {
		t.Fatalf("RedactionError = %q, want empty on a valid block", settings.RedactionError())
	}
}

// A bad redaction block is dropped ALONE: dropping it is already fail-closed
// for exec (no command is honored from it), so a purely personal OPF typo
// must not take global tracking down machine-wide the way a `global` error
// does. The reason is recorded, and the block's bytes survive a
// read-modify-write so the user's content is never deleted out from under
// their fix.
func TestLoadUserSettings_BadRedactionBlockDropsAloneAndIsPreserved(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"unknown key in openai_privacy_filter", `{"global":{"enabled":true},"redaction":{"openai_privacy_filter":{"command":"opf","comand":"x"}}}`},
		{"team-policy key enabled", `{"global":{"enabled":true},"redaction":{"openai_privacy_filter":{"enabled":true}}}`},
		{"team-policy key categories", `{"global":{"enabled":true},"redaction":{"openai_privacy_filter":{"categories":{"private_person":true}}}}`},
		{"unknown key in redaction", `{"global":{"enabled":true},"redaction":{"pii":{}}}`},
		{"invalid prompt_default", `{"global":{"enabled":true},"redaction":{"openai_privacy_filter":{"prompt_default":"sometimes"}}}`},
		{"negative timeout", `{"global":{"enabled":true},"redaction":{"openai_privacy_filter":{"timeout_seconds":-1}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ENTIRE_CONFIG_DIR", dir)
			writeUserSettingsForTest(t, dir, tt.body)
			settings, err := LoadUserSettings(t.Context())
			if err != nil {
				t.Fatalf("a redaction error must drop the block, not fail the file: %v", err)
			}
			if !settings.GlobalEnabled() {
				t.Fatal("the global tier must survive a redaction-block error")
			}
			if settings.OPFConfig() != nil {
				t.Fatal("nothing from a dropped block may be honored")
			}
			if settings.RedactionError() == "" {
				t.Fatal("RedactionError must record the drop reason")
			}

			// A read-modify-write must round-trip the malformed block.
			if err := ModifyUserSettings(t.Context(), func(us *UserSettings) error {
				us.Global.TrustAll = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			reloaded, err := LoadUserSettings(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.RedactionError() == "" {
				t.Fatalf("the malformed redaction block must survive the rewrite; got a clean reload")
			}
			var orig, round map[string]json.RawMessage
			mustUnmarshalRaw(t, []byte(tt.body), &orig)
			raw, ok := reloaded.Block(userSettingsRedactionKey)
			if !ok {
				t.Fatal("dropped block bytes must be readable via Block")
			}
			var origCompact, roundCompact bytes.Buffer
			if err := json.Compact(&origCompact, orig["redaction"]); err != nil {
				t.Fatal(err)
			}
			if err := json.Compact(&roundCompact, raw); err != nil {
				t.Fatal(err)
			}
			if origCompact.String() != roundCompact.String() {
				t.Fatalf("redaction bytes changed across the rewrite: %s != %s", roundCompact.String(), origCompact.String())
			}
		})
	}
}

func mustUnmarshalRaw(t *testing.T, data []byte, dst *map[string]json.RawMessage) {
	t.Helper()
	if err := json.Unmarshal(data, dst); err != nil {
		t.Fatal(err)
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
	if settings.Redaction != nil || settings.Global != nil || settings.OPFConfig() != nil || settings.RedactionError() != "" {
		t.Fatalf("settings = %+v, want both blocks unset with no error", settings)
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
