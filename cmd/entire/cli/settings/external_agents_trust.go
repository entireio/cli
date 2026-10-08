package settings

import (
	"context"
	"encoding/json"
)

// externalAgentsRejectionNotLocal and ...NotVerified explain why an
// external_agents grant was dropped. They mirror the OPF command's reasons
// because the hazard is the same one: a JSON settings diff that grants
// execution does not read as executable to a reviewer.
const (
	externalAgentsRejectionNotLocal   = "it did not come from .entire/settings.local.json"
	externalAgentsRejectionUnverified = "the local settings file could not be verified as untracked"
)

// enforceExternalAgentsTrust drops external_agents unless it came from a local
// file positively verified as this developer's own.
//
// external_agents is not a preference. It turns on the $PATH scan that globs
// for entire-agent-* binaries and runs each one's "info" subcommand, and it
// keeps running them for every hook thereafter. Honoring it from the
// version-controlled .entire/settings.json would let an ordinary pull request
// pair `{"external_agents": true}` with a binary committed next to it (or
// merely present on a shared machine's $PATH) and get it executed on every
// developer who pulls. The reasoning in enforceOPFCommandTrust applies
// unchanged: one line of JSON is a poor place to hide an exec grant, and there
// is no prompt in front of this one at all.
//
// localData is the raw local-file bytes, nil when the file is absent or its
// layer was dropped as tracked. The deep (index AND HEAD) check is used and
// localOwn is required: like the OPF command and unlike the layer as a whole,
// an unverifiable repository fails CLOSED, because being wrong means running
// someone else's binary rather than losing a preference.
//
// Rejection is a downgrade, never an error. Discovery simply does not run, and
// the user's own agents are unaffected — the interactive setup flows call
// DiscoverAndRegisterAlways and reach external plugins regardless, so the
// remedy stays available from the command that needs it.
func enforceExternalAgentsTrust(ctx context.Context, s *EntireSettings, localSettingsPath string, localData []byte) {
	// Only a true value grants anything, so only a true value is gated.
	// Warning about a false one would be noise about a privilege nobody asked
	// for.
	if s == nil || !s.ExternalAgents {
		return
	}

	var reason string
	switch {
	case !localSetsExternalAgents(localData):
		reason = externalAgentsRejectionNotLocal
	case classifyLocalSettingsDeep(ctx, localSettingsPath) != localOwn:
		reason = externalAgentsRejectionUnverified
	default:
		return
	}

	// Recorded rather than logged, for the reasons in enforceOPFCommandTrust:
	// the loader runs while the log level is still being resolved, and a line
	// in .entire/logs is not a signal the user sees.
	s.externalAgentsRejection = reason
	s.ExternalAgents = false
}

// localSetsExternalAgents reports whether the local override file explicitly
// sets external_agents.
//
// Presence of the key is what matters, not the merged value: both files may
// set true, and comparing effective values after the merge would credit the
// project layer for the local file's grant. A malformed local file yields
// false, the safe direction — the merge reports the parse error separately.
func localSetsExternalAgents(data []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return rawHasKey(raw, "external_agents")
}
