package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"

	"github.com/spf13/cobra"
)

type headLinkage struct {
	commitHash    string
	checkpointIDs []string
}

func newStatusCmd() *cobra.Command {
	var detailed bool
	var jsonFlag bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show Entire status",
		Long:  "Show whether Entire is currently enabled or disabled",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout(), detailed, jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&detailed, "detailed", false, "Show detailed status for each settings file")
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.MarkFlagsMutuallyExclusive("detailed", "json")

	return cmd
}

func runStatus(ctx context.Context, w io.Writer, detailed, jsonOutput bool) error {
	if jsonOutput {
		return runStatusJSON(ctx, w)
	}

	// Check if we're in a git repository. Not being in one is a valid status
	// now that the global tier exists machine-wide: report the global line
	// plus a short note instead of failing.
	if _, repoErr := paths.WorktreeRoot(ctx); repoErr != nil {
		sty := newStatusStyles(w)
		fmt.Fprintln(w, "✕ not a git repository")
		writeGlobalTrackingLine(ctx, w, sty)
		fmt.Fprintln(w, sty.render(sty.dim, "Run inside a git repository for repo-level status."))
		return nil //nolint:nilerr // Not being in a git repo is a valid status, not an error
	}

	// Get absolute paths for settings files
	settingsPath, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		settingsPath = EntireSettingsFile
	}
	localSettingsPath, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		localSettingsPath = EntireSettingsLocalFile
	}

	// Check which settings files exist
	_, projectErr := os.Lstat(settingsPath)
	if projectErr != nil && !errors.Is(projectErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot access project settings file: %w", projectErr)
	}
	_, localErr := os.Lstat(localSettingsPath)
	if localErr != nil && !errors.Is(localErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot access local settings file: %w", localErr)
	}
	projectExists := projectErr == nil
	localExists := localErr == nil

	sty := newStatusStyles(w)

	if !projectExists && !localExists {
		// This is exactly where the global tier matters: a repo with no
		// repo-level setup may still be tracked machine-wide. Telling that
		// user to run `entire enable` would contradict the feature — so a
		// globally tracked repo gets the enabled-shaped block instead.
		info := computeGlobalTrackingInfo(ctx)
		if info.trackedHere() {
			fmt.Fprintln(w, formatGloballyTrackedStatusShort(ctx, sty))
			renderGlobalTrackingLine(w, sty, info)
			writeUserLayerRejections(ctx, w, nil)
			writeActiveSessions(ctx, w, sty)
			writeAgentHelpHint(w, sty)
			return nil
		}
		fmt.Fprintln(w, "○ not set up (run `entire enable` to get started)")
		renderGlobalTrackingLine(w, sty, info)
		writeUserLayerRejections(ctx, w, nil)
		return nil
	}

	// Repo-level setup exists. If the user's exclude lists carve this repo
	// out anyway, say so instead of "● Enabled": the hooks are inactive here
	// and nothing syncs, whatever the committed settings file says.
	info := computeGlobalTrackingInfo(ctx)
	if info.excludedHere() {
		fmt.Fprintln(w, formatExcludedStatusShort(ctx, sty, info))
		renderGlobalTrackingLine(w, sty, info)
		writeUserLayerRejections(ctx, w, nil)
		writeAgentHelpHint(w, sty)
		return nil
	}

	if detailed {
		return runStatusDetailed(ctx, w, sty, settingsPath, localSettingsPath, projectExists, localExists)
	}

	// Short output: just show the effective/merged state
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	fmt.Fprintln(w, formatSettingsStatusShort(ctx, s, sty))
	renderGlobalTrackingLine(w, sty, info)
	writeUserLayerRejections(ctx, w, s)
	if s.Enabled {
		writeActiveSessions(ctx, w, sty)
	}
	writeAgentHelpHint(w, sty)

	return nil
}

// writeUserLayerRejections prints the user-settings preference blocks that
// Load dropped. The user file is machine-wide and its preference blocks apply
// regardless of repo enablement, so EVERY branch of runStatus shows this.
// Callers that already hold loaded settings pass them; the globally-tracked,
// not-set-up, and excluded branches pass nil and the rejections are computed
// from the user file ALONE (settings.UserPreferenceRejections) — never
// through a full settings load, whose failure on a broken repo-side file
// would otherwise silently hide warnings about an unrelated file.
func writeUserLayerRejections(ctx context.Context, w io.Writer, s *EntireSettings) {
	var rejections []string
	if s != nil {
		rejections = s.UserLayerRejections()
	} else {
		rejections = settings.UserPreferenceRejections(ctx)
	}
	// A dropped user-settings block is otherwise invisible in the short view:
	// the file is machine-wide, so unlike a repo settings problem there is no
	// second surface (a teammate, a PR diff) that would ever catch it.
	for _, reason := range rejections {
		fmt.Fprintf(w, "  user settings: %s ignored (%s) · run `entire status --detailed`\n", reason, settings.UserSettingsPath())
	}
}

// globalTrackingInfo is the shared computation behind the text and JSON
// global-tracking status, so the two outputs cannot drift. Local reads only.
type globalTrackingInfo struct {
	// Configured is false while the machine-wide question was never answered
	// (global tier absent from the user settings file, or the file is
	// unreadable) — the status line is omitted entirely then.
	Configured   bool
	Enabled      bool
	SettingsPath string
	// SettingsError is set when the user settings file exists but cannot be
	// read or parsed; the tier is then off machine-wide (fail closed).
	SettingsError    string
	ActivationSource repopolicy.ActivationSource
	// AgentsCovered counts agents whose user-level hooks are installed; only
	// meaningful when Enabled.
	AgentsCovered int
	// InRepo reports whether the per-repo activation could be evaluated (a
	// worktree resolved); ActiveHere/InactiveReason are meaningless otherwise.
	InRepo bool
	// ActiveHere is the hook gate's answer for THIS worktree
	// (settings.IsActiveForRepoWithReason) — the machine-wide "on" must not
	// read as coverage in a repo the gate carves out (excluded, disabled).
	ActiveHere     bool
	InactiveReason settings.InactiveReason
	// PolicyError is set when the per-repo classification itself failed
	// (unreadable settings, git unavailable); ActiveHere is then unknown.
	PolicyError string
	// TrustState is the per-repo checkpoint egress consent
	// (trustStateTrusted/Untrusted/NotApplicable), "" outside a repository.
	TrustState  string
	TrustSource settings.TrustSource
	// TrustReason explains an untrusted state ("untrusted" — no grant;
	// "identity_unresolved" — the consent key could not be derived, so only
	// trust_all can clear it; "settings_error"); "" otherwise.
	TrustReason string
	// HeldCheckpoints counts checkpoints held locally (untrusted state only).
	HeldCheckpoints int
	// SyncRemote names the elected checkpoint sync remote the trust decision
	// is keyed on ("" when none is configured or the tier is off).
	SyncRemote string
}

