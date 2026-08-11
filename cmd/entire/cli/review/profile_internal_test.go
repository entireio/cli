package review

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// TestNormalizeLegacyCodexDefaultSkills_RetainsDollarSkills pins that the
// legacy-config repair only rewrites codex's exact ["/review"] artifact and
// leaves real configured $name skills (and multi-skill configs) untouched —
// so a user's $code-reviewer is never clobbered by the migration.
func TestNormalizeLegacyCodexDefaultSkills_RetainsDollarSkills(t *testing.T) {
	t.Parallel()
	configs := map[string]settings.ReviewConfig{
		"codex":        {Agent: "codex", Skills: []string{"$code-reviewer"}},
		"codex-multi":  {Agent: "codex", Skills: []string{"$code-reviewer", "$review-swarm"}},
		"codex-legacy": {Agent: "codex", Skills: []string{"/review"}},
	}
	normalizeLegacyCodexDefaultSkills(configs)

	if got := configs["codex"].Skills; len(got) != 1 || got[0] != "$code-reviewer" {
		t.Errorf("configured $code-reviewer must be retained; got %v", got)
	}
	if got := configs["codex-multi"].Skills; len(got) != 2 {
		t.Errorf("multi-skill codex config must be retained; got %v", got)
	}
	if got := configs["codex-legacy"].Skills; got != nil {
		t.Errorf("legacy exact [\"/review\"] codex must be cleared to prompt-only; got %v", got)
	}
}
