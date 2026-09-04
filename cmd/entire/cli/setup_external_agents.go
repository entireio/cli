package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// discoverNamedExternalAgent resolves one external agent binary by name,
// bypassing the external_agents setting.
//
// Named rather than the full ungated sweep, wherever the caller already knows
// which agent it wants: DiscoverAndRegisterAlways globs every $PATH directory
// and executes every entire-agent-* binary it finds, in repositories that may
// never have opted into external agents at all. The named lookup returns
// immediately for a built-in, so passing an ordinary agent name costs nothing.
//
// The error is deliberately dropped. Every call site reports an unresolvable
// agent a few lines later in its own terms — validateSummaryProvider,
// applyAgentChanges, agent.Get plus printWrongAgentError — and surfacing this
// one instead would replace a message about the agent the user named with one
// about the plugin protocol.
func discoverNamedExternalAgent(ctx context.Context, name types.AgentName) {
	//nolint:errcheck,gosec // see doc comment: the caller reports the failure
	external.DiscoverAndRegisterNamedAlways(ctx, name)
}

// enableExternalAgentsLocally turns external_agents on in
// .entire/settings.local.json.
//
// Always the local file, whatever --local/--project said about the rest of the
// write. external_agents grants execution of entire-agent-* binaries found on
// $PATH, and settings.Load honors that grant only from an untracked local file
// (see settings.enforceExternalAgentsTrust). Writing it to the project file
// would produce a setting the user can read back but that never takes effect:
// the plugin they just chose would silently stop being discovered on the next
// command.
//
// The choice is machine-specific anyway — it depends on what is on this
// developer's $PATH — so the local file is where it belonged before the trust
// gate existed too. persistSummaryProviderSelection already reasoned its way
// to the same place.
//
// Raw read-modify-write, not a struct save: the merged struct carries the
// project file's fields as well, so writing it into the local file would copy
// everyone's settings into this developer's overrides. Same rule as
// setEnabledRaw.
//
// The write is verified rather than assumed. A tracked settings.local.json makes
// loadMergedSettings drop the whole local layer, so the key lands somewhere the
// loader will never read, and the usual rejection channel stays silent about it:
// enforceExternalAgentsTrust returns early on !s.ExternalAgents, which is the
// very state a dropped layer produces. Without the read-back the caller prints
// "external agents are now enabled" over a grant that does nothing.
func enableExternalAgentsLocally(ctx context.Context) (externalAgentsGrant, error) {
	path, raw, _, err := settings.LoadLocalRaw(ctx)
	if err != nil {
		return externalAgentsGrant{}, fmt.Errorf("failed to load local settings: %w", err)
	}
	raw["external_agents"] = json.RawMessage("true")
	if err := settings.SaveLocalRaw(path, raw); err != nil {
		return externalAgentsGrant{}, fmt.Errorf("failed to save external_agents setting: %w", err)
	}
	return verifyExternalAgentsGrant(ctx), nil
}

// externalAgentsGrant is what a grant write actually achieved, as opposed to
// what it attempted.
type externalAgentsGrant struct {
	// Effective reports that a fresh load honors the grant.
	Effective bool
	// Reason explains an ineffective grant in the user's terms. Empty when
	// Effective.
	Reason string
}

// verifyExternalAgentsGrant re-loads settings and reports whether the grant is
// honored, preferring whichever rejection channel actually recorded something.
//
// Two different mechanisms can swallow the grant and they record in different
// places: an unverifiable repository keeps the local layer but drops
// external_agents specifically (ExternalAgentsRejection), while a tracked local
// file drops the layer wholesale before the gate ever sees the key
// (LocalLayerRejection). Neither is guaranteed to be set, so there is a
// last-resort phrasing rather than an empty reason.
func verifyExternalAgentsGrant(ctx context.Context) externalAgentsGrant {
	s, err := settings.Load(ctx)
	if err != nil {
		return externalAgentsGrant{Reason: fmt.Sprintf("settings could not be re-read to confirm it (%v)", err)}
	}
	if s.ExternalAgents {
		return externalAgentsGrant{Effective: true}
	}
	if reason, rejected := s.ExternalAgentsRejection(); rejected {
		return externalAgentsGrant{Reason: reason}
	}
	if reason := s.LocalLayerRejection(); reason != "" {
		return externalAgentsGrant{Reason: reason}
	}
	return externalAgentsGrant{Reason: "the setting is not honored from " + settings.EntireSettingsLocalFile + " in this repository"}
}

// reportExternalAgentsGrant tells the user what the grant did. Never an error:
// a committed settings.local.json must not fail the command the user asked
// for, and the agent they chose still works for the rest of this run.
func reportExternalAgentsGrant(w io.Writer, grant externalAgentsGrant) {
	if grant.Effective {
		fmt.Fprintln(w, externalAgentsAutoEnabledNotice)
		return
	}
	warnIneffectiveExternalAgentsGrant(w, grant)
}

// warnIneffectiveExternalAgentsGrant is the half of the report that the grant
// sites which never announced success still need. Silent on success, because
// those sites treat a working grant as an implementation detail of "the agent
// you picked now works", and only a grant that did NOT take effect is news.
func warnIneffectiveExternalAgentsGrant(w io.Writer, grant externalAgentsGrant) {
	if grant.Effective {
		return
	}
	fmt.Fprintf(w, "Warning: external agents could not be enabled: %s.\n"+
		"  If %s is tracked by git, run `git rm --cached %s` and keep it out of version control.\n",
		grant.Reason, settings.EntireSettingsLocalFile, settings.EntireSettingsLocalFile)
}