// trackedHere reports that the user-global tier is capturing sessions in
// this worktree: the tier is on and readable, the repo classified, and the
// hook gate did not carve it out. This is the condition under which a repo
// with no repo-level settings is "enabled" from the user's point of view.
func (info globalTrackingInfo) trackedHere() bool {
	return info.Configured && info.SettingsError == "" && info.Enabled &&
		info.InRepo && info.PolicyError == "" && info.ActiveHere
}

// excludedHere reports that the tier is on and the hook gate carves this
// worktree out by the user's exclude lists. For a repo WITH repo-level setup
// that is the one state where the raw settings file ("enabled") and the
// policy ("inactive") disagree, and status must side with the policy — the
// header, the sync lines, and --json's `enabled` all describe what the hooks
// will actually do.
func (info globalTrackingInfo) excludedHere() bool {
	return info.Configured && info.SettingsError == "" && info.Enabled &&
		info.InRepo && info.PolicyError == "" && !info.ActiveHere &&
		info.InactiveReason == settings.InactiveReasonGlobalExcluded
}

// Trust-state identifiers, shared between the human line and the JSON field so
// the two outputs cannot drift.
const (
	trustStateTrusted       = "trusted"
	trustStateUntrusted     = "untrusted"
	trustStateNotApplicable = "not_applicable"
)

func computeGlobalTrackingInfo(ctx context.Context) globalTrackingInfo {
	us, err := settings.LoadUserSettings(ctx)
	if err != nil {
		// The file exists but cannot be read: global tracking is silently off
		// machine-wide. Unlike an absent file, that deserves a line.
		return globalTrackingInfo{Configured: true, SettingsPath: settings.UserSettingsPath(), SettingsError: err.Error()}
	}
	if !us.GlobalConfigured() {
		return globalTrackingInfo{}
	}
	info := globalTrackingInfo{
		Configured:   true,
		Enabled:      us.GlobalEnabled(),
		SettingsPath: settings.UserSettingsPath(),
	}
	if !info.Enabled {
		if _, err := paths.WorktreeRoot(ctx); err == nil {
			info.TrustState = trustStateNotApplicable
			info.TrustSource = settings.TrustSourceNone
		}
		return info
	}
	supports, _ := agent.UserHookSupports()
	for _, ua := range supports {
		// A config that cannot be read is not verified coverage; doctor
		// surfaces the error itself.
		if ok, err := ua.Support.AreUserHooksInstalled(ctx); err == nil && ok {
			info.AgentsCovered++
		}
	}
	if _, err := paths.WorktreeRoot(ctx); err == nil {
		info.InRepo = true
		policy, policyErr := repopolicy.ClassifyRepoPolicy(ctx)
		if policyErr != nil {
			// Not "inactive here": the answer is unknown. Say so instead of
			// letting the zero-value reason read as a deliberate carve-out.
			info.PolicyError = policyErr.Error()
		} else {
			ctx = repopolicy.WithRepoPolicy(ctx, policy)
			info.ActiveHere = policy.Active
			info.InactiveReason = policy.InactiveReason
			info.ActivationSource = policy.ActivationSource
			info.TrustState, info.TrustSource = computeRepoTrustState(policy)
			if info.TrustState == trustStateUntrusted {
				info.TrustReason = string(policy.Trust.Reason)
			}
			info.SyncRemote = policy.Trust.Identity.RemoteName
		}
		if info.TrustState == trustStateUntrusted {
			info.HeldCheckpoints = heldCheckpointCount(ctx)
		}
	}
	return info
}

// computeRepoTrustState classifies this repo's egress consent for status.
// While the global tier is off an active repo syncs as on main and has no
// per-repo decision (not_applicable); with the tier on, every active repo
// is either trusted or held.
func computeRepoTrustState(policy repopolicy.RepoPolicy) (string, settings.TrustSource) {
	if !policy.Active || policy.Trust.Source == settings.TrustSourceLocal {
		return trustStateNotApplicable, settings.TrustSourceNone
	}
	if !policy.Trust.Allowed {
		return trustStateUntrusted, settings.TrustSourceNone
	}
	return trustStateTrusted, policy.Trust.Source
}

// heldCheckpointCount counts checkpoints held locally against the elected
// checkpoint sync remote, shared by status, doctor, and `entire trust`.
// Best-effort: any failure reads as 0. In dedicated checkpoint_remote mode the
// git-branch comparison is against a remote-tracking ref no push to a raw URL
// ever updates and would read "all held" forever, so — as in
// computeCheckpointSyncInfo — the count is reported only on the refs backend,
// where the local push queue is exact.
func heldCheckpointCount(ctx context.Context) int {
	elected, err := strategy.ResolveCheckpointSyncRemoteForTrust(ctx)
	if err != nil || elected.Name == "" {
		return 0
	}
	if s, loadErr := settings.Load(ctx); loadErr == nil && s.GetCheckpointRemote() != nil {
		if _, enabled, purlErr := checkpointremote.PushURL(ctx, elected.Name); purlErr == nil && enabled {
			if cpCfg, cfgErr := settings.LoadCheckpointsConfig(ctx); cfgErr == nil && checkpoint.PrimaryIsRefs(cpCfg) {
				return countUnpushedCheckpointsForStatus(ctx, "")
			}
			return 0
		}
	}
	return countUnpushedCheckpointsForStatus(ctx, elected.Name)
}

// writeGlobalTrackingLine prints the machine-wide tracking line: on with the
// user-level agent hook coverage count, off, or nothing while unconfigured.
// In a repo the hook gate carves out of the tier, the parenthetical names the
// carve-out instead — "on (2 agents covered)" in an excluded repo reads as
// covered when no session here is tracked.
func writeGlobalTrackingLine(ctx context.Context, w io.Writer, sty statusStyles) {
	renderGlobalTrackingLine(w, sty, computeGlobalTrackingInfo(ctx))
}

// renderGlobalTrackingLine is writeGlobalTrackingLine for a caller that has
// already computed the info (the not-set-up branch reads it twice otherwise).
func renderGlobalTrackingLine(w io.Writer, sty statusStyles, info globalTrackingInfo) {
	if !info.Configured {
		return
	}
	if info.SettingsError != "" {
		fmt.Fprintln(w, sty.render(sty.yellow, "global tracking: off — "+info.SettingsPath+" cannot be read; run `entire doctor`"))
		return
	}
	if !info.Enabled {
		fmt.Fprintln(w, sty.render(sty.dim, "global tracking: off"))
		return
	}
	if info.InRepo && info.PolicyError != "" {
		fmt.Fprintln(w, sty.render(sty.yellow, "global tracking: on (this repo could not be classified: "+info.PolicyError+")"))
		return
	}
	if info.InRepo && !info.ActiveHere {
		fmt.Fprintln(w, sty.render(sty.dim, "global tracking: on ("+globalInactiveHereText(info.InactiveReason)+")"))
		return
	}
	noun := "agents"
	if info.AgentsCovered == 1 {
		noun = "agent"
	}
	fmt.Fprintln(w, sty.render(sty.dim, fmt.Sprintf("global tracking: on (%d %s covered)", info.AgentsCovered, noun)))
	writeGlobalTrustLine(w, sty, info)
}

