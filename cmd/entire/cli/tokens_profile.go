package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
	"github.com/spf13/cobra"
)

// tokensProfileReport is the `tokens profile` report and its --json document:
// committed checkpoint metadata grouped by agent, with legacy running-total
// rows collapsed per session. No transcripts are read, so there is no
// attribution and no recommendation; every caveat is a Limitations line.
type tokensProfileReport struct {
	Source                   string               `json:"source"`
	CheckpointsAvailable     int                  `json:"checkpoints_available"`
	CheckpointsAnalyzed      int                  `json:"checkpoints_analyzed"`
	CheckpointsWithTokenData int                  `json:"checkpoints_with_token_data"`
	Collapsed                int                  `json:"collapsed"`
	ExcludedTestAgents       int                  `json:"excluded_test_agents"`
	MetadataReadWarnings     int                  `json:"metadata_read_warnings"`
	Agents                   []tokensProfileAgent `json:"agents"`
	TotalTokens              int                  `json:"total_tokens"`
	Limitations              []string             `json:"limitations,omitempty"`
}

// tokensProfileAgent is one agent's block: counts, per-checkpoint
// distributions, the cost view over its priced checkpoints, and the two
// checkpoints most worth opening.
type tokensProfileAgent struct {
	Agent               string                      `json:"agent"`
	Checkpoints         int                         `json:"checkpoints"`
	WithTokens          int                         `json:"with_tokens"`
	Collapsed           int                         `json:"collapsed"`
	TokensPerCheckpoint *tokensProfilePercentiles   `json:"tokens_per_checkpoint,omitempty"`
	DurationSeconds     tokensProfileDuration       `json:"duration_seconds"`
	TokensPerHourMedian int                         `json:"tokens_per_hour_median,omitempty"`
	LargestCostClass    map[string]int              `json:"largest_cost_class"`
	CostByClass         *tokensProfileCostByClass   `json:"cost_by_class,omitempty"`
	ThinkingShare       tokensProfileThinkingShare  `json:"thinking_share"`
	Effort              string                      `json:"effort"`
	WorthOpening        []tokensProfileWorthOpening `json:"worth_opening"`
}

// tokensProfilePercentiles is a nearest-rank median and p90 over the
// checkpoints that recorded the figure.
type tokensProfilePercentiles struct {
	Median int `json:"median"`
	P90    int `json:"p90"`
}

// tokensProfileDuration is the per-checkpoint duration distribution in
// seconds (a checkpoint's kept session durations summed); RecordedOn counts
// the checkpoints with at least one recorded duration, with or without tokens.
type tokensProfileDuration struct {
	Median     int `json:"median"`
	P90        int `json:"p90"`
	RecordedOn int `json:"recorded_on"`
}

// tokensProfileCostByClass is the cost-weighted class split summed over the
// agent's priced checkpoints (shares of 1). CacheWriteRecordedOn counts the
// priced checkpoints with cache writes on a model that prices the two TTLs
// differently; CacheWrite1hRecordedOn how many of those recorded the 1-hour
// split. CacheWriteUnpriced is true when any of the rest had cache writes
// that could not be priced (TTL unknown).
type tokensProfileCostByClass struct {
	Input                  float64 `json:"input"`
	CacheWrite             float64 `json:"cache_write"`
	CacheRead              float64 `json:"cache_read"`
	Output                 float64 `json:"output"`
	Priced                 int     `json:"priced"`
	CacheWriteUnpriced     bool    `json:"cache_write_unpriced"`
	CacheWriteRecordedOn   int     `json:"cache_write_recorded_on"`
	CacheWrite1hRecordedOn int     `json:"cache_write_1h_recorded_on"`
}

// tokensProfileThinkingShare is the median thinking-to-output share over the
// checkpoints that recorded thinking (see tokensProfileThinkingRecorded).
type tokensProfileThinkingShare struct {
	Median     float64 `json:"median"`
	RecordedOn int     `json:"recorded_on"`
}

// tokensProfileWorthOpening is one of the agent's top checkpoints by cost
// with the figure that makes it stand out.
type tokensProfileWorthOpening struct {
	CheckpointID string `json:"checkpoint_id"`
	Tokens       int    `json:"tokens"`
	Standout     string `json:"standout"`
}

// Cost class labels, in the fixed order ties are broken in.
const (
	tokensProfileClassInput      = "input"
	tokensProfileClassCacheWrite = "cache write"
	tokensProfileClassCacheRead  = "cache read"
	tokensProfileClassOutput     = "output"
)

