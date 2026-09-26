package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// Why allow_symlinked_agent_dirs is dropped. The first two mirror the OPF
// command's and external_agents' reasons, because the hazard is the same shape:
// a JSON diff that changes WHERE Entire writes does not read as dangerous.
const (
	symlinkedAgentDirsRejectionNotLocal   = "it did not come from .entire/settings.local.json"
	symlinkedAgentDirsRejectionUnverified = "the local settings file could not be verified as untracked"
)

// enforceSymlinkedAgentDirsTrust drops allow_symlinked_agent_dirs unless it came
// from a local file positively verified as this developer's own.
//
// The entry it grants is not a preference. `.claude` and its siblings live in
// the working tree, and a working tree arrives by clone, so a repository that
// could both ship a symlink at `.claude` AND vouch for it in a tracked settings
// file would have `entire enable` create directories and write JSON wherever
// the link pointed. That is the exact attack the refusal exists to stop, so the
// permission to skip the refusal has to come from somewhere the repository
// cannot reach.
//
// localData is the raw local-file bytes, nil when the file is absent or its
// layer was dropped as tracked. Like the OPF command and external_agents, the
// deep (index AND HEAD) check is used and localOwn is required: an unverifiable
// repository fails CLOSED, because being wrong means writing through someone
// else's link rather than losing a preference.
//
// Rejection is a downgrade, never an error. The link is refused, which is
// exactly what happens with no setting at all, and the recorded reason is what
// tells the two apart.
func enforceSymlinkedAgentDirsTrust(ctx context.Context, s *EntireSettings, localSettingsPath string, localData []byte) {
	// An empty list grants nothing, so there is nothing to gate and nothing to
	// warn about.
	if s == nil || len(s.AllowSymlinkedAgentDirs) == 0 {
		return
	}

	var reason string
	switch {
	case !localSetsSymlinkedAgentDirs(localData):
		reason = symlinkedAgentDirsRejectionNotLocal
	case classifyLocalSettingsDeep(ctx, localSettingsPath) != localOwn:
		reason = symlinkedAgentDirsRejectionUnverified
	default:
		return
	}

	// Recorded rather than logged, for the reason enforceOPFCommandTrust gives:
	// the loader runs while the log level is still being resolved, and a line in
	// .entire/logs is not a signal the user sees.
	s.symlinkedAgentDirsRejection = reason
	s.AllowSymlinkedAgentDirs = nil
}

// localSetsSymlinkedAgentDirs reports whether the local override file
// explicitly sets allow_symlinked_agent_dirs.
//
// Presence of the key, not the merged value, for the reason
// localSetsExternalAgents gives: both files may set it, and comparing effective
// values after the merge would credit the project layer for the local file's
// grant. A malformed local file yields false, the safe direction.
func localSetsSymlinkedAgentDirs(data []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	return rawHasKey(raw, "allow_symlinked_agent_dirs")
}

// applyVouchedAgentDirs installs the surviving set as the process-wide policy
// and records anything the agent package refused by name.
//
// The call lives here because of the import direction: `settings` may import
// `agent`, and `agent` may not import `settings`, so the package that owns the
// value is the one that has to push it. Every consumer of the policy reaches it
// through a settings load first, including the eighteen files that call
// settings.Load directly and never pass the root pre-run.
//
// It runs on EVERY load, including one that produced an empty list, so a
// process whose settings change mid-run cannot keep following a link the user
// has since removed from the file.
//
// worktreeRoot scopes the policy to the tree these settings came from. The
// value is the settings file's grandparent, which is exactly what
// settingsAbsPaths built the path from (<root>/.entire/settings.json), so the
// key agent stores matches the worktreeRoot OpenHookConfig is later given.
//
// The name check inside SetVouchedSymlinkedDirs is a second, different
// boundary from the trust gate above, and both are needed. The gate answers
// "may this file grant anything"; the name check answers "is this path one an
// agent config actually lives in", which is what stops a developer's own local
// file from vouching for `.entire` or `.git/hooks` -- paths whose refusals are
// not up for negotiation whoever is asking.
func applyVouchedAgentDirs(s *EntireSettings, worktreeRoot string) {
	if s == nil {
		agent.SetVouchedSymlinkedDirs(worktreeRoot, nil)
		return
	}
	rejected := agent.SetVouchedSymlinkedDirs(worktreeRoot, s.AllowSymlinkedAgentDirs)
	if len(rejected) == 0 {
		return
	}
	unusable := fmt.Sprintf("%s is not an agent config directory (allowed: %s)",
		strings.Join(quoteAll(rejected), ", "), strings.Join(agent.VouchableSymlinkedDirs(), ", "))
	if s.symlinkedAgentDirsRejection == "" {
		s.symlinkedAgentDirsRejection = unusable
		return
	}
	s.symlinkedAgentDirsRejection += "; " + unusable
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}
