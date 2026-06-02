package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointid "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/spf13/cobra"
)

type replayCheckpointOptions struct {
	Agent        string
	Model        string
	TestCommand  string
	KeepWorktree bool
	JSON         bool
	Timeout      time.Duration
}

type replayEvalOptions struct {
	Checkpoints     []string
	FromCheckpoints bool
	Limit           int
	Agents          []string
	Model           string
	TestCommand     string
	KeepWorktrees   bool
	JSON            bool
	Timeout         time.Duration
}

type replayReportOptions struct {
	JSON bool
}

type ReplaySpec struct {
	CheckpointID  string            `json:"checkpoint_id"`
	SessionID     string            `json:"session_id,omitempty"`
	Prompt        string            `json:"prompt"`
	TargetCommit  string            `json:"target_commit"`
	BaseCommit    string            `json:"base_commit"`
	FilesTouched  []string          `json:"files_touched"`
	OriginalAgent string            `json:"original_agent,omitempty"`
	OriginalModel string            `json:"original_model,omitempty"`
	TokenUsage    *agent.TokenUsage `json:"token_usage,omitempty"`
}

type ReplayRun struct {
	ID           string            `json:"id"`
	Spec         ReplaySpec        `json:"spec"`
	Agent        string            `json:"agent"`
	Model        string            `json:"model,omitempty"`
	Status       string            `json:"status"`
	StartedAt    time.Time         `json:"started_at"`
	FinishedAt   time.Time         `json:"finished_at"`
	DurationMS   int64             `json:"duration_ms"`
	WorktreePath string            `json:"worktree_path,omitempty"`
	ChangedFiles []string          `json:"changed_files"`
	Diff         string            `json:"diff,omitempty"`
	Test         ReplayTestRun     `json:"test"`
	Metrics      ReplayMetrics     `json:"metrics"`
	TokenUsage   *agent.TokenUsage `json:"token_usage,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
	Error        string            `json:"error,omitempty"`
	Output       string            `json:"output,omitempty"`
	ResultPath   string            `json:"result_path,omitempty"`
}

type ReplayTestRun struct {
	Status     string `json:"status"`
	Command    string `json:"command,omitempty"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
}

type ReplayMetrics struct {
	FilePrecision      int      `json:"file_precision"`
	FileRecall         int      `json:"file_recall"`
	FileOverlap        int      `json:"file_overlap"`
	MissingFiles       []string `json:"missing_files,omitempty"`
	ExtraFiles         []string `json:"extra_files,omitempty"`
	RiskyFiles         []string `json:"risky_files,omitempty"`
	MissingTests       bool     `json:"missing_tests,omitempty"`
	RiskScore          int      `json:"risk_score"`
	SemanticAvailable  bool     `json:"semantic_available"`
	SemanticSimilarity int      `json:"semantic_similarity,omitempty"`
}

type ReplayEvalAgentSummary struct {
	Agent                 string `json:"agent"`
	Runs                  int    `json:"runs"`
	Passed                int    `json:"passed"`
	Failed                int    `json:"failed"`
	Skipped               int    `json:"skipped"`
	PassRate              int    `json:"pass_rate"`
	AvgFileRecall         int    `json:"avg_file_recall"`
	AvgFilePrecision      int    `json:"avg_file_precision"`
	AvgSemanticSimilarity int    `json:"avg_semantic_similarity,omitempty"`
	SemanticRuns          int    `json:"semantic_runs,omitempty"`
	AvgDurationMS         int64  `json:"avg_duration_ms"`
	RiskScore             int    `json:"risk_score"`
	InputTokens           int    `json:"input_tokens,omitempty"`
	OutputTokens          int    `json:"output_tokens,omitempty"`
}

type ReplayEvalRun struct {
	ID         string                   `json:"id"`
	StartedAt  time.Time                `json:"started_at"`
	FinishedAt time.Time                `json:"finished_at"`
	Agents     []string                 `json:"agents"`
	Summaries  []ReplayEvalAgentSummary `json:"summaries,omitempty"`
	Runs       []ReplayRun              `json:"runs"`
	ResultPath string                   `json:"result_path,omitempty"`
}

type ReplayRunnerRequest struct {
	Spec         ReplaySpec
	Agent        string
	Model        string
	Prompt       string
	WorktreePath string
}

type ReplayRunnerResult struct {
	Output     string
	TokenUsage *agent.TokenUsage
	Warnings   []string
}

type ReplayRunner interface {
	Name() string
	Run(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error)
}

type replayRunnerFunc struct {
	name string
	fn   func(context.Context, ReplayRunnerRequest) (ReplayRunnerResult, error)
}

func (f replayRunnerFunc) Name() string { return f.name }

func (f replayRunnerFunc) Run(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
	return f.fn(ctx, req)
}

var replayRunnerFor = defaultReplayRunnerFor

const (
	replayAgentGeminiCLI    = "gemini-cli"
	replayResultOutputLimit = 64 * 1024
	replayStatusFailed      = "failed"
	replayStatusPassed      = "passed"
	replayStatusRunning     = "running"
	replayStatusSkipped     = "skipped"
	replayTestStatusSkipped = replayStatusSkipped
)

func newReplayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay checkpoint tasks in isolated worktrees",
		Long:  "Replay historical Entire checkpoints against coding agents and compare their output to the original commit.",
	}
	cmd.AddCommand(newReplayCheckpointCmd())
	cmd.AddCommand(newReplayReportCmd())
	return cmd
}

func newReplayCheckpointCmd() *cobra.Command {
	opts := replayCheckpointOptions{Agent: string(agent.AgentNameClaudeCode)}
	cmd := &cobra.Command{
		Use:   "checkpoint <checkpoint-id>",
		Short: "Replay one checkpoint with one agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := runReplayCheckpoint(cmd.Context(), args[0], opts)
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeReplayJSON(cmd.OutOrStdout(), run)
			}
			renderReplayRun(cmd.OutOrStdout(), run)
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Agent, "agent", opts.Agent, "Agent to replay with: claude-code, codex, or gemini")
	cmd.Flags().StringVar(&opts.Model, "model", "", "Model override passed to the agent when supported")
	cmd.Flags().StringVar(&opts.TestCommand, "test-cmd", "", "Optional test command to run after replay")
	cmd.Flags().BoolVar(&opts.KeepWorktree, "keep-worktree", false, "Keep the replay worktree for inspection")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output replay result as JSON")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "Maximum duration for the replay agent and test command")
	return cmd
}