// tokensProfileClasses is the fixed class order.
var tokensProfileClasses = []string{tokensProfileClassInput, tokensProfileClassCacheWrite, tokensProfileClassCacheRead, tokensProfileClassOutput}

// Display strings and layout constants of the profile writer.
const (
	tokensProfileSource      = "committed_checkpoints"
	tokensProfileNotPriced   = "not priced"
	tokensProfileLabelWidth  = 27
	tokensProfileWorthCount  = 2
	tokensProfileHeaderWrap  = 80
	tokensProfileAgentLegacy = types.AgentType("Agent")
	// tokensProfileWorthIndent aligns the later Worth opening entries and the
	// hint under the first entry.
	tokensProfileWorthIndent = "                  "
)

// tokensProfileExcludedAgents are the test-only agents never counted in a
// profile. The agent package exports no constant for the mock lifecycle
// agent's type name, so both names live here.
var tokensProfileExcludedAgents = []types.AgentType{"Mock Lifecycle Agent", "Vogon"}

// tokensProfileStore is the store surface the profile reads: root summaries
// and per-session metadata. checkpoint.PersistentStore satisfies it.
type tokensProfileStore interface {
	Read(ctx context.Context, checkpointID id.CheckpointID) (*checkpoint.CheckpointSummary, error)
	ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, error)
}

// tokensBareNoSessionHint is what bare `entire tokens` prints when the current
// worktree has no session to report on: one line, exit 0, no picker.
const tokensBareNoSessionHint = "no active session — try 'entire checkpoint tokens <id>' or 'entire tokens profile'" //nolint:gosec // G101: a hint line, not a credential

// newTokensGroupCmd is the `tokens` group. Bare `entire tokens` reports on
// the current worktree's most recent session, exactly as
// `entire session tokens --current` does; the subcommands cover checkpoint
// history.
func newTokensGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "tokens",
		Short:  "Analyze token usage across sessions and checkpoints",
		Hidden: true,
		Long: `Analyze token usage across sessions and checkpoints.

Run without a subcommand to report on the current worktree's most recent
session, as 'entire session tokens --current' does. Without an active session
it prints a one-line hint and exits 0.

Commands:
  profile  Aggregate token usage across committed checkpoints

Examples:
  entire tokens                current session's token report
  entire tokens profile
  entire tokens profile --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			sessionID := strategy.FindMostRecentSessionInCurrentWorktree(ctx)
			if sessionID == "" {
				fmt.Fprintln(cmd.OutOrStdout(), tokensBareNoSessionHint)
				return nil
			}
			return runSessionTokens(ctx, cmd, sessionID, true, false, false)
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
		Short: "Profile token usage per agent across checkpoint history",
		Long: `Profile token usage per agent across committed checkpoint history.

The profile reads committed checkpoint metadata only. It does not inspect
transcripts or source files, so it is deterministic and adds no token cost
while diagnosing token usage; recurring contributors are therefore not
computed. Legacy checkpoints whose token_usage may be a session running total
are collapsed per session. By default it scans the latest 50 committed
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

	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{BlobFetcher: FetchBlobsByHash, RefFetcher: FetchCheckpointRef, ReadRemotes: strategy.CheckpointReadRemotes(ctx)})
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

// buildTokensProfileReport loads the latest limit checkpoints (all when limit
// is 0), groups their session rows by agent, collapses legacy running totals
// per session and aggregates each agent's block.
func buildTokensProfileReport(ctx context.Context, store tokensProfileStore, infos []checkpoint.CheckpointInfo, limit int) (tokensProfileReport, error) {
	checkpointsAvailable := len(infos)
	infos = limitTokensProfileCheckpoints(infos, limit)
	load, err := loadTokensProfileRows(ctx, store, infos)
	if err != nil {
		return tokensProfileReport{}, err
	}

	report := tokensProfileReport{
		Source:               tokensProfileSource,
		CheckpointsAvailable: checkpointsAvailable,
		CheckpointsAnalyzed:  len(infos),
		MetadataReadWarnings: load.metadataReadWarnings,
		Agents:               []tokensProfileAgent{},
	}
	groups, excluded := groupTokensProfileRows(load.rows)
	report.ExcludedTestAgents = excluded

	var totals tokensProfileTotals
	for name, rows := range groups {
		block, agentTotals := buildTokensProfileAgent(name, rows)
		report.Agents = append(report.Agents, block)
		report.Collapsed += block.Collapsed
		report.CheckpointsWithTokenData += block.WithTokens
		totals.add(agentTotals)
	}
	slices.SortFunc(report.Agents, func(a, b tokensProfileAgent) int {
		if c := cmp.Compare(b.Checkpoints, a.Checkpoints); c != 0 {
			return c
		}
		return strings.Compare(a.Agent, b.Agent)
	})
	report.TotalTokens = totals.tokens
	report.Limitations = tokensProfileLimitations(report, load, totals)
	return report, nil
}

