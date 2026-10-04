// Package review — see env.go for package-level rationale.
//
// runnerconfig.go is a SPIKE (see the Slack thread of 2026-09-07 and Stefan's
// `--use-runner-config` suggestion). It runs the repo's trail runner configs
// under `.entire/runners/` as local reviewers, so a developer can see what the
// trail's runners will say before pushing instead of discovering it afterwards.
//
// It deliberately bypasses review profiles entirely: a runner config already
// names its own agent, model, prompt and timeout, so there is nothing for a
// profile to contribute. Everything here is additive — no existing review path
// changes shape.
//
// Known gaps, all deliberate for the spike:
//   - Runner configs are read from disk only. Runners defined in the UI/db
//     (the slop monitor, for one) are invisible to this.
//   - The remote runs each runner in a sandbox with a read-only repo token.
//     Locally the agent runs with the developer's own permissions and no
//     sandbox, so a committed runner template is an instruction channel into
//     the machine. That is the same class as GHSA-hqjp-v5g5-vxvf. Before this
//     ships to anyone else it needs the trust gate the OPF `command` and
//     `external_agents` settings already use.
//   - Runner timeouts are per-runner in the config but RunMulti applies one
//     shared deadline, so the longest runner's timeout governs the run.
//   - Nothing is persisted; `entire review --findings` does not see these runs.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// runnersDirName is the runner-config directory relative to the .entire root,
// which is also how it is read back through the entiredir root.
const runnersDirName = "runners"

// Runner result types this path knows how to run and render. A summary runner
// exists to fill a trail body and has no local meaning, so it is skipped.
const (
	resultTypeComments = "code_review_comments"
	resultTypeMonitor  = "trail_monitor"
	resultTypeSummary  = "trail_summary"
)

// runnerConfig is the subset of a `.entire/runners/*.json` file this path uses.
// The full schema belongs to the server; decoding only what we run keeps an
// unrelated server-side field from breaking a local run.
type runnerConfig struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	Scope       string `json:"scope"`
	Runtime     struct {
		Kind      string `json:"kind"`
		Agent     string `json:"agent"`
		Model     string `json:"model"`
		TimeoutMS int    `json:"timeout_ms"`
	} `json:"runtime"`
	Prompt struct {
		Template string `json:"template"`
	} `json:"prompt"`
	Output struct {
		ResultType   string `json:"result_type"`
		TrailMonitor struct {
			Key      string `json:"key"`
			Label    string `json:"label"`
			Polarity string `json:"polarity"`
		} `json:"trail_monitor"`
	} `json:"output"`

	// file is the config's basename, used in messages when id is absent.
	file string
}

// label is the runner's human name for output. Both display_name and id come
// from a repo-committed JSON file, so they are attacker-supplied text on the
// way to a terminal and go through the same single-line sanitizer
// notifyDroppedReviewPrompts uses for settings-derived field paths.
func (r runnerConfig) label() string {
	if s := strings.TrimSpace(r.DisplayName); s != "" {
		return tuiutil.SanitizeDisplayText(s)
	}
	return tuiutil.SanitizeDisplayText(r.ID)
}

// loadRunnerConfigs reads every runner config under <worktreeRoot>/.entire/runners
// and returns the ones this path can run: enabled, trail-scoped prompt runners
// that produce comments or a monitor score. Skipped configs are reported with a
// reason so a runner going missing from a local run is never silent.
func loadRunnerConfigs(worktreeRoot, filter string) (runners []runnerConfig, skipped []string, err error) {
	dir := filepath.Join(worktreeRoot, paths.EntireDir, runnersDirName)
	root, err := entiredir.OpenAtForRead(worktreeRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()

	entries, err := osroot.ReadDirNoSymlinks(root, runnersDirName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("no runner configs: %s does not exist", dir)
		}
		return nil, nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	want := normalizeRunnerConfigID(filter)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, readErr := entiredir.ReadFile(root, runnersDirName+"/"+e.Name())
		if readErr != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", filepath.Join(dir, e.Name()), readErr)
		}
		var cfg runnerConfig
		if unmarshalErr := json.Unmarshal(raw, &cfg); unmarshalErr != nil {
			return nil, nil, fmt.Errorf("parsing %s: %w", filepath.Join(dir, e.Name()), unmarshalErr)
		}
		cfg.file = e.Name()
		if strings.TrimSpace(cfg.ID) == "" {
			cfg.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		if want != "" && normalizeRunnerConfigID(cfg.ID) != want {
			continue
		}
		if reason := runnerSkipReason(cfg); reason != "" {
			skipped = append(skipped, fmt.Sprintf("%s: %s", cfg.ID, reason))
			continue
		}
		runners = append(runners, cfg)
	}

	sort.Slice(runners, func(i, j int) bool { return runners[i].ID < runners[j].ID })
	sort.Strings(skipped)
	if len(runners) == 0 {
		if want != "" {
			return nil, skipped, fmt.Errorf("no runnable runner matching %q under %s", filter, dir)
		}
		return nil, skipped, fmt.Errorf("no runnable runner configs under %s", dir)
	}
	return runners, skipped, nil
}