func newReplayReportCmd() *cobra.Command {
	var opts replayReportOptions
	cmd := &cobra.Command{
		Use:   "report <run-id>",
		Short: "Show a saved checkpoint replay report",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := readReplayRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeReplayJSON(cmd.OutOrStdout(), run)
			}
			renderReplayRun(cmd.OutOrStdout(), run)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output replay report as JSON")
	return cmd
}

func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run private agent evals from Entire checkpoints",
		Long:  "Run checkpoint replay tasks across one or more agents and rank the results.",
	}
	cmd.AddCommand(newEvalRunCmd())
	cmd.AddCommand(newEvalReportCmd())
	return cmd
}

func newEvalRunCmd() *cobra.Command {
	opts := replayEvalOptions{Limit: 10, Agents: []string{string(agent.AgentNameClaudeCode)}}
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a replay eval",
		RunE: func(cmd *cobra.Command, _ []string) error {
			run, err := runReplayEval(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeReplayJSON(cmd.OutOrStdout(), run)
			}
			renderReplayEval(cmd.OutOrStdout(), run)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&opts.Checkpoints, "checkpoint", nil, "Checkpoint ID to include (repeatable)")
	cmd.Flags().BoolVar(&opts.FromCheckpoints, "from-checkpoints", false, "Use recent committed checkpoints")
	cmd.Flags().IntVar(&opts.Limit, "limit", opts.Limit, "Maximum checkpoints when using --from-checkpoints")
	cmd.Flags().StringSliceVar(&opts.Agents, "agent", opts.Agents, "Agents to run, comma-separated or repeated")
	cmd.Flags().StringVar(&opts.Model, "model", "", "Model override passed to each agent when supported")
	cmd.Flags().StringVar(&opts.TestCommand, "test-cmd", "", "Optional test command to run after each replay")
	cmd.Flags().BoolVar(&opts.KeepWorktrees, "keep-worktree", false, "Keep replay worktrees for inspection")
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output eval result as JSON")
	cmd.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "Maximum duration for each replay agent and test command")
	return cmd
}

func newEvalReportCmd() *cobra.Command {
	var opts replayReportOptions
	cmd := &cobra.Command{
		Use:   "report <run-id>",
		Short: "Show a saved replay eval report",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			run, err := readReplayEval(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeReplayJSON(cmd.OutOrStdout(), run)
			}
			renderReplayEval(cmd.OutOrStdout(), run)
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "Output eval report as JSON")
	return cmd
}

func runReplayCheckpoint(ctx context.Context, checkpointRef string, opts replayCheckpointOptions) (*ReplayRun, error) {
	spec, err := buildReplaySpec(ctx, checkpointRef)
	if err != nil {
		return nil, err
	}
	return executeReplay(ctx, spec, opts)
}

func runReplayEval(ctx context.Context, opts replayEvalOptions) (*ReplayEvalRun, error) {
	checkpoints := append([]string(nil), opts.Checkpoints...)
	if opts.FromCheckpoints {
		recent, err := recentReplayCheckpoints(ctx, opts.Limit)
		if err != nil {
			return nil, err
		}
		checkpoints = append(checkpoints, recent...)
	}
	checkpoints = uniqueNonEmpty(checkpoints)
	if len(checkpoints) == 0 {
		return nil, errors.New("no checkpoints selected; pass --checkpoint or --from-checkpoints")
	}
	agents := uniqueNonEmpty(opts.Agents)
	if len(agents) == 0 {
		return nil, errors.New("no agents selected")
	}

	eval := &ReplayEvalRun{
		ID:        newReplayID(),
		StartedAt: time.Now().UTC(),
		Agents:    agents,
	}
	for _, cp := range checkpoints {
		spec, err := buildReplaySpec(ctx, cp)
		if err != nil {
			now := time.Now().UTC()
			eval.Runs = append(eval.Runs, ReplayRun{
				ID:         newReplayID(),
				Status:     replayStatusFailed,
				StartedAt:  now,
				FinishedAt: now,
				Error:      err.Error(),
				Spec:       ReplaySpec{CheckpointID: cp},
				Test:       ReplayTestRun{Status: replayTestStatusSkipped},
			})
			continue
		}
		for _, agentName := range agents {
			if replayRunnerFor(agentName) == nil {
				now := time.Now().UTC()
				eval.Runs = append(eval.Runs, ReplayRun{
					ID:         newReplayID(),
					Spec:       spec,
					Agent:      agentName,
					Model:      opts.Model,
					Status:     replayStatusSkipped,
					StartedAt:  now,
					FinishedAt: now,
					Test:       ReplayTestRun{Status: replayTestStatusSkipped},
					Error:      fmt.Sprintf("agent %q is not launchable for replay yet", agentName),
				})
				continue
			}
			run, err := executeReplay(ctx, spec, replayCheckpointOptions{
				Agent:        agentName,
				Model:        opts.Model,
				TestCommand:  opts.TestCommand,
				KeepWorktree: opts.KeepWorktrees,
				JSON:         opts.JSON,
				Timeout:      opts.Timeout,
			})
			if err != nil {
				now := time.Now().UTC()
				run = &ReplayRun{
					ID:         newReplayID(),
					Spec:       spec,
					Agent:      agentName,
					Model:      opts.Model,
					Status:     replayStatusFailed,
					StartedAt:  now,
					FinishedAt: now,
					Test:       ReplayTestRun{Status: replayTestStatusSkipped},
					Error:      err.Error(),
				}
			}
			eval.Runs = append(eval.Runs, *run)
		}
	}
	sortReplayRuns(eval.Runs)
	eval.Summaries = summarizeReplayEvalAgents(eval.Runs)
	eval.FinishedAt = time.Now().UTC()
	path, err := saveReplayEval(ctx, eval)
	if err != nil {
		return nil, err
	}
	eval.ResultPath = path
	return eval, nil
}

