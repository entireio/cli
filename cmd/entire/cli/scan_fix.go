package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// scanFixAction is a single `entire enable` invocation the fix flow will run
// inside one repository. An empty AgentName means the bare `entire enable`
// (re-enable a repo that was set up and then disabled).
type scanFixAction struct {
	RepoRoot  string
	AgentName string
	Force     bool
}

// describe renders the action as the command it will run, for logs and dry
// reporting.
func (a scanFixAction) describe() string {
	parts := []string{"entire", "enable"}
	if a.AgentName != "" {
		parts = append(parts, "--agent", a.AgentName)
	}
	if a.Force {
		parts = append(parts, "--force")
	}
	return strings.Join(parts, " ")
}

// scanFixRunner executes one fix action. It exists so tests can record the plan
// without spawning subprocesses; production wires runScanFixWithCLI.
type scanFixRunner func(ctx context.Context, action scanFixAction, out io.Writer) error

// planScanFixes turns scan results into the ordered list of enable invocations
// that would bring the repositories up to date.
func planScanFixes(entries []repoScanEntry, agentOverride string) []scanFixAction {
	var actions []scanFixAction
	for _, entry := range entries {
		actions = append(actions, planScanFixesForRepo(entry, agentOverride)...)
	}
	return actions
}

// planScanFixesForRepo computes one repository's actions.
//
// With an explicit --agent, the user has named what they want installed, so
// that single action applies to every selected repo — including repos where
// nothing was detected, which is the only way to enable an agent whose presence
// we cannot see (Codex, whose DetectPresence is defined as AreHooksInstalled).
//
// Without --agent the plan is derived purely from what was detected, so a repo
// with no agent in evidence is left alone rather than guessed at.
func planScanFixesForRepo(entry repoScanEntry, agentOverride string) []scanFixAction {
	if agentOverride != "" {
		return []scanFixAction{{
			RepoRoot:  entry.Path,
			AgentName: agentOverride,
			Force:     slices.Contains(entry.HooksOutdated, agentOverride),
		}}
	}

	var actions []scanFixAction
	// Re-enable first: the repo has to be active before per-agent hooks mean
	// anything.
	if entry.SetUp && !entry.Enabled {
		actions = append(actions, scanFixAction{RepoRoot: entry.Path})
	}
	for _, name := range entry.AgentsDetectedUnhooked {
		actions = append(actions, scanFixAction{RepoRoot: entry.Path, AgentName: name})
	}
	for _, name := range entry.HooksOutdated {
		actions = append(actions, scanFixAction{RepoRoot: entry.Path, AgentName: name, Force: true})
	}
	return actions
}

// fixableScanRepos lists, in scan order, the repositories that have at least
// one planned action.
func fixableScanRepos(entries []repoScanEntry, agentOverride string) []string {
	var repos []string
	for _, entry := range entries {
		if len(planScanFixesForRepo(entry, agentOverride)) > 0 {
			repos = append(repos, entry.Path)
		}
	}
	return repos
}

// selectScanFixRepos resolves which repositories to fix.
//
// With --yes (or no terminal to draw on plus --yes) every fixable repo is
// selected. Interactively the user gets a pre-selected multi-select. Without a
// terminal and without --yes the command refuses rather than silently editing
// every repo it found.
func selectScanFixRepos(fixable []string, assumeYes bool) ([]string, error) {
	if len(fixable) == 0 {
		return nil, nil
	}
	if assumeYes {
		return fixable, nil
	}
	if !interactive.CanPromptInteractively() {
		return nil, errors.New("--fix needs a terminal to choose repositories; pass --yes to fix non-interactively")
	}

	selected := slices.Clone(fixable)
	options := make([]huh.Option[string], 0, len(fixable))
	for _, repo := range fixable {
		options = append(options, huh.NewOption(abbreviateHomePath(repo), repo))
	}
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Enable Entire in which repositories?").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.Run(); err != nil {
		return nil, fmt.Errorf("selecting repositories: %w", err)
	}
	return selected, nil
}