// runnerSkipReason names why a config is not run locally, or "" when it is.
func runnerSkipReason(cfg runnerConfig) string {
	switch {
	case !cfg.Enabled:
		return "disabled"
	case strings.TrimSpace(cfg.Prompt.Template) == "":
		return "no prompt template"
	case cfg.Runtime.Kind != "" && cfg.Runtime.Kind != "prompt_runner":
		return "runtime kind " + cfg.Runtime.Kind + " is not a prompt runner"
	case cfg.Scope != "" && cfg.Scope != "trail":
		return "scope " + cfg.Scope + " is not a trail scope"
	case cfg.Output.ResultType == resultTypeSummary:
		// A summary fills the trail body. Locally you are the one who would read
		// it, and you already know what you changed.
		return "produces a trail summary, which has no local meaning"
	case cfg.Output.ResultType != resultTypeComments && cfg.Output.ResultType != resultTypeMonitor:
		return "unsupported result type " + cfg.Output.ResultType
	}
	return ""
}

func normalizeRunnerConfigID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "trail-")
}

// runnerAgentRegistryName maps a runner's agent id to the CLI's agent registry
// key. The runner configs say "claude" where the registry says "claude-code";
// the rest already agree. An unknown id returns "" and the runner is skipped
// rather than guessed at.
func runnerAgentRegistryName(runnerAgent string) string {
	switch strings.ToLower(strings.TrimSpace(runnerAgent)) {
	case "claude", "claude-code":
		return string(agent.AgentNameClaudeCode)
	case "codex":
		return string(agent.AgentNameCodex)
	case "gemini":
		return string(agent.AgentNameGemini)
	case "pi":
		return string(agent.AgentNamePi)
	default:
		return ""
	}
}

// renderRunnerPrompt fills the runner template's placeholders and prepends the
// local-scope statement.
//
// The statement is PREPENDED, not appended: the templates hand the agent
// numbered commands (`git diff origin/<base>...HEAD`) that see committed work
// only, and the whole point of running this before pushing is that the work may
// not be committed yet. Put after the template, a contradicting scope line
// reads as a footnote; put first, it reads as the governing instruction. This
// is still a contradiction the model resolves rather than a guarantee — the
// honest fix is per-runner templates that take the diff command as a variable.
func renderRunnerPrompt(cfg runnerConfig, branch, baseBranch string) string {
	template := cfg.Prompt.Template
	template = strings.ReplaceAll(template, "{{branch}}", branch)
	template = strings.ReplaceAll(template, "{{base_branch}}", baseBranch)
	// Locally there is no trail to read open findings from, so the runner sees
	// none and may repeat something already dismissed upstream.
	template = strings.ReplaceAll(template, "{{previous_findings}}", "[]")

	return localScopeStatement(branch, baseBranch) + "\n\n---\n\n" + template
}

func localScopeStatement(branch, baseBranch string) string {
	return fmt.Sprintf(
		"You are running LOCALLY on a developer's machine, before these changes are pushed, "+
			"not in the trail runner sandbox. The instructions after the divider are the trail "+
			"runner's own; follow them, with one change that overrides them where they conflict.\n\n"+
			"Scope: review the commits unique to branch %q versus %q, PLUS any uncommitted changes "+
			"in the working tree. Where the instructions below tell you to run "+
			"`git diff origin/%s...HEAD`, ALSO inspect `git diff HEAD` and `git status --porcelain`, "+
			"and treat what they show as part of the change under review. Uncommitted work is the "+
			"common case here and must not be skipped.\n\n"+
			"Everything else below applies unchanged, including the output format.",
		branch, baseBranch, baseBranch)
}