// writeGlobalTrustLine renders the per-repo egress consent under the tracking
// line: the trusted line names its source (a revoke masked by trust_all is
// auditable); not_applicable states stay silent.
func writeGlobalTrustLine(w io.Writer, sty statusStyles, info globalTrackingInfo) {
	switch info.TrustState {
	case trustStateUntrusted:
		line := "  sync held — repo not trusted · run `entire trust`"
		if info.SyncRemote != "" {
			line = "  sync held — repo not trusted for " + info.SyncRemote + " · run `entire trust`"
		}
		if info.HeldCheckpoints > 0 {
			line += fmt.Sprintf(" (%d held)", info.HeldCheckpoints)
		}
		fmt.Fprintln(w, sty.render(sty.yellow, line))
	case trustStateTrusted:
		label := "this repo"
		if info.TrustSource == settings.TrustSourceAll {
			label = "trust_all"
		}
		line := "  checkpoint sync: trusted (" + label + ")"
		if info.SyncRemote != "" {
			line += " → " + info.SyncRemote
		}
		fmt.Fprintln(w, sty.render(sty.dim, line))
	}
}

// globalInactiveHereText renders the hook gate's per-repo carve-out for the
// human tracking line, covering every inactive reason — not just exclusion —
// so a repo the gate carves out never reads as covered. Mirrors the JSON
// identifiers in inactiveReasonJSON.
func globalInactiveHereText(reason settings.InactiveReason) string {
	switch reason {
	case settings.InactiveReasonGlobalExcluded:
		return "this repo is excluded"
	case settings.InactiveReasonRepoDisabled:
		return "inactive here: repo-level setup has Entire disabled"
	case settings.InactiveReasonGlobalOff, settings.InactiveReasonNone:
		return "inactive here"
	default:
		return "inactive here"
	}
}

// agentHelpCommand is the invocation a coding agent runs to get machine-readable
// usage. It is surfaced both in the human status footer (writeAgentHelpHint) and
// in `entire status --json` (statusJSON.AgentHelp), so no-channel agents (Cursor,
// Copilot CLI, Factory Droid, MCP hosts) can discover entire's surface by reading
// either output.
const agentHelpCommand = "entire agent-help"

// writeAgentHelpHint prints a one-line pointer at `entire agent-help` for coding
// agents that have no context-injection channel (Cursor, Copilot CLI, Factory
// Droid) and so discover entire's surface only by reading command output.
func writeAgentHelpHint(w io.Writer, sty statusStyles) {
	fmt.Fprintln(w, sty.render(sty.dim, "Agents: run `"+agentHelpCommand+"` for machine-readable usage."))
}

// runStatusDetailed shows the effective status plus detailed status for each settings file.
func runStatusDetailed(ctx context.Context, w io.Writer, sty statusStyles, settingsPath, localSettingsPath string, projectExists, localExists bool) error {
	// First show the effective/merged status
	effectiveSettings, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	fmt.Fprintln(w, formatSettingsStatusShort(ctx, effectiveSettings, sty))
	writeGlobalTrackingLine(ctx, w, sty)
	fmt.Fprintln(w) // blank line

	// Show project settings if it exists
	if projectExists {
		projectSettings, err := settings.LoadFromFile(settingsPath)
		if err != nil {
			return fmt.Errorf("failed to load project settings: %w", err)
		}
		fmt.Fprintln(w, formatSettingsStatus("Project", projectSettings, sty))
	}

	// Show local settings if it exists. LoadFromFile is ungated, so this
	// renders the file's own contents — say so when the loader ignored them,
	// or the display contradicts the settings actually in effect.
	if localExists {
		localSettings, err := settings.LoadFromFile(localSettingsPath)
		if err != nil {
			return fmt.Errorf("failed to load local settings: %w", err)
		}
		label := "Local"
		if effectiveSettings.LocalLayerRejection() != "" {
			label = "Local (ignored)"
		}
		fmt.Fprintln(w, formatSettingsStatus(label, localSettings, sty))
		if reason := effectiveSettings.LocalLayerRejection(); reason != "" {
			fmt.Fprintf(w, "  %s\n  fix with: git rm --cached %s\n", reason, settings.EntireSettingsLocalFile)
		}
	}

	// Dropped user-settings-file preference blocks (unknown key, malformed
	// repos entry). The strict blocks (`global`, `redaction`) are not listed
	// here — a failure there fails the whole file, which doctor reports.
	if rejections := effectiveSettings.UserLayerRejections(); len(rejections) > 0 {
		fmt.Fprintf(w, "User settings (%s): %d block(s) ignored\n", settings.UserSettingsPath(), len(rejections))
		for _, reason := range rejections {
			fmt.Fprintf(w, "  %s\n", reason)
		}
	}

	if effectiveSettings.Enabled {
		writeActiveSessions(ctx, w, sty)
	}
	writeAgentHelpHint(w, sty)

	return nil
}

// writeBranchSegment appends " · branch <name>" whenever the worktree branch
// can be resolved.
func writeBranchSegment(ctx context.Context, b *strings.Builder, sty statusStyles) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}
	if branch := resolveWorktreeBranch(ctx, repoRoot); branch != "" {
		b.WriteString(sty.render(sty.dim, " · "))
		b.WriteString("branch ")
		b.WriteString(sty.render(sty.cyan, branch))
	}
}

// formatGloballyTrackedStatusShort is the header for a repo with no
// repo-level settings that the user-global tier captures anyway:
// "● Tracked globally · branch main". The agents covered and the trust state
// follow on the global-tracking lines, so the header carries neither.
func formatGloballyTrackedStatusShort(ctx context.Context, sty statusStyles) string {
	var b strings.Builder
	b.WriteString(sty.render(sty.green, "●"))
	b.WriteString(" ")
	b.WriteString(sty.render(sty.bold, "Tracked globally"))
	writeBranchSegment(ctx, &b, sty)
	return b.String()
}