func buildReplaySpec(ctx context.Context, checkpointRef string) (ReplaySpec, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return ReplaySpec{}, errors.New("not a git repository")
	}
	fullID, targetCommit, err := resolveReplayCheckpointCommit(ctx, repoRoot, checkpointRef)
	if err != nil {
		return ReplaySpec{}, err
	}
	baseCommit, err := replayCommitParent(ctx, repoRoot, targetCommit)
	if err != nil {
		return ReplaySpec{}, err
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return ReplaySpec{}, fmt.Errorf("open repository: %w", err)
	}
	defer repo.Close()
	store := checkpoint.NewGitStore(repo)
	store.SetBlobFetcher(FetchBlobsByHash)

	cpID, err := checkpointid.NewCheckpointID(fullID)
	if err != nil {
		return ReplaySpec{}, fmt.Errorf("parse checkpoint id %s: %w", fullID, err)
	}
	summary, err := checkpoint.ReadCommittedCheckpoint(ctx, store, cpID)
	if err != nil {
		return ReplaySpec{}, fmt.Errorf("read checkpoint %s: %w", fullID, err)
	}
	content, err := checkpoint.ReadLatestSessionContent(ctx, store, cpID, summary)
	if err != nil {
		return ReplaySpec{}, fmt.Errorf("read checkpoint prompt %s: %w", fullID, err)
	}
	prompt := strings.TrimSpace(content.Prompts)
	if prompt == "" && content.Metadata.ReviewPrompt != "" {
		prompt = strings.TrimSpace(content.Metadata.ReviewPrompt)
	}
	if prompt == "" {
		prompt = replayPromptFromTranscript(content.Transcript, content.Metadata.Agent)
	}
	if prompt == "" && content.Metadata.Summary != nil {
		prompt = strings.TrimSpace(content.Metadata.Summary.Intent)
	}
	if prompt == "" {
		return ReplaySpec{}, fmt.Errorf("checkpoint %s has no replayable prompt", fullID)
	}

	files := normalizeReplayPaths(summary.FilesTouched)
	if len(files) == 0 {
		files = normalizeReplayPaths(content.Metadata.FilesTouched)
	}
	if len(files) == 0 {
		files, err = replayFilesChangedBetween(ctx, repoRoot, baseCommit, targetCommit)
		if err != nil {
			return ReplaySpec{}, fmt.Errorf("resolve checkpoint changed files: %w", err)
		}
	}
	return ReplaySpec{
		CheckpointID:  fullID,
		SessionID:     content.Metadata.SessionID,
		Prompt:        prompt,
		TargetCommit:  targetCommit,
		BaseCommit:    baseCommit,
		FilesTouched:  files,
		OriginalAgent: string(content.Metadata.Agent),
		OriginalModel: content.Metadata.Model,
		TokenUsage:    content.Metadata.TokenUsage,
	}, nil
}

func replayPromptFromTranscript(transcript []byte, agentType agenttypes.AgentType) string {
	prompts := extractPromptsFromTranscript(transcript, agentType)
	if len(prompts) == 0 {
		return ""
	}
	return strings.TrimSpace(strings.Join(prompts, "\n\n"))
}

func executeReplay(ctx context.Context, spec ReplaySpec, opts replayCheckpointOptions) (*ReplayRun, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, errors.New("not a git repository")
	}
	runner := replayRunnerFor(opts.Agent)
	if runner == nil {
		return nil, fmt.Errorf("agent %q is not launchable for replay yet", opts.Agent)
	}
	runCtx := ctx
	cancel := func() {}
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	run := &ReplayRun{
		ID:        newReplayID(),
		Spec:      spec,
		Agent:     runner.Name(),
		Model:     opts.Model,
		Status:    replayStatusRunning,
		StartedAt: time.Now().UTC(),
		Test:      ReplayTestRun{Status: replayTestStatusSkipped},
	}

	worktree, err := createReplayWorktree(runCtx, repoRoot, spec.BaseCommit)
	if err != nil {
		return nil, err
	}

	result, runnerErr := runner.Run(runCtx, ReplayRunnerRequest{
		Spec:         spec,
		Agent:        runner.Name(),
		Model:        opts.Model,
		Prompt:       replayPrompt(spec),
		WorktreePath: worktree,
	})
	run.Output = truncateReplayOutput(result.Output)
	run.TokenUsage = result.TokenUsage
	run.Warnings = append(run.Warnings, result.Warnings...)
	if runnerErr != nil {
		run.Status = replayStatusFailed
		run.Error = runnerErr.Error()
	} else {
		run.Status = replayStatusPassed
	}

	if opts.TestCommand != "" {
		run.Test = runReplayTestCommand(runCtx, worktree, opts.TestCommand)
		if run.Test.Status == replayStatusFailed && run.Status == replayStatusPassed {
			run.Status = replayStatusFailed
		}
	}
	files, diff, diffErr := replayChangedFilesAndDiff(runCtx, worktree, spec.BaseCommit)
	if diffErr != nil {
		run.Warnings = append(run.Warnings, fmt.Sprintf("failed to read replay diff: %v", diffErr))
	} else {
		run.ChangedFiles = files
		run.Diff = diff
	}
	run.Metrics = replayMetrics(runCtx, repoRoot, worktree, spec, run.ChangedFiles)

	run.FinishedAt = time.Now().UTC()
	run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	if opts.KeepWorktree {
		run.WorktreePath = worktree
	} else if err := removeReplayWorktree(context.Background(), repoRoot, worktree); err != nil {
		run.Warnings = append(run.Warnings, fmt.Sprintf("failed to remove replay worktree: %v", err))
	}
	path, err := saveReplayRun(ctx, run)
	if err != nil {
		return nil, err
	}
	run.ResultPath = path
	return run, nil
}