// runScanFixes executes actions for the selected repositories, in order,
// one at a time. Sequential on purpose: each action shells out to `entire
// enable`, which writes agent config and git hooks — interleaving two of those
// in the same repo (or racing on a shared linked-worktree hooks dir) is not
// worth the wall-clock saving.
//
// A failing repo does not stop the run; the error count is reported at the end.
func runScanFixes(ctx context.Context, w io.Writer, actions []scanFixAction, selected []string, run scanFixRunner) error {
	wanted := make(map[string]struct{}, len(selected))
	for _, repo := range selected {
		wanted[repo] = struct{}{}
	}

	failures := 0
	applied := 0
	for _, action := range actions {
		if _, ok := wanted[action.RepoRoot]; !ok {
			continue
		}
		label := abbreviateHomePath(action.RepoRoot)
		fmt.Fprintf(w, "\n%s: %s\n", label, action.describe())
		applied++

		prefixed := newLinePrefixWriter(w, "  "+label+" | ")
		runErr := run(ctx, action, prefixed)
		prefixed.Flush()
		if runErr != nil {
			failures++
			fmt.Fprintf(w, "%s: failed: %v\n", label, runErr)
		}
	}

	fmt.Fprintf(w, "\nRan %d enable %s", applied, pluralize("command", applied))
	if failures > 0 {
		fmt.Fprintf(w, ", %d failed", failures)
	}
	fmt.Fprintln(w, ".")

	if failures > 0 {
		return fmt.Errorf("%d of %d enable %s failed", failures, applied, pluralize("command", applied))
	}
	return nil
}

// runScanFixWithCLI is the production scanFixRunner: it re-invokes this binary
// as `entire enable` inside the target repository.
//
// A subprocess rather than an in-process call, deliberately. `entire enable`
// resolves the repository from the process working directory in many places
// (git hook installation, strategy setup, remote reporting); a per-repo context
// override covers reads, not that whole write path. Giving each repo its own
// process with its own cmd.Dir is the only way to reuse the real enable flow
// without auditing every write for cwd-independence.
func runScanFixWithCLI(ctx context.Context, action scanFixAction, out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the entire binary: %w", err)
	}

	args := []string{"enable"}
	if action.AgentName != "" {
		args = append(args, "--agent", action.AgentName)
	}
	if action.Force {
		args = append(args, "--force")
	}

	// Detached from the TTY so a prompt in any enable path fails fast instead of
	// stalling a scan that may be fixing a dozen repositories.
	cmd := execx.NonInteractive(ctx, exe, args...)
	cmd.Dir = action.RepoRoot
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", action.describe(), err)
	}
	return nil
}

// linePrefixWriter prefixes every complete line written through it, so the
// output of several `entire enable` runs stays attributable.
type linePrefixWriter struct {
	w      io.Writer
	prefix string
	buf    bytes.Buffer
}

func newLinePrefixWriter(w io.Writer, prefix string) *linePrefixWriter {
	return &linePrefixWriter{w: w, prefix: prefix}
}

// Flush emits any buffered partial line. Call it once the producer is done —
// `entire enable` output does not always end in a newline.
func (p *linePrefixWriter) Flush() {
	if p.buf.Len() == 0 {
		return
	}
	_, _ = io.WriteString(p.w, p.prefix+p.buf.String()+"\n")
	p.buf.Reset()
}

func (p *linePrefixWriter) Write(data []byte) (int, error) {
	p.buf.Write(data)
	for {
		line, err := p.buf.ReadString('\n')
		if err != nil {
			// Partial line: put it back and wait for the rest.
			p.buf.Reset()
			p.buf.WriteString(line)
			break
		}
		if _, werr := io.WriteString(p.w, p.prefix+line); werr != nil {
			return len(data), fmt.Errorf("writing prefixed output: %w", werr)
		}
	}
	return len(data), nil
}