// runnerResult is one runner's parsed outcome.
type runnerResult struct {
	cfg    runnerConfig
	status reviewtypes.AgentStatus
	err    error

	// monitor fields, set when the runner produced a trail_monitor object.
	haveScore bool
	score     float64
	rationale string

	// comments, set when the runner produced code_review_comments.
	haveComments bool
	comments     []runnerComment

	// raw is the last non-empty output line, kept so an unparseable result is
	// shown rather than swallowed.
	raw string
}

type runnerComment struct {
	Severity   string  `json:"severity"`
	Confidence float64 `json:"confidence"`
	Body       string  `json:"body"`
	Location   struct {
		FilePath  string `json:"file_path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	} `json:"location"`
}

// parseRunnerOutput applies the runner's `last_json_line` output adapter to the
// agent's narrative and decodes it according to the runner's result type.
func parseRunnerOutput(cfg runnerConfig, narrative string) runnerResult {
	res := runnerResult{cfg: cfg}
	line := lastJSONLine(narrative)
	res.raw = line
	if line == "" {
		return res
	}
	switch cfg.Output.ResultType {
	case resultTypeMonitor:
		var doc struct {
			Value     *float64 `json:"value"`
			Rationale string   `json:"rationale"`
		}
		if err := json.Unmarshal([]byte(line), &doc); err == nil && doc.Value != nil {
			res.haveScore = true
			res.score = *doc.Value
			res.rationale = strings.TrimSpace(doc.Rationale)
		}
	case resultTypeComments:
		var doc struct {
			Comments []runnerComment `json:"comments"`
		}
		// A well-formed empty array is a real result ("clean"), so decode
		// success is the signal, not a non-empty list.
		if err := json.Unmarshal([]byte(line), &doc); err == nil {
			res.haveComments = true
			res.comments = doc.Comments
		}
	}
	return res
}

// lastJSONLine returns the last line of text that parses as a JSON object,
// mirroring the runners' `last_json_line` output adapter. Scanning upward
// rather than taking the literal last line tolerates an agent that appends a
// trailing note after its JSON.
func lastJSONLine(narrative string) string {
	lines := strings.Split(narrative, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		line = strings.TrimPrefix(line, "```json")
		line = strings.TrimSuffix(line, "```")
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		if !json.Valid([]byte(line)) {
			continue
		}
		return line
	}
	return ""
}

// printRunnerResults renders one block per runner: monitors as a score with
// their polarity and rationale, the review runner as its comment list. There is
// no consolidating judge — each runner's own verdict is what the trail would
// show, and putting a judge over five different output shapes would only
// reinterpret numbers the remote reports verbatim.
func printRunnerResults(w io.Writer, results []runnerResult) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Runner results")
	fmt.Fprintln(w)

	for _, res := range results {
		fmt.Fprintf(w, "  %s (%s)\n", res.cfg.label(), tuiutil.SanitizeDisplayText(res.cfg.ID))
		// A runner can emit its JSON result and still be classed failed — the
		// claude adapter marks a run failed on a single unparseable stream-json
		// envelope, long after the agent has said its piece. Printing that score
		// unlabelled would present a degraded run as a clean one, so say so and
		// then show what it did produce.
		if res.err == nil && res.status != reviewtypes.AgentStatusSucceeded {
			fmt.Fprintf(w, "    (run status: %s — result below may be incomplete)\n", res.status)
		}
		switch {
		case res.err != nil:
			fmt.Fprintf(w, "    failed: %v\n", res.err)
		case res.haveScore:
			fmt.Fprintf(w, "    %s %.0f/100 (%s)\n",
				monitorLabel(res.cfg), res.score, polarityPhrase(res.cfg.Output.TrailMonitor.Polarity))
			if res.rationale != "" {
				fmt.Fprintf(w, "    %s\n", tuiutil.SanitizeDisplayText(res.rationale))
			}
		case res.haveComments && len(res.comments) == 0:
			fmt.Fprintln(w, "    no comments")
		case res.haveComments:
			fmt.Fprintf(w, "    %s\n", pluralComments(len(res.comments)))
			for _, c := range res.comments {
				fmt.Fprintf(w, "      %s\n", formatRunnerComment(c))
			}
		case res.raw != "":
			fmt.Fprintf(w, "    unrecognized result: %s\n", tuiutil.SanitizeDisplayText(truncateForDisplay(res.raw, 200)))
		default:
			fmt.Fprintln(w, "    no result (the runner produced no JSON output line)")
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "These ran locally against your working tree, not in the trail sandbox.")
	fmt.Fprintln(w, "The trail's own runners can still disagree; this is a preview, not a verdict.")
}

