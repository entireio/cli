package cli

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/tokenreport"
	"github.com/spf13/cobra"
)

// Source values of a checkpoint token report.
const (
	// checkpointTokensSourceTranscript means every session's totals were
	// recomputed from its stored transcript.
	checkpointTokensSourceTranscript = tokenSourceTranscript
	// checkpointTokensSourceCommitted means at least one session's totals
	// came from its committed token_usage metadata.
	checkpointTokensSourceCommitted = "committed_checkpoint"
)

// checkpointTokenSession is one session of a checkpoint as read from the
// store: its metadata and, when readable, its stored transcript.
type checkpointTokenSession struct {
	index int
	meta  *checkpoint.Metadata
	// transcript is the stored full.jsonl; nil when it could not be read.
	transcript []byte
	// transcriptErr is why transcript is nil; nil when it was read.
	transcriptErr error
}

// checkpointTokenInputs is everything buildCheckpointTokensReport needs,
// gathered by loadCheckpointTokenInputs.
type checkpointTokenInputs struct {
	checkpointID id.CheckpointID
	summary      *checkpoint.CheckpointSummary
	sessions     []checkpointTokenSession
	records      []checkpoint.StoredTaskRecord
	// metadataWarnings counts sessions whose metadata could not be read.
	metadataWarnings int
	// recordsErr is a non-fatal ReadTaskRecords failure, reported as a note.
	recordsErr error
}

// loadCheckpointTokensReport resolves the checkpoint the way `checkpoint
// explain` does, reads its sessions and task records, and builds the report.
// The returned lookup must be closed by the caller even on error.
func loadCheckpointTokensReport(ctx context.Context, cmd *cobra.Command, checkpointIDPrefix string) (checkpointTokensReport, *explainCheckpointLookup, error) {
	cpID, lookup, err := resolveExplainCheckpointID(ctx, cmd.ErrOrStderr(), explainExportOptions{target: checkpointIDPrefix})
	if err != nil {
		return checkpointTokensReport{}, lookup, err
	}

	summary, err := lookup.store.Read(ctx, cpID)
	if err != nil {
		return checkpointTokensReport{}, lookup, fmt.Errorf("failed to read checkpoint: %w", err)
	}
	if summary == nil || len(summary.Sessions) == 0 {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Checkpoint not found.")
		return checkpointTokensReport{}, lookup, NewSilentError(fmt.Errorf("%w: %s", checkpoint.ErrCheckpointNotFound, checkpointIDPrefix))
	}

	inputs, err := loadCheckpointTokenInputs(ctx, lookup.store, cpID, summary)
	if err != nil {
		return checkpointTokensReport{}, lookup, err
	}
	return buildCheckpointTokensReport(ctx, inputs), lookup, nil
}

// loadCheckpointTokenInputs reads each session's metadata and transcript and
// the checkpoint's task records. Only context cancellation is an error: an
// unreadable session or task tree becomes a note on the report.
func loadCheckpointTokenInputs(ctx context.Context, store checkpoint.SessionReader, cpID id.CheckpointID, summary *checkpoint.CheckpointSummary) (checkpointTokenInputs, error) {
	inputs := checkpointTokenInputs{checkpointID: cpID, summary: summary}
	sessions, warnings, err := readCheckpointTokenSessions(ctx, store, cpID, len(summary.Sessions))
	if err != nil {
		return checkpointTokenInputs{}, err
	}
	inputs.sessions = sessions
	inputs.metadataWarnings = warnings

	records, err := store.ReadTaskRecords(ctx, cpID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return checkpointTokenInputs{}, ctxErr //nolint:wrapcheck // Propagating context cancellation.
		}
		logging.Warn(ctx, "checkpoint tokens: task records unreadable",
			slog.String("checkpoint_id", cpID.String()),
			slog.String("error", err.Error()))
		inputs.recordsErr = err
	}
	inputs.records = records
	return inputs, nil
}

