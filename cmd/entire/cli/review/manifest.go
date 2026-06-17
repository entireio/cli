package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

const localReviewManifestVersion = 1

// reviewTokenMaxDepth caps recursion when summing SubagentTokens. Subagent
// trees are shallow in practice (single-digit depth), so this is defensive
// insurance against a malformed/cyclic *agent.TokenUsage causing stack
// overflow during a review run.
const reviewTokenMaxDepth = 16

// agentTypeLookup resolves an agent.AgentType to its Agent implementation.
// Threaded as an explicit dependency through the hydration helpers so tests
// can inject a fake without mutating the package-level agent registry — and
// without parallel-test footguns from a shared mutable variable.
type agentTypeLookup func(agenttypes.AgentType) (agent.Agent, error)

// LocalReviewManifest records one local `entire review` invocation. It lets
// `entire review --fix <session-id>` use a single session id as the lookup
// handle while still loading sibling agent outputs from the same review run.
type LocalReviewManifest struct {
	Version         int              `json:"version"`
	WorktreePath    string           `json:"worktree_path"`
	CreatedAt       time.Time        `json:"created_at"`
	StartingSHA     string           `json:"starting_sha,omitempty"`
	Sources         []ManifestSource `json:"sources"`
	AggregateOutput string           `json:"aggregate_output,omitempty"`
}

type ManifestSource struct {
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Label     string `json:"label"`
	Status    string `json:"status,omitempty"`
	Output    string `json:"output,omitempty"`
}

func buildLocalReviewManifestFromSummary(
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	states []*session.State,
	aggregateOutput string,
) LocalReviewManifest {
	manifest := LocalReviewManifest{
		Version:         localReviewManifestVersion,
		WorktreePath:    worktreeRoot,
		CreatedAt:       summary.StartedAt,
		StartingSHA:     headSHA,
		AggregateOutput: strings.TrimSpace(aggregateOutput),
	}
	usedSessions := map[string]bool{}
	for _, run := range summary.AgentRuns {
		st := matchReviewSessionState(worktreeRoot, headSHA, summary.StartedAt, run.Name, states, usedSessions)
		if st == nil || st.SessionID == "" {
			continue
		}
		usedSessions[st.SessionID] = true
		manifest.Sources = append(manifest.Sources, ManifestSource{
			SessionID: st.SessionID,
			Agent:     run.Name,
			Label:     labelForReviewAgent(run.Name),
			Status:    run.Status.String(),
			Output:    agentRunOutput(run),
		})
	}
	return manifest
}

func localReviewManifestFromCurrentState(
	ctx context.Context,
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	aggregateOutput string,
) (LocalReviewManifest, []*session.State, error) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return LocalReviewManifest{}, nil, fmt.Errorf("create session state store: %w", err)
	}
	states, err := store.List(ctx)
	if err != nil {
		return LocalReviewManifest{}, nil, fmt.Errorf("list session states: %w", err)
	}
	return buildLocalReviewManifestFromSummary(worktreeRoot, headSHA, summary, states, aggregateOutput), states, nil
}

