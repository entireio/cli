package redact

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// resetCustomRulesForTest clears the package-level config so tests don't leak
// state into each other. Tests cannot run in parallel against the global, so
// the helper is invoked at top-of-test in a t.Cleanup pattern.
func resetCustomRulesForTest(t *testing.T) {
	t.Helper()
	customConfigMu.Lock()
	customConfig = nil
	customConfigMu.Unlock()
	t.Cleanup(func() {
		customConfigMu.Lock()
		customConfig = nil
		customConfigMu.Unlock()
	})
}

// captureSlogForTest installs a slog handler that writes JSON lines to buf.
// Returns a restore function the caller defers. Tests use this to assert
// that ConfigureCustomRules emits the right warnings for failing samples.
func captureSlogForTest(buf *bytes.Buffer) func() {
	prev := slog.Default()
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	slog.SetDefault(slog.New(h))
	return func() { slog.SetDefault(prev) }
}

func TestConfigureCustomRules_SkipsInvalidRegexAndContinues(t *testing.T) {
	resetCustomRulesForTest(t)

	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{
			"valid":   `[A-Z]{8}`,
			"invalid": `[unterminated`,
		},
	})

	cfg := getCustomRulesConfig()
	if cfg == nil {
		t.Fatal("expected config")
	}
	if got := len(cfg.rules); got != 1 {
		t.Fatalf("rules: want 1 (invalid dropped), have %d", got)
	}
	if cfg.rules[0].label != "valid" {
		t.Errorf("label: want valid, have %q", cfg.rules[0].label)
	}
}

func TestConfigureCustomRules_ConcurrentReadsAndWrites(t *testing.T) {
	resetCustomRulesForTest(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"x": `[a-z]+`}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_ = getCustomRulesConfig()
		}
	}()
	wg.Wait()
}

func TestString_CustomRuleEndToEnd(t *testing.T) {
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{
			"acme_token": `ACME_TOKEN_[A-Za-z0-9]{20,}`,
		},
	})

	in := "first=ACME_TOKEN_abc123def456ghi789jkl second=ACME_TOKEN_zyx987wvu654tsr321qpo"
	out := String(in)

	if got := strings.Count(out, "REDACTED"); got != 2 {
		t.Errorf("REDACTED count: want 2, have %d in %q", got, out)
	}
	if contains(t, out, "ACME_TOKEN_") {
		t.Errorf("raw token leaked into output: %q", out)
	}
	if contains(t, out, "[REDACTED_") {
		t.Errorf("custom rule used PII-style token: %q", out)
	}
}

func TestString_CustomRuleNotConfiguredIsNoop(t *testing.T) {
	resetCustomRulesForTest(t)

	in := "T_aaaaaa T_bbbbbb"
	if got := String(in); got != in {
		t.Errorf("expected unchanged %q, got %q", in, got)
	}
}

func contains(t *testing.T, s, sub string) bool {
	t.Helper()
	return strings.Contains(s, sub)
}

func TestConfigureCustomRules_SamplesPassEmitNoWarn(t *testing.T) {
	resetCustomRulesForTest(t)

	var buf bytes.Buffer
	restore := captureSlogForTest(&buf)
	defer restore()

	pack := &Pack{
		Name:    "ok",
		Version: "1.0.0",
		Rules: []Rule{
			{
				ID:    "match",
				Regex: `T_[a-z]{6}`,
				Samples: []Sample{
					{Input: "T_abcdef", Redacted: true},
					{Input: "T_short", Redacted: false},
				},
			},
		},
		sourcePath: "ok.yaml",
	}

	ConfigureCustomRules(CustomRulesConfig{Packs: []*Pack{pack}})

	if strings.Contains(buf.String(), `"sample"`) {
		t.Errorf("expected no sample warnings, got logs: %s", buf.String())
	}
}

func TestConfigureCustomRules_SamplesFailEmitWarnButKeepRule(t *testing.T) {
	resetCustomRulesForTest(t)

	var buf bytes.Buffer
	restore := captureSlogForTest(&buf)
	defer restore()

	pack := &Pack{
		Name:    "bad-sample",
		Version: "1.0.0",
		Rules: []Rule{
			{
				ID:    "match",
				Regex: `T_[a-z]{6}`,
				Samples: []Sample{
					{Input: "no_match", Redacted: true},
				},
			},
		},
		sourcePath: "bad-sample.yaml",
	}

	ConfigureCustomRules(CustomRulesConfig{Packs: []*Pack{pack}})

	logs := buf.String()
	if !strings.Contains(logs, `bad-sample.yaml`) {
		t.Errorf("warn missing pack path: %s", logs)
	}
	if !strings.Contains(logs, `"rule":"match"`) {
		t.Errorf("warn missing rule id: %s", logs)
	}

	cfg := getCustomRulesConfig()
	if cfg == nil || len(cfg.rules) != 1 {
		t.Fatalf("rule should remain active despite failing sample, have %v", cfg)
	}
}

func TestConfigureCustomRules_SampleMismatchWarnDoesNotLogRawInput(t *testing.T) {
	resetCustomRulesForTest(t)

	var buf bytes.Buffer
	restore := captureSlogForTest(&buf)
	defer restore()

	const sample = "ACME_TOKEN_should_not_appear_in_logs_1234567890"
	pack := &Pack{
		Name:    "safe-logs",
		Version: "1.0.0",
		Rules: []Rule{
			{
				ID:      "sample",
				Regex:   `NEVER_MATCHES`,
				Samples: []Sample{{Input: sample, Redacted: true}},
			},
		},
		sourcePath: "safe-logs.yaml",
	}

	ConfigureCustomRules(CustomRulesConfig{Packs: []*Pack{pack}})

	logs := buf.String()
	if strings.Contains(logs, sample) {
		t.Fatalf("sample mismatch log leaked raw sample: %s", logs)
	}
	if !strings.Contains(logs, `"sample_index":0`) {
		t.Errorf("warn missing sample index: %s", logs)
	}
	if !strings.Contains(logs, `"sample_length":`) {
		t.Errorf("warn missing sample length: %s", logs)
	}
}