func defaultReplayRunnerFor(agentName string) *replayRunnerFunc {
	switch agentName {
	case string(agent.AgentNameClaudeCode):
		return &replayRunnerFunc{name: agentName, fn: runClaudeReplay}
	case string(agent.AgentNameCodex):
		return &replayRunnerFunc{name: agentName, fn: runCodexReplay}
	case string(agent.AgentNameGemini), replayAgentGeminiCLI:
		return &replayRunnerFunc{name: string(agent.AgentNameGemini), fn: runGeminiReplay}
	default:
		return nil
	}
}

func runClaudeReplay(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
	args := []string{"-p", req.Prompt, "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return runReplayProcess(ctx, req.WorktreePath, "claude", args, nil)
}

func runCodexReplay(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
	args := []string{"exec", "--skip-git-repo-check", "--json", "--sandbox", "workspace-write"}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	args = append(args, "-")
	return runReplayProcess(ctx, req.WorktreePath, "codex", args, strings.NewReader(req.Prompt))
}

func runGeminiReplay(ctx context.Context, req ReplayRunnerRequest) (ReplayRunnerResult, error) {
	args := []string{"-p", " "}
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	return runReplayProcess(ctx, req.WorktreePath, "gemini", args, strings.NewReader(req.Prompt))
}

func runReplayProcess(ctx context.Context, dir, name string, args []string, stdin io.Reader) (ReplayRunnerResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = replayAgentEnv(os.Environ())
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	stdoutText := stdout.String()
	output := strings.TrimSpace(stdoutText)
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += strings.TrimSpace(stderr.String())
	}
	if err != nil {
		return ReplayRunnerResult{Output: output, TokenUsage: extractReplayTokenUsage(stdoutText)}, fmt.Errorf("%s replay failed: %w", name, err)
	}
	return ReplayRunnerResult{Output: output, TokenUsage: extractReplayTokenUsage(stdoutText)}, nil
}

func replayAgentEnv(env []string) []string {
	filtered := agent.StripGitEnv(env)
	out := filtered[:0]
	for _, item := range filtered {
		switch {
		case item == "GIT_CONFIG_COUNT" || strings.HasPrefix(item, "GIT_CONFIG_COUNT="):
			continue
		case strings.HasPrefix(item, "GIT_CONFIG_KEY_"):
			continue
		case strings.HasPrefix(item, "GIT_CONFIG_VALUE_"):
			continue
		}
		out = append(out, item)
	}
	return append(out,
		"ENTIRE_REPLAY=1",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=/dev/null",
	)
}

func extractReplayTokenUsage(output string) *agent.TokenUsage {
	var usage *agent.TokenUsage
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		eventType, ok := env["type"].(string)
		if !ok {
			continue
		}
		switch eventType {
		case "result", "turn.completed":
			if parsed := replayTokenUsageFromAny(env["usage"]); parsed != nil {
				usage = parsed
			}
		}
	}
	return usage
}

func replayTokenUsageFromAny(value any) *agent.TokenUsage {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	input := replayIntField(raw, "input_tokens")
	cacheCreate := replayIntField(raw, "cache_creation_input_tokens", "cache_creation_tokens")
	cacheRead := replayIntField(raw, "cache_read_input_tokens", "cached_input_tokens", "cache_read_tokens")
	output := replayIntField(raw, "output_tokens")
	if input == 0 && cacheCreate == 0 && cacheRead == 0 && output == 0 {
		return nil
	}
	return &agent.TokenUsage{
		InputTokens:         input,
		CacheCreationTokens: cacheCreate,
		CacheReadTokens:     cacheRead,
		OutputTokens:        output,
		APICallCount:        1,
	}
}

func replayIntField(raw map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := raw[key].(type) {
		case float64:
			return int(value)
		case int:
			return value
		case json.Number:
			if n, err := value.Int64(); err == nil {
				return int(n)
			}
		}
	}
	return 0
}

func replayPrompt(spec ReplaySpec) string {
	return strings.TrimSpace(fmt.Sprintf(`You are replaying a historical coding task in an isolated git worktree.

Original user prompt:
%s

Complete the task normally in this worktree. Do not inspect Entire checkpoint metadata, git history, or the original target commit to find the previous answer. Make the necessary code changes and stop when done. Do not commit unless the original prompt explicitly asks for a commit.`, spec.Prompt))
}

func resolveReplayCheckpointCommit(ctx context.Context, repoRoot, checkpointRef string) (string, string, error) {
	out, err := replayGit(ctx, repoRoot, "rev-list", "--all")
	if err != nil {
		return "", "", fmt.Errorf("list commits: %w", err)
	}
	type match struct {
		cpID string
		sha  string
	}
	var matches []match
	seen := map[string]struct{}{}
	for _, sha := range strings.Fields(out) {
		msg, msgErr := replayGit(ctx, repoRoot, "show", "-s", "--format=%B", sha)
		if msgErr != nil {
			continue
		}
		for _, cpID := range trailers.ParseAllCheckpoints(msg) {
			if strings.HasPrefix(cpID.String(), checkpointRef) {
				key := cpID.String() + ":" + sha
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				matches = append(matches, match{cpID: cpID.String(), sha: sha})
			}
		}
	}
	if len(matches) == 0 {
		return "", "", fmt.Errorf("checkpoint %s was not found in commit trailers", checkpointRef)
	}
	if len(matches) > 1 {
		var labels []string
		for _, m := range matches {
			labels = append(labels, m.cpID+"@"+shortReplaySHA(m.sha))
		}
		sort.Strings(labels)
		return "", "", fmt.Errorf("checkpoint %s is ambiguous: %s", checkpointRef, strings.Join(labels, ", "))
	}
	return matches[0].cpID, matches[0].sha, nil
}