// explainEmptyManifest returns a single-line diagnostic explaining why
// matchReviewSessionState produced no matches for any agent run in summary,
// plus a sentinel flag indicating the function fell through every known
// rejection cause. The sentinel means matcher and explainer drifted and
// callers should escalate logging.
//
// Filter precedence mirrors matchReviewSessionState: worktree path,
// BaseCommit, StartedAt window, then AgentType. Filters apply cumulatively
// to a candidate set; the function reports the filter that empties the
// set. This matters for heterogeneous failures across multiple tagged
// states (e.g. one wrong-worktree, one right-worktree but wrong-SHA): the
// reported cause is the filter that eliminated the last surviving
// candidate, not the first filter to find any non-matching state.
// AgentType is checked per-agent so a multi-agent run with heterogeneous
// type mismatches names the specific failing agent.
func explainEmptyManifest(
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	states []*session.State,
) (reason string, sentinel bool) {
	if len(states) == 0 {
		return "no session states found (lifecycle hook never created session state for any agent in this run)", false
	}
	tagged := make([]*session.State, 0, len(states))
	for _, st := range states {
		if st != nil && st.Kind == session.KindAgentReview {
			tagged = append(tagged, st)
		}
	}
	if len(tagged) == 0 {
		return fmt.Sprintf("found %d session state(s) but none tagged as a review session (env-var handshake did not reach the hook)", len(states)), false
	}

	candidates := tagged

	// Empty-SessionID filter (cumulative). The matcher returns these states,
	// but buildLocalReviewManifestFromSummary drops them on st.SessionID == ""
	// before adding a manifest source — without an explicit explainer cause,
	// the sentinel would fire and surface a misleading "report this as a bug"
	// for what is really a partial-write or corrupt-state-file condition.
	survivors, _ := applyExplainerFilter(candidates, func(st *session.State) bool {
		return st.SessionID != ""
	})
	if len(survivors) == 0 {
		return fmt.Sprintf("found %d tagged review session(s) but all have empty SessionID (partial write or corrupt state file)", len(tagged)), false
	}
	candidates = survivors

	// Worktree filter (cumulative).
	var droppedExample *session.State
	survivors, droppedExample = applyExplainerFilter(candidates, func(st *session.State) bool {
		return worktreeRoot == "" || st.WorktreePath == "" || st.WorktreePath == worktreeRoot
	})
	if len(survivors) == 0 {
		return fmt.Sprintf("found %d tagged review session(s) but worktree path mismatch: state=%q, run=%q", len(tagged), droppedExample.WorktreePath, worktreeRoot), false
	}
	candidates = survivors

	// BaseCommit filter (cumulative).
	survivors, droppedExample = applyExplainerFilter(candidates, func(st *session.State) bool {
		return headSHA == "" || st.BaseCommit == "" || st.BaseCommit == headSHA
	})
	if len(survivors) == 0 {
		return fmt.Sprintf("found %d tagged review session(s) but BaseCommit mismatch: state=%q, run=%q (HEAD moved between review start and first agent turn?)", len(tagged), droppedExample.BaseCommit, headSHA), false
	}
	candidates = survivors

	// StartedAt window filter (cumulative).
	survivors, _ = applyExplainerFilter(candidates, func(st *session.State) bool {
		return summary.StartedAt.IsZero() || !st.StartedAt.Before(summary.StartedAt.Add(-5*time.Second))
	})
	if len(survivors) == 0 {
		return fmt.Sprintf("found %d tagged review session(s) but they started before the review run window (stale session state from a prior run?)", len(tagged)), false
	}
	candidates = survivors

	// AgentType filter (per-agent). Each run's wantType is checked against
	// the remaining candidates; if no candidate's AgentType matches, that
	// specific agent is named. Lenient cases (state.AgentType=="" or
	// wantType=="") count as a match, matching the matcher's behavior. The
	// observed-type list deduplicates and sorts so the diagnostic is stable
	// across store.List orderings and faithfully represents the full set of
	// mismatched types rather than whichever happened to be iterated last.
	for _, run := range summary.AgentRuns {
		wantType := agentTypeForReviewAgent(run.Name)
		if wantType == "" {
			continue
		}
		seen := map[string]struct{}{}
		observedTypes := []string{}
		anyMatch := false
		for _, st := range candidates {
			if st.AgentType == "" || st.AgentType == wantType {
				anyMatch = true
				break
			}
			t := string(st.AgentType)
			if _, ok := seen[t]; !ok {
				seen[t] = struct{}{}
				observedTypes = append(observedTypes, t)
			}
		}
		if !anyMatch {
			sort.Strings(observedTypes)
			return fmt.Sprintf("found %d tagged review session(s) but AgentType mismatch for agent %q: state=%q, run=%q", len(tagged), run.Name, strings.Join(observedTypes, ", "), wantType), false
		}
	}

	return fmt.Sprintf("found %d tagged review session(s) but matcher rejected all of them (no filter explained the rejection — please report this as a bug)", len(tagged)), true
}

// applyExplainerFilter returns the subset of candidates for which keep is
// true plus a pointer to the first dropped state (or nil if none dropped).
// The dropped example is used to populate observed-vs-expected values in
// the diagnostic when a filter empties the candidate set.
func applyExplainerFilter(candidates []*session.State, keep func(*session.State) bool) (survivors []*session.State, droppedExample *session.State) {
	for _, st := range candidates {
		if keep(st) {
			survivors = append(survivors, st)
			continue
		}
		if droppedExample == nil {
			droppedExample = st
		}
	}
	return survivors, droppedExample
}

func hydrateReviewSummaryTokensFromCurrentState(
	ctx context.Context,
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	lookup agentTypeLookup,
) (reviewtypes.RunSummary, error) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return summary, fmt.Errorf("create session state store: %w", err)
	}
	states, err := store.List(ctx)
	if err != nil {
		return summary, fmt.Errorf("list session states: %w", err)
	}
	return hydrateReviewSummaryTokensFromStates(ctx, worktreeRoot, headSHA, summary, states, lookup), nil
}

func hydrateReviewAgentRunTokensFromCurrentState(
	ctx context.Context,
	worktreeRoot string,
	headSHA string,
	run reviewtypes.AgentRun,
	lookup agentTypeLookup,
) (reviewtypes.AgentRun, error) {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		return run, fmt.Errorf("create session state store: %w", err)
	}
	states, err := store.List(ctx)
	if err != nil {
		return run, fmt.Errorf("list session states: %w", err)
	}
	return hydrateReviewAgentRunTokensFromStates(ctx, worktreeRoot, headSHA, run, states, lookup), nil
}