// formatExcludedStatusShort is the header for a repo whose repo-level setup
// says enabled while the user's exclude lists keep it inactive on this
// machine: "○ Excluded on this machine · branch main", plus the reason.
func formatExcludedStatusShort(ctx context.Context, sty statusStyles, info globalTrackingInfo) string {
	var b strings.Builder
	b.WriteString(sty.render(sty.red, "○"))
	b.WriteString(" ")
	b.WriteString(sty.render(sty.bold, "Excluded on this machine"))
	writeBranchSegment(ctx, &b, sty)
	b.WriteString("\n")
	b.WriteString(sty.render(sty.dim, "  Repo-level setup would enable Entire here, but this repo matches an exclude list in "+info.SettingsPath))
	return b.String()
}

// formatSettingsStatusShort formats a short settings status line.
// Output format: "● Enabled · branch main" or "○ Disabled · branch main"
// (the branch segment is appended whenever it can be resolved).
func formatSettingsStatusShort(ctx context.Context, s *EntireSettings, sty statusStyles) string {
	var b strings.Builder

	if s.Enabled {
		b.WriteString(sty.render(sty.green, "●"))
		b.WriteString(" ")
		b.WriteString(sty.render(sty.bold, "Enabled"))
	} else {
		b.WriteString(sty.render(sty.red, "○"))
		b.WriteString(" ")
		b.WriteString(sty.render(sty.bold, "Disabled"))
	}

	writeBranchSegment(ctx, &b, sty)

	// Show enabled agents
	if s.Enabled {
		if displayNames := InstalledAgentDisplayNames(ctx); len(displayNames) > 0 {
			b.WriteString("\n")
			b.WriteString(sty.render(sty.dim, "  Agents · "))

			b.WriteString(strings.Join(displayNames, ", "))
		}

		// Warn when installed hooks are out of date (read-only; fix is manual).
		for _, displayName := range OutdatedHookAgentDisplayNames(ctx) {
			b.WriteString("\n")
			b.WriteString(sty.render(sty.yellow, "  ! "+displayName+" hooks out of date"))
			b.WriteString(sty.render(sty.dim, " · run 'entire enable --force'"))
		}
	}

	// Where checkpoint data syncs (the single elected remote), and how many
	// checkpoints have not reached it yet. Local-only computation.
	if s.Enabled {
		writeCheckpointSyncLines(ctx, &b, s, sty)
	}

	if s.Enabled {
		writeSecretScannersLine(&b, s, sty)
	}

	// Show review status for HEAD's checkpoint, if any.
	if reviewed, meta := headHasReviewCheckpoint(ctx); reviewed {
		b.WriteString("\n")
		b.WriteString(sty.render(sty.dim, "  Review · "))
		b.WriteString("reviewed (")
		b.WriteString(meta)
		b.WriteString(")")
	}

	// Show investigation status for HEAD's checkpoint, if any. Review and
	// investigation can both be true on the same checkpoint, so we render
	// both lines independently rather than gating one on the other.
	if investigated, meta := headHasInvestigateCheckpoint(ctx); investigated {
		b.WriteString("\n")
		b.WriteString(sty.render(sty.dim, "  Investigation · "))
		b.WriteString("investigated (")
		b.WriteString(meta)
		b.WriteString(")")
	}

	return b.String()
}

// nonDefaultSecretScanners reports the enabled engines (betterleaks, then
// goredact) when the selection differs from the default (betterleaks only);
// nil otherwise. Both disabled is unreachable via these callers:
// validateScannerSettings fail-closes merged settings before status loads them.
func nonDefaultSecretScanners(s *EntireSettings) []string {
	if s.BetterleaksEnabled() && !s.GoredactEnabled() {
		return nil
	}
	var parts []string
	if s.BetterleaksEnabled() {
		parts = append(parts, "betterleaks")
	}
	if s.GoredactEnabled() {
		parts = append(parts, "goredact")
	}
	return parts
}

func writeSecretScannersLine(b *strings.Builder, s *EntireSettings, sty statusStyles) {
	parts := nonDefaultSecretScanners(s)
	if len(parts) == 0 {
		return
	}
	b.WriteString("\n")
	b.WriteString(sty.render(sty.dim, "  Secret scanners · "))
	b.WriteString(strings.Join(parts, ", "))
}

// formatSettingsStatus formats a settings status line with source prefix.
// Output format: "Project · enabled" or "Local · disabled"
func formatSettingsStatus(prefix string, s *EntireSettings, sty statusStyles) string {
	var b strings.Builder
	b.WriteString(sty.render(sty.bold, prefix))
	b.WriteString(sty.render(sty.dim, " · "))

	if s.Enabled {
		b.WriteString("enabled")
	} else {
		b.WriteString("disabled")
	}

	return b.String()
}

// checkpointSyncSourceDedicated is synthesized by the status layer when a
// structured checkpoint_remote resolves to a dedicated store. It is never
// returned by strategy.ResolveCheckpointSyncRemote — the resolver's contract
// stays pure "which configured git remote" (spec Unit 1).
const checkpointSyncSourceDedicated = "dedicated"

// checkpointSyncInfo is the single shared computation behind both the text and
// JSON checkpoint-sync sections of `entire status`, so the two outputs cannot
// drift. Everything here reads local state only (settings, .git/config, local
// refs, the push queue) — status must stay network-free.
type checkpointSyncInfo struct {
	// Remote is the elected git remote name, or the org/repo slug in
	// dedicated checkpoint_remote mode. Empty when nothing resolved (no
	// remotes configured, or the fail-closed case).
	Remote string
	// Source is config|observed|default|sole|first (resolver values) or
	// "dedicated".
	Source string
	// Err is the fail-closed misconfiguration message from the resolver.
	Err string
	// Unpushed approximates checkpoints not yet on the sync destination; 0
	// when none, when counting failed, or when the count would be a lie
	// (dedicated URL mode on the git-branch backend).
	Unpushed int
}

func computeCheckpointSyncInfo(ctx context.Context, s *EntireSettings) checkpointSyncInfo {
	elected, err := strategy.ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		// Fail-closed: checkpoint_push_remote names a remote that does not
		// exist. The pre-push gate is silently skipping checkpoint sync, so
		// status is the user's signal.
		// Accepted divergence: if a structured checkpoint_remote is also
		// configured, the gate's dedicated exemption may still sync checkpoint
		// data even while this fail-closed warning is shown, since there is no
		// elected remote left to probe PushURL against here.
		return checkpointSyncInfo{Err: err.Error()}
	}
	if elected.Name == "" {
		return checkpointSyncInfo{} // no remotes configured: show nothing
	}

	// Dedicated checkpoint_remote mode is reported only when PushURL derives
	// an eligible URL for the elected remote, mirroring the pre-push
	// exemption (ps.hasCheckpointURL); otherwise the gate applies normal
	// single-remote sync, so status reports that instead. PushURL is
	// local-only; never call resolvePushSettings here — its follow-up
	// metadata fetch dials, and status must stay network-free.
	// Accepted divergence: a real push to a different named remote may derive
	// PushURL differently than this elected-remote probe does.
	if cr := s.GetCheckpointRemote(); cr != nil {
		if _, enabled, purlErr := checkpointremote.PushURL(ctx, elected.Name); purlErr == nil && enabled {
			info := checkpointSyncInfo{Remote: cr.Repo, Source: checkpointSyncSourceDedicated}
			// The unpushed counter is meaningful here only on the git-refs
			// backend (push-queue length is local and accurate). The
			// git-branch comparison is omitted: pushes to a raw URL update
			// no remote-tracking ref, so it would permanently read "all
			// unpushed".
			if cpCfg, cfgErr := settings.LoadCheckpointsConfig(ctx); cfgErr == nil && checkpoint.PrimaryIsRefs(cpCfg) {
				info.Unpushed = countUnpushedCheckpointsForStatus(ctx, "")
			}
			return info
		}
	}

	return checkpointSyncInfo{
		Remote:   elected.Name,
		Source:   string(elected.Source),
		Unpushed: countUnpushedCheckpointsForStatus(ctx, elected.Name),
	}
}