// readCheckpointTokenSessions reads every session's metadata and stored
// transcript. A session whose metadata cannot be read is dropped and counted
// in warnings; one whose transcript cannot be read is kept with
// transcriptErr set. Context cancellation stops the loop and is returned.
//
// The transcript read is the raw full.jsonl (SessionContent.Transcript), not
// the compact transcript.jsonl: compaction keeps only input and output token
// counts and drops thinking blocks, cache-read and cache-write counts and the
// per-call model, all of which the attribution and the cost shares need.
func readCheckpointTokenSessions(ctx context.Context, store checkpoint.SessionReader, cpID id.CheckpointID, sessionCount int) ([]checkpointTokenSession, int, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, ctxErr //nolint:wrapcheck // Propagating context cancellation.
	}
	sessions := make([]checkpointTokenSession, 0, sessionCount)
	var warnings int
	for i := range sessionCount {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, 0, ctxErr //nolint:wrapcheck // Propagating context cancellation.
		}
		meta, err := store.ReadSessionMetadata(ctx, cpID, i)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, ctxErr //nolint:wrapcheck // Propagating context cancellation.
			}
			logging.Warn(ctx, "checkpoint tokens: session metadata unreadable",
				slog.String("checkpoint_id", cpID.String()),
				slog.Int("session_index", i),
				slog.String("error", err.Error()))
			warnings++
			continue
		}
		s := checkpointTokenSession{index: i, meta: meta}
		content, err := store.ReadSessionContent(ctx, cpID, i)
		switch {
		case err != nil:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, ctxErr //nolint:wrapcheck // Propagating context cancellation.
			}
			s.transcriptErr = err
		case content == nil || len(content.Transcript) == 0:
			s.transcriptErr = checkpoint.ErrNoTranscript
		default:
			s.transcript = content.Transcript
		}
		sessions = append(sessions, s)
	}
	return sessions, warnings, nil
}

// sessionTokenAnalysis is what one session contributes to the report.
type sessionTokenAnalysis struct {
	meta *checkpoint.Metadata
	// attribution is the session's per-call usage; nil when the session was
	// not attributed (no attributor, no transcript, zero calls, parse error).
	attribution *types.Attribution
	// legacy is true for a checkpoint without token_usage_version: the
	// breakdown covers the whole stored transcript while the totals stay the
	// committed token_usage, which has no per-call counterpart.
	legacy bool
	// recomputed is true when usage was summed from the attributed calls and
	// subagent records rather than read from the committed token_usage.
	recomputed bool
	// usage is the session's flattened total; nil when nothing was recorded.
	usage *types.TokenUsage
	// subagent is the flattened subagent part of usage; nil when none.
	subagent *types.TokenUsage
	// attributed is the contributor table; zero when not attributed.
	attributed tokenreport.Attributed
	costParts  []tokenreport.CostShares
	duration   time.Duration
	// calls is the parent session's API calls with recorded usage: Σ
	// APICallCount over the attributed calls, or the committed count minus
	// the subagent count on the metadata path. Subagent calls are never
	// included, so the long_session replay gate sees the same figure on
	// every path.
	calls             int
	unknownUsageCalls int
	efforts           map[string]int
	models            map[string]int
	agentReportedCost float64
	// unmatchedSubagentRefs counts subagent tool calls with no task record.
	unmatchedSubagentRefs int
	// transcriptThinking and transcriptCacheWrite1h are the thinking and
	// 1-hour cache-write counts summed over a legacy session's attributed
	// calls — subset figures the committed token_usage predates. They are
	// surfaced beside the committed totals, never added into them.
	transcriptThinking     int
	transcriptCacheWrite1h int
	notes                  []string
}

// buildCheckpointTokensReport turns the loaded inputs into the report:
// attribution per session (best-effort), task records matched to their
// spawning calls, totals, cost shares, merged contributors, recommendations
// and notes.
//
// Totals come from the first rung of this ladder that applies, per session;
// Source is "transcript" only when every session stopped at rung 1:
//
//  1. attributed version-2 session → totals recomputed per call plus its
//     subagent records (finishSessionTokenAnalysis);
//  2. attributed legacy session → committed token_usage for the totals, the
//     whole stored transcript for the breakdown (finishLegacyFromTranscript);
//  3. not attributed (no attributor, unreadable transcript, parse error, no
//     API calls in the window) → committed token_usage (finishFromMetadata);
//  4. a session's metadata unreadable → the root summary's aggregate stands
//     in for every session's totals (applyRootSummaryFallback);
//  5. nothing recorded anywhere → "not recorded".
func buildCheckpointTokensReport(ctx context.Context, in checkpointTokenInputs) checkpointTokensReport {
	report := checkpointTokensReport{CheckpointID: in.checkpointID.String(), Source: checkpointTokensSourceCommitted}
	metas := make([]*checkpoint.Metadata, 0, len(in.sessions))
	for _, s := range in.sessions {
		metas = append(metas, s.meta)
	}
	fillCheckpointTokensIdentity(&report, in.summary, metas)

	legacy := in.summary == nil || in.summary.TokenUsageVersion < checkpoint.TokenUsageVersionDelta
	analyses := analyzeCheckpointTokenSessions(ctx, in, legacy)
	view := assembleTokenReportView(analyses, metas)
	rootFallback := applyRootSummaryFallback(&view, in)
	view.Limitations = append(view.Limitations, checkpointTokenNotes(in, analyses, legacy, rootFallback)...)
	if legacy && view.HasUsage {
		view.FromTranscript = legacyTranscriptSubsets(&view.Report.Usage, analyses)
		view.Legacy = legacyInfo(&view, in.summary, metas)
	}
	if view.HasUsage {
		view.Recommendations = tokenreport.Recommend(view.Report)
	}

	recomputed, fallback := countTokenSources(analyses)
	if recomputed > 0 && fallback == 0 && !rootFallback {
		report.Source = checkpointTokensSourceTranscript
	}
	if report.SessionCount == 1 && len(metas) == 1 && metas[0] != nil && metas[0].SessionMetrics != nil {
		report.Context = buildSessionTokensContext(metas[0].SessionMetrics.ContextTokens, metas[0].SessionMetrics.ContextWindowSize)
	}
	if len(view.Report.Attributed.Contributors) > 0 || view.Attributed {
		report.Models = mergeModelLabels(report.Models, view.Report.Model)
		if report.SessionCount == 1 && view.Report.Model != "" {
			report.Model = view.Report.Model
		}
	}
	report.applyView(view)
	return report
}

