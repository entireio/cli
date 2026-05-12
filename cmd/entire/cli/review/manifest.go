package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

const localReviewManifestVersion = 1

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
// matchReviewSessionState produced no matches for any agent run in summary.
// It inspects the candidate session states against the same filters used by
// the matcher and reports the most likely cause: missing tag, worktree path
// mismatch, BaseCommit mismatch, StartedAt window, or AgentType mismatch.
//
// The diagnostic surfaces in the user-facing warning printed by
// warnManifestNotWritten when the manifest has no sources, so the user can
// distinguish a true env-handshake failure from one of the four other
// post-tag filter rejections.
//
// Filter precedence in this function matches matchReviewSessionState so the
// reported cause aligns with what the matcher actually decided.
func explainEmptyManifest(
	worktreeRoot string,
	headSHA string,
	summary reviewtypes.RunSummary,
	states []*session.State,
) string {
	if len(states) == 0 {
		return "no session states found (lifecycle hook never created session state for any agent in this run)"
	}
	tagged := make([]*session.State, 0, len(states))
	for _, st := range states {
		if st != nil && st.Kind == session.KindAgentReview {
			tagged = append(tagged, st)
		}
	}
	if len(tagged) == 0 {
		return fmt.Sprintf("found %d session state(s) but none tagged as a review session (env-var handshake did not reach the hook)", len(states))
	}
	// At least one tagged session exists. Check each post-tag filter against
	// any one of the run's agents — we just need one representative cause.
	// Pick the first agent in summary.AgentRuns since they all share the
	// same worktree/SHA/StartedAt window.
	var runName string
	if len(summary.AgentRuns) > 0 {
		runName = summary.AgentRuns[0].Name
	}
	wantType := agentTypeForReviewAgent(runName)
	for _, st := range tagged {
		if worktreeRoot != "" && st.WorktreePath != "" && st.WorktreePath != worktreeRoot {
			return fmt.Sprintf("found %d tagged review session(s) but worktree path mismatch: state=%q, run=%q", len(tagged), st.WorktreePath, worktreeRoot)
		}
	}
	for _, st := range tagged {
		if headSHA != "" && st.BaseCommit != "" && st.BaseCommit != headSHA {
			return fmt.Sprintf("found %d tagged review session(s) but BaseCommit mismatch: state=%q, run=%q (HEAD moved between review start and first agent turn?)", len(tagged), st.BaseCommit, headSHA)
		}
	}
	for _, st := range tagged {
		if !summary.StartedAt.IsZero() && st.StartedAt.Before(summary.StartedAt.Add(-5*time.Second)) {
			return fmt.Sprintf("found %d tagged review session(s) but they started before the review run window (stale session state from a prior run?)", len(tagged))
		}
	}
	for _, st := range tagged {
		if wantType != "" && st.AgentType != "" && st.AgentType != wantType {
			return fmt.Sprintf("found %d tagged review session(s) but AgentType mismatch: state=%q, run=%q", len(tagged), st.AgentType, wantType)
		}
	}
	return fmt.Sprintf("found %d tagged review session(s) but matcher rejected all of them (no filter explained the rejection — please report this as a bug)", len(tagged))
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
