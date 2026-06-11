package investigate

import (
	"slices"
	"strings"
	"testing"
)

// TestEnvNamesAreStable pins each ENTIRE_INVESTIGATE_* constant by direct
// comparison so a rename surfaces on the specific constant that broke,
// rather than as one ambiguous map-iteration failure.
func TestEnvNamesAreStable(t *testing.T) {
	t.Parallel()
	if EnvSession != "ENTIRE_INVESTIGATE_SESSION" {
		t.Errorf("EnvSession: got %q, want ENTIRE_INVESTIGATE_SESSION", EnvSession)
	}
	if EnvAgent != "ENTIRE_INVESTIGATE_AGENT" {
		t.Errorf("EnvAgent: got %q, want ENTIRE_INVESTIGATE_AGENT", EnvAgent)
	}
	if EnvRunID != "ENTIRE_INVESTIGATE_RUN_ID" {
		t.Errorf("EnvRunID: got %q, want ENTIRE_INVESTIGATE_RUN_ID", EnvRunID)
	}
	if EnvTopic != "ENTIRE_INVESTIGATE_TOPIC" {
		t.Errorf("EnvTopic: got %q, want ENTIRE_INVESTIGATE_TOPIC", EnvTopic)
	}
	if EnvFindingsDoc != "ENTIRE_INVESTIGATE_FINDINGS_DOC" {
		t.Errorf("EnvFindingsDoc: got %q, want ENTIRE_INVESTIGATE_FINDINGS_DOC", EnvFindingsDoc)
	}
	if EnvStateDoc != "ENTIRE_INVESTIGATE_STATE_DOC" {
		t.Errorf("EnvStateDoc: got %q, want ENTIRE_INVESTIGATE_STATE_DOC", EnvStateDoc)
	}
	if EnvStartingSHA != "ENTIRE_INVESTIGATE_STARTING_SHA" {
		t.Errorf("EnvStartingSHA: got %q, want ENTIRE_INVESTIGATE_STARTING_SHA", EnvStartingSHA)
	}
}

// TestIsInvestigateEnvEntry pins the prefix-matching helper used to strip
// stale ENTIRE_INVESTIGATE_* entries before AppendInvestigateEnv writes new
// ones.
func TestIsInvestigateEnvEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kv   string
		want bool
	}{
		{EnvSession + "=1", true},
		{EnvAgent + "=claude-code", true},
		{EnvRunID + "=abcdef012345", true},
		{EnvTopic + "=topic", true},
		{EnvFindingsDoc + "=/tmp/x", true},
		{EnvStateDoc + "=/tmp/state.json", true},
		{EnvStartingSHA + "=deadbeef", true},
		{"PATH=/usr/bin", false},
		{"HOME=/home/u", false},
		{"ENTIRE_REVIEW_SESSION=1", false},    // review entries are not investigate entries
		{"ENTIRE_INVESTIGATE_OTHER=1", false}, // unknown investigate-style key
		{"NOT_ENTIRE_INVESTIGATE_SESSION", false},
	}
	for _, tc := range tests {
		if got := IsInvestigateEnvEntry(tc.kv); got != tc.want {
			t.Errorf("IsInvestigateEnvEntry(%q) = %v, want %v", tc.kv, got, tc.want)
		}
	}
}

