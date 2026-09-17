package investigate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"charm.land/huh/v2"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
)

// AgentChoice is one row in the investigate picker. Name is the agent
// registry key (used for spawning); Label is the picker-visible string.
type AgentChoice struct {
	Name  string
	Label string
}

// newAccessibleForm creates a huh form with Entire's standard theme,
// switching to accessibility mode when ACCESSIBLE is set.
func newAccessibleForm(groups ...*huh.Group) *huh.Form {
	return uiform.New(groups...)
}

// ConfirmFirstRunSetup prints a banner framing the picker as first-run
// setup (rather than the investigation itself) and waits for the user to
// confirm.
func ConfirmFirstRunSetup(ctx context.Context, out io.Writer) bool {
	fmt.Fprintln(out, "No investigate config found — let's set one up first.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "You'll pick which agents take turns during an investigation, and the")
	fmt.Fprintln(out, "max-turns / quorum the loop should use. The selection is saved to local")
	fmt.Fprintln(out, "preferences (.entire/settings.local.json, not committed); edit later with `entire investigate --edit`.")
	fmt.Fprintln(out, "After setup, the investigation will run with your selection.")
	fmt.Fprintln(out)

	proceed := true
	form := newAccessibleForm(huh.NewGroup(
		huh.NewConfirm().
			Title("Set up investigate now?").
			Affirmative("Yes").
			Negative("Cancel").
			Value(&proceed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		fmt.Fprintln(out, "Setup cancelled.")
		return false
	}
	if !proceed {
		fmt.Fprintln(out, "Setup cancelled.")
	}
	return proceed
}

// eligibleAgentsForInvestigate filters and sorts the eligible-agent list
// for picker display. An agent is eligible iff it has a non-nil Spawner
// (i.e. is launchable by the CLI) AND has hooks installed in the current
// repo.
func eligibleAgentsForInvestigate(_ context.Context, spawnerFor func(string) spawn.Spawner, hookInstalled []types.AgentName) []AgentChoice {
	if spawnerFor == nil {
		return nil
	}
	out := make([]AgentChoice, 0, len(hookInstalled))
	for _, n := range hookInstalled {
		name := string(n)
		if spawnerFor(name) == nil {
			continue
		}
		out = append(out, AgentChoice{Name: name, Label: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// pickerFormFn renders the multi-select + max-turns + quorum form.
type pickerFormFn func(ctx context.Context, eligible []AgentChoice, picks *[]string, maxTurns, quorum *int) error

// pickerFormOverride, when non-nil, replaces the production huh form
// inside RunInvestigateConfigPicker. Test seam.
//
// Stored as an atomic pointer so parallel tests that swap the override
// via SetPickerFormFnForTest don't race with each other or with the
// production read path. The variable is still process-global, so tests
// that install conflicting overrides must not run in parallel with each
// other — but they can coexist with parallel tests that never touch the
// override at all.
var pickerFormOverride atomic.Pointer[pickerFormFn]

// SetPickerFormFnForTest swaps the picker form function. Returns a
// cleanup function the caller must defer to restore the previous value.
func SetPickerFormFnForTest(fn pickerFormFn) func() {
	prev := pickerFormOverride.Load()
	if fn == nil {
		pickerFormOverride.Store(nil)
	} else {
		pickerFormOverride.Store(&fn)
	}
	return func() { pickerFormOverride.Store(prev) }
}

// loadPickerFormOverride returns the current override (or nil if none
// is installed). Reads are wait-free.
func loadPickerFormOverride() pickerFormFn {
	p := pickerFormOverride.Load()
	if p == nil {
		return nil
	}
	return *p
}

// RunInvestigateConfigPicker shows a multi-select of eligible agents and
// prompts for max-turns / quorum. Returns a populated InvestigateConfig
// the caller can persist via settings.Save.
//
// Eligibility: agent has a non-nil Spawner AND has hooks installed.
// Non-spawnable agents (cursor, opencode, factoryai-droid, copilot-cli)
// are filtered out at the SpawnerFor check.
//
// Returns (nil, nil) when there is no terminal to render the picker on: the
// non-interactive equivalent is printed to out and the caller should treat it
// as a clean no-op (both call sites already guard on a nil cfg). Callers must
// therefore check cfg for nil before dereferencing it, even when err is nil.
func RunInvestigateConfigPicker(
	ctx context.Context,
	out io.Writer,
	spawnerFor func(agentName string) spawn.Spawner,
	getAgentsWithHooksInstalled func(ctx context.Context) []types.AgentName,
) (*settings.InvestigateConfig, error) {
	if getAgentsWithHooksInstalled == nil {
		return nil, errors.New("RunInvestigateConfigPicker: GetAgentsWithHooksInstalled not wired")
	}
	if spawnerFor == nil {
		return nil, errors.New("RunInvestigateConfigPicker: SpawnerFor not wired")
	}

	installed := getAgentsWithHooksInstalled(ctx)
	eligible := eligibleAgentsForInvestigate(ctx, spawnerFor, installed)
	if len(eligible) == 0 {
		return nil, errors.New(
			"no launchable agents with hooks installed; " +
				"run `entire configure --agent <name>` for one of: " +
				"claude-code, codex, gemini-cli",
		)
	}

	// The picker is a full-screen huh/Bubble Tea form. Without a controlling
	// terminal it dies inside Bubble Tea ("error opening TTY: ... /dev/tty"),
	// and the caller surfaced that raw library error after this function had
	// already printed "Configuring investigate with N eligible agent(s)" —
	// a progress line implying work was underway. Degrade the way `entire
	// agent` and `entire review --configure` do instead: name the
	// non-interactive equivalent and leave cleanly, before any pre-print.
	//
	// A test-installed form override bypasses the check, mirroring how
	// runManageAgents' selectFn bypasses the same gate in setup.go.
	//
	// Ordering: this sits after the eligibility check so that a repo with no
	// launchable agents still gets the more specific "run `entire configure`"
	// error, and so the guidance below can name the agents actually available.
	formFn := loadPickerFormOverride()
	if formFn == nil && !interactive.CanPromptInteractively() {
		printNonInteractivePickerGuidance(out, eligible)
		return nil, nil
	}
	if formFn == nil {
		formFn = runInvestigatePickerForm
	}

	// Defaults: select all eligible agents, MaxTurns=2, Quorum=0 (== all).
	picks := make([]string, len(eligible))
	for i, c := range eligible {
		picks[i] = c.Name
	}
	maxTurns := 2
	quorum := 0

	fmt.Fprintf(out, "Configuring investigate with %d eligible agent(s).\n", len(eligible))
	fmt.Fprintln(out, "(Space to toggle, enter to confirm.)")
	fmt.Fprintln(out)

	if err := formFn(ctx, eligible, &picks, &maxTurns, &quorum); err != nil {
		return nil, fmt.Errorf("investigate picker: %w", err)
	}
	if len(picks) == 0 {
		return nil, errors.New("no agents selected")
	}
	if maxTurns < 0 {
		return nil, errors.New("max-turns must be non-negative")
	}
	if quorum < 0 {
		return nil, errors.New("quorum must be non-negative")
	}
	if quorum > len(picks) {
		return nil, fmt.Errorf("quorum (%d) cannot exceed agent count (%d)", quorum, len(picks))
	}

	cfg := &settings.InvestigateConfig{
		Agents:   picks,
		MaxTurns: maxTurns,
		Quorum:   quorum,
	}
	// Note: the "saved" confirmation is printed by saveInvestigateConfig
	// after persistence succeeds — printing here before the caller writes
	// the file would lie when SaveLocal then errors out.
	return cfg, nil
}

// printNonInteractivePickerGuidance prints the complete non-interactive
// equivalent of the picker: the file to edit, the exact JSON shape the picker
// would have produced, and the agent names that are actually eligible in this
// repo. Per CLAUDE.md's agent-safe-fallback rule, an agent reading this must be
// able to finish the workflow without a terminal, so the example is a
// ready-to-paste block rather than a pointer at the docs.
func printNonInteractivePickerGuidance(out io.Writer, eligible []AgentChoice) {
	names := make([]string, 0, len(eligible))
	quoted := make([]string, 0, len(eligible))
	for _, c := range eligible {
		names = append(names, c.Name)
		quoted = append(quoted, strconv.Quote(c.Name))
	}
	fmt.Fprintln(out, "Cannot show the investigate config picker in non-interactive mode.")
	fmt.Fprintln(out, "Use: set \"investigate\" in .entire/settings.local.json, for example:")
	fmt.Fprintf(out, "  {\"investigate\": {\"agents\": [%s], \"max_turns\": 2, \"quorum\": 0}}\n", strings.Join(quoted, ", "))
	fmt.Fprintf(out, "Eligible agents: %s\n", strings.Join(names, ", "))
	fmt.Fprintln(out, "max_turns is the per-agent turn budget; quorum is the approve count needed to stop (0 = all).")
	fmt.Fprintln(out, "Or re-run `entire investigate --edit` from an interactive terminal.")
}

// runInvestigatePickerForm renders the production huh picker form.
func runInvestigatePickerForm(ctx context.Context, eligible []AgentChoice, picks *[]string, maxTurns, quorum *int) error {
	options := make([]huh.Option[string], 0, len(eligible))
	preselected := map[string]struct{}{}
	if picks != nil {
		for _, p := range *picks {
			preselected[p] = struct{}{}
		}
	}
	for _, c := range eligible {
		opt := huh.NewOption(c.Label, c.Name)
		if _, ok := preselected[c.Name]; ok {
			opt = opt.Selected(true)
		}
		options = append(options, opt)
	}

	maxTurnsStr := strconv.Itoa(*maxTurns)
	quorumStr := strconv.Itoa(*quorum)

	form := newAccessibleForm(huh.NewGroup(
		huh.NewMultiSelect[string]().
			Title("Agents (round-robin)").
			Description("Selected agents take turns during the investigation.").
			Options(options...).
			Height(min(len(options)+2, 12)).
			Value(picks),
		huh.NewInput().
			Title("Max turns per agent").
			Description("Per-agent turn budget. Defaults to 2.").
			Value(&maxTurnsStr),
		huh.NewInput().
			Title("Quorum").
			Description("Approve stances needed to terminate. 0 = all agents must approve.").
			Value(&quorumStr),
	))
	if err := form.RunWithContext(ctx); err != nil {
		return fmt.Errorf("picker form: %w", err)
	}
	mt, err := strconv.Atoi(maxTurnsStr)
	if err != nil {
		return fmt.Errorf("max-turns: %w", err)
	}
	q, err := strconv.Atoi(quorumStr)
	if err != nil {
		return fmt.Errorf("quorum: %w", err)
	}
	*maxTurns = mt
	*quorum = q
	return nil
}
