package skilldiscovery

// CuratedSkill is an entry in the curated per-agent built-in list. Name is
// the skill's invocation form (slash-prefixed); Desc is the picker-visible
// description.
type CuratedSkill struct {
	Name string
	Desc string
}

// InstallHint is a per-agent message shown in the "Install more" section of
// the picker. ProvidesAny lists the discovered skill names whose presence
// means "this plugin is already installed" — if any of those appear in the
// discovered set, the hint is suppressed.
//
// When ProvidesAny is nil, the hint is always shown — use this for
// ecosystems where we can't predict plugin skill names (e.g. Gemini).
type InstallHint struct {
	Message     string
	ProvidesAny []string
}

// curatedBuiltins lists the review-adjacent commands that ship with each
// agent binary (no plugin install required). See
// docs/superpowers/specs/2026-04-22-entire-review-picker-install-awareness-design.md
// §Data model for the sources these names came from. Gemini CLI has no
// built-in review command and relies on the install hint below.
var curatedBuiltins = map[string][]CuratedSkill{
	"claude-code": {
		{Name: "/review", Desc: "Review changes and find issues"},
		{Name: "/security-review", Desc: "Scan git diff for security issues"},
		{Name: "/simplify", Desc: "Review recent changes for code quality"},
	},
	"codex":      {{Name: "/review", Desc: "Review current changes and find issues"}},
	"gemini-cli": {},
}

// installHints lists the passive install pointers shown in the picker when
// discovery didn't surface the plugin already. Messages are free-form text;
// ProvidesAny is the suppression fingerprint.
//
// Install commands below are placeholders until marketplace URLs are pinned.
// Tests do not assert on Message text — only on ProvidesAny semantics — so
// prose revisions do not break the suite.
var installHints = map[string][]InstallHint{
	"claude-code": {
		{
			Message: "Install `pr-review-toolkit` via `claude plugin install entireio/pr-review-toolkit`",
			ProvidesAny: []string{
				"/pr-review-toolkit:review-pr",
				"/pr-review-toolkit:code-reviewer",
				"/pr-review-toolkit:silent-failure-hunter",
			},
		},
		{
			Message:     "Install `test-auditor` via the superpowers plugin",
			ProvidesAny: []string{"/test-auditor"},
		},
	},
	"codex": {
		{
			Message:     "Install `codex-review-pack` via `codex plugins add <url>`",
			ProvidesAny: []string{"/codex:adversarial-review"},
		},
	},
	"gemini-cli": {
		{
			Message:     "Install `gemini-code-review` via `gemini extensions install <url>`",
			ProvidesAny: nil,
		},
	},
}

// CuratedBuiltinsFor returns the curated built-in list for agentName, or
// an empty slice if the agent is unknown. Callers must treat the return
// value as read-only.
func CuratedBuiltinsFor(agentName string) []CuratedSkill {
	return curatedBuiltins[agentName]
}

// ActiveInstallHintsFor returns the subset of installHints[agentName] whose
// ProvidesAny does NOT intersect the discovered set. When ProvidesAny is
// nil, the hint is always active.
//
// discovered is a set of skill names (map for O(1) membership); pass nil
// or an empty map when no skills have been discovered.
func ActiveInstallHintsFor(agentName string, discovered map[string]struct{}) []InstallHint {
	raw := installHints[agentName]
	if len(raw) == 0 {
		return nil
	}
	active := make([]InstallHint, 0, len(raw))
	for _, h := range raw {
		if isSuppressed(h, discovered) {
			continue
		}
		active = append(active, h)
	}
	return active
}

func isSuppressed(h InstallHint, discovered map[string]struct{}) bool {
	if len(h.ProvidesAny) == 0 {
		return false
	}
	for _, name := range h.ProvidesAny {
		if _, ok := discovered[name]; ok {
			return true
		}
	}
	return false
}

// IsEligible reports whether the given agent has any registry entry — either
// a curated built-in or an install hint. The picker uses this (intersected
// with "hooks installed") to decide whether to show a section for the agent.
func IsEligible(agentName string) bool {
	if len(curatedBuiltins[agentName]) > 0 {
		return true
	}
	if len(installHints[agentName]) > 0 {
		return true
	}
	return false
}