func limitTokensProfileCheckpoints(infos []checkpoint.CheckpointInfo, limit int) []checkpoint.CheckpointInfo {
	if limit <= 0 || len(infos) <= limit {
		return infos
	}
	return infos[:limit]
}

// tokensProfileRow is one (checkpoint, session) entry as loaded from the
// store: the dedupe view plus the metadata fields the profile aggregates.
type tokensProfileRow struct {
	tokenreport.CheckpointRow

	agent      types.AgentType
	model      string
	durationMs int64
}

// tokensProfileLoad is what loadTokensProfileRows read and what it had to
// skip.
type tokensProfileLoad struct {
	rows []tokensProfileRow
	// unreadable counts checkpoints whose root summary could not be read.
	unreadable int
	// metadataReadWarnings counts checkpoints with at least one session whose
	// metadata could not be read; those sessions are not counted.
	metadataReadWarnings int
	// skippedNoCreatedAt counts session rows dropped because neither the
	// listing nor the session metadata carried a created_at.
	skippedNoCreatedAt int
}

// loadTokensProfileRows reads every session's metadata for infos into rows.
// A row's CreatedAt is the listing's, falling back to the session
// metadata's; a row with neither is skipped (and logged by checkpoint ID)
// rather than fed to tokenreport.DedupeLegacyCheckpoints, where a zero time
// would sort first and silently collapse the row.
func loadTokensProfileRows(ctx context.Context, store tokensProfileStore, infos []checkpoint.CheckpointInfo) (tokensProfileLoad, error) {
	var load tokensProfileLoad
	for _, info := range infos {
		if err := ctx.Err(); err != nil {
			return tokensProfileLoad{}, err //nolint:wrapcheck // Propagating context cancellation.
		}
		summary, err := store.Read(ctx, info.CheckpointID)
		if err != nil {
			return tokensProfileLoad{}, fmt.Errorf("failed to read checkpoint %s: %w", info.CheckpointID, err)
		}
		if summary == nil {
			load.unreadable++
			continue
		}

		warned := false
		for i := range len(summary.Sessions) {
			meta, err := store.ReadSessionMetadata(ctx, info.CheckpointID, i)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return tokensProfileLoad{}, ctxErr //nolint:wrapcheck // Propagating context cancellation.
				}
				warned = true
				logging.Warn(ctx, "tokens profile: session metadata unreadable; session not counted",
					slog.String("checkpoint_id", info.CheckpointID.String()),
					slog.Int("session_index", i))
				continue
			}
			created := info.CreatedAt
			if created.IsZero() {
				created = meta.CreatedAt
			}
			if created.IsZero() {
				load.skippedNoCreatedAt++
				logging.Warn(ctx, "tokens profile: checkpoint has no created_at; skipped",
					slog.String("checkpoint_id", info.CheckpointID.String()),
					slog.Int("session_index", i))
				continue
			}
			load.rows = append(load.rows, tokensProfileRow{
				CheckpointRow: tokenreport.CheckpointRow{
					CheckpointID:              info.CheckpointID.String(),
					SessionID:                 meta.SessionID,
					Version:                   summary.TokenUsageVersion,
					CheckpointTranscriptStart: meta.CheckpointTranscriptStart,
					Usage:                     meta.TokenUsage,
					CreatedAt:                 created,
				},
				agent:      meta.Agent,
				model:      meta.Model,
				durationMs: tokensProfileDurationMs(meta),
			})
		}
		if warned {
			load.metadataReadWarnings++
		}
	}
	return load, nil
}

// tokensProfileDurationMs is the session's recorded duration, 0 when absent.
func tokensProfileDurationMs(meta *checkpoint.Metadata) int64 {
	if meta.SessionMetrics == nil || meta.SessionMetrics.DurationMs <= 0 {
		return 0
	}
	return meta.SessionMetrics.DurationMs
}

// tokensProfileAgentName is the grouping key for a session's agent: the
// pre-agent-field values "" and "Agent" fold into Claude Code, which wrote
// them.
func tokensProfileAgentName(agentType types.AgentType) types.AgentType {
	if agentType == "" || agentType == tokensProfileAgentLegacy {
		return agent.AgentTypeClaudeCode
	}
	return agentType
}