func replayCommitParent(ctx context.Context, repoRoot, targetCommit string) (string, error) {
	parent, err := replayGit(ctx, repoRoot, "rev-parse", "--verify", targetCommit+"^")
	if err != nil {
		return "", fmt.Errorf("checkpoint target commit %s has no parent; replay needs a committed base", shortReplaySHA(targetCommit))
	}
	return parent, nil
}

func createReplayWorktree(ctx context.Context, repoRoot, baseCommit string) (string, error) {
	dir, err := os.MkdirTemp("", "entire-replay-*")
	if err != nil {
		return "", fmt.Errorf("create replay temp dir: %w", err)
	}
	if err := os.Remove(dir); err != nil {
		return "", fmt.Errorf("prepare replay temp dir: %w", err)
	}
	if _, err := replayGit(ctx, repoRoot, "worktree", "add", "--detach", dir, baseCommit); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("create replay worktree: %w", err)
	}
	return dir, nil
}

func removeReplayWorktree(ctx context.Context, repoRoot, worktree string) error {
	if _, err := replayGit(ctx, repoRoot, "worktree", "remove", "--force", worktree); err != nil {
		_ = os.RemoveAll(worktree)
		return err
	}
	return nil
}

func replayChangedFilesAndDiff(ctx context.Context, worktree, baseCommit string) ([]string, string, error) {
	if _, err := replayGit(ctx, worktree, "add", "-N", "."); err != nil {
		return nil, "", fmt.Errorf("index replay untracked files: %w", err)
	}
	modified, err := replayGit(ctx, worktree, "diff", "--name-only", baseCommit)
	if err != nil {
		return nil, "", err
	}
	files := normalizeReplayPaths(strings.Fields(modified))
	diff, err := replayGit(ctx, worktree, "diff", "--binary", baseCommit)
	if err != nil {
		return files, "", err
	}
	return files, diff, nil
}

func replayFilesChangedBetween(ctx context.Context, repoRoot, baseCommit, targetCommit string) ([]string, error) {
	out, err := replayGit(ctx, repoRoot, "diff", "--name-only", baseCommit, targetCommit, "--")
	if err != nil {
		return nil, err
	}
	return normalizeReplayPaths(strings.Fields(out)), nil
}

func runReplayTestCommand(ctx context.Context, worktree, command string) ReplayTestRun {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = worktree
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	result := ReplayTestRun{
		Status:     replayStatusPassed,
		Command:    command,
		Output:     truncateReplayOutput(output.String()),
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		result.Status = replayStatusFailed
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
	}
	return result
}

func replayMetrics(ctx context.Context, repoRoot, worktree string, spec ReplaySpec, changedFiles []string) ReplayMetrics {
	original := normalizeReplayPaths(spec.FilesTouched)
	produced := normalizeReplayPaths(changedFiles)
	overlap, missing, extra := replayFileSets(original, produced)
	metrics := ReplayMetrics{
		FileOverlap:   len(overlap),
		MissingFiles:  missing,
		ExtraFiles:    extra,
		RiskyFiles:    riskyReplayFiles(produced),
		FilePrecision: percent(len(overlap), len(produced)),
		FileRecall:    percent(len(overlap), len(original)),
	}
	metrics.RiskScore = len(metrics.ExtraFiles) + len(metrics.RiskyFiles)
	metrics.MissingTests = sourceChangedWithoutTests(produced)
	if metrics.MissingTests {
		metrics.RiskScore++
	}
	if score, ok := replaySemanticSimilarity(ctx, repoRoot, worktree, spec); ok {
		metrics.SemanticAvailable = true
		metrics.SemanticSimilarity = score
	}
	return metrics
}

func replaySemanticSimilarity(ctx context.Context, repoRoot, worktree string, spec ReplaySpec) (int, bool) {
	if _, err := exec.LookPath("entire-sem"); err != nil {
		return 0, false
	}
	gold, err := replaySemanticKeys(ctx, repoRoot, spec.BaseCommit, spec.TargetCommit)
	if err != nil {
		return 0, false
	}
	replayHead, cleanup, err := commitReplayResultForSemantic(ctx, worktree)
	if err != nil {
		return 0, false
	}
	replayed, err := replaySemanticKeys(ctx, worktree, spec.BaseCommit, replayHead)
	if err != nil {
		cleanupReplaySemanticCommit(cleanup)
		return 0, false
	}
	score := jaccardPercent(gold, replayed)
	if !cleanupReplaySemanticCommit(cleanup) {
		return 0, false
	}
	return score, true
}

func cleanupReplaySemanticCommit(cleanup func() error) bool {
	return cleanup() == nil
}

func commitReplayResultForSemantic(ctx context.Context, worktree string) (string, func() error, error) {
	if _, err := replayGit(ctx, worktree, "diff", "--quiet"); err == nil {
		head, headErr := replayGit(ctx, worktree, "rev-parse", "HEAD")
		if headErr != nil {
			return "", func() error { return nil }, headErr
		}
		return head, func() error { return nil }, nil
	}
	if _, err := replayGit(ctx, worktree, "add", "-A"); err != nil {
		return "", func() error { return nil }, err
	}
	if _, err := replayGit(ctx, worktree,
		"-c", "user.name=Entire Replay",
		"-c", "user.email=replay@entire.local",
		"commit", "--no-gpg-sign", "-m", "entire replay result",
	); err != nil {
		return "", func() error { return nil }, err
	}
	head, err := replayGit(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", func() error { return nil }, err
	}
	cleanup := func() error {
		if _, err := replayGit(context.Background(), worktree, "reset", "--mixed", "HEAD^"); err != nil {
			return fmt.Errorf("reset temporary semantic replay commit: %w", err)
		}
		return nil
	}
	return head, cleanup, nil
}

func replaySemanticKeys(ctx context.Context, dir, base, head string) (map[string]struct{}, error) {
	cmd := exec.CommandContext(ctx, "entire-sem", "diff", "--base", base, "--head", head, "--json")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return nil, fmt.Errorf("entire-sem: %s", msg)
		}
		return nil, fmt.Errorf("run entire-sem: %w", err)
	}
	var raw any
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse entire-sem json: %w", err)
	}
	keys := map[string]struct{}{}
	collectReplaySemanticKeys(raw, keys)
	return keys, nil
}