// countUnpushedCheckpointsForStatus counts best-effort: status must never fail
// because counting failed, so errors log at debug and read as "no counter".
func countUnpushedCheckpointsForStatus(ctx context.Context, remoteName string) int {
	n, err := strategy.CountUnpushedCheckpoints(ctx, remoteName)
	if err != nil {
		logging.Debug(ctx, "unpushed checkpoint count failed; omitting from status",
			slog.String("error", err.Error()))
		return 0
	}
	return n
}

// writeCheckpointSyncLines appends the checkpoint sync destination line (and
// the unpushed counter, when non-zero) to the enabled status block. Rendered
// whenever something resolved: an elected remote, a dedicated store, or the
// fail-closed misconfiguration. No remotes configured -> no lines.
func writeCheckpointSyncLines(ctx context.Context, b *strings.Builder, s *EntireSettings, sty statusStyles) {
	info := computeCheckpointSyncInfo(ctx, s)
	switch {
	case info.Err != "":
		b.WriteString("\n")
		b.WriteString(sty.render(sty.yellow, "  ! Checkpoints NOT syncing: "+info.Err))
	case info.Remote == "":
		return
	case info.Source == checkpointSyncSourceDedicated:
		b.WriteString("\n  Checkpoints sync to: ")
		b.WriteString(sty.render(sty.cyan, "dedicated checkpoint remote ("+info.Remote+")"))
	default:
		b.WriteString("\n  Checkpoints sync to: ")
		b.WriteString(sty.render(sty.cyan, info.Remote))
		switch info.Source {
		case string(strategy.SyncRemoteSourceConfig):
			b.WriteString(sty.render(sty.dim, " (set by checkpoint_push_remote)"))
		case string(strategy.SyncRemoteSourceObserved):
			b.WriteString(sty.render(sty.dim, " (follows your branch's push destination)"))
		}
	}
	if info.Unpushed > 0 {
		b.WriteString("\n  ")
		b.WriteString(sty.render(sty.dim, formatUnpushedCheckpointsLine(info)))
	}
}

// formatUnpushedCheckpointsLine phrases the unpushed counter. Dedicated URL
// mode has no git remote to name (and only reaches here on the git-refs
// backend), so it drops the remote-name phrasing.
func formatUnpushedCheckpointsLine(info checkpointSyncInfo) string {
	noun := "checkpoints"
	pronoun := "they sync"
	if info.Unpushed == 1 {
		noun = "checkpoint"
		pronoun = "it syncs"
	}
	if info.Source == checkpointSyncSourceDedicated {
		return fmt.Sprintf("%d %s not yet pushed", info.Unpushed, noun)
	}
	return fmt.Sprintf("%d %s not yet on %s — %s with your next 'git push %s'",
		info.Unpushed, noun, info.Remote, pronoun, info.Remote)
}

// timeAgo formats a time as a human-readable relative duration.
func timeAgo(t time.Time) string {
	return formatRelativeDuration(time.Since(t))
}

// formatRelativeDuration renders a positive duration as "just now" / "Xm ago"
// / "Xh ago" / "Xd ago". Shared between `entire status` and `entire auth list`
// so the bucket thresholds and labels stay consistent.
func formatRelativeDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return lastUsedJustNow
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// worktreeGroup groups sessions by worktree path for display.
type worktreeGroup struct {
	path     string
	branch   string
	sessions []*session.State
}

const (
	unknownPlaceholder  = "(unknown)"
	detachedHEADDisplay = "HEAD"
)