// groupTokensProfileRows buckets rows by agent, dropping the test-only
// agents; excluded counts the distinct checkpoints dropped that way.
func groupTokensProfileRows(rows []tokensProfileRow) (map[types.AgentType][]tokensProfileRow, int) {
	groups := make(map[types.AgentType][]tokensProfileRow)
	excludedCheckpoints := make(map[string]struct{})
	for _, row := range rows {
		name := tokensProfileAgentName(row.agent)
		if slices.Contains(tokensProfileExcludedAgents, name) {
			excludedCheckpoints[row.CheckpointID] = struct{}{}
			continue
		}
		groups[name] = append(groups[name], row)
	}
	return groups, len(excludedCheckpoints)
}

// tokensProfileSample is one checkpoint's figures within an agent group: its
// kept session rows merged. Volume is the four classes summed; cost is the
// SumCostShares of the rows whose model priced. A checkpoint is priced when
// any of its rows priced and volumeOnly when it has tokens but none did.
type tokensProfileSample struct {
	checkpointID string
	volume       int
	priced       bool
	cost         tokenreport.CostShares
	// cacheWriteTTLMatters is true when a priced row had cache writes on a
	// model that prices the 1h and 5m TTLs differently; cacheWrite1h when
	// such a row recorded the 1h split.
	cacheWriteTTLMatters bool
	cacheWrite1h         bool
	// thinkingRecorded is true when any row satisfied
	// tokensProfileThinkingRecorded; thinking and output are summed over
	// those rows only.
	thinkingRecorded bool
	thinking         int
	output           int
	// durationMs is the rows' recorded durations summed; 0 when none recorded.
	durationMs int64
}

// tokensProfileThinkingRecorded reports whether a row's ThinkingTokens is a
// recorded figure rather than an absent field: the agent's transcript must
// record thinking at all, and either the row is a version-2 checkpoint
// (which writes the field, so 0 means 0) or a legacy row with a non-zero
// count (a legacy 0 cannot be told from "not written").
func tokensProfileThinkingRecorded(row tokensProfileRow, flat *types.TokenUsage) bool {
	if !tokenreport.ProfileFor(tokensProfileAgentName(row.agent)).RecordsThinking || flat.OutputTokens <= 0 {
		return false
	}
	return row.Version >= checkpoint.TokenUsageVersionDelta || flat.ThinkingTokens > 0
}

// add merges one kept row into the sample.
func (s *tokensProfileSample) add(row tokensProfileRow) {
	s.durationMs += row.durationMs
	flat := flattenTokenUsage(row.Usage)
	if flat == nil {
		return
	}
	s.volume += tokenVolume(flat)
	if w, _, ok := tokenreport.WeightsFor(row.model); ok {
		s.priced = true
		s.cost = tokenreport.SumCostShares(s.cost, tokenreport.ComputeCostShares(flat, w))
		if flat.CacheCreationTokens > 0 && w.CacheWrite1h != w.CacheWrite5m {
			s.cacheWriteTTLMatters = true
			s.cacheWrite1h = s.cacheWrite1h || flat.CacheCreation1hTokens > 0
		}
	}
	if tokensProfileThinkingRecorded(row, flat) {
		s.thinkingRecorded = true
		s.thinking += flat.ThinkingTokens
		s.output += flat.OutputTokens
	}
}

// hasTokens reports whether the checkpoint recorded any token volume.
func (s *tokensProfileSample) hasTokens() bool { return s.volume > 0 }

// volumeOnly reports whether the checkpoint has tokens but no priced row.
func (s *tokensProfileSample) volumeOnly() bool { return s.hasTokens() && !s.priced }

// thinkingShare is thinking ÷ output over the recorded rows.
func (s *tokensProfileSample) thinkingShare() float64 {
	if !s.thinkingRecorded || s.output <= 0 {
		return 0
	}
	return float64(s.thinking) / float64(s.output)
}

// largestCostClass is the class with the biggest cost share, ties broken in
// tokensProfileClasses order; "" for an unpriced checkpoint or zero units.
func (s *tokensProfileSample) largestCostClass() (string, float64) {
	if !s.priced || s.cost.Units <= 0 {
		return "", 0
	}
	best, bestShare := "", -1.0
	for _, class := range tokensProfileClasses {
		if share := tokensProfileClassShare(s.cost, class); share > bestShare {
			best, bestShare = class, share
		}
	}
	return best, bestShare
}

// tokensProfileClassShare picks one class share out of cs.
func tokensProfileClassShare(cs tokenreport.CostShares, class string) float64 {
	switch class {
	case tokensProfileClassInput:
		return cs.Input
	case tokensProfileClassCacheWrite:
		return cs.CacheWrite
	case tokensProfileClassCacheRead:
		return cs.CacheRead
	case tokensProfileClassOutput:
		return cs.Output
	default:
		return 0
	}
}

