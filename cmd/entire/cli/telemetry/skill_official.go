package telemetry

import "strings"

// Skill telemetry never sends a raw skill name unless it is recognized as one
// of Entire's own. Skill names are arbitrary user tokens — a custom slash
// command or third-party skill name can carry sensitive identifiers (project,
// vendor, customer), exactly like third-party plugin names (see the cli
// package's IsOfficialPlugin). Unlike plugins, the event itself is still worth
// counting — the metric this feeds compares skill adoption against agent-help —
// so unrecognized names are reported under a fixed category instead of being
// dropped.
//
// The emitted vocabulary is therefore closed: an allowlisted name, one of the
// two fixed categories below, and nothing else.

// entireSkillNamespace is the plugin name Entire publishes its skills under
// (entireio/skills and entireio/claude-plugins both ship a plugin named
// "entire"). Claude Code invokes a plugin skill as "<plugin>:<skill>", so its
// skills arrive namespaced: "entire:search".
//
// The namespace alone is NOT proof of provenance — it comes from whatever
// plugin the user installed or authored locally — so the leaf name is still
// matched against entireSkillNames before being sent. Everything after the
// colon is otherwise arbitrary user text.
const entireSkillNamespace = "entire:"

// scaffoldedAgentHelpSkill is the skill `entire enable --agent-help-skill`
// writes (.claude/skills/entire/SKILL.md and the codex/gemini equivalents).
// It is installed unnamespaced, under Entire's own name.
const scaffoldedAgentHelpSkill = "entire"

// entireSkillNames are the leaf names shipped by the "entire" plugin.
//
// Upstream is entireio/skills (marketplace plugin "entire"); entireio/
// claude-plugins ships an "explain" command under the same namespace. This
// list is hand-maintained across repositories, so a skill released upstream
// before this list is updated reports as unlistedEntireSkill rather than
// vanishing into customSkillCategory — see skillNameForTelemetry.
//
//nolint:gochecknoglobals // package-level allowlist, mirroring officialPlugins.
var entireSkillNames = map[string]struct{}{
	"address-findings":  {},
	"explain":           {},
	"recall":            {},
	"replay":            {},
	"review":            {},
	"search":            {},
	"session-crosslink": {},
	"session-handoff":   {},
	"session-to-skill":  {},
	"teach":             {},
	"using-entire":      {},
	"what-happened":     {},
}

const (
	// customSkillCategory is reported for any name not recognized as Entire's.
	customSkillCategory = "custom"
	// unlistedEntireSkill is reported for a skill invoked under Entire's
	// namespace whose leaf name this build does not know. It keeps adoption
	// volume correct across releases without sending the unrecognized name.
	unlistedEntireSkill = "entire:unlisted"
)

// skillNameForTelemetry maps a raw skill name to the value safe to send.
//
// Only the namespaced form is matched. Entire's skills are advertised as
// cross-agent and can be installed unnamespaced (Pi normalizes
// "/skill:<name>" to a bare name, and a hand-copied SKILL.md has no
// namespace), so those invocations report as custom. Matching bare leaf names
// would fold Claude Code's built-in /review and any user's local /search into
// Entire's numbers — an overcount that is invisible in the data, where this
// undercount at least has a known cause.
func skillNameForTelemetry(name string) string {
	if name == scaffoldedAgentHelpSkill {
		return name
	}
	leaf, namespaced := strings.CutPrefix(name, entireSkillNamespace)
	if !namespaced || leaf == "" {
		return customSkillCategory
	}
	if _, ok := entireSkillNames[leaf]; ok {
		// Sent verbatim (e.g. "entire:search") so the namespace is preserved
		// and a plugin invocation stays distinguishable from the scaffolded
		// skill above.
		return name
	}
	return unlistedEntireSkill
}
