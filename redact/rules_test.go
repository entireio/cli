package redact

import (
	"testing"

	gitleaksconfig "github.com/zricethezav/gitleaks/v8/config"
)

func TestParseGitleaksConfig_BasicRule(t *testing.T) {
	t.Parallel()
	data := []byte(`
[[rules]]
id = "test-rule"
description = "Test rule"
regex = '''test_[a-z]{10}'''
keywords = ["test_"]
entropy = 3.0
`)
	rs, err := ParseGitleaksConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rs.rules) == 0 {
		t.Fatal("expected at least 1 rule")
	}
	// Find our test rule (custom rules are appended too).
	var found bool
	for _, r := range rs.rules {
		if r.ID == "test-rule" {
			found = true
			if r.Description != "Test rule" {
				t.Errorf("description = %q, want %q", r.Description, "Test rule")
			}
			if r.Entropy != 3.0 {
				t.Errorf("entropy = %f, want 3.0", r.Entropy)
			}
			if len(r.Keywords) != 1 || r.Keywords[0] != "test_" {
				t.Errorf("keywords = %v, want [test_]", r.Keywords)
			}
		}
	}
	if !found {
		t.Error("test-rule not found in parsed rules")
	}
}

func TestParseGitleaksConfig_InvalidRegex(t *testing.T) {
	t.Parallel()
	data := []byte(`
[[rules]]
id = "bad-rule"
description = "Bad regex"
regex = '''[invalid'''

[[rules]]
id = "good-rule"
description = "Good rule"
regex = '''good_[a-z]+'''
keywords = ["good_"]
`)
	rs, err := ParseGitleaksConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// bad-rule should be skipped, good-rule + custom rules should remain.
	var foundGood, foundBad bool
	for _, r := range rs.rules {
		if r.ID == "good-rule" {
			foundGood = true
		}
		if r.ID == "bad-rule" {
			foundBad = true
		}
	}
	if foundBad {
		t.Error("bad-rule should have been skipped")
	}
	if !foundGood {
		t.Error("good-rule should have been parsed")
	}
}

func TestParseGitleaksConfig_WithAllowlist(t *testing.T) {
	t.Parallel()
	data := []byte(`
[[rules]]
id = "test-with-allowlist"
description = "Test rule with allowlist"
regex = '''SECRET_[A-Z]{10}'''
keywords = ["secret_"]

[[rules.allowlists]]
regexes = ['''EXAMPLE$''']
stopwords = ["test"]
regexTarget = "match"
`)
	rs, err := ParseGitleaksConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var found bool
	for _, r := range rs.rules {
		if r.ID == "test-with-allowlist" {
			found = true
			if len(r.Allowlist.Regexes) != 1 {
				t.Errorf("expected 1 allowlist regex, got %d", len(r.Allowlist.Regexes))
			}
			if len(r.Allowlist.Stopwords) != 1 || r.Allowlist.Stopwords[0] != "test" {
				t.Errorf("stopwords = %v, want [test]", r.Allowlist.Stopwords)
			}
			if r.Allowlist.RegexTarget != "match" {
				t.Errorf("regexTarget = %q, want %q", r.Allowlist.RegexTarget, "match")
			}
		}
	}
	if !found {
		t.Error("test-with-allowlist not found")
	}
}

func TestParseGitleaksConfig_EmptyRegex(t *testing.T) {
	t.Parallel()
	data := []byte(`
[[rules]]
id = "empty-regex"
description = "No regex"
regex = ''
keywords = ["test"]
`)
	rs, err := ParseGitleaksConfig(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range rs.rules {
		if r.ID == "empty-regex" {
			t.Error("empty-regex should have been skipped")
		}
	}
}

func TestLoadDefaultRules(t *testing.T) {
	t.Parallel()
	rs, err := ParseGitleaksConfig([]byte(gitleaksconfig.DefaultConfig))
	if err != nil {
		t.Fatalf("failed to parse gitleaks DefaultConfig: %v", err)
	}
	if len(rs.rules) < 100 {
		t.Errorf("expected 100+ rules, got %d", len(rs.rules))
	}
}

func TestApplyBroadeningPatches_AWSAccessToken(t *testing.T) {
	t.Parallel()
	originalRegex := `\b((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z2-7]{16})\b`
	rules := []gitleaksRule{
		{
			ID:      "aws-access-token",
			Regex:   originalRegex,
			Entropy: 3,
			Allowlists: []gitleaksAllowlist{
				{Regexes: []string{`.+EXAMPLE$`}},
			},
		},
	}
	patched := applyBroadeningPatches(rules)
	if patched[0].Entropy != 0 {
		t.Errorf("expected entropy=0 after patch, got %f", patched[0].Entropy)
	}
	if patched[0].Regex == originalRegex {
		t.Error("expected regex to be broadened")
	}
	if patched[0].Allowlists != nil {
		t.Error("expected allowlists to be cleared for zero-false-negative priority")
	}
}

func TestApplyBroadeningPatches_GitHubPAT(t *testing.T) {
	t.Parallel()
	rules := []gitleaksRule{
		{ID: "github-pat", Regex: `ghp_[0-9a-zA-Z]{36}`, Entropy: 3},
		{ID: "github-fine-grained-pat", Regex: `github_pat_[0-9a-zA-Z]{82}`, Entropy: 3},
	}
	patched := applyBroadeningPatches(rules)
	for _, r := range patched {
		if r.Entropy != 0 {
			t.Errorf("rule %s: expected entropy=0 after patch, got %f", r.ID, r.Entropy)
		}
	}
}

func TestAppendCustomRules_AnthropicKey(t *testing.T) {
	t.Parallel()
	rules := appendCustomRules(nil)
	var found bool
	for _, r := range rules {
		if r.ID == "anthropic-api-key" {
			found = true
			if len(r.Keywords) == 0 || r.Keywords[0] != "sk-ant-" {
				t.Errorf("keywords = %v, want [sk-ant-]", r.Keywords)
			}
		}
	}
	if !found {
		t.Error("anthropic-api-key rule not found in custom rules")
	}
}