// standout is the figure that makes the checkpoint worth opening: its
// thinking share when thinking exceeds half its output, else its largest
// cost class and share; "volume only" for an unpriced checkpoint.
func (s *tokensProfileSample) standout() string {
	if share := s.thinkingShare(); share > 0.5 {
		return "thinking " + tokenreport.FormatPercent(share)
	}
	class, share := s.largestCostClass()
	if class == "" {
		return "volume only"
	}
	return class + " " + tokenreport.FormatPercent(share)
}

// tokensProfileTotals are the report-wide figures each agent block adds to.
type tokensProfileTotals struct {
	tokens     int
	volumeOnly int // checkpoints with tokens but no priced row
	unknownTTL int // priced checkpoints with cache writes at an unknown TTL
	priced     int
}

func (t *tokensProfileTotals) add(o tokensProfileTotals) {
	t.tokens += o.tokens
	t.volumeOnly += o.volumeOnly
	t.unknownTTL += o.unknownTTL
	t.priced += o.priced
}

// buildTokensProfileAgent collapses the agent's legacy running totals per
// session, merges the kept rows into one sample per checkpoint and
// aggregates the block. Session IDs belong to one agent, so deduping per
// agent group equals deduping the whole set.
func buildTokensProfileAgent(name types.AgentType, rows []tokensProfileRow) (tokensProfileAgent, tokensProfileTotals) {
	// A checkpoint stores each session once, so (CheckpointID, SessionID) is unique per row.
	byKey := make(map[[2]string]tokensProfileRow, len(rows))
	checkpointRows := make([]tokenreport.CheckpointRow, 0, len(rows))
	for _, row := range rows {
		byKey[[2]string{row.CheckpointID, row.SessionID}] = row
		checkpointRows = append(checkpointRows, row.CheckpointRow)
	}
	kept, collapsed := tokenreport.DedupeLegacyCheckpoints(checkpointRows)

	byCheckpoint := make(map[string]*tokensProfileSample)
	var samples []*tokensProfileSample
	for _, keptRow := range kept {
		row := byKey[[2]string{keptRow.CheckpointID, keptRow.SessionID}]
		sample := byCheckpoint[row.CheckpointID]
		if sample == nil {
			sample = &tokensProfileSample{checkpointID: row.CheckpointID}
			byCheckpoint[row.CheckpointID] = sample
			samples = append(samples, sample)
		}
		sample.add(row)
	}
	slices.SortFunc(samples, func(a, b *tokensProfileSample) int { return strings.Compare(a.checkpointID, b.checkpointID) })

	block := tokensProfileAgent{
		Agent:            string(name),
		Checkpoints:      len(samples),
		Collapsed:        collapsed,
		LargestCostClass: map[string]int{},
		Effort:           tokenNotRecorded,
		WorthOpening:     []tokensProfileWorthOpening{},
	}
	totals := aggregateTokensProfileSamples(&block, samples)
	return block, totals
}

// aggregateTokensProfileSamples fills block's distributions from samples and
// returns the agent's contribution to the report totals.
func aggregateTokensProfileSamples(block *tokensProfileAgent, samples []*tokensProfileSample) tokensProfileTotals {
	var totals tokensProfileTotals
	var volumes, durations, perHour []int
	var thinking []float64
	var priced []tokenreport.CostShares
	for _, s := range samples {
		totals.tokens += s.volume
		if s.durationMs > 0 {
			durations = append(durations, int(s.durationMs/1000))
		}
		if !s.hasTokens() {
			continue
		}
		block.WithTokens++
		volumes = append(volumes, s.volume)
		if s.durationMs > 0 {
			// Tokens per hour is over the with-tokens subset of the recorded durations.
			perHour = append(perHour, int(int64(s.volume)*int64(time.Hour/time.Millisecond)/s.durationMs))
		}
		if s.thinkingRecorded {
			thinking = append(thinking, s.thinkingShare())
		}
		if s.volumeOnly() {
			totals.volumeOnly++
			continue
		}
		totals.priced++
		priced = append(priced, s.cost)
		if s.cost.CacheWriteUnpriced {
			totals.unknownTTL++
		}
		if class, _ := s.largestCostClass(); class != "" {
			block.LargestCostClass[class]++
		}
	}

	if len(volumes) > 0 {
		slices.Sort(volumes)
		block.TokensPerCheckpoint = &tokensProfilePercentiles{Median: percentile(volumes, 50), P90: percentile(volumes, 90)}
	}
	slices.Sort(durations)
	slices.Sort(perHour)
	block.DurationSeconds = tokensProfileDuration{Median: percentile(durations, 50), P90: percentile(durations, 90), RecordedOn: len(durations)}
	block.TokensPerHourMedian = percentile(perHour, 50)
	slices.Sort(thinking)
	block.ThinkingShare = tokensProfileThinkingShare{Median: percentile(thinking, 50), RecordedOn: len(thinking)}
	if len(priced) > 0 {
		block.CostByClass = tokensProfileCostByClassFor(samples, priced)
	}
	block.WorthOpening = tokensProfileWorthOpeningFor(samples)
	return totals
}