// applyRootSummaryFallback replaces the view's totals with the checkpoint's
// root token_usage when a session's metadata could not be read: the root
// summary was aggregated over every session at write time, so it is the
// complete total while the readable sessions' sum undercounts. The
// breakdown keeps the readable sessions' attribution; the class shares are
// re-priced over the root total with the report model's base ratios. Returns
// whether the fallback applied.
func applyRootSummaryFallback(view *tokenReportView, in checkpointTokenInputs) bool {
	if in.metadataWarnings == 0 || in.summary == nil || in.summary.TokenUsage == nil {
		return false
	}
	flat := flattenTokenUsage(in.summary.TokenUsage)
	if tokenVolume(flat) == 0 && flat.APICallCount == 0 {
		return false
	}
	flat.Model = ""
	view.Report.Usage = *flat
	view.HasUsage = true
	view.Report.Calls = flat.APICallCount
	view.Subagent = types.TokenUsage{}
	if sub := flattenTokenUsage(in.summary.TokenUsage.SubagentTokens); sub != nil {
		sub.Model = ""
		view.Subagent = *sub
		view.Report.Calls = max(flat.APICallCount-sub.APICallCount, 0)
	}
	view.Report.Cost = tokenreport.CostShares{}
	if w, _, ok := tokenreport.WeightsFor(view.Report.Model); ok {
		view.Report.Cost = tokenreport.ComputeCostShares(flat, w)
	}
	return true
}

// legacyTranscriptSubsets returns the thinking and 1-hour cache-write counts
// a legacy checkpoint's whole-transcript attribution found that the committed
// usage lacks (its subset fields are 0). Nil when there is nothing to add.
// The figures are shown beside the committed totals with a "(from stored
// transcript)" marker and are never added into Report.Usage or the Total: the
// committed total cannot confirm them, and the whole transcript may cover
// more than this checkpoint's window.
func legacyTranscriptSubsets(usage *types.TokenUsage, analyses []sessionTokenAnalysis) *tokenTranscriptSubsets {
	var ft tokenTranscriptSubsets
	for i := range analyses {
		ft.Thinking += analyses[i].transcriptThinking
		ft.CacheWrite1h += analyses[i].transcriptCacheWrite1h
	}
	if usage.ThinkingTokens > 0 {
		ft.Thinking = 0
	}
	if usage.CacheCreation1hTokens > 0 || usage.CacheCreationTokens == 0 {
		ft.CacheWrite1h = 0
	}
	if ft.Thinking == 0 && ft.CacheWrite1h == 0 {
		return nil
	}
	return &ft
}

// legacyInfo builds the JSON `legacy` object: whether the totals may be a
// running total, and whether thinking / cache-TTL counts are known — from
// the committed usage or, marked as such, from the stored transcript.
func legacyInfo(view *tokenReportView, summary *checkpoint.CheckpointSummary, metas []*checkpoint.Metadata) *tokenLegacyInfo {
	info := &tokenLegacyInfo{
		Cumulative:       anyLegacyFromStart(summary, metas),
		ThinkingRecorded: view.Report.Usage.ThinkingTokens > 0,
		CacheTTLRecorded: view.Report.Usage.CacheCreation1hTokens > 0,
	}
	if ft := view.FromTranscript; ft != nil {
		if ft.Thinking > 0 {
			info.ThinkingRecorded = true
			info.ThinkingFromTranscript = ft.Thinking
		}
		if ft.CacheWrite1h > 0 {
			info.CacheTTLRecorded = true
			info.CacheWrite1hFromTranscript = ft.CacheWrite1h
		}
	}
	return info
}