// writeActiveSessions writes active session information grouped by worktree.
func writeActiveSessions(ctx context.Context, w io.Writer, sty statusStyles) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return
	}

	states, err := store.List(ctx)
	if err != nil || len(states) == 0 {
		return
	}

	// Finalize any non-ended session whose agent process has exited without a
	// SessionStop hook firing, so it doesn't linger as "active" until the
	// inactivity timeout. The sweep marks them ended in place, so the filter
	// below drops them.
	if n := finalizeExitedSessions(ctx, states, time.Now().Add(interactiveSweepCondenseBudget)); n > 0 {
		fmt.Fprintln(w, sty.render(sty.dim, fmt.Sprintf("Finalized %d exited session(s) (agent process gone).", n)))
	}

	// Filter to active sessions only, per session.State.IsEnded — the same rule
	// `entire session stop` filters on, so status can't advertise a session that
	// stop then refuses to list. EndedAt alone is not it: `entire session attach`
	// sets Phase to ended without stamping EndedAt.
	var active []*session.State
	for _, s := range states {
		if !s.IsEnded() {
			active = append(active, s)
		}
	}
	if len(active) == 0 {
		return
	}

	repoRoot, head, headErr := currentHeadLinkage(ctx)
	divergenceWarnings := make(map[string]string)
	if headErr == nil && repoRoot != "" && head.commitHash != "" {
		divergenceWarnings = computeSessionDivergenceWarnings(repoRoot, active, head)
	}

	// Group by worktree path
	groups := make(map[string]*worktreeGroup)
	for _, s := range active {
		wp := s.WorktreePath
		if wp == "" {
			wp = unknownPlaceholder
		}
		g, ok := groups[wp]
		if !ok {
			g = &worktreeGroup{path: wp}
			groups[wp] = g
		}
		g.sessions = append(g.sessions, s)
	}

	// Resolve branch names for each worktree (skip for unknown paths)
	for _, g := range groups {
		if g.path != unknownPlaceholder {
			g.branch = resolveWorktreeBranch(ctx, g.path)
		}
	}

	// Sort groups: alphabetical by path
	sortedGroups := make([]*worktreeGroup, 0, len(groups))
	for _, g := range groups {
		sortedGroups = append(sortedGroups, g)
	}
	sort.Slice(sortedGroups, func(i, j int) bool {
		return sortedGroups[i].path < sortedGroups[j].path
	})

	// Sort sessions within each group by StartedAt (newest first)
	for _, g := range sortedGroups {
		sort.Slice(g.sessions, func(i, j int) bool {
			return g.sessions[i].StartedAt.After(g.sessions[j].StartedAt)
		})
	}

	// Track aggregate totals
	var totalSessions int

	fmt.Fprintln(w)
	printedHeader := false
	for _, g := range sortedGroups {
		if !printedHeader {
			fmt.Fprintln(w, sty.sectionRule("Active Sessions", sty.width))
			fmt.Fprintln(w)
			printedHeader = true
		}

		for _, st := range g.sessions {
			totalSessions++

			agentLabel := string(st.AgentType)
			if agentLabel == "" {
				agentLabel = unknownPlaceholder
			}

			// Line 1: Agent (model) · sessionID
			if st.ModelName != "" {
				fmt.Fprintf(w, "%s %s %s %s\n",
					sty.render(sty.agent, agentLabel),
					sty.render(sty.dim, "("+st.ModelName+")"),
					sty.render(sty.dim, "·"),
					st.SessionID)
			} else {
				fmt.Fprintf(w, "%s %s %s\n",
					sty.render(sty.agent, agentLabel),
					sty.render(sty.dim, "·"),
					st.SessionID)
			}

			// Line 2: > "first prompt" (chevron + quoted, truncated)
			if st.LastPrompt != "" {
				prompt := stringutil.TruncateRunes(st.LastPrompt, 60, "...")
				fmt.Fprintf(w, "%s \"%s\"\n", sty.render(sty.dim, ">"), prompt)
			}

			// Line 3: stats line — started Xd ago · active now · files N · tokens X.Xk
			var stats []string
			stats = append(stats, "started "+timeAgo(st.StartedAt))

			if st.LastInteractionTime != nil && st.LastInteractionTime.Sub(st.StartedAt) > time.Minute {
				stats = append(stats, activeTimeDisplay(st.LastInteractionTime))
			}

			if t := totalTokens(st.TokenUsage); t > 0 {
				stats = append(stats, "tokens "+formatTokenCount(t))
			}

			statsLine := strings.Join(stats, sty.render(sty.dim, " · "))
			switch {
			case st.OwnerExited():
				// Agent process is gone but the session couldn't be finalized
				// above (e.g. condense/transition error); flag it explicitly.
				fmt.Fprintf(w, "%s %s %s\n", sty.render(sty.dim, statsLine),
					sty.render(sty.dim, "·"),
					sty.render(sty.yellow, "exited")+" (run 'entire doctor')")
			case st.IsStuckActive():
				fmt.Fprintf(w, "%s %s %s\n", sty.render(sty.dim, statsLine),
					sty.render(sty.dim, "·"),
					sty.render(sty.yellow, "stale")+" (run 'entire doctor')")
			default:
				fmt.Fprintln(w, sty.render(sty.dim, statsLine))
			}
			if warning := divergenceWarnings[st.SessionID]; warning != "" {
				fmt.Fprintf(w, "%s %s\n", sty.render(sty.yellow, "!"), sty.render(sty.yellow, warning))
			}
			if st.CaptureDegradedAt != nil {
				warning := fmt.Sprintf("capture degraded %s: status scan over budget; new-file detection skipped (see 'entire doctor logs')",
					timeAgo(*st.CaptureDegradedAt))
				fmt.Fprintf(w, "%s %s\n", sty.render(sty.yellow, "!"), sty.render(sty.yellow, warning))
			}
			fmt.Fprintln(w)
		}
	}

	// Footer: horizontal rule + session count
	fmt.Fprintln(w, sty.horizontalRule(sty.width))
	var footer string
	if totalSessions == 1 {
		footer = "1 session"
	} else {
		footer = fmt.Sprintf("%d sessions", totalSessions)
	}
	fmt.Fprintln(w, sty.render(sty.dim, footer))
	fmt.Fprintln(w)
}

// resolveWorktreeBranch resolves the current branch for a worktree path
// by reading the HEAD ref directly from the filesystem
func resolveWorktreeBranch(ctx context.Context, worktreePath string) string {
	gitPath := filepath.Join(worktreePath, ".git")

	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}

	var headPath string
	if fi.IsDir() {
		// Regular repo: .git is a directory
		headPath = filepath.Join(gitPath, "HEAD")
	} else {
		// Worktree: .git is a file containing "gitdir: <path>"
		data, err := os.ReadFile(gitPath) //nolint:gosec // path derived from known worktree dir
		if err != nil {
			return ""
		}
		content := strings.TrimSpace(string(data))
		if !strings.HasPrefix(content, "gitdir: ") {
			return ""
		}
		gitdirPath := strings.TrimPrefix(content, "gitdir: ")
		if !filepath.IsAbs(gitdirPath) {
			gitdirPath = filepath.Join(worktreePath, gitdirPath)
		}
		headPath = filepath.Join(gitdirPath, "HEAD")
	}

	data, err := os.ReadFile(headPath) //nolint:gosec // path constructed from .git/HEAD
	if err != nil {
		return ""
	}

	ref := strings.TrimSpace(string(data))

	// Symbolic ref: "ref: refs/heads/<branch>"
	if strings.HasPrefix(ref, "ref: refs/heads/") {
		branch := strings.TrimPrefix(ref, "ref: refs/heads/")
		// Reftable ref storage uses "ref: refs/heads/.invalid" as a dummy HEAD stub.
		// Fall back to git to resolve the actual branch in that case.
		if branch == ".invalid" {
			return resolveWorktreeBranchGit(ctx, worktreePath)
		}
		return branch
	}

	// Detached HEAD or other ref type
	return detachedHEADDisplay
}

// resolveWorktreeBranchGit resolves the branch name by shelling out to git.
// Used as a fallback for reftable ref storage where .git/HEAD is a stub.
func resolveWorktreeBranchGit(ctx context.Context, worktreePath string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", worktreePath, "rev-parse", "--symbolic-full-name", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return detachedHEADDisplay
	}
	ref := strings.TrimSpace(string(out))
	if strings.HasPrefix(ref, "refs/heads/") {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	return detachedHEADDisplay
}

func currentHeadLinkage(ctx context.Context) (string, headLinkage, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("resolve worktree root: %w", err)
	}

	repo, err := gitrepo.OpenPath(repoRoot)
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("open repo: %w", err)
	}
	defer repo.Close()

	headRef, err := repo.Head()
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("resolve HEAD: %w", err)
	}

	commit, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return "", headLinkage{}, fmt.Errorf("load HEAD commit: %w", err)
	}

	head := headLinkage{commitHash: headRef.Hash().String()}
	if checkpointIDs := trailers.ParseAllCheckpoints(commit.Message); len(checkpointIDs) > 0 {
		head.checkpointIDs = make([]string, 0, len(checkpointIDs))
		for _, checkpointID := range checkpointIDs {
			head.checkpointIDs = append(head.checkpointIDs, checkpointID.String())
		}
	}

	return repoRoot, head, nil
}

