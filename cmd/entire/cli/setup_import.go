package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// eligibleImport pairs a just-selected agent with its importer and the number
// of sessions discoverable for the current repo within the lookback window.
type eligibleImport struct {
	imp          agentimport.Importer
	displayName  string
	sessionCount int
}

// Seams for testing the orchestration in maybeOfferSessionImport without disk
// discovery, a real TTY, or real checkpoint writes. Production wiring uses the
// real implementations below.
var (
	sessionImportDiscover = discoverImportableAgents
	sessionImportPrompt   = promptImportSelection
	sessionImportRun      = runSelectedImports
)

// importHistoryFlagUsage is the --import-history help text. It lives here, next
// to the behavior it describes, so the advertised lookback cannot drift from
// agentimport's.
var importHistoryFlagUsage = fmt.Sprintf(
	"During first-time setup, import the selected agents' existing session history (last %d days) without prompting",
	agentimport.LookbackDays)

// noteImportHistoryNotApplicable tells a user who asked for a history import
// that this run cannot do one. The offer is first-time-setup only, so on an
// already-configured repo the standalone command is the way in; silently
// dropping the flag would leave them believing history had been imported.
func noteImportHistoryNotApplicable(w io.Writer) {
	fmt.Fprintf(w, "Note: --%s applies to first-time setup. Run 'entire import <agent>' to import existing history.\n",
		flagImportHistory)
}

// maybeOfferSessionImport offers, on first-time enable only, to import
// pre-existing agent history for the just-selected agents. Granularity is
// agent-level: choosing an agent imports all its discoverable sessions (30-day
// lookback, matching `entire import`). It is best-effort — discovery or import
// failures are logged and reported to the user but never fail enable.
//
// Import only happens on an explicit choice. An interactive run presents a
// multi-select with nothing pre-checked, so its default is to import nothing;
// `--import-history` is the non-interactive way to say yes.
//
// `--yes` deliberately does NOT import. It means "accept all defaults", and the
// interactive default here is to skip — so implying an import from it would
// make the unattended path do the opposite of the attended one. Ingesting a
// month of local transcripts (which a later push publishes) is its own
// decision, not a setup default, so `--yes` takes the same path as any other
// run that makes no choice: import nothing, and point at `entire import`.
func maybeOfferSessionImport(ctx context.Context, w io.Writer, agents []agent.Agent, opts EnableOptions, firstRun bool) {
	if !firstRun {
		if opts.ImportHistory {
			noteImportHistoryNotApplicable(w)
		}
		return
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// No worktree root => nothing to import against. Enabling still succeeds.
		logging.Warn(ctx, "session import offer skipped: no worktree root", "error", err)
		return
	}

	eligible := sessionImportDiscover(ctx, agents, repoRoot)
	if len(eligible) == 0 {
		return
	}

	// No explicit opt-in, and no way (or no intent) to ask: don't silently
	// import. Leave a pointer so scripted/agent/--yes enables can still import
	// on demand. The hint names the standalone command rather than the flag:
	// by the time this prints, setup has written its settings, so a re-run of
	// enable is no longer a first run and the flag would not apply.
	if !opts.ImportHistory && (opts.Yes || !interactive.CanPromptInteractively()) {
		logging.Info(ctx, "session import offer skipped: no explicit opt-in",
			"eligible", len(eligible), "yes", opts.Yes)
		fmt.Fprintf(w, "Found importable history for %s. Run 'entire import <agent>' to import it.\n", pluralAgents(len(eligible)))
		return
	}

	selected := eligible
	if !opts.ImportHistory {
		selected, err = sessionImportPrompt(ctx, w, eligible)
		if err != nil {
			// Best-effort: a prompt/UI failure must never fail enable. Log,
			// note it, and skip import.
			logging.Warn(ctx, "session import offer skipped: prompt failed", "error", err)
			fmt.Fprintf(w, "Note: could not show import prompt: %v\n", err)
			return
		}
	}
	if len(selected) == 0 {
		return
	}

	sessionImportRun(ctx, w, repoRoot, selected)
}

// discoverImportableAgents keeps the selected agents that have a registered
// importer and at least one discoverable session for the repo.
func discoverImportableAgents(ctx context.Context, agents []agent.Agent, repoRoot string) []eligibleImport {
	now := time.Now()
	var out []eligibleImport
	for _, ag := range agents {
		imp := importerForAgent(ag)
		if imp == nil {
			continue
		}
		sessions, err := imp.Discover(repoRoot, "", now, nil)
		if err != nil {
			logging.Warn(ctx, "session import discovery failed", "agent", string(ag.Type()), "error", err)
			continue
		}
		if len(sessions) == 0 {
			continue
		}
		out = append(out, eligibleImport{
			imp:          imp,
			displayName:  string(ag.Type()),
			sessionCount: len(sessions),
		})
	}
	return out
}

// importerForAgent finds the importer for an agent by matching AgentType, which
// is the shared display-name identity between the two seams (importer Name and
// AgentType are distinct concepts, so match on type rather than name).
func importerForAgent(ag agent.Agent) agentimport.Importer {
	for _, imp := range agentimport.All() {
		if imp.AgentType() == ag.Type() {
			return imp
		}
	}
	return nil
}