// fillCheckpointTokensIdentity sets the header fields: session count, the
// single session's ID/agent/model, the agent and model lists, the branch.
func fillCheckpointTokensIdentity(report *checkpointTokensReport, summary *checkpoint.CheckpointSummary, metas []*checkpoint.Metadata) {
	if summary != nil {
		report.Branch = summary.Branch
		report.SessionCount = len(summary.Sessions)
	}
	if report.SessionCount == 0 {
		report.SessionCount = len(metas)
	}
	report.Agents = checkpointAgentLabels(metas)
	report.Models = checkpointModelLabels(metas)
	if report.SessionCount == 1 && len(metas) == 1 && metas[0] != nil {
		meta := metas[0]
		report.SessionID = meta.SessionID
		if len(report.Agents) > 0 {
			report.Agent = report.Agents[0]
		}
		if len(report.Models) > 0 {
			report.Model = report.Models[0]
		}
		if report.Branch == "" {
			report.Branch = meta.Branch
		}
	} else if report.Branch == "" {
		report.Branch = firstCheckpointBranch(metas)
	}
}

// analyzeCheckpointTokenSessions attributes every session (best-effort),
// assigns each task record to the session whose window spawned it (leftovers
// go to the last attributed session as orphan rows), then computes each
// session's totals and cost parts.
func analyzeCheckpointTokenSessions(ctx context.Context, in checkpointTokenInputs, legacy bool) []sessionTokenAnalysis {
	analyses := make([]sessionTokenAnalysis, 0, len(in.sessions))
	for i := range in.sessions {
		analyses = append(analyses, attributeCheckpointTokenSession(ctx, in.checkpointID, &in.sessions[i], legacy))
	}
	assignTaskRecords(analyses, in.records)
	for i := range analyses {
		finishSessionTokenAnalysis(&analyses[i])
	}
	return analyses
}

// attributeCheckpointTokenSession runs the agent's TokenAttributor over the
// session's stored transcript, sliced at Metadata.TokenTranscriptStart for a
// version-2 checkpoint and at 0 for a legacy one (which has no token
// offset). Any reason attribution cannot run becomes a note; the session
// then falls back to its committed token_usage in finishSessionTokenAnalysis.
func attributeCheckpointTokenSession(ctx context.Context, cpID id.CheckpointID, s *checkpointTokenSession, legacy bool) sessionTokenAnalysis {
	a := sessionTokenAnalysis{meta: s.meta, legacy: legacy, efforts: make(map[string]int), models: make(map[string]int)}
	meta := s.meta
	if meta == nil {
		return a
	}
	label := fmt.Sprintf("session %d", s.index+1)
	attributor, reason, ok := resolveTokenAttributor(meta.Agent)
	if !ok {
		if reason != "" {
			a.notes = append(a.notes, label+": "+reason+"; totals from committed metadata")
		}
		return a
	}
	if s.transcript == nil {
		a.notes = append(a.notes, label+": stored transcript unavailable; totals from committed metadata")
		if s.transcriptErr != nil {
			logging.Warn(ctx, "checkpoint tokens: session transcript unreadable",
				slog.String("checkpoint_id", cpID.String()),
				slog.Int("session_index", s.index),
				slog.String("error", s.transcriptErr.Error()))
		}
		return a
	}
	start := meta.TokenTranscriptStart
	if legacy {
		start = 0
	}
	attribution, err := attributor.AttributeTokens(s.transcript, start, "")
	if err != nil {
		a.notes = append(a.notes, label+": transcript could not be attributed; totals from committed metadata")
		logging.Warn(ctx, "checkpoint tokens: attribution failed",
			slog.String("checkpoint_id", cpID.String()),
			slog.Int("session_index", s.index),
			slog.String("agent", string(meta.Agent)),
			slog.String("error", err.Error()))
		return a
	}
	if attribution == nil || len(attribution.Calls) == 0 {
		a.notes = append(a.notes, label+": no API calls in the token window; totals from committed metadata")
		return a
	}
	applySkillEventAnchors(attribution, meta.SkillEvents)
	a.attribution = attribution
	return a
}

// applySkillEventAnchors labels skill loads the attributor could not name:
// a tool-use ref whose ID matches a skill event's transcript anchor takes
// that event's skill name. The harness-stamped ActiveSkill on each call
// stays the first source; this is the second.
func applySkillEventAnchors(attribution *types.Attribution, events []types.SkillEvent) {
	names := make(map[string]string)
	for _, e := range events {
		if e.TranscriptAnchor != nil && e.TranscriptAnchor.ToolUseID != "" && e.Skill.Name != "" {
			names[e.TranscriptAnchor.ToolUseID] = e.Skill.Name
		}
	}
	if len(names) == 0 {
		return
	}
	label := func(ref *types.ToolUseRef) {
		if ref.SkillName != "" || ref.SubagentType != "" || ref.Tool == "" {
			return
		}
		if name, ok := names[ref.ID]; ok {
			ref.SkillName = name
			if ref.Detail == "" {
				ref.Detail = name
			}
		}
	}
	for i := range attribution.Calls {
		call := &attribution.Calls[i]
		for j := range call.Emitted {
			label(&call.Emitted[j])
		}
		for j := range call.Consumed {
			label(&call.Consumed[j].ToolUse)
		}
	}
}