// TestAppendInvestigateEnv_StripsStaleInvestigateAndReview pins the contract
// that AppendInvestigateEnv removes both ENTIRE_INVESTIGATE_* and
// ENTIRE_REVIEW_* entries before appending fresh values. The review-strip
// is the risk-mitigation guard for a child investigate process inheriting
// review env from a parent shell.
func TestAppendInvestigateEnv_StripsStaleInvestigateAndReview(t *testing.T) {
	t.Parallel()
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		// stale investigate vars from a previous run
		EnvSession + "=stale",
		EnvAgent + "=stale-agent",
		EnvRunID + "=staleeeeeeee",
		EnvTopic + "=stale topic",
		EnvFindingsDoc + "=/tmp/stale-findings.md",
		EnvStateDoc + "=/tmp/stale-state.json",
		EnvStartingSHA + "=stalehash",
		// stale review vars from an outer review process
		"ENTIRE_REVIEW_SESSION=1",
		"ENTIRE_REVIEW_AGENT=stale-review-agent",
		"ENTIRE_REVIEW_SKILLS=[\"/stale\"]",
		"ENTIRE_REVIEW_PROMPT=stale review prompt",
		"ENTIRE_REVIEW_STARTING_SHA=stalehash",
	}
	got := AppendInvestigateEnv(base, AppendOptions{
		AgentName:   "claude-code",
		RunID:       "abcdef012345",
		Topic:       "fresh topic",
		FindingsDoc: "/tmp/fresh-findings.md",
		StateDoc:    "/tmp/fresh-state.json",
		StartingSHA: "freshhash",
	})

	want := map[string]string{
		EnvSession:     "1",
		EnvAgent:       "claude-code",
		EnvRunID:       "abcdef012345",
		EnvTopic:       "fresh topic",
		EnvFindingsDoc: "/tmp/fresh-findings.md",
		EnvStateDoc:    "/tmp/fresh-state.json",
		EnvStartingSHA: "freshhash",
	}
	counts := make(map[string]int)
	values := make(map[string]string)
	for _, kv := range got {
		for key := range want {
			prefix := key + "="
			if strings.HasPrefix(kv, prefix) {
				counts[key]++
				values[key] = kv[len(prefix):]
			}
		}
	}
	for key, wantVal := range want {
		if counts[key] != 1 {
			t.Errorf("%s: expected exactly 1 occurrence, got %d", key, counts[key])
		}
		if values[key] != wantVal {
			t.Errorf("%s: got %q, want %q", key, values[key], wantVal)
		}
	}

	// Review entries from the parent must NOT survive — the contract is that
	// they are stripped to prevent cross-tagging.
	for _, kv := range got {
		for _, name := range []string{
			"ENTIRE_REVIEW_SESSION=",
			"ENTIRE_REVIEW_AGENT=",
			"ENTIRE_REVIEW_SKILLS=",
			"ENTIRE_REVIEW_PROMPT=",
			"ENTIRE_REVIEW_STARTING_SHA=",
		} {
			if strings.HasPrefix(kv, name) {
				t.Errorf("review env entry survived strip: %q", kv)
			}
		}
	}

	// Non-investigate, non-review entries must survive unchanged.
	pathSeen := false
	homeSeen := false
	for _, kv := range got {
		if kv == "PATH=/usr/bin" {
			pathSeen = true
		}
		if kv == "HOME=/home/u" {
			homeSeen = true
		}
	}
	if !pathSeen || !homeSeen {
		t.Errorf("unrelated env entries should survive: PATH=%v HOME=%v", pathSeen, homeSeen)
	}
}

// TestAppendInvestigateEnv_AppendsAllKeys checks that even when base has no
// stale entries, every contract key is appended to the returned slice with
// the value from AppendOptions.
func TestAppendInvestigateEnv_AppendsAllKeys(t *testing.T) {
	t.Parallel()
	got := AppendInvestigateEnv(nil, AppendOptions{
		AgentName:   "codex",
		RunID:       "0123456789ab",
		Topic:       "topic",
		FindingsDoc: "/abs/findings.md",
		StateDoc:    "/abs/state.json",
		StartingSHA: "abc123",
	})
	want := []string{
		EnvSession + "=1",
		EnvAgent + "=codex",
		EnvRunID + "=0123456789ab",
		EnvTopic + "=topic",
		EnvFindingsDoc + "=/abs/findings.md",
		EnvStateDoc + "=/abs/state.json",
		EnvStartingSHA + "=abc123",
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing env entry %q in %v", w, got)
		}
	}
}