func monitorLabel(cfg runnerConfig) string {
	if s := strings.TrimSpace(cfg.Output.TrailMonitor.Label); s != "" {
		return s
	}
	if s := strings.TrimSpace(cfg.Output.TrailMonitor.Key); s != "" {
		return s
	}
	return "score"
}

func polarityPhrase(polarity string) string {
	switch strings.TrimSpace(polarity) {
	case "higher_is_better":
		return "higher is better"
	case "lower_is_better":
		return "lower is better"
	default:
		return "no polarity declared"
	}
}

func pluralComments(n int) string {
	if n == 1 {
		return "1 comment"
	}
	return fmt.Sprintf("%d comments", n)
}

func formatRunnerComment(c runnerComment) string {
	severity := strings.TrimSpace(c.Severity)
	if severity == "" {
		severity = "unrated"
	}
	loc := tuiutil.SanitizeDisplayText(strings.TrimSpace(c.Location.FilePath))
	if loc != "" && c.Location.StartLine > 0 {
		loc = fmt.Sprintf("%s:%d", loc, c.Location.StartLine)
	}
	body := tuiutil.SanitizeDisplayText(strings.Join(strings.Fields(c.Body), " "))
	parts := []string{"[" + severity + "]"}
	if loc != "" {
		parts = append(parts, loc)
	}
	if c.Confidence > 0 {
		parts = append(parts, fmt.Sprintf("(confidence %.2f)", c.Confidence))
	}
	if body != "" {
		parts = append(parts, body)
	}
	return strings.Join(parts, " ")
}

func truncateForDisplay(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// runRunnerConfigReview is the `--use-runner-config` path. It reads the repo's
// runner configs, launches one reviewer per runner concurrently, and prints
// each runner's own result.
//
// It always exits 0 on a completed run, findings or not. This is meant to sit
// in front of a push, and a preview that fails the shell on a low-confidence
// score would be a preview nobody runs.
func runRunnerConfigReview(
	ctx context.Context,
	cmd *cobra.Command,
	runnerFilter, baseOverride string,
	timeout time.Duration,
	deps Deps,
	out io.Writer,
) error {
	silentErr := deps.NewSilentError
	errOut := cmd.ErrOrStderr()

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(errOut, "Not a git repository. Run `entire enable` first.")
		return silentErr(errors.New("not a git repository"))
	}

	runners, skipped, err := loadRunnerConfigs(worktreeRoot, runnerFilter)
	for _, s := range skipped {
		fmt.Fprintf(errOut, "skipping runner %s\n", s)
	}
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(errOut, err.Error())
		return silentErr(err)
	}

	branch, baseBranch, scopeErr := runnerScope(ctx, worktreeRoot, baseOverride, out)
	if scopeErr != nil {
		cmd.SilenceUsage = true
		return scopeErr
	}

	installed := map[string]struct{}{}
	for _, name := range deps.GetAgentsWithHooksInstalled(ctx) {
		installed[string(name)] = struct{}{}
	}

	reviewers, configs, maxTimeout := buildRunnerReviewers(runnerReviewerInputs{
		runners:    runners,
		branch:     branch,
		baseBranch: baseBranch,
		installed:  installed,
		reviewerFor: func(name string) reviewtypes.AgentReviewer {
			return deps.ReviewerFor(name)
		},
		errOut: errOut,
	})
	if len(reviewers) == 0 {
		cmd.SilenceUsage = true
		err := errors.New("no runner is runnable here: every runner's agent is missing hooks or a review runner adapter")
		fmt.Fprintln(errOut, err.Error())
		return silentErr(err)
	}

	fmt.Fprintf(out, "Running %d runner config(s) from .entire/runners locally.\n", len(reviewers))

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	names := make([]string, len(reviewers))
	for i, r := range reviewers {
		names[i] = r.Name()
	}
	// No synthesis provider: each runner's own output is the deliverable.
	sinks := composeMultiAgentSinks(multiAgentSinkInputs{
		out:        out,
		isTTY:      reviewCommandIsInteractive(cmd),
		agentNames: names,
		cancelRun:  cancelRun,
		runContext: runCtx,
	})
	if tuiSink, ok := findTUISink(sinks); ok {
		tuiSink.Start()
		defer tuiSink.Wait()
	}

	// RunMulti applies one deadline to every reviewer, so the longest runner's
	// own timeout governs the run. An explicit --timeout wins.
	effectiveTimeout := timeout
	if effectiveTimeout == 0 {
		effectiveTimeout = maxTimeout
	}
	summary, waitErr := RunMulti(runCtx, reviewers, reviewtypes.RunConfig{ReviewerTimeout: effectiveTimeout}, sinks)
	if shouldAbortMultiReview(summary, waitErr) && runCtx.Err() == nil && ctx.Err() == nil {
		cmd.SilenceUsage = true
		return multiReviewFailureError(waitErr)
	}

	printRunnerResults(out, collectRunnerResults(summary, configs))
	return nil
}