// assignTaskRecords gives each committed task record, in StartedAt order, to
// the session whose attributed window emitted or consumed the spawning tool
// call, as a types.SubagentRecord, so tokenreport.Attribute folds it into
// that session's Subagent row. Records no session claims go to the last
// attributed session, where Attribute renders them as orphan rows. With no
// attributed session at all they are dropped: each session's committed
// token_usage already includes its subagents.
func assignTaskRecords(analyses []sessionTokenAnalysis, records []checkpoint.StoredTaskRecord) {
	if len(records) == 0 {
		return
	}
	sorted := slices.Clone(records)
	slices.SortStableFunc(sorted, func(a, b checkpoint.StoredTaskRecord) int { return a.StartedAt.Compare(b.StartedAt) })

	last := -1
	seen := make([]map[string]bool, len(analyses))
	for i := range analyses {
		if analyses[i].attribution == nil {
			continue
		}
		last = i
		seen[i] = toolUseIDsSeen(analyses[i].attribution)
	}
	for _, rec := range sorted {
		owner := last
		for i := range analyses {
			if seen[i] != nil && seen[i][rec.ToolUseID] {
				owner = i
				break
			}
		}
		if owner < 0 {
			// No attributed session: the committed token_usage each session
			// falls back to already includes its subagents.
			continue
		}
		analyses[owner].attribution.Subagents = append(analyses[owner].attribution.Subagents, subagentRecordFromTask(rec))
	}
}

// toolUseIDsSeen collects every tool-use ID an attribution emitted or consumed.
func toolUseIDsSeen(a *types.Attribution) map[string]bool {
	seen := make(map[string]bool)
	for _, call := range a.Calls {
		for _, ref := range call.Emitted {
			seen[ref.ID] = true
		}
		for _, res := range call.Consumed {
			seen[res.ToolUse.ID] = true
		}
	}
	return seen
}

// subagentRecordFromTask converts a committed task record to the attribution
// input type. The record carries no model of its own; TokenUsage.Model, when
// set, is what Attribute's precedence reads.
func subagentRecordFromTask(rec checkpoint.StoredTaskRecord) types.SubagentRecord {
	return types.SubagentRecord{
		ToolUseID:    rec.ToolUseID,
		SubagentType: rec.SubagentType,
		Usage:        rec.TokenUsage,
		Start:        rec.StartedAt,
		End:          rec.CompletedAt,
	}
}

// finishSessionTokenAnalysis computes a session's totals, cost parts,
// duration, call and effort counts. An attributed version-2 session is
// recomputed from its calls plus its subagent records; a legacy session
// keeps its breakdown but takes totals, calls and class shares from its
// committed token_usage (see sessionTokenAnalysis.legacy); any other session
// uses its committed token_usage, priced with the session model's base
// ratios.
func finishSessionTokenAnalysis(a *sessionTokenAnalysis) {
	if a.attribution == nil {
		a.finishFromMetadata()
		return
	}
	attr := a.attribution
	if a.legacy {
		a.finishLegacyFromTranscript()
		return
	}
	a.recomputed = true
	var usage *types.TokenUsage
	for i := range attr.Calls {
		call := &attr.Calls[i]
		if call.UsageUnknown {
			a.unknownUsageCalls++
		}
		u := call.Usage
		u.Model = ""
		usage = types.AddTokenUsage(usage, &u)
		a.calls += call.Usage.APICallCount
		if call.Effort != "" {
			a.efforts[call.Effort]++
		}
		if call.Model != "" {
			a.models[call.Model]++
		}
		if w, ok := callWeights(call); ok {
			// A per-call usage block records its TTL split: 0 means all 5m.
			a.costParts = append(a.costParts, tokenreport.ComputeCostSharesKnownTTL(&call.Usage, w))
		}
	}
	for i := range attr.Subagents {
		rec := &attr.Subagents[i]
		if rec.Usage == nil {
			continue
		}
		u := *rec.Usage
		u.Model = ""
		a.subagent = types.AddTokenUsage(a.subagent, &u)
		model := rec.Model
		if model == "" {
			model = rec.Usage.Model
		}
		if w, _, ok := tokenreport.WeightsFor(model); ok {
			a.costParts = append(a.costParts, tokenreport.ComputeCostSharesKnownTTL(rec.Usage, w))
		}
	}
	a.usage = types.AddTokenUsage(usage, a.subagent)
	a.attributed = tokenreport.Attribute(attr, nil)
	a.agentReportedCost = attr.AgentReportedCost
	a.unmatchedSubagentRefs = countUnmatchedSubagentRefs(attr)
	a.duration = attributionDuration(attr, a.meta)
}

