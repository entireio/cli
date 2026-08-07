package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/spf13/cobra"
)

// scanHint is printed when a scan finds repositories that could be improved.
const scanHint = "run 'entire scan --fix' to enable them"

// repoScanReport is the `entire scan --json` document.
type repoScanReport struct {
	ScannedDirs []string        `json:"scanned_dirs"`
	Repos       []repoScanEntry `json:"repos"`
	Summary     repoScanSummary `json:"summary"`
}

// repoScanSummary is the aggregate line, duplicated into JSON so consumers do
// not have to recompute it.
type repoScanSummary struct {
	Total          int `json:"total"`
	SetUp          int `json:"set_up"`
	Enabled        int `json:"enabled"`
	NeedsAttention int `json:"needs_attention"`
}

type scanOptions struct {
	Depth     int
	JSON      bool
	Fix       bool
	Yes       bool
	AgentName string
}

func newScanCmd() *cobra.Command {
	var opts scanOptions

	cmd := &cobra.Command{
		Use:   "scan [dir...]",
		Short: "Scan folders for git repos and report Entire enablement per repo",
		Long: `Scan one or more folders for git repositories and report, per repo, whether
Entire is set up, enabled, which agents have hooks installed, and which agents
are in use without hooks.

With no arguments, scans the folder containing the current repository (so
sibling projects are covered); outside a git repository it scans the current
directory.

--fix re-runs 'entire enable' in the repositories that need it.`,
		Example: "  entire scan\n" +
			"  entire scan ~/dev ~/work --depth 3\n" +
			"  entire scan --json\n" +
			"  entire scan --fix --yes\n" +
			"  entire scan --fix --yes --agent claude-code",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScan(cmd.Context(), cmd.OutOrStdout(), args, opts, runScanFixWithCLI)
		},
	}

	cmd.Flags().IntVar(&opts.Depth, "depth", scanDefaultDepth, "How many directory levels below each scanned folder to search")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output the scan as JSON")
	cmd.Flags().BoolVar(&opts.Fix, "fix", false, "Run 'entire enable' in the repositories that need it")
	cmd.Flags().BoolVarP(&opts.Yes, "yes", "y", false, "With --fix, fix every repository without prompting")
	cmd.Flags().StringVar(&opts.AgentName, agentFlagName, "", "With --fix, enable this specific agent in every selected repository")
	cmd.MarkFlagsMutuallyExclusive("json", "fix")

	return cmd
}

func runScan(ctx context.Context, w io.Writer, args []string, opts scanOptions, run scanFixRunner) error {
	if opts.Depth < 1 {
		return fmt.Errorf("--depth must be at least 1, got %d", opts.Depth)
	}

	// Discover external agent plugins once, before the fan-out: discovery
	// executes plugin binaries, and doing it per repo would run each plugin as
	// many times as there are repositories.
	external.DiscoverAndRegisterAlways(ctx)

	if opts.AgentName != "" {
		if _, err := agent.Get(types.AgentName(opts.AgentName)); err != nil {
			return fmt.Errorf("unknown agent %q (available: %s)", opts.AgentName, strings.Join(agent.StringList(), ", "))
		}
	}

	dirs, err := resolveScanDirs(ctx, args)
	if err != nil {
		return err
	}

	candidates, err := findGitRepos(ctx, dirs, opts.Depth)
	if err != nil {
		return err
	}
	entries, err := inspectReposForScan(ctx, candidates)
	if err != nil {
		return err
	}

	report := newRepoScanReport(dirs, entries)
	if opts.JSON {
		return writeRepoScanJSON(w, report)
	}

	writeRepoScanTable(w, report, opts.Fix)
	if !opts.Fix {
		return nil
	}
	return applyScanFixes(ctx, w, report, opts, run)
}

// resolveScanDirs turns the positional arguments into absolute scan roots.
//
// The no-argument default mirrors the dispatch wizard's sibling semantics: from
// inside a repository the interesting set is the *other* projects next to it,
// so the repo's parent folder is scanned rather than the repo itself.
func resolveScanDirs(ctx context.Context, args []string) ([]string, error) {
	if len(args) > 0 {
		dirs := make([]string, 0, len(args))
		for _, arg := range args {
			abs, err := filepath.Abs(arg)
			if err != nil {
				return nil, fmt.Errorf("resolving %q: %w", arg, err)
			}
			dirs = append(dirs, filepath.Clean(abs))
		}
		return dirs, nil
	}

	if root, err := paths.WorktreeRoot(ctx); err == nil && root != "" {
		return []string{filepath.Dir(filepath.Clean(root))}, nil
	}

	cwd, err := currentDirForScan()
	if err != nil {
		return nil, err
	}
	return []string{cwd}, nil
}

// currentDirForScan resolves the process working directory. Unlike most of the
// codebase this genuinely wants the cwd: it is the scan root of last resort
// when the user is not inside a repository at all.
func currentDirForScan() (string, error) {
	cwd, err := os.Getwd() //nolint:forbidigo // the scan root outside a repo is the literal working directory
	if err != nil {
		return "", fmt.Errorf("resolving the current directory: %w", err)
	}
	return filepath.Clean(cwd), nil
}