func collectReplaySemanticKeys(value any, keys map[string]struct{}) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			collectReplaySemanticKeys(item, keys)
		}
	case map[string]any:
		name := replayStringField(v, "name", "symbol", "new_name")
		kind := replayStringField(v, "kind", "entity_kind", "node_kind")
		change := replayStringField(v, "change_type", "change", "type", "status")
		if name != "" || change != "" {
			keys[strings.Join([]string{kind, name, change}, ":")] = struct{}{}
		}
		for _, child := range v {
			collectReplaySemanticKeys(child, keys)
		}
	}
}

func replayStringField(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func saveReplayRun(ctx context.Context, run *ReplayRun) (string, error) {
	dir, err := replayRunsDir(ctx)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, run.ID+".json")
	run.ResultPath = path
	return path, writeReplayFile(path, run)
}

func readReplayRun(ctx context.Context, runID string) (*ReplayRun, error) {
	dir, err := replayRunsDir(ctx)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(runID), ".json")
	path := filepath.Join(dir, name+".json")
	data, err := os.ReadFile(path) //nolint:gosec // runID is filepath.Base'd above
	if err != nil {
		return nil, fmt.Errorf("read replay report: %w", err)
	}
	var run ReplayRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("parse replay report: %w", err)
	}
	run.ResultPath = path
	return &run, nil
}

func saveReplayEval(ctx context.Context, run *ReplayEvalRun) (string, error) {
	dir, err := replayEvalsDir(ctx)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, run.ID+".json")
	run.ResultPath = path
	return path, writeReplayFile(path, run)
}

func readReplayEval(ctx context.Context, runID string) (*ReplayEvalRun, error) {
	dir, err := replayEvalsDir(ctx)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSuffix(filepath.Base(runID), ".json")
	path := filepath.Join(dir, name+".json")
	data, err := os.ReadFile(path) //nolint:gosec // runID is filepath.Base'd above
	if err != nil {
		return nil, fmt.Errorf("read eval report: %w", err)
	}
	var run ReplayEvalRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("parse eval report: %w", err)
	}
	run.ResultPath = path
	return &run, nil
}

func replayRunsDir(ctx context.Context) (string, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, "entire-replay", "runs"), nil
}

func replayEvalsDir(ctx context.Context) (string, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	return filepath.Join(commonDir, "entire-replay", "evals"), nil
}

func writeReplayFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create replay result dir: %w", err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal replay result: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write replay result: %w", err)
	}
	return nil
}

func recentReplayCheckpoints(ctx context.Context, limit int) ([]string, error) {
	repo, err := openRepository(ctx)
	if err != nil {
		return nil, err
	}
	defer repo.Close()
	infos, err := checkpoint.NewGitStore(repo).ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("list committed checkpoints: %w", err)
	}
	if limit <= 0 || limit > len(infos) {
		limit = len(infos)
	}
	out := make([]string, 0, limit)
	for i := range limit {
		out = append(out, infos[i].CheckpointID.String())
	}
	return out, nil
}

func replayFileSets(original, produced []string) (overlap, missing, extra []string) {
	origSet := make(map[string]struct{}, len(original))
	prodSet := make(map[string]struct{}, len(produced))
	for _, file := range original {
		origSet[file] = struct{}{}
	}
	for _, file := range produced {
		prodSet[file] = struct{}{}
		if _, ok := origSet[file]; ok {
			overlap = append(overlap, file)
		} else {
			extra = append(extra, file)
		}
	}
	for _, file := range original {
		if _, ok := prodSet[file]; !ok {
			missing = append(missing, file)
		}
	}
	sort.Strings(overlap)
	sort.Strings(missing)
	sort.Strings(extra)
	return overlap, missing, extra
}

func riskyReplayFiles(files []string) []string {
	var risky []string
	for _, file := range files {
		lower := strings.ToLower(file)
		if strings.Contains(lower, "auth") ||
			strings.Contains(lower, "token") ||
			strings.Contains(lower, "secret") ||
			strings.Contains(lower, "credential") ||
			strings.Contains(lower, "permission") ||
			strings.Contains(lower, "payment") ||
			strings.Contains(lower, "billing") ||
			strings.Contains(lower, "/db/") ||
			strings.Contains(lower, "database") ||
			strings.Contains(lower, "migration") ||
			strings.Contains(lower, "schema") ||
			strings.Contains(lower, "policy") ||
			strings.HasSuffix(lower, ".sql") ||
			strings.Contains(lower, "config") {
			risky = append(risky, file)
		}
	}
	sort.Strings(risky)
	return risky
}

func sourceChangedWithoutTests(files []string) bool {
	hasSource := false
	hasTest := false
	for _, file := range files {
		lower := strings.ToLower(file)
		if strings.Contains(lower, "test") || strings.Contains(lower, "spec") {
			hasTest = true
			continue
		}
		switch {
		case strings.HasSuffix(lower, ".go"),
			strings.HasSuffix(lower, ".py"),
			strings.HasSuffix(lower, ".js"),
			strings.HasSuffix(lower, ".ts"),
			strings.HasSuffix(lower, ".tsx"),
			strings.HasSuffix(lower, ".rs"):
			hasSource = true
		}
	}
	return hasSource && !hasTest
}

func sortReplayRuns(runs []ReplayRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i], runs[j]
		if a.Status != b.Status {
			return a.Status == replayStatusPassed
		}
		if a.Test.Status != b.Test.Status {
			return a.Test.Status == replayStatusPassed
		}
		if a.Metrics.FileRecall != b.Metrics.FileRecall {
			return a.Metrics.FileRecall > b.Metrics.FileRecall
		}
		if a.Metrics.FilePrecision != b.Metrics.FilePrecision {
			return a.Metrics.FilePrecision > b.Metrics.FilePrecision
		}
		if a.Metrics.RiskScore != b.Metrics.RiskScore {
			return a.Metrics.RiskScore < b.Metrics.RiskScore
		}
		return a.DurationMS < b.DurationMS
	})
}