func computeSessionDivergenceWarnings(
	repoRoot string,
	active []*session.State,
	head headLinkage,
) map[string]string {
	warnings := make(map[string]string)
	normalizedRepoRoot := normalizeWorktreePath(repoRoot)

	for _, st := range active {
		if normalizeWorktreePath(st.WorktreePath) != normalizedRepoRoot {
			continue
		}

		if st.BaseCommit == "" {
			// Session linkage is incomplete (migration refuses to run and save-step
			// must reinitialize). Surface this explicitly rather than skipping silently,
			// so operators don't see a false-clean status for a session that cannot
			// be attributed until the next prompt reinitializes it.
			warnings[st.SessionID] = "session linkage incomplete; awaiting reinitialization"
			continue
		}

		if st.BaseCommit == head.commitHash {
			if st.AttributionBaseCommit != "" && st.AttributionBaseCommit != st.BaseCommit {
				warnings[st.SessionID] = "attribution base diverged after history movement; figures may be off until next checkpoint"
			}
			continue
		}

		// BaseCommit != HEAD — hooks haven't reconciled/migrated yet
		if len(head.checkpointIDs) > 0 {
			warnings[st.SessionID] = "tracking diverged from current HEAD; HEAD links to checkpoint(s) " + strings.Join(head.checkpointIDs, ", ")
			continue
		}

		warnings[st.SessionID] = "tracking diverged from current HEAD after git history movement"
	}

	return warnings
}

func normalizeWorktreePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

// statusJSON is the JSON output for `entire status --json`.
type statusJSON struct {
	// Enabled reports whether Entire captures sessions in this repo — through
	// repo-level settings or through the user-global tier (then
	// global_tracking.activation_source is "global" and agents is empty,
	// because the hooks are user-level; see global_tracking.agents_covered).
	Enabled        bool               `json:"enabled"`
	Agents         []string           `json:"agents"`
	ActiveSessions []sessionBriefJSON `json:"active_sessions"`
	// AgentHelp is the machine-readable pointer for no-channel agents that parse
	// `entire status --json` instead of the human footer. Set only on the
	// success path (mirrors writeAgentHelpHint, which only renders when set up).
	AgentHelp string `json:"agent_help,omitempty"`
	// HooksOutdated lists agents whose installed hook config is out of date and
	// should be refreshed with `entire enable --force`.
	HooksOutdated []string `json:"hooks_outdated,omitempty"`
	// CheckpointSyncRemote is the elected checkpoint sync remote name, or the
	// org/repo slug in dedicated checkpoint_remote mode. Deliberately not named
	// checkpoint_remote, which is the existing GitHub-coupled setting.
	CheckpointSyncRemote       string `json:"checkpoint_sync_remote,omitempty"`
	CheckpointSyncRemoteSource string `json:"checkpoint_sync_remote_source,omitempty"` // config|observed|default|sole|first|dedicated
	CheckpointSyncError        string `json:"checkpoint_sync_error,omitempty"`         // fail-closed message
	UnpushedCheckpoints        int    `json:"unpushed_checkpoints,omitempty"`
	// SecretScanners lists the enabled engines when non-default; omitted when default.
	SecretScanners []string `json:"secret_scanners,omitempty"`
	// GlobalTracking reports the machine-wide tracking tier. Omitted while
	// unconfigured (the one-time question was never answered). Present on
	// every output shape, including the not-a-git-repository one — status
	// works outside a repo now that the global tier is machine-wide.
	GlobalTracking *globalTrackingJSON `json:"global_tracking,omitempty"`
	Error          string              `json:"error,omitempty"`
}

// globalTrackingJSON mirrors globalTrackingInfo for `entire status --json`.
type globalTrackingJSON struct {
	Enabled          bool   `json:"enabled"`
	SettingsPath     string `json:"settings_path,omitempty"`
	ActivationSource string `json:"activation_source,omitempty"`
	// AgentsCovered counts agents whose user-level hooks are installed.
	AgentsCovered int `json:"agents_covered"`
	// ActiveHere is the hook gate's per-repo answer and InactiveReason names
	// the carve-out ("repo_excluded", "repo_disabled", "global_off") when it
	// is false. Both are only meaningful inside a repository and are omitted
	// outside one.
	ActiveHere     *bool  `json:"active_here,omitempty"`
	InactiveReason string `json:"inactive_reason,omitempty"`
	// TrustState is "trusted"|"untrusted"|"not_applicable"; TrustSource is
	// "trust_all"|"repo"|"none". Both omitted outside a repository.
	TrustState  string `json:"trust_state,omitempty"`
	TrustSource string `json:"trust_source,omitempty"`
	// TrustReason distinguishes the untrusted states: "untrusted" (no grant
	// yet — `entire trust` fixes it), "identity_unresolved" (the consent key
	// could not be derived; only trust_all clears it), "settings_error".
	TrustReason     string `json:"trust_reason,omitempty"`
	HeldCheckpoints int    `json:"held_checkpoints,omitempty"`
	// SyncRemote is the elected checkpoint sync remote the trust state is
	// keyed on — where this repo's checkpoints leave the machine.
	SyncRemote string `json:"sync_remote,omitempty"`
	// PolicyError is set when this repo could not be classified; active_here
	// and inactive_reason are then omitted rather than reported as false.
	PolicyError string `json:"policy_error,omitempty"`
	// SettingsError is set when the user settings file exists but is unreadable.
	SettingsError string `json:"settings_error,omitempty"`
}

// inactiveReasonJSON maps the gate's inactive reason to its stable JSON
// identifier; "" for none.
func inactiveReasonJSON(reason settings.InactiveReason) string {
	switch reason {
	case settings.InactiveReasonGlobalExcluded:
		return "repo_excluded"
	case settings.InactiveReasonRepoDisabled:
		return "repo_disabled"
	case settings.InactiveReasonGlobalOff:
		return "global_off"
	case settings.InactiveReasonNone:
		return ""
	default:
		return ""
	}
}

type sessionBriefJSON struct {
	Agent  string `json:"agent"`
	Model  string `json:"model,omitempty"`
	Status string `json:"status"`
	// CaptureDegraded reports that a session for this agent last turned with a
	// status scan over budget, so new-file detection was skipped.
	CaptureDegraded bool `json:"capture_degraded,omitempty"`
}