func hydrateReviewAgentRunTokensFromStates(
	ctx context.Context,
	worktreeRoot string,
	headSHA string,
	run reviewtypes.AgentRun,
	states []*session.State,
	lookup agentTypeLookup,
) reviewtypes.AgentRun {
	st := matchReviewSessionState(worktreeRoot, headSHA, run.StartedAt, run.Name, states, map[string]bool{})
	if st == nil || st.SessionID == "" {
		return run
	}
	tokens := reviewTokensFromTokenUsage(reviewTokenUsageForSession(ctx, st, lookup))
	if tokens.In == 0 && tokens.Out == 0 {
		return run
	}
	run.Tokens = tokens
	return run
}

func hydrateReviewSummaryTokensFromStates(
	ctx context.Context,
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	states []*session.State,
	lookup agentTypeLookup,
) reviewtypes.RunSummary {
	usedSessions := map[string]bool{}
	for i, run := range summary.AgentRuns {
		st := matchReviewSessionState(worktreeRoot, headSHA, summary.StartedAt, run.Name, states, usedSessions)
		if st == nil || st.SessionID == "" {
			continue
		}
		usedSessions[st.SessionID] = true
		tokens := reviewTokensFromTokenUsage(reviewTokenUsageForSession(ctx, st, lookup))
		if tokens.In == 0 && tokens.Out == 0 {
			continue
		}
		summary.AgentRuns[i].Tokens = tokens
	}
	return summary
}

func reviewTokenUsageForSession(ctx context.Context, st *session.State, lookup agentTypeLookup) *agent.TokenUsage {
	if st == nil {
		return nil
	}
	if hasReviewTokenUsageData(st.TokenUsage) {
		return st.TokenUsage
	}
	if st.TranscriptPath == "" || st.AgentType == "" {
		return nil
	}
	if lookup == nil {
		lookup = agent.GetByAgentType
	}
	ag, err := lookup(st.AgentType)
	if err != nil {
		// Distinct from "no token data" — the session references an agent
		// that's not in the registry. Surfacing this at Debug lets operators
		// triage "tokens missing" reports without source-diving.
		logging.Debug(ctx, "review token usage: agent type not registered",
			slog.String("session_id", st.SessionID),
			slog.String("agent_type", string(st.AgentType)),
			slog.String("error", err.Error()))
		return nil
	}
	transcript, err := os.ReadFile(st.TranscriptPath)
	if err != nil {
		logging.Debug(ctx, "review token usage: transcript read failed",
			slog.String("session_id", st.SessionID),
			slog.String("path", st.TranscriptPath),
			slog.String("error", err.Error()))
		return nil
	}
	return agent.CalculateTokenUsage(ctx, ag, transcript, st.CheckpointTranscriptStart, reviewSubagentsDir(st))
}

func reviewSubagentsDir(st *session.State) string {
	if st == nil || st.TranscriptPath == "" || st.SessionID == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(st.TranscriptPath), st.SessionID, "subagents")
}

func reviewTokensFromTokenUsage(usage *agent.TokenUsage) reviewtypes.Tokens {
	return reviewTokensFromTokenUsageAtDepth(usage, 0)
}

func reviewTokensFromTokenUsageAtDepth(usage *agent.TokenUsage, depth int) reviewtypes.Tokens {
	if usage == nil || depth >= reviewTokenMaxDepth {
		return reviewtypes.Tokens{}
	}
	tokens := reviewtypes.Tokens{
		In:  usage.InputTokens + usage.CacheCreationTokens + usage.CacheReadTokens,
		Out: usage.OutputTokens,
	}
	subagentTokens := reviewTokensFromTokenUsageAtDepth(usage.SubagentTokens, depth+1)
	tokens.In += subagentTokens.In
	tokens.Out += subagentTokens.Out
	return tokens
}

func hasReviewTokenUsageData(usage *agent.TokenUsage) bool {
	return hasReviewTokenUsageDataAtDepth(usage, 0)
}

func hasReviewTokenUsageDataAtDepth(usage *agent.TokenUsage, depth int) bool {
	if usage == nil || depth >= reviewTokenMaxDepth {
		return false
	}
	if usage.InputTokens != 0 || usage.CacheCreationTokens != 0 || usage.CacheReadTokens != 0 || usage.OutputTokens != 0 || usage.APICallCount != 0 {
		return true
	}
	return hasReviewTokenUsageDataAtDepth(usage.SubagentTokens, depth+1)
}