// attributionDuration is the transcript's span (End − Start), falling back
// to the hook-reported session duration when the slice has no timestamps.
func attributionDuration(attr *types.Attribution, meta *checkpoint.Metadata) time.Duration {
	if !attr.Start.IsZero() && !attr.End.IsZero() && attr.End.After(attr.Start) {
		return attr.End.Sub(attr.Start)
	}
	return metadataDuration(meta)
}

// finishLegacyFromTranscript fills a legacy session: the contributor table,
// effort and model counts, duration and the calls-without-usage count come
// from the attributed transcript; totals, calls and class shares from the
// committed token_usage, whose cache writes carry no TTL and so stay
// unpriced (tokenreport.CostShares.CacheWriteUnpriced).
func (a *sessionTokenAnalysis) finishLegacyFromTranscript() {
	attr := a.attribution
	for i := range attr.Calls {
		call := &attr.Calls[i]
		if call.UsageUnknown {
			a.unknownUsageCalls++
		}
		if call.Effort != "" {
			a.efforts[call.Effort]++
		}
		if call.Model != "" {
			a.models[call.Model]++
		}
		a.transcriptThinking += call.Usage.ThinkingTokens
		a.transcriptCacheWrite1h += call.Usage.CacheCreation1hTokens
	}
	a.attributed = tokenreport.Attribute(attr, nil)
	a.agentReportedCost = attr.AgentReportedCost
	a.unmatchedSubagentRefs = countUnmatchedSubagentRefs(attr)
	a.duration = attributionDuration(attr, a.meta)
	a.finishFromMetadata()
}

// finishFromMetadata fills a session's totals from its committed
// token_usage, which already includes any subagent usage. The duration is
// the hook-reported one unless the transcript already supplied it.
func (a *sessionTokenAnalysis) finishFromMetadata() {
	if a.duration == 0 {
		a.duration = metadataDuration(a.meta)
	}
	if a.meta == nil || a.meta.TokenUsage == nil {
		return
	}
	flat := flattenTokenUsage(a.meta.TokenUsage)
	flat.Model = ""
	a.usage = flat
	a.subagent = flattenTokenUsage(a.meta.TokenUsage.SubagentTokens)
	a.calls = flat.APICallCount
	if a.subagent != nil {
		a.calls = max(flat.APICallCount-a.subagent.APICallCount, 0)
	}
	if a.meta.Model != "" {
		a.models[a.meta.Model] += max(flat.APICallCount, 1)
		if w, _, ok := tokenreport.WeightsFor(a.meta.Model); ok {
			a.costParts = append(a.costParts, tokenreport.ComputeCostShares(flat, w))
		}
	}
}

// callWeights returns the price ratios for one call at the long-context tier
// its total input puts it in; false for an unknown or unrecorded model.
func callWeights(call *types.CallUsage) (tokenreport.Weights, bool) {
	if _, _, ok := tokenreport.WeightsFor(call.Model); !ok {
		return tokenreport.Weights{}, false
	}
	u := &call.Usage
	return tokenreport.WeightsForCall(call.Model, u.InputTokens+u.CacheReadTokens+u.CacheCreationTokens), true
}

// countUnmatchedSubagentRefs counts the distinct subagent tool calls seen in
// the window — emitted, or consumed from before it — that no SubagentRecord
// accounts for: their tokens are not in the report.
func countUnmatchedSubagentRefs(a *types.Attribution) int {
	recorded := make(map[string]bool, len(a.Subagents))
	for _, rec := range a.Subagents {
		recorded[rec.ToolUseID] = true
	}
	unmatched := make(map[string]bool)
	note := func(ref *types.ToolUseRef) {
		if ref.SubagentType != "" && !recorded[ref.ID] {
			unmatched[ref.ID] = true
		}
	}
	for i := range a.Calls {
		call := &a.Calls[i]
		for j := range call.Emitted {
			note(&call.Emitted[j])
		}
		for j := range call.Consumed {
			note(&call.Consumed[j].ToolUse)
		}
	}
	return len(unmatched)
}

// metadataDuration is the hook-reported session duration, or 0.
func metadataDuration(meta *checkpoint.Metadata) time.Duration {
	if meta == nil || meta.SessionMetrics == nil || meta.SessionMetrics.DurationMs <= 0 {
		return 0
	}
	return time.Duration(meta.SessionMetrics.DurationMs) * time.Millisecond
}