// tokensProfileCostByClassFor sums the priced samples' shares and counts the
// TTL bookkeeping.
func tokensProfileCostByClassFor(samples []*tokensProfileSample, priced []tokenreport.CostShares) *tokensProfileCostByClass {
	sum := tokenreport.SumCostShares(priced...)
	cost := &tokensProfileCostByClass{
		Input:              sum.Input,
		CacheWrite:         sum.CacheWrite,
		CacheRead:          sum.CacheRead,
		Output:             sum.Output,
		Priced:             len(priced),
		CacheWriteUnpriced: sum.CacheWriteUnpriced,
	}
	for _, s := range samples {
		if s.priced && s.cacheWriteTTLMatters {
			cost.CacheWriteRecordedOn++
			if s.cacheWrite1h {
				cost.CacheWrite1hRecordedOn++
			}
		}
	}
	return cost
}

// tokensProfileWorthOpeningFor picks the top tokensProfileWorthCount
// checkpoints: priced ones first by cost units, then volume-only ones by
// volume; checkpoint ID (the samples' order) breaks ties.
func tokensProfileWorthOpeningFor(samples []*tokensProfileSample) []tokensProfileWorthOpening {
	ranked := make([]*tokensProfileSample, 0, len(samples))
	for _, s := range samples {
		if s.hasTokens() {
			ranked = append(ranked, s)
		}
	}
	slices.SortStableFunc(ranked, func(a, b *tokensProfileSample) int {
		if a.priced != b.priced {
			if a.priced {
				return -1
			}
			return 1
		}
		if a.priced {
			return cmp.Compare(b.cost.Units, a.cost.Units)
		}
		return cmp.Compare(b.volume, a.volume)
	})
	worth := make([]tokensProfileWorthOpening, 0, tokensProfileWorthCount)
	for _, s := range ranked[:min(len(ranked), tokensProfileWorthCount)] {
		worth = append(worth, tokensProfileWorthOpening{CheckpointID: s.checkpointID, Tokens: s.volume, Standout: s.standout()})
	}
	return worth
}

// tokensProfileLimitations is the Notes list (and JSON `limitations`): the
// scope, what could not be read or ordered, the legacy dedupe, unpriced
// entries, the no-attribution statement and the pricing caveat.
func tokensProfileLimitations(report tokensProfileReport, load tokensProfileLoad, totals tokensProfileTotals) []string {
	var lines []string
	if report.CheckpointsAvailable > report.CheckpointsAnalyzed {
		lines = append(lines, fmt.Sprintf("Limited to latest %s of %s committed checkpoints; use --limit or --all to change scope.", formatThousands(report.CheckpointsAnalyzed), formatThousands(report.CheckpointsAvailable)))
	}
	if report.CheckpointsAnalyzed == 0 {
		lines = append(lines, "No committed checkpoints found.")
	}
	if n := load.unreadable; n > 0 {
		lines = append(lines, fmt.Sprintf("%d checkpoint%s could not be read and %s not counted.", n, tokenPluralSuffix(n), tokensProfileIsAre(n)))
	}
	if n := load.metadataReadWarnings; n > 0 {
		lines = append(lines, fmt.Sprintf("%d checkpoint%s had unreadable session metadata; those sessions are not counted.", n, tokenPluralSuffix(n)))
	}
	if n := load.skippedNoCreatedAt; n > 0 {
		lines = append(lines, fmt.Sprintf("%d checkpoint%s skipped: no created_at recorded (needed to order legacy rows for dedupe).", n, tokenPluralSuffix(n)))
	}
	if n := report.Collapsed; n > 0 {
		lines = append(lines, fmt.Sprintf("Legacy checkpoints (no token_usage_version) were deduped per session; %d legacy running-total row%s collapsed.", n, tokenPluralSuffix(n)))
	}
	if n := totals.volumeOnly; n > 0 {
		lines = append(lines, fmt.Sprintf("%d checkpoint%s ha%s no recorded model and %s counted by volume only.", n, tokenPluralSuffix(n), tokensProfileHasSuffix(n), tokensProfileIsAre(n)))
	}
	if n := totals.unknownTTL; n > 0 {
		lines = append(lines, fmt.Sprintf("Cache writes on %d checkpoint%s have no recorded TTL and are not priced.", n, tokenPluralSuffix(n)))
	}
	lines = append(lines, "Recurring contributors are not computed for profiles (no transcripts are read).")
	if totals.priced > 0 {
		lines = append(lines, "Cost shares use list-price ratios per model family, not your plan's rates.")
	}
	return lines
}