func summarizeReplayEvalAgents(runs []ReplayRun) []ReplayEvalAgentSummary {
	type totals struct {
		summary      ReplayEvalAgentSummary
		recall       int
		precision    int
		semantic     int
		duration     int64
		durationRuns int
		qualityRuns  int
	}
	byAgent := make(map[string]*totals)
	for _, run := range runs {
		if strings.TrimSpace(run.Agent) == "" {
			continue
		}
		total := byAgent[run.Agent]
		if total == nil {
			total = &totals{summary: ReplayEvalAgentSummary{Agent: run.Agent}}
			byAgent[run.Agent] = total
		}
		total.summary.Runs++
		switch run.Status {
		case replayStatusPassed:
			total.summary.Passed++
		case replayStatusSkipped:
			total.summary.Skipped++
		default:
			total.summary.Failed++
		}
		total.qualityRuns++
		total.recall += run.Metrics.FileRecall
		total.precision += run.Metrics.FilePrecision
		if run.Metrics.SemanticAvailable {
			total.summary.SemanticRuns++
			total.semantic += run.Metrics.SemanticSimilarity
		}
		if run.DurationMS > 0 {
			total.durationRuns++
			total.duration += run.DurationMS
		}
		total.summary.RiskScore += run.Metrics.RiskScore
		if run.TokenUsage != nil {
			total.summary.InputTokens += run.TokenUsage.InputTokens + run.TokenUsage.CacheCreationTokens + run.TokenUsage.CacheReadTokens
			total.summary.OutputTokens += run.TokenUsage.OutputTokens
		}
	}

	summaries := make([]ReplayEvalAgentSummary, 0, len(byAgent))
	for _, total := range byAgent {
		summary := total.summary
		summary.PassRate = percent(summary.Passed, summary.Runs)
		if total.qualityRuns > 0 {
			summary.AvgFileRecall = total.recall / total.qualityRuns
			summary.AvgFilePrecision = total.precision / total.qualityRuns
		}
		if summary.SemanticRuns > 0 {
			summary.AvgSemanticSimilarity = total.semantic / summary.SemanticRuns
		}
		if total.durationRuns > 0 {
			summary.AvgDurationMS = total.duration / int64(total.durationRuns)
		}
		summaries = append(summaries, summary)
	}
	sortReplayEvalSummaries(summaries)
	return summaries
}

func sortReplayEvalSummaries(summaries []ReplayEvalAgentSummary) {
	sort.SliceStable(summaries, func(i, j int) bool {
		a, b := summaries[i], summaries[j]
		if a.PassRate != b.PassRate {
			return a.PassRate > b.PassRate
		}
		if a.AvgFileRecall != b.AvgFileRecall {
			return a.AvgFileRecall > b.AvgFileRecall
		}
		if a.AvgFilePrecision != b.AvgFilePrecision {
			return a.AvgFilePrecision > b.AvgFilePrecision
		}
		if a.AvgSemanticSimilarity != b.AvgSemanticSimilarity {
			return a.AvgSemanticSimilarity > b.AvgSemanticSimilarity
		}
		if a.RiskScore != b.RiskScore {
			return a.RiskScore < b.RiskScore
		}
		if a.AvgDurationMS != b.AvgDurationMS {
			return a.AvgDurationMS < b.AvgDurationMS
		}
		return a.Agent < b.Agent
	})
}

func renderReplayRun(w io.Writer, run *ReplayRun) {
	sty := newStatusStyles(w)
	fmt.Fprintf(w, "\n  %s %s\n", sty.render(sty.bold, "Replay"), sty.render(sty.cyan, run.ID))
	fmt.Fprintf(w, "  checkpoint %s %s agent %s %s status %s\n\n",
		sty.render(sty.cyan, run.Spec.CheckpointID),
		sty.render(sty.dim, "·"),
		run.Agent,
		sty.render(sty.dim, "·"),
		renderReplayStatus(sty, run.Status),
	)
	fmt.Fprintf(w, "  %s %s..%s\n", sty.render(sty.bold, "Range:"), shortReplaySHA(run.Spec.BaseCommit), shortReplaySHA(run.Spec.TargetCommit))
	fmt.Fprintf(w, "  %s %s\n", sty.render(sty.bold, "Files:"), replayFileMetricText(run.Metrics))
	if run.Test.Status != replayStatusSkipped {
		fmt.Fprintf(w, "  %s %s", sty.render(sty.bold, "Tests:"), renderReplayStatus(sty, run.Test.Status))
		if run.Test.Command != "" {
			fmt.Fprintf(w, " %s %s", sty.render(sty.dim, "·"), run.Test.Command)
		}
		fmt.Fprintln(w)
	}
	if run.TokenUsage != nil {
		fmt.Fprintf(w, "  %s %s\n", sty.render(sty.bold, "Tokens:"), replayTokenUsageText(run.TokenUsage))
	}
	if run.Metrics.SemanticAvailable {
		fmt.Fprintf(w, "  %s %d%% semantic match\n", sty.render(sty.bold, "Semantic:"), run.Metrics.SemanticSimilarity)
	}
	if run.Metrics.RiskScore > 0 {
		fmt.Fprintf(w, "  %s %s\n", sty.render(sty.bold, "Risk:"), replayRiskText(run.Metrics))
	}
	if run.WorktreePath != "" {
		fmt.Fprintf(w, "  %s %s\n", sty.render(sty.bold, "Worktree:"), run.WorktreePath)
	}
	if run.ResultPath != "" {
		fmt.Fprintf(w, "  %s %s\n", sty.render(sty.dim, "Saved:"), run.ResultPath)
	}
	if run.Error != "" {
		fmt.Fprintf(w, "\n  %s %s\n", sty.render(sty.red, "Error:"), run.Error)
	}
	if len(run.Warnings) > 0 {
		fmt.Fprintf(w, "\n  %s\n", sty.render(sty.dim, "Warnings:"))
		for _, warning := range run.Warnings {
			fmt.Fprintf(w, "  - %s\n", sty.render(sty.dim, warning))
		}
	}
	fmt.Fprintln(w)
}