func matchReviewSessionState(
	worktreeRoot string,
	headSHA string,
	runStartedAt time.Time,
	agentName string,
	states []*session.State,
	used map[string]bool,
) *session.State {
	wantAgentType := agentTypeForReviewAgent(agentName)
	var best *session.State
	for _, st := range states {
		if st == nil || used[st.SessionID] || st.Kind != session.KindAgentReview {
			continue
		}
		if worktreeRoot != "" && st.WorktreePath != "" && st.WorktreePath != worktreeRoot {
			continue
		}
		if headSHA != "" && st.BaseCommit != "" && st.BaseCommit != headSHA {
			continue
		}
		if !runStartedAt.IsZero() && st.StartedAt.Before(runStartedAt.Add(-5*time.Second)) {
			continue
		}
		if wantAgentType != "" && st.AgentType != "" && st.AgentType != wantAgentType {
			continue
		}
		if best == nil || st.StartedAt.After(best.StartedAt) {
			best = st
		}
	}
	return best
}

func agentTypeForReviewAgent(agentName string) agenttypes.AgentType {
	ag, err := agent.Get(agenttypes.AgentName(agentName))
	if err != nil {
		return ""
	}
	return ag.Type()
}

func labelForReviewAgent(agentName string) string {
	if typ := agentTypeForReviewAgent(agentName); typ != "" {
		return string(typ)
	}
	return agentName
}

func agentRunOutput(run reviewtypes.AgentRun) string {
	if narrative := joinAssistantText(run.Buffer); narrative != "" {
		return narrative
	}
	if run.Err != nil {
		return "Failed: " + run.Err.Error()
	}
	return ""
}

func writeLocalReviewManifest(ctx context.Context, manifest LocalReviewManifest) error {
	if len(manifest.Sources) == 0 {
		return errors.New("review manifest has no sources")
	}
	if manifest.Version == 0 {
		manifest.Version = localReviewManifestVersion
	}
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now()
	}

	dir, err := localReviewManifestDir(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create review manifest dir: %w", err)
	}

	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode review manifest: %w", err)
	}
	path := filepath.Join(dir, localReviewManifestFilename(manifest))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write review manifest: %w", err)
	}
	return nil
}

func resolveLocalReviewManifestBySessionID(ctx context.Context, worktreeRoot, sessionID string) (LocalReviewManifest, ManifestSource, error) {
	manifests, err := loadLocalReviewManifests(ctx, worktreeRoot)
	if err != nil {
		return LocalReviewManifest{}, ManifestSource{}, err
	}

	var (
		matches       []LocalReviewManifest
		sourceMatches []ManifestSource
	)
	for _, manifest := range manifests {
		for _, source := range manifest.Sources {
			if source.SessionID == sessionID || strings.HasPrefix(source.SessionID, sessionID) {
				matches = append(matches, manifest)
				sourceMatches = append(sourceMatches, source)
				break
			}
		}
	}
	switch len(matches) {
	case 0:
		return LocalReviewManifest{}, ManifestSource{}, fmt.Errorf("review session %q not found", sessionID)
	case 1:
		return matches[0], sourceMatches[0], nil
	default:
		return LocalReviewManifest{}, ManifestSource{}, fmt.Errorf("review session prefix %q is ambiguous", sessionID)
	}
}

func loadLocalReviewManifests(ctx context.Context, worktreeRoot string) ([]LocalReviewManifest, error) {
	dir, err := localReviewManifestDir(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read review manifest dir: %w", err)
	}

	manifests := make([]LocalReviewManifest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // entry names come directly from os.ReadDir(dir).
		if readErr != nil {
			return nil, fmt.Errorf("read review manifest %s: %w", entry.Name(), readErr)
		}
		var manifest LocalReviewManifest
		if err := json.Unmarshal(b, &manifest); err != nil {
			return nil, fmt.Errorf("decode review manifest %s: %w", entry.Name(), err)
		}
		if worktreeRoot != "" && manifest.WorktreePath != "" && manifest.WorktreePath != worktreeRoot {
			continue
		}
		manifests = append(manifests, manifest)
	}
	sort.SliceStable(manifests, func(i, j int) bool {
		return manifests[i].CreatedAt.After(manifests[j].CreatedAt)
	})
	return manifests, nil
}

func localReviewManifestDir(ctx context.Context) (string, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	commonDir, err := runGit(ctx, worktreeRoot, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	commonDir = strings.TrimSpace(commonDir)
	if commonDir == "" {
		return "", errors.New("git common dir is empty")
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreeRoot, commonDir)
	}
	return filepath.Join(commonDir, "entire-review", "manifests"), nil
}

func localReviewManifestFilename(manifest LocalReviewManifest) string {
	name := manifest.CreatedAt.UTC().Format("20060102T150405")
	if len(manifest.Sources) > 0 && manifest.Sources[0].SessionID != "" {
		name += "-" + safeManifestFilenamePart(manifest.Sources[0].SessionID)
	}
	return name + ".json"
}

func safeManifestFilenamePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "review"
	}
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
}
