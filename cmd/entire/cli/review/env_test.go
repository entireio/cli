package review

import (
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

func TestEnvSkillsRoundtrip(t *testing.T) {
	t.Parallel()
	skills := []string{"/pr-review-toolkit:review-pr", "/test-auditor"}
	encoded, err := EncodeSkills(skills)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeSkills(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(skills) {
		t.Fatalf("got %d skills, want %d", len(decoded), len(skills))
	}
	for i := range skills {
		if decoded[i] != skills[i] {
			t.Errorf("skill[%d]: got %q, want %q", i, decoded[i], skills[i])
		}
	}
}

func TestEnvSkillsEmptyRoundtrip(t *testing.T) {
	t.Parallel()
	encoded, err := EncodeSkills(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if encoded != "[]" {
		t.Fatalf("EncodeSkills(nil) = %q, want []", encoded)
	}
	decoded, err := DecodeSkills(encoded)
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(decoded) != 0 {
		t.Fatalf("decoded empty skills = %v, want empty slice", decoded)
	}
}

func TestEnvNamesAreStable(t *testing.T) {
	t.Parallel()
	// Direct comparisons (not map iteration) so each constant is pinned
	// independently and the failure message names which constant broke.
	if EnvSession != "ENTIRE_REVIEW_SESSION" {
		t.Errorf("EnvSession: got %q, want ENTIRE_REVIEW_SESSION", EnvSession)
	}
	if EnvAgent != "ENTIRE_REVIEW_AGENT" {
		t.Errorf("EnvAgent: got %q, want ENTIRE_REVIEW_AGENT", EnvAgent)
	}
	if EnvSkills != "ENTIRE_REVIEW_SKILLS" {
		t.Errorf("EnvSkills: got %q, want ENTIRE_REVIEW_SKILLS", EnvSkills)
	}
	if EnvPrompt != "ENTIRE_REVIEW_PROMPT" {
		t.Errorf("EnvPrompt: got %q, want ENTIRE_REVIEW_PROMPT", EnvPrompt)
	}
	if EnvStartingSHA != "ENTIRE_REVIEW_STARTING_SHA" {
		t.Errorf("EnvStartingSHA: got %q, want ENTIRE_REVIEW_STARTING_SHA", EnvStartingSHA)
	}
}

func TestDecodeSkillsRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	if _, err := DecodeSkills("not json"); err == nil {
		t.Error("expected error for invalid JSON")
	}
	if _, err := DecodeSkills(""); err != nil {
		t.Errorf("expected empty string to decode as empty slice, got error: %v", err)
	}
}

// TestAppendReviewEnv_StripsPreExistingReviewVars pins the contract that
// AppendReviewEnv removes any pre-existing ENTIRE_REVIEW_* entries before
// appending the new values. Defense against nested invocations and stale
// env inheritance from a parent shell — duplicate keys would otherwise have
// implementation-defined precedence.
func TestAppendReviewEnv_StripsPreExistingReviewVars(t *testing.T) {
	t.Parallel()
	base := []string{
		"PATH=/usr/bin",
		"HOME=/home/u",
		EnvSession + "=stale",
		EnvAgent + "=stale-agent",
		EnvSkills + "=[\"/stale\"]",
		EnvPrompt + "=stale prompt",
		EnvStartingSHA + "=stalehash",
	}
	got := AppendReviewEnv(base, "claude-code", reviewtypes.RunConfig{
		Skills:      []string{"/fresh"},
		StartingSHA: "freshhash",
	}, "fresh prompt")

	// Each ENTIRE_REVIEW_* key should appear exactly once with the fresh value.
	want := map[string]string{
		EnvSession:     "1",
		EnvAgent:       "claude-code",
		EnvSkills:      `["/fresh"]`,
		EnvPrompt:      "fresh prompt",
		EnvStartingSHA: "freshhash",
	}
	counts := make(map[string]int)
	values := make(map[string]string)
	for _, kv := range got {
		for key := range want {
			prefix := key + "="
			if len(kv) > len(prefix) && kv[:len(prefix)] == prefix {
				counts[key]++
				values[key] = kv[len(prefix):]
			} else if kv == prefix {
				counts[key]++
				values[key] = ""
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

	// Non-review entries (PATH, HOME) must survive unchanged.
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
		t.Errorf("non-review env entries should survive: PATH=%v HOME=%v", pathSeen, homeSeen)
	}
}