// promptImportSelection asks the user which discovered agents to import. With a
// single eligible agent a multi-select's "space to select / select none to
// skip" wording is confusing (there is nothing to choose between), so that case
// uses a plain Import/Skip confirmation instead. An empty selection (or user
// abort) returns an empty slice, which the caller treats as "skip import".
func promptImportSelection(ctx context.Context, w io.Writer, eligible []eligibleImport) ([]eligibleImport, error) {
	if len(eligible) == 1 {
		return promptImportConfirmSingle(ctx, w, eligible[0])
	}

	byName := make(map[string]eligibleImport, len(eligible))
	options := make([]huh.Option[string], 0, len(eligible))
	for _, e := range eligible {
		byName[e.imp.Name()] = e
		label := fmt.Sprintf("%s  (%s, last %d days)", e.displayName, pluralSessions(e.sessionCount), agentimport.LookbackDays)
		options = append(options, huh.NewOption(label, e.imp.Name()))
	}

	var chosen []string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Import existing sessions into Entire? (optional)").
				Description("Space to select, enter to confirm. Select none to skip.").
				Options(options...).
				Value(&chosen),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		// Cancellation (including a cancelled ctx) returns nil here => skip
		// import; other errors are surfaced for the caller to downgrade.
		return nil, handleFormCancellation(w, "Import", err)
	}

	out := make([]eligibleImport, 0, len(chosen))
	for _, name := range chosen {
		if e, ok := byName[name]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// promptImportConfirmSingle offers a single discovered agent's history with a
// plain Import/Skip confirmation. Declining (or aborting) returns an empty
// slice so the caller skips the import.
func promptImportConfirmSingle(ctx context.Context, w io.Writer, e eligibleImport) ([]eligibleImport, error) {
	var confirmed bool
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Import existing %s sessions into Entire? (optional)", e.displayName)).
				Description(fmt.Sprintf("%s from the last %d days. Enter to confirm.", pluralSessions(e.sessionCount), agentimport.LookbackDays)).
				Affirmative("Import").
				Negative("Skip").
				Value(&confirmed),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		// Cancellation (including a cancelled ctx) returns nil here => skip
		// import; other errors are surfaced for the caller to downgrade.
		return nil, handleFormCancellation(w, "Import", err)
	}
	if !confirmed {
		return nil, nil
	}
	return []eligibleImport{e}, nil
}

// runSelectedImports imports each chosen agent's history, mirroring the
// standalone `entire import` command. Per-agent failures are logged and
// reported but do not stop the remaining imports or fail enable.
func runSelectedImports(ctx context.Context, w io.Writer, repoRoot string, selected []eligibleImport) {
	repo, err := openRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "session import skipped: open repository failed", "error", err)
		fmt.Fprintf(w, "Note: could not import agent history: %v\n", err)
		return
	}
	defer repo.Close()

	// Gate on the checkpoint policy before writing any checkpoint data, matching
	// the standalone `entire import` command. Best-effort: an unsupported or
	// unreadable policy skips the import (logged and noted) instead of failing
	// enable, since the offer must never break enable.
	if err := ensureCheckpointPolicyAllowsCheckpointData(ctx, repo); err != nil {
		logging.Warn(ctx, "session import skipped: checkpoint policy not satisfied", "error", err)
		fmt.Fprintf(w, "Note: skipping agent history import: %v\n", err)
		return
	}

	// Load repo/user-configured redaction before any checkpoint write, matching
	// import_cmd.go; without it only always-on secret scanning would run.
	strategy.EnsureRedactionConfigured(ctx)

	var importedLocalHistory bool
	for _, e := range selected {
		progress, stopProgress := newImportProgressReporter(w, e.displayName)
		res, err := agentimport.Run(ctx, repo, e.imp, agentimport.Options{
			RepoRoot:    repoRoot,
			Now:         time.Now(),
			Progress:    progress,
			ReadRemotes: strategy.CheckpointReadRemotes(ctx),
		})
		stopProgress(err == nil)
		if err != nil {
			// Ctrl-C: stop here instead of moving on to the next agent's
			// history, which is the last thing a user who just interrupted
			// wants. Report what landed and how to finish it.
			if errors.Is(err, context.Canceled) {
				logging.Info(ctx, "session import interrupted",
					"agent", e.imp.Name(), "turns", res.TurnsImported)
				if res.TurnsImported > 0 {
					importedLocalHistory = true
				}
				fmt.Fprintf(w, "Import interrupted after %d turn(s). Run 'entire import %s' to finish.\n",
					res.TurnsImported, e.imp.Name())
				break
			}
			logging.Warn(ctx, "session import failed", "agent", e.imp.Name(), "error", err)
			fmt.Fprintf(w, "Note: could not import %s history: %v\n", e.displayName, err)
			continue
		}
		if res.TurnsImported > 0 || res.TurnsSkipped > 0 {
			importedLocalHistory = true
		}
		fmt.Fprintf(w, "Imported %d turn(s) from %d session(s) (%d already imported).\n",
			res.TurnsImported, res.SessionsScanned, res.TurnsSkipped)
	}
	// Enable often runs before the user has logged in; surface once that a
	// logged-out import stays local and won't reach the dashboard (issue #1773).
	warnIfImportNotSynced(w, importedLocalHistory)
	// When it runs logged in, push what was just imported: enable creates no
	// commit either, so nothing else would trigger the pre-push hook (#1773).
	syncImportedCheckpoints(ctx, w, repo, "")
}

// pluralSessions renders a session count with correct pluralization.
func pluralSessions(n int) string {
	if n == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", n)
}

// pluralAgents renders an agent count with correct pluralization.
func pluralAgents(n int) string {
	if n == 1 {
		return "1 agent"
	}
	return fmt.Sprintf("%d agents", n)
}