func runStatusJSON(ctx context.Context, w io.Writer) error {
	// Attach the global-tracking tier to every output shape (including the
	// error shapes) so `status --json` is useful outside a git repository.
	var gt *globalTrackingJSON
	info := computeGlobalTrackingInfo(ctx)
	if info.Configured {
		gt = &globalTrackingJSON{
			SettingsError:    info.SettingsError,
			Enabled:          info.Enabled,
			AgentsCovered:    info.AgentsCovered,
			SettingsPath:     info.SettingsPath,
			ActivationSource: string(info.ActivationSource),
			HeldCheckpoints:  info.HeldCheckpoints,
			SyncRemote:       info.SyncRemote,
		}
		switch {
		case info.InRepo && info.PolicyError != "":
			gt.PolicyError = info.PolicyError
		case info.InRepo:
			active := info.ActiveHere
			gt.ActiveHere = &active
			if !active {
				gt.InactiveReason = inactiveReasonJSON(info.InactiveReason)
			}
		}
		// TrustState doubles as the in-repo marker (set even for a disabled tier).
		if info.TrustState != "" {
			gt.TrustState = info.TrustState
			gt.TrustSource = string(info.TrustSource)
			gt.TrustReason = info.TrustReason
		}
	}
	writeJSON := func(v statusJSON) error {
		v.GlobalTracking = gt
		return json.NewEncoder(w).Encode(v)
	}

	if _, err := paths.WorktreeRoot(ctx); err != nil {
		return writeJSON(statusJSON{Error: "not a git repository"})
	}

	settingsPath, err := paths.AbsPath(ctx, EntireSettingsFile)
	if err != nil {
		settingsPath = EntireSettingsFile
	}
	localSettingsPath, err := paths.AbsPath(ctx, EntireSettingsLocalFile)
	if err != nil {
		localSettingsPath = EntireSettingsLocalFile
	}

	_, projectErr := os.Lstat(settingsPath)
	if projectErr != nil && !errors.Is(projectErr, fs.ErrNotExist) {
		return writeJSON(statusJSON{Error: fmt.Sprintf("cannot access project settings file: %v", projectErr)})
	}
	_, localErr := os.Lstat(localSettingsPath)
	if localErr != nil && !errors.Is(localErr, fs.ErrNotExist) {
		return writeJSON(statusJSON{Error: fmt.Sprintf("cannot access local settings file: %v", localErr)})
	}

	if projectErr != nil && localErr != nil {
		if !info.trackedHere() {
			return writeJSON(statusJSON{Error: "not set up"})
		}
		// Same reasoning as the text path: a globally tracked repo is enabled
		// from the agent's point of view, and it must find the agent-help
		// pointer here — this is the discovery path for no-channel agents.
		return writeJSON(statusJSON{
			Enabled:        true,
			Agents:         []string{},
			ActiveSessions: collectActiveSessionsJSON(ctx),
			AgentHelp:      agentHelpCommand,
		})
	}

	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return writeJSON(statusJSON{Error: fmt.Sprintf("failed to load settings: %v", err)})
	}

	// `enabled` describes what the hooks will do here, so a repo the user's
	// exclude lists carve out reads as disabled even with repo-level setup.
	result := statusJSON{
		Enabled:        s.Enabled && !info.excludedHere(),
		Agents:         []string{},
		ActiveSessions: []sessionBriefJSON{},
		AgentHelp:      agentHelpCommand,
	}

	// Guard on the EFFECTIVE state, not the raw file: an excluded repo with
	// repo-level enabled:true must not report agents, sync lines or sessions
	// (collectActiveSessionsJSON also finalizes exited sessions — a write a
	// read-only status on an inactive repo must not perform).
	if result.Enabled {
		if names := InstalledAgentDisplayNames(ctx); len(names) > 0 {
			result.Agents = names
		}

		result.SecretScanners = nonDefaultSecretScanners(s)

		for _, name := range OutdatedHookAgents(ctx) {
			result.HooksOutdated = append(result.HooksOutdated, string(name))
		}

		// Same computation as the text path (writeCheckpointSyncLines);
		// empty fields drop out via omitempty when nothing resolved.
		syncInfo := computeCheckpointSyncInfo(ctx, s)
		result.CheckpointSyncRemote = syncInfo.Remote
		result.CheckpointSyncRemoteSource = syncInfo.Source
		result.CheckpointSyncError = syncInfo.Err
		result.UnpushedCheckpoints = syncInfo.Unpushed

		result.ActiveSessions = collectActiveSessionsJSON(ctx)
	}

	return writeJSON(result)
}

// collectActiveSessionsJSON lists the live sessions for `status --json`, one
// entry per agent with "active" winning over "idle". Always non-nil so the
// field encodes as [] rather than null. Errors yield the empty list: session
// state is best-effort context, not a reason to fail status.
func collectActiveSessionsJSON(ctx context.Context) []sessionBriefJSON {
	out := []sessionBriefJSON{}
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return out
	}
	states, err := store.List(ctx)
	if err != nil {
		return out
	}
	// Finalize sessions whose agent has exited (matches the human status
	// path) so --json doesn't leave them orphaned or report them under
	// active_sessions.
	finalizeExitedSessions(ctx, states, time.Now().Add(interactiveSweepCondenseBudget))
	type agentEntry struct {
		brief    sessionBriefJSON
		isActive bool
	}
	byAgent := make(map[string]*agentEntry)
	for _, st := range states {
		if st.IsEnded() {
			continue
		}
		agent := string(st.AgentType)
		if agent == "" {
			agent = unknownPlaceholder
		}
		active := st.Phase == session.PhaseActive
		if existing, ok := byAgent[agent]; ok {
			if active && !existing.isActive {
				existing.brief.Model = st.ModelName
				existing.brief.Status = sessionStatusLabel(st)
				existing.isActive = true
			}
			// Degradation is sticky across the dedupe: any degraded session
			// for this agent must not be hidden by a healthy one.
			existing.brief.CaptureDegraded = existing.brief.CaptureDegraded || st.CaptureDegradedAt != nil
			continue
		}
		byAgent[agent] = &agentEntry{
			brief: sessionBriefJSON{
				Agent:           agent,
				Model:           st.ModelName,
				Status:          sessionStatusLabel(st),
				CaptureDegraded: st.CaptureDegradedAt != nil,
			},
			isActive: active,
		}
	}
	for _, e := range byAgent {
		out = append(out, e.brief)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out
}

// sessionStatusLabel derives a display status from a session state.
func sessionStatusLabel(s *session.State) string {
	if s.IsEnded() {
		return "ended"
	}
	if s.OwnerExited() {
		// ACTIVE on disk, but the owning agent process is gone.
		return "exited"
	}
	if s.Phase != "" {
		return string(s.Phase)
	}
	return string(session.PhaseIdle)
}