// tokensProfileIsAre is "is" for one, "are" otherwise.
func tokensProfileIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// tokensProfileHasSuffix completes "ha" to "has" for one, "have" otherwise.
func tokensProfileHasSuffix(n int) string {
	if n == 1 {
		return "s"
	}
	return "ve"
}

// writeTokensProfileText prints the header, one block per agent and the
// Notes with the grand total last.
func writeTokensProfileText(w io.Writer, report tokensProfileReport) {
	fmt.Fprintln(w, wrapText(tokensProfileHeader(report), tokensProfileHeaderWrap))
	for _, block := range report.Agents {
		fmt.Fprintln(w)
		writeTokensProfileAgent(w, block)
	}
	notes := append(slices.Clone(report.Limitations), fmt.Sprintf("Total: %s tokens (sum after collapsing overlaps).", tokenreport.FormatTokenCount(report.TotalTokens)))
	writeTokenNotes(w, notes)
}

// tokensProfileHeader is the one-sentence scope line; the collapsed and
// excluded clauses appear only when non-zero.
func tokensProfileHeader(report tokensProfileReport) string {
	var clauses []string
	if n := report.Collapsed; n > 0 {
		clauses = append(clauses, fmt.Sprintf("%d overlapping checkpoint%s collapsed", n, tokenPluralSuffix(n)))
	}
	if n := report.ExcludedTestAgents; n > 0 {
		clauses = append(clauses, fmt.Sprintf("%d test-agent checkpoint%s excluded", n, tokenPluralSuffix(n)))
	}
	scope := formatThousands(report.CheckpointsAvailable) + " available"
	if len(clauses) > 0 {
		scope += "; " + strings.Join(clauses, ", ")
	}
	return fmt.Sprintf("Token profile — last %d committed checkpoints (%s)", report.CheckpointsAnalyzed, scope)
}

// formatThousands renders a non-negative n with a comma every three digits.
func formatThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// writeTokensProfileAgent prints one agent block: the title with its counts,
// one line per metric and the Worth opening entries (one per line, aligned
// under the first) with the hint once.
func writeTokensProfileAgent(w io.Writer, block tokensProfileAgent) {
	fmt.Fprintln(w, tokensProfileAgentTitle(block))
	line := func(label, value string) {
		fmt.Fprintf(w, "  %s%s\n", tokenPadRight(label, tokensProfileLabelWidth), value)
	}
	line("duration / session", tokensProfileDurationValue(block))
	line("tokens / checkpoint", tokensProfileTokensValue(block))
	line("largest cost class", tokensProfileLargestValue(block))
	line("cost by class (sum)", tokensProfileCostValue(block))
	line("thinking share of output", tokensProfileThinkingValue(block))
	line("effort", block.Effort)
	if len(block.WorthOpening) == 0 {
		return
	}
	fmt.Fprintln(w)
	for i, item := range block.WorthOpening {
		lead := tokensProfileWorthIndent
		if i == 0 {
			lead = "  Worth opening   "
		}
		fmt.Fprintf(w, "%s%s (%s, %s)\n", lead, item.CheckpointID, tokenreport.FormatTokenCount(item.Tokens), item.Standout)
	}
	fmt.Fprintln(w, tokensProfileWorthIndent+"→ entire checkpoint tokens <id>")
}

// tokensProfileAgentTitle is "Agent · N checkpoints" with the with-tokens
// and collapsed counts in parentheses when they add information.
func tokensProfileAgentTitle(block tokensProfileAgent) string {
	title := fmt.Sprintf("%s · %d checkpoint%s", block.Agent, block.Checkpoints, tokenPluralSuffix(block.Checkpoints))
	var extras []string
	if block.WithTokens < block.Checkpoints {
		extras = append(extras, fmt.Sprintf("%d with tokens", block.WithTokens))
	}
	if block.Collapsed > 0 {
		extras = append(extras, fmt.Sprintf("%d overlapping collapsed", block.Collapsed))
	}
	if len(extras) > 0 {
		title += " (" + strings.Join(extras, "; ") + ")"
	}
	return title
}