func renderReplayEval(w io.Writer, eval *ReplayEvalRun) {
	sty := newStatusStyles(w)
	fmt.Fprintf(w, "\n  %s %s\n\n", sty.render(sty.bold, "Replay Eval"), sty.render(sty.cyan, eval.ID))
	if len(eval.Runs) == 0 {
		fmt.Fprintf(w, "  %s\n\n", sty.render(sty.dim, "No runs recorded."))
		return
	}
	if len(eval.Summaries) > 0 {
		fmt.Fprintf(w, "  %s\n", sty.render(sty.bold, "Agent Ranking"))
		fmt.Fprintf(w, "  %-18s  %-4s  %-5s  %-7s  %-7s  %-5s  %-8s  %s\n", "Agent", "Runs", "Pass", "Recall", "Prec.", "Risk", "Duration", "Tokens")
		fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", 88)))
		for _, summary := range eval.Summaries {
			fmt.Fprintf(w, "  %-18s  %4d  %4d%%  %6d%%  %6d%%  %5d  %8s  %s\n",
				stringutil.TruncateRunes(summary.Agent, 18, ""),
				summary.Runs,
				summary.PassRate,
				summary.AvgFileRecall,
				summary.AvgFilePrecision,
				summary.RiskScore,
				formatReplayDuration(summary.AvgDurationMS),
				replayEvalTokenText(summary),
			)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %s\n", sty.render(sty.bold, "Runs"))
	fmt.Fprintf(w, "  %-12s  %-18s  %-8s  %-7s  %-7s  %-5s  %s\n", "Checkpoint", "Agent", "Status", "Recall", "Prec.", "Risk", "Tests")
	fmt.Fprintf(w, "  %s\n", sty.render(sty.dim, strings.Repeat("─", 82)))
	for _, run := range eval.Runs {
		fmt.Fprintf(w, "  %-12s  %-18s  %-8s  %6d%%  %6d%%  %5d  %s\n",
			run.Spec.CheckpointID,
			stringutil.TruncateRunes(run.Agent, 18, ""),
			run.Status,
			run.Metrics.FileRecall,
			run.Metrics.FilePrecision,
			run.Metrics.RiskScore,
			run.Test.Status,
		)
	}
	if eval.ResultPath != "" {
		fmt.Fprintf(w, "\n  %s %s\n", sty.render(sty.dim, "Saved:"), eval.ResultPath)
	}
	fmt.Fprintln(w)
}

func renderReplayStatus(sty statusStyles, status string) string {
	switch status {
	case replayStatusPassed:
		return sty.render(sty.green, status)
	case replayStatusFailed:
		return sty.render(sty.red, status)
	case replayStatusSkipped:
		return sty.render(sty.dim, status)
	default:
		return sty.render(sty.yellow, status)
	}
}

func replayFileMetricText(metrics ReplayMetrics) string {
	return fmt.Sprintf("%d%% recall, %d%% precision (%d overlap, %d missing, %d extra)",
		metrics.FileRecall,
		metrics.FilePrecision,
		metrics.FileOverlap,
		len(metrics.MissingFiles),
		len(metrics.ExtraFiles),
	)
}

func replayRiskText(metrics ReplayMetrics) string {
	var details []string
	if len(metrics.ExtraFiles) > 0 {
		details = append(details, fmt.Sprintf("%d extra", len(metrics.ExtraFiles)))
	}
	if len(metrics.RiskyFiles) > 0 {
		details = append(details, fmt.Sprintf("%d risky", len(metrics.RiskyFiles)))
	}
	if metrics.MissingTests {
		details = append(details, "missing tests")
	}
	if len(details) == 0 {
		return fmt.Sprintf("risk score %d", metrics.RiskScore)
	}
	return fmt.Sprintf("risk score %d (%s)", metrics.RiskScore, strings.Join(details, ", "))
}

func replayTokenUsageText(usage *agent.TokenUsage) string {
	if usage == nil {
		return ""
	}
	input := usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens
	return fmt.Sprintf("%d in, %d out", input, usage.OutputTokens)
}

func replayEvalTokenText(summary ReplayEvalAgentSummary) string {
	if summary.InputTokens == 0 && summary.OutputTokens == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", summary.InputTokens, summary.OutputTokens)
}

func formatReplayDuration(ms int64) string {
	switch {
	case ms <= 0:
		return "-"
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	default:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
}

func replayGit(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("git %s: %w (stderr: %s)", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func normalizeReplayPaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		normalized := filepath.ToSlash(strings.Trim(strings.TrimSpace(p), "/"))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func uniqueNonEmpty(values []string) []string {
	var out []string
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func percent(numerator, denominator int) int {
	if denominator == 0 {
		if numerator == 0 {
			return 100
		}
		return 0
	}
	return numerator * 100 / denominator
}

func jaccardPercent(a, b map[string]struct{}) int {
	if len(a) == 0 && len(b) == 0 {
		return 100
	}
	intersection := 0
	union := make(map[string]struct{}, len(a)+len(b))
	for key := range a {
		union[key] = struct{}{}
		if _, ok := b[key]; ok {
			intersection++
		}
	}
	for key := range b {
		union[key] = struct{}{}
	}
	return percent(intersection, len(union))
}

func truncateReplayOutput(output string) string {
	output = strings.TrimSpace(output)
	if len(output) <= replayResultOutputLimit {
		return output
	}
	return output[:replayResultOutputLimit] + "\n...[truncated]"
}

func shortReplaySHA(sha string) string {
	if len(sha) <= 8 {
		return sha
	}
	return sha[:8]
}

func newReplayID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(b[:])
}

func writeReplayJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}