// assembleTokenReportView merges the per-session analyses into the view the
// renderer prints: summed usage, merged contributors (Report.Sessions counts
// the attributed sessions merged), cost shares summed by units, the modal
// model and effort, summed duration and calls.
func assembleTokenReportView(analyses []sessionTokenAnalysis, metas []*checkpoint.Metadata) tokenReportView {
	var view tokenReportView
	var usage, subagent *types.TokenUsage
	var perSession []tokenreport.Attributed
	var costParts []tokenreport.CostShares
	efforts, models := make(map[string]int), make(map[string]int)
	for i := range analyses {
		a := &analyses[i]
		usage = types.AddTokenUsage(usage, a.usage)
		subagent = types.AddTokenUsage(subagent, a.subagent)
		if a.attribution != nil {
			perSession = append(perSession, a.attributed)
			view.Attributed = true
		}
		costParts = append(costParts, a.costParts...)
		view.Report.Duration += a.duration
		view.Report.Calls += a.calls
		view.UnknownUsageCalls += a.unknownUsageCalls
		view.AgentReportedCost += a.agentReportedCost
		for k, n := range a.efforts {
			efforts[k] += n
		}
		for k, n := range a.models {
			models[k] += n
		}
	}
	view.Report.Agent = reportAgent(metas)
	view.Report.Profile = tokenreport.ProfileFor(view.Report.Agent)
	view.Report.Model = modalKey(models)
	view.Report.Effort, view.EffortCalls = modalKeyCount(efforts)
	if usage != nil {
		view.HasUsage = tokenVolume(usage) > 0 || usage.APICallCount > 0
		usage.SubagentTokens = nil
		view.Report.Usage = *usage
	}
	if subagent != nil {
		subagent.SubagentTokens = nil
		view.Subagent = *subagent
	}
	view.Report.Sessions = len(perSession)
	if len(perSession) == 1 {
		view.Report.Attributed = perSession[0]
	} else if len(perSession) > 1 {
		view.Report.Attributed = tokenreport.MergeContributors(perSession)
	}
	view.Report.Cost = tokenreport.SumCostShares(costParts...)
	return view
}

// reportAgent is the agent whose gates and profile the report uses: the one
// agent of a single-agent checkpoint, else the agent of the most sessions
// (ties: first seen).
func reportAgent(metas []*checkpoint.Metadata) types.AgentType {
	counts := make(map[string]int)
	var order []string
	for _, m := range metas {
		if m == nil || m.Agent == "" {
			continue
		}
		key := string(m.Agent)
		if counts[key] == 0 {
			order = append(order, key)
		}
		counts[key]++
	}
	best := ""
	for _, key := range order {
		if best == "" || counts[key] > counts[best] {
			best = key
		}
	}
	return types.AgentType(best)
}

// modalKey returns the key with the highest count (ties: lexically first),
// or "" for an empty map.
func modalKey(counts map[string]int) string {
	k, _ := modalKeyCount(counts)
	return k
}

// modalKeyCount returns the key with the highest count and that count.
func modalKeyCount(counts map[string]int) (string, int) {
	best, bestN := "", 0
	for _, k := range slices.Sorted(maps.Keys(counts)) {
		if n := counts[k]; n > bestN {
			best, bestN = k, n
		}
	}
	return best, bestN
}

// anyLegacyFromStart reports whether any session of a legacy checkpoint is
// tokenreport.ScopeLegacyFromStart — its token_usage may be a running total.
func anyLegacyFromStart(summary *checkpoint.CheckpointSummary, metas []*checkpoint.Metadata) bool {
	version := 0
	if summary != nil {
		version = summary.TokenUsageVersion
	}
	for _, m := range metas {
		if m == nil {
			continue
		}
		row := tokenreport.CheckpointRow{Version: version, CheckpointTranscriptStart: m.CheckpointTranscriptStart}
		if tokenreport.ClassifyScope(row) == tokenreport.ScopeLegacyFromStart {
			return true
		}
	}
	return false
}

// checkpointTokenNotes collects the report-level notes: unreadable metadata
// (and whether the root summary stood in for the totals),
// unreadable task records, per-session attribution notes, subagent calls
// without records (worded per agent), the mixed-source note, and the
// multi-session lower-bound note.
func checkpointTokenNotes(in checkpointTokenInputs, analyses []sessionTokenAnalysis, legacy, rootFallback bool) []string {
	var notes []string
	switch {
	case rootFallback:
		notes = append(notes, fmt.Sprintf("%d session metadata file%s could not be read; totals are the checkpoint's root summary (aggregated at write time) and the breakdown covers only the readable sessions.",
			in.metadataWarnings, tokenPluralSuffix(in.metadataWarnings)))
	case in.metadataWarnings > 0:
		notes = append(notes, fmt.Sprintf("%d session metadata file%s could not be read; those sessions are not in the totals.",
			in.metadataWarnings, tokenPluralSuffix(in.metadataWarnings)))
	}
	if in.recordsErr != nil {
		notes = append(notes, "subagent task records could not be read; subagent usage may be missing.")
	}
	recomputed, fallback := countTokenSources(analyses)
	attributed, unmatched := 0, 0
	var unmatchedAgent types.AgentType
	for i := range analyses {
		a := &analyses[i]
		notes = append(notes, a.notes...)
		if a.attribution != nil {
			attributed++
		}
		if a.unmatchedSubagentRefs > 0 {
			unmatched += a.unmatchedSubagentRefs
			if a.meta != nil {
				unmatchedAgent = a.meta.Agent
			}
		}
	}
	if unmatched > 0 {
		notes = append(notes, unmatchedSubagentNote(unmatchedAgent, unmatched))
	}
	if recomputed > 0 && fallback > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d sessions recomputed from their transcripts; the rest use committed token_usage.", recomputed, recomputed+fallback))
	}
	if attributed > 1 {
		notes = append(notes, fmt.Sprintf("Breakdown merged across %d sessions; sub-row call and token counts are lower bounds.", attributed))
	}
	if legacy && attributed > 0 && !anyLegacyFromStart(in.summary, metasOf(analyses)) {
		notes = append(notes, "Legacy checkpoint: the breakdown covers the whole stored transcript, not only this checkpoint's window.")
	}
	return notes
}