// runnerReviewerInputs collects what buildRunnerReviewers needs, keeping the
// signature readable and the helper testable without a cobra command.
type runnerReviewerInputs struct {
	runners     []runnerConfig
	branch      string
	baseBranch  string
	installed   map[string]struct{}
	reviewerFor func(string) reviewtypes.AgentReviewer
	errOut      io.Writer
}

// buildRunnerReviewers turns runner configs into launchable reviewers, one per
// runner. Runners naming an agent this machine cannot launch are dropped with a
// note rather than failing the whole run: four working previews beat none.
//
// configs is parallel to the returned reviewers, so a result can be matched
// back to the runner that produced it by index. maxTimeout is the largest
// per-runner timeout, which the caller uses as the shared deadline.
func buildRunnerReviewers(in runnerReviewerInputs) (reviewers []reviewtypes.AgentReviewer, configs []runnerConfig, maxTimeout time.Duration) {
	for _, cfg := range in.runners {
		agentName := runnerAgentRegistryName(cfg.Runtime.Agent)
		if agentName == "" {
			fmt.Fprintf(in.errOut, "skipping runner %s: unknown agent %q\n", cfg.ID, cfg.Runtime.Agent)
			continue
		}
		if _, ok := in.installed[agentName]; !ok {
			fmt.Fprintf(in.errOut, "skipping runner %s: hooks are not installed for %s (run `entire configure --agent %s`)\n", cfg.ID, agentName, agentName)
			continue
		}
		inner := in.reviewerFor(agentName)
		if inner == nil {
			fmt.Fprintf(in.errOut, "skipping runner %s: %s has no review runner adapter\n", cfg.ID, agentName)
			continue
		}
		if _, agErr := agent.Get(agenttypes.AgentName(agentName)); agErr != nil {
			fmt.Fprintf(in.errOut, "skipping runner %s: %v\n", cfg.ID, agErr)
			continue
		}
		if d := time.Duration(cfg.Runtime.TimeoutMS) * time.Millisecond; d > maxTimeout {
			maxTimeout = d
		}
		reviewers = append(reviewers, &perAgentConfiguredReviewer{
			name:  cfg.ID,
			inner: inner,
			cfg: reviewtypes.RunConfig{
				// PromptOverride sends the runner's own prompt verbatim, without
				// review's task/skills/scope composition. Fidelity to what the
				// trail runner sends is the entire point.
				PromptOverride: renderRunnerPrompt(cfg, in.branch, in.baseBranch),
				ProfileName:    cfg.ID,
				Model:          strings.TrimSpace(cfg.Runtime.Model),
			},
		})
		configs = append(configs, cfg)
	}
	return reviewers, configs, maxTimeout
}

// collectRunnerResults pairs each agent run with the runner config that
// produced it and parses its output. RunMulti preserves reviewer order in
// summary.AgentRuns, which is what makes the index pairing sound; the ID check
// guards that assumption rather than trusting it.
func collectRunnerResults(summary reviewtypes.RunSummary, configs []runnerConfig) []runnerResult {
	byID := make(map[string]runnerConfig, len(configs))
	for _, cfg := range configs {
		byID[cfg.ID] = cfg
	}
	results := make([]runnerResult, 0, len(summary.AgentRuns))
	for i, run := range summary.AgentRuns {
		cfg, ok := byID[run.Name]
		if !ok {
			if i < len(configs) {
				cfg = configs[i]
			} else {
				cfg = runnerConfig{ID: run.Name}
			}
		}
		res := parseRunnerOutput(cfg, joinAssistantText(run.Buffer))
		res.status = run.Status
		res.err = run.Err
		results = append(results, res)
	}
	return results
}