// tokensProfileRecordedOn is the "(recorded on x of y)" suffix, empty when
// every checkpoint recorded the figure.
func tokensProfileRecordedOn(recorded, total int) string {
	if recorded >= total {
		return ""
	}
	return fmt.Sprintf("   (recorded on %d of %d)", recorded, total)
}

func tokensProfileDurationValue(block tokensProfileAgent) string {
	d := block.DurationSeconds
	if d.RecordedOn == 0 {
		return tokenNotRecorded
	}
	return fmt.Sprintf("median  %s   p90  %s      tokens per hour  median %s%s",
		tokenreport.FormatDuration(time.Duration(d.Median)*time.Second),
		tokenreport.FormatDuration(time.Duration(d.P90)*time.Second),
		tokenreport.FormatTokenCount(block.TokensPerHourMedian),
		tokensProfileRecordedOn(d.RecordedOn, block.Checkpoints))
}

func tokensProfileTokensValue(block tokensProfileAgent) string {
	p := block.TokensPerCheckpoint
	if p == nil {
		return tokenNotRecorded
	}
	return fmt.Sprintf("median  %s    p90  %s%s", tokenreport.FormatTokenCount(p.Median), tokenreport.FormatTokenCount(p.P90), tokensProfileRecordedOn(block.WithTokens, block.Checkpoints))
}

// tokensProfileLargestValue lists the classes by how often each was the
// largest cost, count descending then class order.
func tokensProfileLargestValue(block tokensProfileAgent) string {
	classes := make([]string, 0, len(block.LargestCostClass))
	for _, class := range tokensProfileClasses {
		if block.LargestCostClass[class] > 0 {
			classes = append(classes, class)
		}
	}
	if len(classes) == 0 {
		return tokensProfileNotPriced
	}
	slices.SortStableFunc(classes, func(a, b string) int { return cmp.Compare(block.LargestCostClass[b], block.LargestCostClass[a]) })
	parts := make([]string, 0, len(classes))
	for _, class := range classes {
		parts = append(parts, fmt.Sprintf("%s in %d", class, block.LargestCostClass[class]))
	}
	return strings.Join(parts, " · ")
}

// tokensProfileCostValue lists the summed cost shares, largest first, with
// the 1-hour bookkeeping after cache write when a TTL mattered.
func tokensProfileCostValue(block tokensProfileAgent) string {
	cost := block.CostByClass
	if cost == nil {
		return tokensProfileNotPriced
	}
	shares := tokenreport.CostShares{Input: cost.Input, CacheWrite: cost.CacheWrite, CacheRead: cost.CacheRead, Output: cost.Output}
	classes := slices.Clone(tokensProfileClasses)
	slices.SortStableFunc(classes, func(a, b string) int {
		return cmp.Compare(tokensProfileClassShare(shares, b), tokensProfileClassShare(shares, a))
	})
	var parts []string
	for _, class := range classes {
		share := tokensProfileClassShare(shares, class)
		if class == tokensProfileClassCacheWrite {
			if part, ok := tokensProfileCacheWritePart(cost, share); ok {
				parts = append(parts, part)
			}
			continue
		}
		if share > 0 {
			parts = append(parts, class+" "+tokenreport.FormatPercent(share))
		}
	}
	return strings.Join(parts, " · ")
}

// tokensProfileCacheWritePart is the cache-write entry of the cost line: the
// share with the 1-hour bookkeeping, "not priced" when every cache write had
// an unknown TTL, and nothing when there were no cache writes at all.
func tokensProfileCacheWritePart(cost *tokensProfileCostByClass, share float64) (string, bool) {
	switch {
	case share > 0 && cost.CacheWriteRecordedOn > 0:
		return fmt.Sprintf("%s %s (1-hour on %d of %d recorded)", tokensProfileClassCacheWrite, tokenreport.FormatPercent(share), cost.CacheWrite1hRecordedOn, cost.CacheWriteRecordedOn), true
	case share > 0:
		return tokensProfileClassCacheWrite + " " + tokenreport.FormatPercent(share), true
	case cost.CacheWriteUnpriced:
		return tokensProfileClassCacheWrite + " " + tokensProfileNotPriced + " (TTL not recorded)", true
	default:
		return "", false
	}
}

func tokensProfileThinkingValue(block tokensProfileAgent) string {
	if block.ThinkingShare.RecordedOn == 0 {
		return tokenNotRecorded
	}
	return "median " + tokenreport.FormatPercent(block.ThinkingShare.Median) + tokensProfileRecordedOn(block.ThinkingShare.RecordedOn, block.Checkpoints)
}