// countTokenSources counts the sessions whose totals were recomputed from
// their transcripts and those that fell back to committed token_usage.
func countTokenSources(analyses []sessionTokenAnalysis) (recomputed, fallback int) {
	for i := range analyses {
		switch {
		case analyses[i].recomputed:
			recomputed++
		case analyses[i].usage != nil:
			fallback++
		}
	}
	return recomputed, fallback
}

// unmatchedSubagentNote words the "subagent tokens not included" note for
// the agent: Codex and OpenCode run subagents as separate sessions with no
// task record (separateSessionSubagentNote); other agents may simply lack
// the record on this backend.
func unmatchedSubagentNote(agentType types.AgentType, n int) string {
	if note := separateSessionSubagentNote(agentType); note != "" {
		return note
	}
	return fmt.Sprintf("%d subagent call%s %s no committed task record; that usage is not included (this backend may not store task records).",
		n, tokenPluralSuffix(n), pluralHaveHas(n))
}

// pluralHaveHas picks "has"/"have" for n.
func pluralHaveHas(n int) string {
	if n == 1 {
		return "has"
	}
	return "have"
}

// metasOf extracts the metadata of each analysis.
func metasOf(analyses []sessionTokenAnalysis) []*checkpoint.Metadata {
	metas := make([]*checkpoint.Metadata, 0, len(analyses))
	for i := range analyses {
		metas = append(metas, analyses[i].meta)
	}
	return metas
}

// mergeModelLabels appends model to labels when it is new and non-empty.
func mergeModelLabels(labels []string, model string) []string {
	if model == "" || slices.Contains(labels, model) {
		return labels
	}
	return append(labels, model)
}

// checkpointAgentLabels lists each distinct agent across the sessions, in
// order of first appearance; a session without an agent is "(unknown)".
func checkpointAgentLabels(metas []*checkpoint.Metadata) []string {
	labels := make([]string, 0, len(metas))
	for _, meta := range metas {
		label := unknownPlaceholder
		if meta != nil && meta.Agent != "" {
			label = string(meta.Agent)
		}
		if !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}

// checkpointModelLabels lists each distinct recorded model across the
// sessions, in order of first appearance.
func checkpointModelLabels(metas []*checkpoint.Metadata) []string {
	labels := make([]string, 0, len(metas))
	for _, meta := range metas {
		if meta == nil || meta.Model == "" || slices.Contains(labels, meta.Model) {
			continue
		}
		labels = append(labels, meta.Model)
	}
	return labels
}

// firstCheckpointBranch is the first non-empty session branch.
func firstCheckpointBranch(metas []*checkpoint.Metadata) string {
	for _, meta := range metas {
		if meta != nil && meta.Branch != "" {
			return meta.Branch
		}
	}
	return ""
}

// tokenPluralSuffix is "s" unless count is 1.
func tokenPluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

// aggregateCheckpointTokenUsage sums the sessions' committed token_usage.
func aggregateCheckpointTokenUsage(metas []*checkpoint.Metadata) *types.TokenUsage {
	var total *types.TokenUsage
	for _, meta := range metas {
		if meta == nil {
			continue
		}
		total = types.AddTokenUsage(total, meta.TokenUsage)
	}
	return total
}

// checkpointTokenUsage is a checkpoint's committed token usage: the sum of
// its readable sessions, or the root summary's aggregate when a session's
// metadata could not be read (the root was aggregated at write time).
func checkpointTokenUsage(summary *checkpoint.CheckpointSummary, metas []*checkpoint.Metadata, metadataReadWarning bool) *types.TokenUsage {
	sessionUsage := aggregateCheckpointTokenUsage(metas)
	if !metadataReadWarning && sessionUsage != nil {
		return sessionUsage
	}
	if summary != nil && summary.TokenUsage != nil {
		return summary.TokenUsage
	}
	return sessionUsage
}
