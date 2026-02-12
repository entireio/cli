package redact

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// gitleaksConfig represents the top-level structure of a gitleaks TOML file.
type gitleaksConfig struct {
	Title   string          `toml:"title"`
	Rules   []gitleaksRule  `toml:"rules"`
	// Global allowlist is parsed but not used (it's path-based, not relevant for content redaction).
}

// gitleaksRule represents a single rule in gitleaks TOML format.
type gitleaksRule struct {
	ID          string               `toml:"id"`
	Description string               `toml:"description"`
	Regex       string               `toml:"regex"`
	Keywords    []string             `toml:"keywords"`
	Entropy     float64              `toml:"entropy"`
	SecretGroup int                  `toml:"secretGroup"`
	Allowlists  []gitleaksAllowlist  `toml:"allowlists"`
}

// gitleaksAllowlist represents the allowlist section of a gitleaks rule.
type gitleaksAllowlist struct {
	Description string   `toml:"description"`
	RegexTarget string   `toml:"regexTarget"` // "match" or "line"
	Regexes     []string `toml:"regexes"`
	Stopwords   []string `toml:"stopwords"`
	Paths       []string `toml:"paths"` // parsed but not used
}

// ParseGitleaksConfig parses TOML bytes into a compiled RuleSet.
// Invalid rules (bad regexes) are skipped rather than causing a failure.
func ParseGitleaksConfig(data []byte) (*RuleSet, error) {
	var cfg gitleaksConfig
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing gitleaks config: %w", err)
	}

	cfg.Rules = applyBroadeningPatches(cfg.Rules)
	cfg.Rules = appendCustomRules(cfg.Rules)

	var compiled []Rule
	for _, gr := range cfg.Rules {
		if gr.Regex == "" {
			continue
		}
		re, err := regexp.Compile(gr.Regex)
		if err != nil {
			// Skip rules with invalid regexes rather than failing.
			continue
		}

		rule := Rule{
			ID:          gr.ID,
			Description: gr.Description,
			Regex:       re,
			Entropy:     gr.Entropy,
			SecretGroup: gr.SecretGroup,
		}

		// Lowercase all keywords for case-insensitive matching.
		for _, kw := range gr.Keywords {
			rule.Keywords = append(rule.Keywords, strings.ToLower(kw))
		}

		// Compile allowlists.
		for _, al := range gr.Allowlists {
			for _, reStr := range al.Regexes {
				alRe, err := regexp.Compile(reStr)
				if err != nil {
					continue
				}
				rule.Allowlist.Regexes = append(rule.Allowlist.Regexes, alRe)
			}
			if al.RegexTarget != "" {
				rule.Allowlist.RegexTarget = al.RegexTarget
			}
			rule.Allowlist.Stopwords = append(rule.Allowlist.Stopwords, al.Stopwords...)
		}
		if rule.Allowlist.RegexTarget == "" {
			rule.Allowlist.RegexTarget = "match"
		}

		compiled = append(compiled, rule)
	}

	return NewRuleSet(compiled), nil
}

// applyBroadeningPatches modifies gitleaks rules that are too restrictive for
// our zero-false-negative priority. Applied programmatically to keep the
// embedded TOML identical to upstream for easy updates.
func applyBroadeningPatches(rules []gitleaksRule) []gitleaksRule {
	for i, r := range rules {
		switch r.ID {
		case "aws-access-token":
			// Broaden charset from [A-Z2-7] to [A-Z0-9] and remove word boundaries.
			// AWS keys use Base32 but we prioritize recall over precision.
			// Clear allowlists — upstream skips "EXAMPLE" keys, but we redact everything.
			rules[i].Regex = `((?:A3T[A-Z0-9]|AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16})`
			rules[i].Entropy = 0
			rules[i].Allowlists = nil

		case "github-pat", "github-fine-grained-pat",
			"github-app-token", "github-oauth", "github-refresh-token":
			// GitHub token prefixes (ghp_, github_pat_, ghu_, gho_, ghs_, ghr_)
			// are deterministic identifiers — no entropy gate needed.
			rules[i].Entropy = 0
		}
	}
	return rules
}

// appendCustomRules adds detection rules not present in the upstream gitleaks config.
func appendCustomRules(rules []gitleaksRule) []gitleaksRule {
	custom := []gitleaksRule{
		{
			ID:          "anthropic-api-key",
			Description: "Anthropic API Key",
			Regex:       `sk-ant-[a-zA-Z0-9_-]{20,}`,
			Keywords:    []string{"sk-ant-"},
			Entropy:     0,
		},
	}
	return append(rules, custom...)
}
