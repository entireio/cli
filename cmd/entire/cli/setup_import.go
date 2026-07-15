package cli

import (
	"context"
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

// maybeOfferSessionImport offers, on first-time enable only, to import
// pre-existing agent history for the just-selected agents. Granularity is
// agent-level: choosing an agent imports all its discoverable sessions (30-day
// lookback, matching `entire import`). It is best-effort — discovery or import
// failures are logged and reported to the user but never fail enable.
//
// Import only happens on an explicit choice: an interactive run presents a
// multi-select (nothing pre-checked) and imports what the user selects; `--yes`
// ("accept all defaults") auto-imports all eligible agents. A non-interactive
// run without `--yes` (a script, a piped shell, or an agent with no TTY) makes
// no choice, so it imports nothing and just points at `entire import` — silently
// importing history there would be surprising.
func maybeOfferSessionImport(ctx context.Context, w io.Writer, agents []agent.Agent, opts EnableOptions, firstRun bool) {
	if !firstRun {
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

	selected := eligible
	if !opts.Yes {
		if !interactive.CanPromptInteractively() {
			// Non-interactive without --yes: don't silently import. Leave a
			// pointer so scripted/agent enables can still import on demand.
			logging.Info(ctx, "session import offer skipped: non-interactive without --yes", "eligible", len(eligible))
			fmt.Fprintf(w, "Found importable history for %s. Run 'entire import <agent>' to import it.\n", pluralAgents(len(eligible)))
			return
		}
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
	strategy.EnsureRedactionConfigured()

	for _, e := range selected {
		res, err := agentimport.Run(ctx, repo, e.imp, agentimport.Options{
			RepoRoot: repoRoot,
			Now:      time.Now(),
		})
		if err != nil {
			logging.Warn(ctx, "session import failed", "agent", e.imp.Name(), "error", err)
			fmt.Fprintf(w, "Note: could not import %s history: %v\n", e.displayName, err)
			continue
		}
		fmt.Fprintf(w, "Imported %d turn(s) from %d session(s) (%d already imported).\n",
			res.TurnsImported, res.SessionsScanned, res.TurnsSkipped)
	}
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
