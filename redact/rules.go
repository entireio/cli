package redact

import (
	"regexp"
	"sort"
	"strings"
)

// Rule represents a single compiled secret detection pattern.
type Rule struct {
	ID          string
	Description string
	Regex       *regexp.Regexp
	Keywords    []string  // lowercase; used for fast pre-filtering
	Entropy     float64   // per-rule entropy threshold (0 = no entropy gate)
	SecretGroup int       // which regex capture group contains the secret (0 = full match)
	Allowlist   Allowlist
}

// Allowlist defines exclusion criteria for a rule.
type Allowlist struct {
	Regexes     []*regexp.Regexp
	RegexTarget string   // "match" or "line" (default: "match")
	Stopwords   []string // if the match contains any of these, skip it
}

// RuleSet holds all compiled rules, organized for fast keyword-based lookup.
type RuleSet struct {
	rules      []Rule
	keywordMap map[string][]int // keyword -> indices into rules slice
	noKeyword  []int            // rules that have no keywords (always checked)
}

// Match records a detected secret region in a string.
type Match struct {
	Start  int
	End    int
	RuleID string // "" for entropy-only matches
	Method string // "pattern", "entropy", or "pattern+entropy"
}

// NewRuleSet creates a RuleSet from compiled rules and builds the keyword index.
func NewRuleSet(rules []Rule) *RuleSet {
	rs := &RuleSet{
		rules:      rules,
		keywordMap: make(map[string][]int),
	}
	for i, r := range rules {
		if len(r.Keywords) == 0 {
			rs.noKeyword = append(rs.noKeyword, i)
		} else {
			for _, kw := range r.Keywords {
				rs.keywordMap[kw] = append(rs.keywordMap[kw], i)
			}
		}
	}
	return rs
}

// FindMatches scans s against all rules and returns matching regions.
func (rs *RuleSet) FindMatches(s string) []Match {
	if rs == nil || len(rs.rules) == 0 {
		return nil
	}

	lowered := strings.ToLower(s)

	// Collect candidate rule indices via keyword pre-filter.
	seen := make(map[int]bool)
	for kw, indices := range rs.keywordMap {
		if strings.Contains(lowered, kw) {
			for _, idx := range indices {
				seen[idx] = true
			}
		}
	}
	// Always include rules without keywords.
	for _, idx := range rs.noKeyword {
		seen[idx] = true
	}

	var matches []Match
	for idx := range seen {
		rule := &rs.rules[idx]
		locs := rule.Regex.FindAllStringSubmatchIndex(s, -1)
		for _, loc := range locs {
			start, end := extractSecretRegion(loc, rule.SecretGroup)
			if start < 0 || end < 0 || start >= end {
				continue
			}
			secret := s[start:end]

			// Check per-rule entropy threshold.
			if rule.Entropy > 0 && shannonEntropy(secret) < rule.Entropy {
				continue
			}

			// Check allowlist.
			if isAllowed(secret, s, &rule.Allowlist) {
				continue
			}

			matches = append(matches, Match{
				Start:  start,
				End:    end,
				RuleID: rule.ID,
				Method: "pattern",
			})
		}
	}
	return matches
}

// extractSecretRegion returns the start/end for the specified capture group.
// group 0 = full match, group N = Nth submatch.
func extractSecretRegion(loc []int, group int) (int, int) {
	idx := group * 2
	if idx+1 >= len(loc) {
		// Fallback to full match if group doesn't exist.
		return loc[0], loc[1]
	}
	return loc[idx], loc[idx+1]
}

// isAllowed returns true if the secret matches any allowlist exclusion.
func isAllowed(secret, fullLine string, al *Allowlist) bool {
	for _, sw := range al.Stopwords {
		if strings.Contains(strings.ToLower(secret), strings.ToLower(sw)) {
			return true
		}
	}
	for _, re := range al.Regexes {
		target := secret
		if al.RegexTarget == "line" {
			target = fullLine
		}
		if re.MatchString(target) {
			return true
		}
	}
	return false
}

// mergeRegions sorts matches by start position and merges overlapping/adjacent regions.
func mergeRegions(matches []Match) []Match {
	if len(matches) == 0 {
		return nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Start == matches[j].Start {
			return matches[i].End > matches[j].End // wider first
		}
		return matches[i].Start < matches[j].Start
	})
	merged := []Match{matches[0]}
	for _, m := range matches[1:] {
		last := &merged[len(merged)-1]
		if m.Start <= last.End {
			// Overlapping or adjacent — extend.
			if m.End > last.End {
				last.End = m.End
			}
			// Combine method info.
			if last.Method != m.Method && last.Method != "pattern+entropy" {
				last.Method = "pattern+entropy"
			}
		} else {
			merged = append(merged, m)
		}
	}
	return merged
}
