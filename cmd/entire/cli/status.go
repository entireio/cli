package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		Use:   cmdStatus,
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

	// Check if we're in a git repository
	if _, repoErr := paths.WorktreeRoot(ctx); repoErr != nil {
		fmt.Fprintln(w, "✕ not a git repository")
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
	projectExists, localExists, err := settings.FilesPresent(ctx)
	if err != nil {
		return err //nolint:wrapcheck // already contextual; a bare %w only changes the concrete type
	}

	if !projectExists && !localExists {
		fmt.Fprintln(w, "○ not set up (run `entire enable` to get started)")
		return nil
	}

	sty := newStatusStyles(w)

	if detailed {
		return runStatusDetailed(ctx, w, sty, settingsPath, localSettingsPath, projectExists, localExists)
	}

	// Short output: just show the effective/merged state
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	fmt.Fprintln(w, formatSettingsStatusShort(ctx, s, sty))
	if s.Enabled {
		writeActiveSessions(ctx, w, sty)
	}
	writeAgentHelpHint(w, sty)

	return nil
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
	fmt.Fprintln(w) // blank line

	// Show project settings if it exists
	if projectExists {
		projectSettings, err := settings.LoadFromFile(settingsPath)
		if err != nil {
			return fmt.Errorf("failed to load project settings: %w", err)
		}
		fmt.Fprintln(w, formatSettingsStatus("Project", projectSettings, sty))
	}

	// An external_agents grant the loader refused. The user can see the
	// setting in their file and has no other way to learn it is inert:
	// discovery simply does not run, and the agent never appears.
	if reason, rejected := effectiveSettings.ExternalAgentsRejection(); rejected {
		fmt.Fprintf(w, "  external_agents is ignored: %s\n  move it to %s, and keep that file out of version control\n",
			reason, settings.EntireSettingsLocalFile)
	}

	// Same reasoning for allow_symlinked_agent_dirs: a refused grant and a
	// setting nobody wrote both end in Entire refusing the link, so without
	// this the two are indistinguishable from the outside.
	if reason, rejected := effectiveSettings.SymlinkedAgentDirsRejection(); rejected {
		fmt.Fprintf(w, "  allow_symlinked_agent_dirs is ignored: %s\n  move it to %s, and keep that file out of version control\n",
			reason, settings.EntireSettingsLocalFile)
	}

	// And say what IS being followed. A link Entire writes through is worth
	// stating plainly every time, not only when something is wrong with it.
	//
	// FollowedSymlinkedDirs, not VouchedSymlinkedDirs: the latter is the
	// configuration, and a vouched path that is an ordinary directory, absent,
	// or a dangling link is not something Entire is following. Scoped to this
	// worktree too, so the report can never name links followed somewhere else.
	if repoRoot, rootErr := paths.WorktreeRoot(ctx); rootErr == nil {
		if followed := agent.FollowedSymlinkedDirs(repoRoot); len(followed) > 0 {
			fmt.Fprintf(w, "  Following symlinked agent directories: %s\n", strings.Join(followed, ", "))
		}
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

	if effectiveSettings.Enabled {
		writeActiveSessions(ctx, w, sty)
	}
	writeAgentHelpHint(w, sty)

	return nil
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

	// Resolve branch from repo root
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		if branch := resolveWorktreeBranch(ctx, repoRoot); branch != "" {
			b.WriteString(sty.render(sty.dim, " · "))
			b.WriteString("branch ")
			b.WriteString(sty.render(sty.cyan, branch))
		}
	}

	// Show enabled agents
	if s.Enabled {
		if displayNames := InstalledAgentDisplayNames(ctx); len(displayNames) > 0 {
			b.WriteString("\n")
			b.WriteString(sty.render(sty.dim, "  Agents · "))

			b.WriteString(strings.Join(displayNames, ", "))
		}

		// Warn when installed hooks are out of date (read-only; fix is manual).
		for _, name := range OutdatedHookAgents(ctx) {
			ag, err := agent.Get(name)
			if err != nil {
				continue
			}
			b.WriteString("\n")
			b.WriteString(sty.render(sty.yellow, "  ! "+string(ag.Type())+" hooks out of date"))
			b.WriteString(sty.render(sty.dim, " · run 'entire enable --force'"))
		}
		if warning := codexStatusWarning(inspectCodexHookIssue(ctx)); warning != "" {
			b.WriteString("\n")
			b.WriteString(sty.render(sty.yellow, "  ! "+warning))
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
	// PushDisabled reflects the explicit automatic-push setting, not every
	// possible reason checkpoint sync might fail.
	//
	// It suppresses nothing else in this struct, because nothing else is a
	// push promise: reads keep working (push_sessions gates only the pre-push
	// hook), the unpushed count is the only signal that checkpoint data is
	// not reaching the elected destination, and the remote-configuration
	// diagnostics explain read behavior too.
	//
	// What it does instead is switch the QUESTION these fields answer, from
	// "where would checkpoints go" to "where do they come from" — so it
	// changes more than phrasing. With a checkpoint_remote configured, the
	// verdict behind Remote and Source is then the FETCH side's rather than
	// the push side's, and in a repo whose two URLs have different owners
	// that is a different VALUE, not a different wording. (With none
	// configured both answer the elected remote, and only the wording
	// differs.) ReadFallback and ReadSourceUnknown exist only while it is
	// set.
	PushDisabled bool
	// Remote is the elected git remote name, or the org/repo slug in
	// dedicated checkpoint_remote mode. Empty when nothing resolved: no
	// remotes configured, or the fail-closed case — except that with pushing
	// disabled a failed election can still leave the dedicated slug here,
	// since reads fail open and the store may serve them with nothing
	// elected.
	Remote string
	// Source is config|observed|default|sole|first (resolver values) or
	// "dedicated".
	Source string
	// Err is the fail-closed misconfiguration message from the resolver.
	Err string
	// ReadSourceUnknown reports that the fetch-side probe failed, so nothing
	// is known about where reads resolve and no read source is named. Set
	// only while pushing is disabled, where that probe is consulted at all.
	ReadSourceUnknown bool
	// ReadFallback is the remote checkpoint READS fall open to when the
	// election failed, so Err is set and nothing was elected. Deliberately
	// not folded into Remote: that field means "the elected remote", and on
	// this path there is none — reporting a fallback there would misstate
	// checkpoint_sync_remote to JSON consumers.
	//
	// TWO preconditions, and its absence means whichever did not hold.
	// Pushing must be disabled — with pushing enabled the headline is the
	// broken setting and the user's next move is to fix it, so that output is
	// left as it was. And the dedicated store must not be what serves reads:
	// a configured checkpoint_remote the fetch side confirms is reported
	// through Remote/Source instead (this field names a git remote), and a
	// failed probe through ReadSourceUnknown. So empty does NOT mean "reads
	// fall open to nothing".
	ReadFallback string
	// Unpushed approximates checkpoints not yet on the sync destination; 0
	// when none, when counting failed, or when the count would be a lie
	// (dedicated URL mode on the git-branch backend).
	Unpushed int
	// IgnoredRemote and IgnoredReason report a configured checkpoint_remote
	// that the ownership check rejected as inherited with the clone. Both
	// reads and pushes then fall back to the elected remote, and status is
	// where a user finds out why — the hooks only log the rejection.
	IgnoredRemote string
	IgnoredReason string
}

// resolveDedicatedReadSource records where checkpoint READS land when the
// configured checkpoint_remote is what serves them. Used by both paths that
// name a read source, so the two cannot answer the question differently — the
// asymmetry between them is what this function exists to remove.
//
// lead is the read candidate whose FETCH url joins origin in the ownership
// vote: the elected remote, or "" when the election failed and reads fall
// open to origin alone.
//
// Reports whether it settled the answer — the dedicated store serves reads
// (Remote/Source), or the probe failed so nothing is known
// (ReadSourceUnknown). False means reads resolve to the caller's own
// candidate, which the caller names.
func resolveDedicatedReadSource(ctx context.Context, s *EntireSettings, lead string, info *checkpointSyncInfo) bool {
	cr := s.GetCheckpointRemote()
	if cr == nil {
		return false
	}
	authoritative, err := checkpointremote.ReadsDedicatedStore(ctx, lead)
	switch {
	case err != nil:
		// Not the same as false. False means reads resolve somewhere else,
		// so the caller's candidate is the answer; an error means no read URL
		// resolves at all — reachable with a configured checkpoint_remote and
		// no remote named origin, since the dedicated derivation is from
		// origin. Naming a candidate there would report a working read source
		// for a repo whose checkpoint reads fail.
		logging.Debug(ctx, "checkpoint read source probe failed; status omits the read source",
			slog.String("error", err.Error()))
		info.ReadSourceUnknown = true
		return true
	case authoritative:
		info.Remote = cr.Repo
		info.Source = checkpointSyncSourceDedicated
		return true
	default:
		return false
	}
}

func computeCheckpointSyncInfo(ctx context.Context, s *EntireSettings) checkpointSyncInfo {
	info := checkpointSyncInfo{PushDisabled: s.IsPushSessionsDisabled()}

	elected, err := strategy.ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		// Fail-closed: checkpoint_push_remote names a remote that does not
		// exist. The pre-push gate is silently skipping checkpoint sync, so
		// status is the user's signal.
		// Accepted divergence: if a structured checkpoint_remote is also
		// configured, the gate's dedicated exemption may still sync checkpoint
		// data even while this fail-closed warning is shown, since there is no
		// elected remote left to probe PushURL against here.
		info.Err = err.Error()
		// Reads fail OPEN where the election failed closed (see
		// strategy.CheckpointReadRemotes), so something is probably still
		// serving them: the dedicated store when one is configured and the
		// fetch side owns it, otherwise the fail-open candidate. Each is
		// asked of its own resolver rather than reproduced here. Both re-run
		// the election, which is why this is on the error path only, and both
		// stay local-only like the rest of status.
		if info.PushDisabled && !resolveDedicatedReadSource(ctx, s, "", &info) {
			info.ReadFallback = strategy.LeadCheckpointReadRemote(ctx)
		}
		return info
	}
	if elected.Name == "" {
		return info // no remotes configured: only report disabled pushing, if set
	}

	// Dedicated checkpoint_remote mode is reported only when the direction
	// this line describes actually resolves to it; otherwise the gate applies
	// normal single-remote sync, so status reports that instead.
	//
	// Which direction that is depends on push_sessions, and the two are
	// separate questions. With pushing enabled the line names a push
	// destination, so PushURL decides, mirroring the pre-push exemption
	// (ps.hasCheckpointURL). With pushing disabled it names a READ source,
	// which is the fetch side's call over a different ownership identity set
	// — origin plus the candidate's FETCH url, not its push urls — so a
	// remote whose two urls have different owners is eligible on one side
	// only, and asking the wrong side reports a store reads do not use.
	//
	// Both probes are local-only; never call resolvePushSettings here — its
	// follow-up metadata fetch dials, and status must stay network-free.
	// Accepted divergence: a real push to a different named remote may derive
	// PushURL differently than this elected-remote probe does.
	if cr := s.GetCheckpointRemote(); cr != nil {
		dedicated := false
		if info.PushDisabled {
			if resolveDedicatedReadSource(ctx, s, elected.Name, &info) {
				if info.ReadSourceUnknown {
					return info
				}
				dedicated = true
			}
		} else if _, enabled, purlErr := checkpointremote.PushURL(ctx, elected.Name); purlErr == nil {
			// The push side may fall soft to the elected remote because
			// resolvePushSettings degrades the same way; the read side may
			// not, which is why the helper above distinguishes error from
			// false and this branch does not need to.
			dedicated = enabled
		}
		if dedicated {
			info.Remote = cr.Repo
			info.Source = checkpointSyncSourceDedicated
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

	info.Remote = elected.Name
	info.Source = string(elected.Source)
	info.Unpushed = countUnpushedCheckpointsForStatus(ctx, elected.Name)
	// A configured checkpoint_remote that did not enable above is being
	// ignored. When the ownership check is what rejected it, say so: this is
	// the one trust-gate rejection a user otherwise experiences only as
	// checkpoints vanishing. Local-only, like everything else here.
	// Accepted divergence: this verdict votes with the push identity set
	// (origin + push URLs of the elected remote), while a fetch votes with its
	// read candidate, so a push-only owner mismatch shows "not in use" here
	// even though a lead-less fetch still resolves the checkpoint remote.
	if cr := s.GetCheckpointRemote(); cr != nil {
		if repo, reason, inherited := checkpointremote.InheritedCheckpointRemote(ctx, s, elected.Name); inherited {
			info.IgnoredRemote = repo
			info.IgnoredReason = reason
		} else if info.PushDisabled {
			// That verdict votes with the push identity set, so it accepts a
			// store the fetch side declined — and with pushing disabled the
			// fetch side is the one that decided the line above. Without this
			// the configured store is reported by nothing at all, which is
			// the silent-ignore the warning exists to prevent.
			//
			// No reason is given: the fetch side returns a verdict and not a
			// cause, and its false covers ownership, an unparseable origin
			// URL and an unmappable protocol alike, so naming one would be a
			// guess. The causes are logged where they are decided.
			info.IgnoredRemote = cr.Repo
			info.IgnoredReason = "checkpoint reads do not resolve to it (see .entire/logs for the reason)"
		}
	}
	return info
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

// writeCheckpointSyncLines reports the checkpoint sync destination (and the
// unpushed counter, when non-zero) in the enabled status block, prefixed by the
// disabled-pushing line when automatic pushing is off. No remotes configured
// means no destination line either way.
//
// Every phrase that promises a push is conditioned on info.PushDisabled: with
// pushing off the elected remote is still the read source and the counter still
// reports local-only data, so the lines are reworded rather than dropped —
// status is the only surface that names either.
func writeCheckpointSyncLines(ctx context.Context, b *strings.Builder, s *EntireSettings, sty statusStyles) {
	info := computeCheckpointSyncInfo(ctx, s)
	destination := "\n  Checkpoints sync to: "
	if info.PushDisabled {
		b.WriteString("\n  Automatic checkpoint pushing: disabled")
		b.WriteString(sty.render(sty.dim, " (push_sessions=false)"))
		// Names the remote reads resolve to FIRST, not the whole chain:
		// CheckpointReadRemotes also appends origin as a legacy tier when it
		// is configured and is not already the elected remote. Rendering the
		// chain would mean either restating its rule here (which then drifts
		// from the resolver) or a second election call, and the label does
		// not claim exclusivity — "read from", never "only".
		destination = "\n  Checkpoints read from: "
	}
	// The misconfiguration warning is emitted independently of the read
	// source below, not as one arm of the same switch: with pushing disabled
	// a failed election does not stop reads, so the two can both have
	// something to say and one must not shadow the other.
	if info.Err != "" {
		b.WriteString("\n")
		// A fail-closed election is not a push failure when nothing is being
		// pushed: name the misconfiguration without claiming a lost sync.
		if info.PushDisabled {
			b.WriteString(sty.render(sty.yellow, "  ! Checkpoint remote configuration: "+info.Err))
		} else {
			b.WriteString(sty.render(sty.yellow, "  ! Checkpoints NOT syncing: "+info.Err))
		}
	}
	switch {
	case info.ReadSourceUnknown:
		b.WriteString("\n")
		b.WriteString(sty.render(sty.yellow, "  ! Could not determine where checkpoints are read from"))
	case info.Source == checkpointSyncSourceDedicated:
		b.WriteString(destination)
		b.WriteString(sty.render(sty.cyan, "dedicated checkpoint remote ("+info.Remote+")"))
	case info.Remote != "":
		b.WriteString(destination)
		b.WriteString(sty.render(sty.cyan, info.Remote))
		// Both suffixes describe how the remote was ELECTED, which is what
		// picks the read source too, so they hold with pushing disabled.
		switch info.Source {
		case string(strategy.SyncRemoteSourceConfig):
			b.WriteString(sty.render(sty.dim, " (set by checkpoint_push_remote)"))
		case string(strategy.SyncRemoteSourceObserved):
			b.WriteString(sty.render(sty.dim, " (follows your branch's push destination)"))
		}
	case info.ReadFallback != "":
		// Same label as a resolved read source — to the reader it is one
		// question, "where do checkpoints come from" — with the suffix
		// saying this one was not chosen, it was fallen back to.
		b.WriteString(destination)
		b.WriteString(sty.render(sty.cyan, info.ReadFallback))
		b.WriteString(sty.render(sty.dim, " (fallback; nothing was elected)"))
	}
	if info.IgnoredRemote != "" {
		b.WriteString("\n")
		b.WriteString(sty.render(sty.yellow,
			"  ! checkpoint_remote "+info.IgnoredRemote+" is not in use: "+info.IgnoredReason+
				". If this checkpoint repo is yours, set checkpoint_remote in .entire/settings.local.json."))
	}
	if info.Unpushed > 0 {
		b.WriteString("\n  ")
		b.WriteString(sty.render(sty.dim, formatUnpushedCheckpointsLine(info)))
	}
}

// formatUnpushedCheckpointsLine phrases the unpushed counter. Dedicated URL
// mode has no git remote to name (and only reaches here on the git-refs
// backend), so it drops the remote-name phrasing.
//
// With pushing disabled the count is not pending anything, so the future tense
// goes — but the phrasing must not overclaim in the other direction either.
// Unpushed is measured against the ELECTED destination only (a tracking-ref
// comparison on git-branch, the push queue on git-refs), which says nothing
// about whether these checkpoints reached some other remote earlier; the read
// chain's legacy origin tier is exactly that case, and stale tracking state
// over-reports too. So it says what the number supports — not on that one
// destination — and never that the data exists nowhere else. Getting this
// backwards would falsely reassure someone asking whether checkpoint data has
// left the machine.
func formatUnpushedCheckpointsLine(info checkpointSyncInfo) string {
	noun := nounCheckpoints
	pronoun := "they sync"
	if info.Unpushed == 1 {
		noun = nounCheckpoint
		pronoun = "it syncs"
	}
	if info.PushDisabled {
		if info.Source == checkpointSyncSourceDedicated {
			return fmt.Sprintf("%d %s not pushed to the checkpoint remote", info.Unpushed, noun)
		}
		return fmt.Sprintf("%d %s not on %s", info.Unpushed, noun, info.Remote)
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
	// CodexHooks reports effective discovery/trust warnings separately from
	// current-checkout installation and freshness semantics.
	CodexHooks *codexHooksStatusJSON `json:"codex_hooks,omitempty"`
	// CheckpointPushDisabled is emitted only when Entire is enabled and the
	// effective push_sessions setting is false. Its absence does not guarantee
	// that a push can succeed.
	//
	// No other field is omitted or suppressed when it is set: read it as
	// requalifying the fields below rather than removing them.
	// CheckpointSyncRemote is then the remote checkpoints are READ from, and
	// the error and ignored-remote diagnostics apply to reads as well.
	// UnpushedCheckpoints keeps its own meaning either way: checkpoints not
	// present on THAT destination, which is not a claim that they exist
	// nowhere else.
	CheckpointPushDisabled bool `json:"checkpoint_push_disabled,omitempty"`
	// CheckpointSyncRemote is the elected checkpoint sync remote name, or the
	// org/repo slug in dedicated checkpoint_remote mode. Deliberately not named
	// checkpoint_remote, which is the existing GitHub-coupled setting.
	CheckpointSyncRemote       string `json:"checkpoint_sync_remote,omitempty"`
	CheckpointSyncRemoteSource string `json:"checkpoint_sync_remote_source,omitempty"` // config|observed|default|sole|first|dedicated
	CheckpointSyncError        string `json:"checkpoint_sync_error,omitempty"`         // fail-closed message
	// CheckpointReadSourceUnknown reports that the read-source probe failed,
	// so no read source could be determined. Emitted only alongside
	// checkpoint_push_disabled, and checkpoint_sync_remote is then absent
	// rather than guessed at.
	CheckpointReadSourceUnknown bool `json:"checkpoint_read_source_unknown,omitempty"`
	// CheckpointReadFallback is the remote reads fall open to when the
	// election failed, so checkpoint_sync_error is set. Emitted only
	// alongside checkpoint_push_disabled, and only when the dedicated store
	// is not what serves reads — if it is, checkpoint_sync_remote carries it
	// (with source "dedicated") even though nothing was elected, and a failed
	// probe sets checkpoint_read_source_unknown instead. Absence here does
	// not mean reads fall open to nothing.
	CheckpointReadFallback string `json:"checkpoint_read_fallback,omitempty"`
	UnpushedCheckpoints    int    `json:"unpushed_checkpoints,omitempty"`
	// CheckpointRemoteIgnored/-Reason report a configured checkpoint_remote the
	// ownership check rejected as inherited with the clone (reads and pushes
	// fall back to the elected remote). Mirrors the text path's warning line.
	CheckpointRemoteIgnored       string `json:"checkpoint_remote_ignored,omitempty"`
	CheckpointRemoteIgnoredReason string `json:"checkpoint_remote_ignored_reason,omitempty"`
	// SecretScanners lists the enabled engines when non-default; omitted when default.
	SecretScanners []string `json:"secret_scanners,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type codexHooksStatusJSON struct {
	State            string   `json:"state"`
	WorktreePath     string   `json:"worktree_path,omitempty"`
	DiscoveredPath   string   `json:"discovered_path,omitempty"`
	ProjectLayerPath string   `json:"project_layer_path,omitempty"`
	Error            string   `json:"error,omitempty"`
	MissingHooks     []string `json:"missing_hooks,omitempty"`
	MissingApprovals []string `json:"missing_approvals,omitempty"`
}

func codexHooksStatusFromIssue(issue *codexHookIssue) *codexHooksStatusJSON {
	if issue == nil {
		return nil
	}
	return &codexHooksStatusJSON{
		State:            issue.State,
		WorktreePath:     issue.WorktreePath,
		DiscoveredPath:   issue.DiscoveredPath,
		ProjectLayerPath: issue.ProjectLayerPath,
		Error:            issue.Error,
		MissingHooks:     issue.MissingHooks,
		MissingApprovals: issue.MissingApprovals,
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
	writeJSON := func(v statusJSON) error {
		return json.NewEncoder(w).Encode(v)
	}

	if _, err := paths.WorktreeRoot(ctx); err != nil {
		return writeJSON(statusJSON{Error: "not a git repository"})
	}

	projectExists, localExists, presenceErr := settings.FilesPresent(ctx)
	if presenceErr != nil {
		return writeJSON(statusJSON{Error: presenceErr.Error()})
	}

	if !projectExists && !localExists {
		return writeJSON(statusJSON{Error: "not set up"})
	}

	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return writeJSON(statusJSON{Error: fmt.Sprintf("failed to load settings: %v", err)})
	}

	result := statusJSON{
		Enabled:        s.Enabled,
		Agents:         []string{},
		ActiveSessions: []sessionBriefJSON{},
		AgentHelp:      agentHelpCommand,
	}

	if s.Enabled {
		if names := InstalledAgentDisplayNames(ctx); len(names) > 0 {
			result.Agents = names
		}

		result.SecretScanners = nonDefaultSecretScanners(s)

		for _, name := range OutdatedHookAgents(ctx) {
			result.HooksOutdated = append(result.HooksOutdated, string(name))
		}
		result.CodexHooks = codexHooksStatusFromIssue(inspectCodexHookIssue(ctx))

		// Same computation as the text path (writeCheckpointSyncLines);
		// empty fields drop out via omitempty when nothing resolved.
		syncInfo := computeCheckpointSyncInfo(ctx, s)
		result.CheckpointPushDisabled = syncInfo.PushDisabled
		result.CheckpointSyncRemote = syncInfo.Remote
		result.CheckpointSyncRemoteSource = syncInfo.Source
		result.CheckpointSyncError = syncInfo.Err
		result.CheckpointReadFallback = syncInfo.ReadFallback
		result.CheckpointReadSourceUnknown = syncInfo.ReadSourceUnknown
		result.UnpushedCheckpoints = syncInfo.Unpushed
		result.CheckpointRemoteIgnored = syncInfo.IgnoredRemote
		result.CheckpointRemoteIgnoredReason = syncInfo.IgnoredReason

		if store, err := session.NewStateStore(ctx); err == nil {
			if states, err := store.List(ctx); err == nil {
				// Finalize sessions whose agent has exited (matches the human
				// status path) so --json doesn't leave them orphaned or
				// report them under active_sessions.
				finalizeExitedSessions(ctx, states, time.Now().Add(interactiveSweepCondenseBudget))
				// Deduplicate by agent: one entry per agent, "active" wins over "idle".
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
						// Degradation is sticky across the dedupe: any degraded
						// session for this agent must not be hidden by a healthy one.
						existing.brief.CaptureDegraded = existing.brief.CaptureDegraded || st.CaptureDegradedAt != nil
					} else {
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
				}
				for _, e := range byAgent {
					result.ActiveSessions = append(result.ActiveSessions, e.brief)
				}
				sort.Slice(result.ActiveSessions, func(i, j int) bool {
					return result.ActiveSessions[i].Agent < result.ActiveSessions[j].Agent
				})
			}
		}
	}

	return writeJSON(result)
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
