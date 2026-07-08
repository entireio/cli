package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/spf13/cobra"
)

type tokensProfileReport struct {
	Source                   string                        `json:"source"`
	UsageScope               string                        `json:"usage_scope"`
	CheckpointsAvailable     int                           `json:"checkpoints_available"`
	CheckpointsAnalyzed      int                           `json:"checkpoints_analyzed"`
	CheckpointsWithTokenData int                           `json:"checkpoints_with_token_data"`
	MissingTokenData         int                           `json:"missing_token_data"`
	MetadataReadWarnings     int                           `json:"metadata_read_warnings,omitempty"`
	Tokens                   *sessionTokensUsage           `json:"tokens,omitempty"`
	Signals                  []tokensProfileSignal         `json:"signals,omitempty"`
	Recommendations          []sessionTokensRecommendation `json:"recommendations,omitempty"`
	Limitations              []string                      `json:"limitations,omitempty"`
}

type tokensProfileSignal struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Count         int      `json:"count"`
	Percent       int      `json:"percent"`
	CheckpointIDs []string `json:"checkpoint_ids,omitempty"`
}

type tokensProfileSignalDefinition struct {
	id    string
	label string
}

var tokensProfileSignalDefinitions = []tokensProfileSignalDefinition{
	{id: "context-replay-hotspot", label: "Cache/context replay hotspot"},
	{id: "api-call-amplification", label: "API call amplification"},
	{id: "subagent-heavy", label: "Subagent-heavy sessions"},
	{id: "missing-token-data", label: "Missing token data"},
}

const tokensProfileUsageScopeCheckpointObserved = "checkpoint_observed"

func newTokensGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "tokens",
		Short:  "Analyze token usage across sessions and checkpoints",
		Hidden: true,
		Long: `Analyze token usage across sessions and checkpoints.

Commands:
  profile  Aggregate token usage across committed checkpoints

Examples:
  entire tokens profile
  entire tokens profile --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newTokensProfileCmd())
	return cmd
}

func newTokensProfileCmd() *cobra.Command {
	var jsonFlag bool
	var limitFlag int
	var allFlag bool

	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Aggregate token usage and recommendations across checkpoint history",
		Long: `Aggregate token usage and recommendations across committed checkpoint history.

The profile reads committed checkpoint metadata only. It does not inspect
transcripts or source files, so it is deterministic and avoids adding token
cost while diagnosing token usage. By default it scans the latest 50 committed
checkpoints; use --limit or --all to change the scope.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			limit := limitFlag
			if allFlag {
				limit = 0
			} else if limit <= 0 {
				return errors.New("--limit must be positive unless --all is used")
			}
			return runTokensProfile(cmd.Context(), cmd, jsonFlag, limit)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().IntVar(&limitFlag, "limit", 50, "Maximum committed checkpoints to analyze")
	cmd.Flags().BoolVar(&allFlag, "all", false, "Analyze all committed checkpoints")
	cmd.MarkFlagsMutuallyExclusive("limit", "all")
	return cmd
}

func runTokensProfile(ctx context.Context, cmd *cobra.Command, jsonOutput bool, limit int) error {
	repo, err := openRepository(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository.")
		return NewSilentError(err)
	}
	defer repo.Close()

	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: FetchBlobsByHash, RefFetcher: FetchCheckpointRef})
	if err != nil {
		return fmt.Errorf("failed to open checkpoint stores: %w", err)
	}
	store := stores.Persistent
	infos, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list checkpoints: %w", err)
	}

	report, err := buildTokensProfileReport(ctx, store, infos, limit)
	if err != nil {
		return err
	}

	if jsonOutput {
		return printJSON(cmd.OutOrStdout(), report)
	}
	writeTokensProfileText(cmd.OutOrStdout(), report)
	return nil
}

func buildTokensProfileReport(ctx context.Context, store checkpoint.PersistentStore, infos []checkpoint.CheckpointInfo, limit int) (tokensProfileReport, error) {
	checkpointsAvailable := len(infos)
	infos = limitTokensProfileCheckpoints(infos, limit)
	report := tokensProfileReport{
		Source:               "committed_checkpoints",
		UsageScope:           tokensProfileUsageScopeCheckpointObserved,
		CheckpointsAvailable: checkpointsAvailable,
		CheckpointsAnalyzed:  len(infos),
	}
	signals := make(map[string]*tokensProfileSignal, len(tokensProfileSignalDefinitions))
	var aggregate *agent.TokenUsage

	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return tokensProfileReport{}, err //nolint:wrapcheck // Propagating context cancellation.
		}

		summary, err := store.Read(ctx, info.CheckpointID)
		if err != nil {
			return tokensProfileReport{}, fmt.Errorf("failed to read checkpoint %s: %w", info.CheckpointID, err)
		}
		if summary == nil {
			report.MissingTokenData++
			addTokensProfileSignal(signals, "missing-token-data", info.CheckpointID, report.CheckpointsAnalyzed)
			continue
		}

		usage, metadataReadWarning, err := tokensProfileCheckpointUsage(ctx, store, info.CheckpointID, summary)
		if err != nil {
			return tokensProfileReport{}, err
		}
		if metadataReadWarning {
			report.MetadataReadWarnings++
		}
		tokens := buildSessionTokensUsage(usage)
		if tokens == nil {
			report.MissingTokenData++
			addTokensProfileSignal(signals, "missing-token-data", info.CheckpointID, report.CheckpointsAnalyzed)
			continue
		}

		report.CheckpointsWithTokenData++
		aggregate = addCheckpointTokenUsage(aggregate, usage)
		addTokensProfileTokenSignals(signals, info.CheckpointID, tokens, report.CheckpointsAnalyzed)
	}

	report.Tokens = buildSessionTokensUsage(aggregate)
	report.Signals = orderedTokensProfileSignals(signals)
	report.Recommendations = tokensProfileRecommendations(report)
	report.Limitations = tokensProfileLimitations(report)
	return report, nil
}

