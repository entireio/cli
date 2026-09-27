package antigravity

import (
	"path/filepath"
	"testing"
)

func TestHooksProbeSupported(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"1.1.22":  true,
		"1.1.12":  true,
		"v1.1.12": true,
		"1.1.11":  false,
		"1.1.1":   false,
		"1.0.16":  false,
		"":        false,
		"dev":     false,
	}
	for version, want := range cases {
		if got := HooksProbeSupported(version); got != want {
			t.Errorf("HooksProbeSupported(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestParseHooksProbeOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooksPath := filepath.Join(dir, ".agents", "hooks.json")

	// Real 1.1.22 envelope shape (agy -p /hooks --output-format json).
	out := []byte(`{"conversation_id":"","status":"SUCCESS","response":"entire\tenabled\tPreToolUse\t*\tcommand\tx\n","duration_seconds":0,"num_turns":0,"usage":{"input_tokens":0},"command":{"name":"hooks","data":{"hooks":[{"name":"entire","enabled":true,"source":"` + hooksPath + `","actions":[{"event":"PreToolUse","matcher":"*","type":"command","command":"x"}]}]}}}`)
	loaded, sources, err := parseHooksProbeOutput(out, hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded {
		t.Fatalf("want loaded=true for matching enabled entire entry, sources=%v", sources)
	}

	// Disabled entry does not count.
	disabled := []byte(`{"command":{"data":{"hooks":[{"name":"entire","enabled":false,"source":"` + hooksPath + `"}]}}}`)
	loaded, _, err = parseHooksProbeOutput(disabled, hooksPath)
	if err != nil || loaded {
		t.Fatalf("disabled entire entry must not count as loaded: loaded=%v err=%v", loaded, err)
	}

	// Another workspace's hooks.json does not count (untrusted-cwd trap).
	other := []byte(`{"command":{"data":{"hooks":[{"name":"entire","enabled":true,"source":"/elsewhere/.agents/hooks.json"}]}}}`)
	loaded, sources, err = parseHooksProbeOutput(other, hooksPath)
	if err != nil || loaded {
		t.Fatalf("other workspace source must not count: loaded=%v err=%v", loaded, err)
	}
	if len(sources) != 1 || sources[0] != "/elsewhere/.agents/hooks.json" {
		t.Fatalf("sources = %v, want the reported path for diagnostics", sources)
	}

	if _, _, err := parseHooksProbeOutput([]byte("not json"), hooksPath); err == nil {
		t.Fatal("garbage output must error, not report loaded=false silently")
	}
}

// A failed probe and a genuine empty hook list both arrive with no hooks, so
// dropping the envelope's status made them indistinguishable — and doctor's
// remediation for an empty list tells the user their hooks are NOT LOADED,
// which is the wrong advice for someone whose probe never ran.
func TestParseHooksProbeOutput_RejectsNonSuccessStatus(t *testing.T) {
	t.Parallel()

	hooksPath := filepath.Join(t.TempDir(), "hooks.json")

	loaded, _, err := parseHooksProbeOutput(
		[]byte(`{"status":"ERROR","command":{"name":"hooks","data":{"hooks":[]}}}`), hooksPath)
	if err == nil {
		t.Error("a non-success envelope must be reported as a failed probe, not as no hooks loaded")
	}
	if loaded {
		t.Error("loaded must stay false for a failed probe")
	}

	// An absent status is not evidence of failure: only a status agy actually
	// reported says anything about the probe.
	if _, _, err := parseHooksProbeOutput(
		[]byte(`{"command":{"name":"hooks","data":{"hooks":[]}}}`), hooksPath); err != nil {
		t.Errorf("an envelope with no status must still parse: %v", err)
	}
}