// runnerScope resolves the branch and base branch the runner templates
// interpolate. The templates spell the base as `origin/{{base_branch}}`, so the
// value must be the bare branch name — passing "origin/main" through would
// produce "origin/origin/main".
func runnerScope(ctx context.Context, worktreeRoot, baseOverride string, out io.Writer) (branch, baseBranch string, err error) {
	repo, openErr := gitrepo.OpenPath(worktreeRoot)
	if openErr != nil {
		return "", "", fmt.Errorf("open repository at %q: %w", worktreeRoot, openErr)
	}
	defer repo.Close()

	stats, statsErr := ComputeScopeStats(ctx, repo, baseOverride)
	if statsErr != nil {
		return "", "", fmt.Errorf("resolve review scope: %w", statsErr)
	}
	fmt.Fprintln(out, formatScopeBanner(stats))

	branch = strings.TrimSpace(stats.CurrentBranch)
	if branch == "" {
		branch = "HEAD"
	}
	baseBranch = strings.TrimPrefix(strings.TrimSpace(stats.BaseRef), "origin/")
	if baseBranch == "" {
		baseBranch = "main"
	}
	return branch, baseBranch, nil
}

// runnerConfigFlagConflict names the first flag given alongside
// --use-runner-config that the runner path cannot honor, or "" when there is
// none. The flags it rejects all select something a runner config already
// carries (its agent, its model, its prompt) or select a different command mode
// entirely; honoring them silently would make the local run diverge from the
// remote runner it exists to predict.
//
// It reads the flags off the command rather than taking them as parameters, so
// a review flag added later cannot silently become one this mode accepts and
// ignores.
func runnerConfigFlagConflict(cmd *cobra.Command, args []string) string {
	for _, name := range []string{
		"configure", "edit", "findings", "list", "agents", "models",
		"agent", "model", "profile", "prompt",
	} {
		if cmd.Flags().Changed(name) {
			return "--" + name
		}
	}
	if len(args) > 0 {
		return "a profile argument"
	}
	return ""
}

// dispatchRunnerConfigReview runs the --use-runner-config path when it was
// asked for. handled reports whether this mode claimed the invocation, so the
// caller returns err verbatim rather than guessing from a nil error.
func dispatchRunnerConfigReview(
	ctx context.Context,
	cmd *cobra.Command,
	useRunnerConfig bool,
	runnerFilter, baseOverride string,
	timeout time.Duration,
	args []string,
	deps Deps,
) (handled bool, err error) {
	if !useRunnerConfig {
		if strings.TrimSpace(runnerFilter) != "" {
			cmd.SilenceUsage = true
			return true, errors.New("--runner requires --use-runner-config")
		}
		return false, nil
	}
	if conflict := runnerConfigFlagConflict(cmd, args); conflict != "" {
		cmd.SilenceUsage = true
		return true, fmt.Errorf("--use-runner-config cannot be combined with %s; a runner config carries its own agent, model and prompt", conflict)
	}
	return true, runRunnerConfigReview(ctx, cmd, runnerFilter, baseOverride, timeout, deps, cmd.OutOrStdout())
}

// installRunnerConfigMode declares the spike's two flags on cmd and returns the
// dispatcher for them. Flags, dispatch, conflict rules and the run all sit in
// this one file, so the whole --use-runner-config surface can be deleted in one
// piece if the spike does not pan out — and NewCommand carries a single call
// instead of a mode's worth of statements.
//
// baseOverride and reviewTimeout are captured by pointer because they are
// NewCommand's own flag variables, populated by cobra at parse time, long after
// this returns.
func installRunnerConfigMode(
	cmd *cobra.Command,
	baseOverride *string,
	reviewTimeout *time.Duration,
	deps Deps,
) func(context.Context, *cobra.Command, []string) (bool, error) {
	var useRunnerConfig bool
	var runnerFilter string
	cmd.Flags().BoolVar(&useRunnerConfig, "use-runner-config", false,
		"SPIKE: review with the repo's .entire/runners trail runner configs instead of a review profile")
	cmd.Flags().StringVar(&runnerFilter, "runner", "",
		`with --use-runner-config: run only this runner id (the leading "trail-" is optional)`)

	return func(ctx context.Context, cmd *cobra.Command, args []string) (bool, error) {
		return dispatchRunnerConfigReview(ctx, cmd, useRunnerConfig, runnerFilter, *baseOverride, *reviewTimeout, args, deps)
	}
}