func limitTokensProfileCheckpoints(infos []checkpoint.CheckpointInfo, limit int) []checkpoint.CheckpointInfo {
	if limit <= 0 || len(infos) <= limit {
		return infos
	}
	return infos[:limit]
}

func tokensProfileCheckpointUsage(ctx context.Context, store checkpoint.PersistentStore, checkpointID id.CheckpointID, summary *checkpoint.CheckpointSummary) (*agent.TokenUsage, bool, error) {
	if summary == nil {
		return nil, false, nil
	}

	metas := make([]*checkpoint.Metadata, 0, len(summary.Sessions))
	metadataReadWarning := false
	for i := range len(summary.Sessions) {
		meta, err := store.ReadSessionMetadata(ctx, checkpointID, i)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, false, ctxErr //nolint:wrapcheck // Propagating context cancellation.
			}
			metadataReadWarning = true
			continue
		}
		metas = append(metas, meta)
	}
	return checkpointTokenUsage(summary, metas, metadataReadWarning), metadataReadWarning, nil
}

func addTokensProfileTokenSignals(signals map[string]*tokensProfileSignal, checkpointID id.CheckpointID, tokens *sessionTokensUsage, denominator int) {
	if tokens == nil {
		return
	}
	topLevelTotal := topLevelSessionTokenTotal(tokens)
	if topLevelTotal > 0 && tokenPercent(tokens.CacheRead, topLevelTotal) >= recommendationHighCacheReadPercent {
		addTokensProfileSignal(signals, "context-replay-hotspot", checkpointID, denominator)
	}
	if tokens.APICalls >= 20 {
		addTokensProfileSignal(signals, "api-call-amplification", checkpointID, denominator)
	}
	if tokenShareAtLeastOneTenth(tokens.SubagentTotal, tokens.Total) {
		addTokensProfileSignal(signals, "subagent-heavy", checkpointID, denominator)
	}
}

func addTokensProfileSignal(signals map[string]*tokensProfileSignal, signalID string, checkpointID id.CheckpointID, denominator int) {
	signal := signals[signalID]
	if signal == nil {
		definition := tokensProfileSignalDefinitionFor(signalID)
		signal = &tokensProfileSignal{
			ID:    definition.id,
			Label: definition.label,
		}
		signals[signalID] = signal
	}
	signal.Count++
	if denominator > 0 {
		signal.Percent = roundedPercent(signal.Count, denominator)
	}
	if checkpointID != "" {
		signal.CheckpointIDs = append(signal.CheckpointIDs, checkpointID.String())
	}
}

func tokensProfileSignalDefinitionFor(signalID string) tokensProfileSignalDefinition {
	for _, definition := range tokensProfileSignalDefinitions {
		if definition.id == signalID {
			return definition
		}
	}
	return tokensProfileSignalDefinition{id: signalID, label: signalID}
}

func orderedTokensProfileSignals(signals map[string]*tokensProfileSignal) []tokensProfileSignal {
	ordered := make([]tokensProfileSignal, 0, len(signals))
	for _, definition := range tokensProfileSignalDefinitions {
		if signal := signals[definition.id]; signal != nil {
			ordered = append(ordered, *signal)
		}
	}
	return ordered
}