func newRepoScanReport(dirs []string, entries []repoScanEntry) repoScanReport {
	report := repoScanReport{
		ScannedDirs: dirs,
		Repos:       entries,
		Summary:     repoScanSummary{Total: len(entries)},
	}
	if report.Repos == nil {
		report.Repos = []repoScanEntry{}
	}
	for _, entry := range entries {
		if entry.SetUp {
			report.Summary.SetUp++
		}
		if entry.Enabled {
			report.Summary.Enabled++
		}
		if entry.needsAttention() {
			report.Summary.NeedsAttention++
		}
	}
	return report
}

func writeRepoScanJSON(w io.Writer, report repoScanReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encoding scan report: %w", err)
	}
	return nil
}

// writeRepoScanTable renders the human view. It is deliberately plain text with
// no terminal detection: an agent running `entire scan` without a TTY sees the
// same complete table a human does.
func writeRepoScanTable(w io.Writer, report repoScanReport, fixing bool) {
	scanned := make([]string, 0, len(report.ScannedDirs))
	for _, dir := range report.ScannedDirs {
		scanned = append(scanned, abbreviateHomePath(dir))
	}
	fmt.Fprintf(w, "Scanned %s\n\n", strings.Join(scanned, ", "))

	if len(report.Repos) == 0 {
		fmt.Fprintln(w, "No git repositories found.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPO\tENTIRE\tGIT HOOKS\tAGENTS\tPRESENT")
	for _, entry := range report.Repos {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			scanRepoLabel(entry),
			scanEntireState(entry),
			yesNo(entry.GitHooksInstalled),
			scanAgentList(entry.AgentsHooked),
			scanAgentList(entry.AgentsDetectedUnhooked),
		)
	}
	_ = tw.Flush()

	writeRepoScanNotes(w, report)

	fmt.Fprintf(w, "\n%d %s scanned: %d set up, %d enabled, %d need attention.\n",
		report.Summary.Total, scanRepoWord(report.Summary.Total),
		report.Summary.SetUp, report.Summary.Enabled, report.Summary.NeedsAttention)

	if report.Summary.NeedsAttention > 0 && !fixing {
		fmt.Fprintf(w, "%s\n", scanHint)
	}
}

// writeRepoScanNotes prints the per-repo details that do not fit a table
// column: stale hook config, untrusted Codex hooks, and inspection errors.
func writeRepoScanNotes(w io.Writer, report repoScanReport) {
	var notes []string
	for _, entry := range report.Repos {
		label := scanRepoLabel(entry)
		if len(entry.HooksOutdated) > 0 {
			notes = append(notes, fmt.Sprintf("%s: hook config outdated for %s (fix: entire enable --agent <name> --force)",
				label, scanAgentList(entry.HooksOutdated)))
		}
		if len(entry.CodexTrustGaps) > 0 {
			notes = append(notes, fmt.Sprintf("%s: codex hooks awaiting trust: %s",
				label, strings.Join(entry.CodexTrustGaps, ", ")))
		}
		if entry.Error != "" {
			notes = append(notes, fmt.Sprintf("%s: %s", label, entry.Error))
		}
	}
	if len(notes) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, note := range notes {
		fmt.Fprintf(w, "  %s\n", note)
	}
}

func applyScanFixes(ctx context.Context, w io.Writer, report repoScanReport, opts scanOptions, run scanFixRunner) error {
	fixable := fixableScanRepos(report.Repos, opts.AgentName)
	if len(fixable) == 0 {
		fmt.Fprintln(w, "\nNothing to fix.")
		return nil
	}

	selected, err := selectScanFixRepos(fixable, opts.Yes)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(w, "\nNo repositories selected.")
		return nil
	}

	return runScanFixes(ctx, w, planScanFixes(report.Repos, opts.AgentName), selected, run)
}

func scanRepoLabel(entry repoScanEntry) string {
	label := abbreviateHomePath(entry.Path)
	if entry.LinkedWorktree {
		label += " (worktree)"
	}
	return label
}

// scanEntireState collapses the two booleans into the state a user thinks in.
func scanEntireState(entry repoScanEntry) string {
	switch {
	case !entry.SetUp:
		return "not set up"
	case entry.Enabled:
		return "enabled"
	default:
		return "disabled"
	}
}

func scanAgentList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// scanRepoWord exists because the shared pluralize helper only appends "s",
// which would render "1 repositorys".
func scanRepoWord(count int) string {
	if count == 1 {
		return "repository"
	}
	return "repositories"
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// abbreviateHomePath renders an absolute path with the user's home directory
// collapsed to "~", so a table of repositories stays readable.
func abbreviateHomePath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	home = filepath.Clean(home)
	if path == home {
		return "~"
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || paths.IsRelativeTraversal(rel) {
		return path
	}
	return "~" + string(filepath.Separator) + rel
}
