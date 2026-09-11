package settings

import (
	"context"
	"encoding/json"
	"sort"
)

// AgentPromptRejection reports one agent instruction field Load dropped as
// untrusted: the field's settings path (for example
// "review_profiles.security.agents.codex.prompt"), the dropped text, and why.
type AgentPromptRejection struct {
	Field  string
	Value  string
	Reason string
}

// agentPromptRejectionNotLocal and ...Unverified explain why an agent
// instruction field was dropped. They mirror the OPF command's reasons because
// the hazard is the same shape: a JSON settings diff carrying instructions for
// an agent that runs with approvals disabled does not read as executable to a
// reviewer.
const (
	agentPromptRejectionNotLocal   = "it did not come from .entire/settings.local.json or clone-local preferences"
	agentPromptRejectionUnverified = "the local settings file could not be verified as untracked"
)

// AgentPromptRejections reports the agent instruction fields Load dropped as
// untrusted. Consumers that would have applied a dropped field (review,
// investigate) should surface these on stderr, because it is the only signal
// that an instruction the user can see in a settings file is not in effect.
func (s *EntireSettings) AgentPromptRejections() []AgentPromptRejection {
	if s == nil {
		return nil
	}
	return s.agentPromptRejections
}

// enforceAgentPromptTrust drops agent instruction fields unless they came from
// a layer that is this developer's own: clone-local preferences (which live in
// the git common dir and cannot arrive by cloning), or a local settings file
// positively verified as untracked.
//
// The gated fields are the free-text instruction channels:
// investigate.always_prompt, every ReviewConfig.Prompt, and every review
// profile's Task. All of them land verbatim in the prompts of agents that
// investigate and review spawn with approval checks disabled (claude-code's
// bypassPermissions, codex's --dangerously-bypass-approvals-and-sandbox), and
// the prompt is the stated control for those spawns, so whoever writes these
// strings gets the last word in it. Task and Prompt are adjacent sections of
// the same composed prompt (see review's BuildReviewerPrompt), which is why
// gating one without the other would be a formality. .entire/settings.json is
// version-controlled, so honoring any of them from there would let an ordinary
// pull request steer an approvals-disabled agent on every developer who pulls.
// That is the enforceOPFCommandTrust reasoning, applied to instructions
// instead of argv.
//
// localData is the raw local-file bytes, nil when the file is absent or its
// layer was dropped as tracked. prefs is the clone-preferences layer as
// loaded, nil when absent. Like the OPF command and unlike the local layer as
// a whole, a local-sourced field on an unverifiable repository fails CLOSED
// (the deep index-and-HEAD check must return localOwn), because being wrong
// means an attacker steering a permission-bypassed agent rather than losing a
// preference.
//
// Provenance follows the merge order. The local layer replaces investigate
// wholesale and review profiles per profile name, so a field whose key is
// present in the local raw JSON was set by the local file. Otherwise a profile
// (or the legacy review map) present in clone preferences was set there, and
// anything left came from the committed project file and is dropped.
//
// Rejection is a downgrade, never an error: the field resets to "" (a dropped
// task falls back to review's built-in default text for the profile name) and
// the drop is recorded on the settings for the consumer to report.
//
// Deliberately not gated, with the reason each one is safe:
//   - Skills: review validates every configured skill against the installed
//     set before spawning (VerifyConfiguredSkillsInstalled), so free text
//     there fails the run rather than reaching an agent.
//   - Agent and Model: registry keys and routing hints, not instruction text.
//   - review_default_profile: it only selects among profiles whose
//     instruction content is itself gated, and gating it would break the
//     legitimate team convention of committing a default.
func enforceAgentPromptTrust(ctx context.Context, s *EntireSettings, localSettingsPath string, localData []byte, prefs *ClonePreferences) {
	if s == nil {
		return
	}

	var localRaw map[string]json.RawMessage
	if len(localData) > 0 {
		if err := json.Unmarshal(localData, &localRaw); err != nil {
			// A malformed local file contributes nothing, the safe direction.
			// The merge itself reports the parse error separately.
			localRaw = nil
		}
	}

	// The deep (index AND HEAD) trackedness check is asked at most once, and
	// only when some gated field's provenance is the local file.
	verified := false
	verdictKnown := false
	localVerified := func() bool {
		if !verdictKnown {
			verified = classifyLocalSettingsDeep(ctx, localSettingsPath) == localOwn
			verdictKnown = true
		}
		return verified
	}

	// decide returns the field's surviving value, recording a rejection when
	// it is dropped. setLocally must win over prefsOwned: a key present in the
	// local file merged last, so the effective value is the local file's even
	// when preferences also carried one.
	decide := func(field, value string, setLocally, prefsOwned bool) string {
		if value == "" {
			return ""
		}
		switch {
		case setLocally:
			if localVerified() {
				return value
			}
			s.agentPromptRejections = append(s.agentPromptRejections,
				AgentPromptRejection{Field: field, Value: value, Reason: agentPromptRejectionUnverified})
			return ""
		case prefsOwned:
			return value
		default:
			s.agentPromptRejections = append(s.agentPromptRejections,
				AgentPromptRejection{Field: field, Value: value, Reason: agentPromptRejectionNotLocal})
			return ""
		}
	}

	if s.Investigate != nil {
		// mergeInvestigate replaces the whole object, so a non-empty effective
		// value with the key present in the local raw JSON came from there.
		// Clone preferences carry no investigate block.
		s.Investigate.AlwaysPrompt = decide("investigate.always_prompt", s.Investigate.AlwaysPrompt,
			rawHasKey(localRaw, "investigate", "always_prompt"), false)
	}

	// keepWorkerPresent keeps a worker whose only configuration was a dropped
	// prompt from reading as unset. ReviewConfig.IsZero would report it empty,
	// review's placeholder filtering would silently remove it, and a profile
	// holding only such workers would fail selection before the drop notice
	// could print. Setting Agent to the map key is semantically a no-op (an
	// empty Agent already means "the key is the agent name"), so the worker
	// survives and runs on the profile task and built-in defaults.
	keepWorkerPresent := func(cfg *ReviewConfig, worker string, hadPrompt bool) {
		if hadPrompt && cfg.IsZero() {
			cfg.Agent = worker
		}
	}

	// Legacy review map: every layer replaces it wholesale, so its owner is
	// the last layer that set the key at all.
	_, localSetsReview := localRaw["review"]
	prefsOwnReview := !localSetsReview && prefs != nil && prefs.Review != nil
	for _, worker := range sortedKeys(s.Review) {
		cfg := s.Review[worker]
		hadPrompt := cfg.Prompt != ""
		cfg.Prompt = decide("review."+worker+".prompt", cfg.Prompt,
			rawHasKey(localRaw, "review", worker, "prompt"), prefsOwnReview)
		keepWorkerPresent(&cfg, worker, hadPrompt)
		s.Review[worker] = cfg
	}

	// Review profiles merge per profile name, so ownership is decided per
	// profile: the local layer if it sets the profile, else clone preferences
	// if they carry it, else the project file.
	for _, name := range sortedKeys(s.ReviewProfiles) {
		profile := s.ReviewProfiles[name]
		localSetsProfile := rawHasKey(localRaw, "review_profiles", name)
		prefsOwnProfile := !localSetsProfile && prefs != nil && prefsHaveProfile(prefs, name)
		// The task is the primary instruction channel: it lands in the same
		// composed prompt the per-agent Prompt is appended to, so leaving it
		// ungated would make the prompt gate a formality. A dropped task falls
		// back to the built-in text for conventional profile names, so the
		// stock profiles keep working.
		profile.Task = decide("review_profiles."+name+".task", profile.Task,
			rawHasKey(localRaw, "review_profiles", name, "task"), prefsOwnProfile)
		for _, worker := range sortedKeys(profile.Agents) {
			cfg := profile.Agents[worker]
			hadPrompt := cfg.Prompt != ""
			cfg.Prompt = decide("review_profiles."+name+".agents."+worker+".prompt", cfg.Prompt,
				rawHasKey(localRaw, "review_profiles", name, "agents", worker, "prompt"), prefsOwnProfile)
			keepWorkerPresent(&cfg, worker, hadPrompt)
			profile.Agents[worker] = cfg
		}
		if profile.Judge != nil {
			// keepWorkerPresent is deliberately not applied here. A judge's
			// identity is its Agent field (there is no map key to restore it
			// from), so a prompt-only judge never named an agent to run and was
			// already degenerate. Reading as zero sends it to review's
			// auto-select-a-judge fallback, which is the sane degradation.
			profile.Judge.Prompt = decide("review_profiles."+name+".judge.prompt", profile.Judge.Prompt,
				rawHasKey(localRaw, "review_profiles", name, "judge", "prompt"), prefsOwnProfile)
		}
		s.ReviewProfiles[name] = profile
	}
}

func prefsHaveProfile(prefs *ClonePreferences, name string) bool {
	_, ok := prefs.ReviewProfiles[name]
	return ok
}

// sortedKeys returns the map's keys in sorted order so rejections are recorded
// deterministically.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