func tokensProfileRecommendations(report tokensProfileReport) []sessionTokensRecommendation {
	var recs []sessionTokensRecommendation

	if report.CheckpointsAnalyzed == 0 {
		return []sessionTokensRecommendation{{
			ID:       "no-checkpoints",
			Severity: "low",
			Message:  "Create checkpoints first; token profiling needs committed checkpoint metadata to identify patterns.",
			Signals:  []string{"empty_checkpoint_history"},
		}}
	}

	if tokensProfileSignalCount(report.Signals, "context-replay-hotspot") > 0 ||
		tokensProfileSignalCount(report.Signals, "api-call-amplification") > 0 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "search-before-reinvestigation",
			Severity: "high",
			Message:  "Use `entire search` for prior decisions/checkpoints before broad re-investigation.",
			Signals:  []string{"cache_read_tokens", "api_call_count"},
		})
	}
	if tokensProfileSignalCount(report.Signals, "api-call-amplification") > 0 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "batch-diagnostics",
			Severity: "medium",
			Message:  "Batch diagnostic reads around one narrowed hypothesis when API call amplification repeats.",
			Signals:  []string{"api_call_count"},
		})
	}
	if tokensProfileSignalCount(report.Signals, "context-replay-hotspot") > 0 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "preserve-then-compact",
			Severity: "medium",
			Message:  "Summarize useful findings before continuing large-context work; compact or restart only after preserving relevant context.",
			Signals:  []string{"cache_read_tokens"},
		})
	}
	if tokensProfileSignalCount(report.Signals, "subagent-heavy") > 0 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "scope-subagents",
			Severity: "medium",
			Message:  "Scope subagent tasks tightly with a narrow objective and expected output.",
			Signals:  []string{"subagent_tokens"},
		})
	}
	if report.MissingTokenData > 0 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "improve-token-coverage",
			Severity: "low",
			Message:  "Increase token coverage by using agents and checkpoints that report token usage.",
			Signals:  []string{"missing_token_usage"},
		})
	}

	if len(recs) == 0 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "no-repeated-hotspots",
			Severity: "low",
			Message:  "No repeated token hotspots were visible in committed checkpoint metadata.",
			Signals:  []string{"checkpoint_token_metadata"},
		})
	}
	return recs
}

func tokensProfileSignalCount(signals []tokensProfileSignal, signalID string) int {
	for _, signal := range signals {
		if signal.ID == signalID {
			return signal.Count
		}
	}
	return 0
}

func tokensProfileLimitations(report tokensProfileReport) []string {
	var limitations []string
	if report.CheckpointsAvailable > report.CheckpointsAnalyzed {
		limitations = append(limitations, fmt.Sprintf("Limited to latest %d of %d committed checkpoints; use --limit or --all to change scope.", report.CheckpointsAnalyzed, report.CheckpointsAvailable))
	}
	if report.CheckpointsAnalyzed == 0 {
		limitations = append(limitations, "No committed checkpoints found.")
	}
	if report.MissingTokenData > 0 {
		limitations = append(limitations, fmt.Sprintf("%d checkpoint%s did not include token usage.", report.MissingTokenData, tokenPluralSuffix(report.MissingTokenData)))
	}
	if report.MetadataReadWarnings > 0 {
		limitations = append(limitations, fmt.Sprintf("%d checkpoint%s had incomplete session metadata; profile used root token summaries or readable sessions where available.", report.MetadataReadWarnings, tokenPluralSuffix(report.MetadataReadWarnings)))
	}
	if report.Tokens != nil {
		limitations = append(limitations, "Token totals are summed from analyzed checkpoints and may include overlapping checkpoint history; treat them as checkpoint-observed volume, not guaranteed unique session spend.")
	}
	if report.CheckpointsAnalyzed > 0 {
		limitations = append(limitations, "Tool-level search/read spend is not captured yet; this profile infers patterns from token totals, cache/context replay, API call counts, and subagent totals.")
	}
	return limitations
}

func writeTokensProfileText(w io.Writer, report tokensProfileReport) {
	fmt.Fprintln(w, "Token profile")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Source:               %s\n", report.Source)
	fmt.Fprintf(w, "Checkpoints available: %d\n", report.CheckpointsAvailable)
	fmt.Fprintf(w, "Checkpoints analyzed: %d\n", report.CheckpointsAnalyzed)
	fmt.Fprintf(w, "With token data:      %d\n", report.CheckpointsWithTokenData)
	fmt.Fprintf(w, "Missing token data:   %d\n", report.MissingTokenData)
	if report.MetadataReadWarnings > 0 {
		fmt.Fprintf(w, "Metadata warnings:    %d\n", report.MetadataReadWarnings)
	}

	writeTokenUsageSectionWithTitle(w, "Checkpoint-observed token usage", report.Tokens)
	writeTokensProfileSignals(w, report.Signals)
	if len(report.Recommendations) > 0 {
		writeTokenRecommendations(w, report.Recommendations)
	}
	writeTokenLimitations(w, report.Limitations)
}

func writeTokensProfileSignals(w io.Writer, signals []tokensProfileSignal) {
	if len(signals) == 0 {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Repeated signals")
	for _, signal := range signals {
		fmt.Fprintf(w, "- %s: %d checkpoint%s", signal.Label, signal.Count, tokenPluralSuffix(signal.Count))
		if signal.Percent > 0 {
			fmt.Fprintf(w, " (%d%%)", signal.Percent)
		}
		fmt.Fprintln(w)
	}
}
