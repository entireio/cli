package telemetry

import "testing"

func TestSkillNameForTelemetry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		skill string
		want  string
	}{
		{"scaffolded agent-help skill", "entire", "entire"},
		{"scaffolded search skill", "entire-search", "entire-search"},
		{"entire plugin skill", "entire:search", "entire:search"},
		{"entire plugin command", "entire:explain", "entire:explain"},
		{"hyphenated entire plugin skill", "entire:what-happened", "entire:what-happened"},
		// A skill released upstream before this build's list was updated: the
		// invocation still counts as Entire adoption, without sending a name
		// the namespace does not vouch for.
		{"unknown skill under entire namespace", "entire:brand-new-skill", "entire:unlisted"},
		// The namespace comes from whatever plugin the user installed or
		// authored, so the leaf is arbitrary user text.
		{"customer identifier under entire namespace", "entire:customer-acme-incident-123", "entire:unlisted"},
		{"degenerate empty leaf", "entire:", "custom"},
		// Bare leaf names are deliberately NOT matched: /review is also a
		// Claude Code built-in and /search could be any local skill, so
		// matching them would overcount Entire adoption.
		{"bare name colliding with a built-in", "review", "custom"},
		{"bare name colliding with a local skill", "search", "custom"},
		{"third-party namespaced skill", "pr-review-toolkit:review-pr", "custom"},
		{"custom slash command", "customer-acme-incident-123", "custom"},
		{"lookalike prefix is not the namespace", "entire-internal:search", "custom"},
		{"case-sensitive namespace", "Entire:search", "custom"},
		{"empty", "", "custom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := skillNameForTelemetry(tt.skill); got != tt.want {
				t.Errorf("skillNameForTelemetry(%q) = %q, want %q", tt.skill, got, tt.want)
			}
		})
	}
}

func TestSkillNameForTelemetry_NeverEmitsAnUnrecognizedName(t *testing.T) {
	t.Parallel()

	// Whatever the input, the emitted value comes from a closed vocabulary:
	// an allowlisted Entire skill name or one of the two fixed categories.
	allowed := map[string]struct{}{
		scaffoldedAgentHelpSkill: {},
		scaffoldedSearchSkill:    {},
		customSkillCategory:      {},
		unlistedEntireSkill:      {},
	}
	for leaf := range entireSkillNames {
		allowed[entireSkillNamespace+leaf] = struct{}{}
	}

	inputs := []string{
		"", "entire", "entire:", "entire:search", "entire:secret-client-name",
		"secret-client-name", "review", "Entire:search", "entire:entire:search",
		"../../etc/passwd", "entire:../../etc/passwd", "skill:entire:search",
	}
	for _, in := range inputs {
		got := skillNameForTelemetry(in)
		if _, ok := allowed[got]; !ok {
			t.Errorf("skillNameForTelemetry(%q) = %q, which is outside the closed vocabulary", in, got)
		}
	}
}
