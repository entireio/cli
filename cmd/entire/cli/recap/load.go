package recap

import (
	"context"
	"fmt"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Scope selects how much of the recap graph to read.
type Scope int

const (
	// ScopeCurrent loads sessions for the current repo only.
	ScopeCurrent Scope = iota
	// ScopeLocal is equivalent to ScopeCurrent but never calls the api.
	ScopeLocal
	// ScopeAll loads sessions across every repo the user has api access to.
	// Requires a login token.
	ScopeAll
)

// LoadOpts controls LoadRecap behavior.
type LoadOpts struct {
	Scope         Scope
	EnrichFromAPI bool
	InsecureHTTP  bool
	// TokenProvider is injected for testing; nil uses defaultTokenProvider.
	TokenProvider TokenProvider
}

func (o *LoadOpts) applyDefaults() {
	// Scope is int with ScopeCurrent == 0 (first iota value), so a
	// zero-valued LoadOpts naturally defaults to ScopeCurrent without
	// this block doing anything. Retained for explicitness in case the
	// enum is reordered.
	if o.Scope == 0 {
		o.Scope = ScopeCurrent
	}
}

// Recap is the full projection returned by LoadRecap. Read-only.
type Recap struct {
	Sessions []RecapSession
	Source   DataSource
}

// LoadRecap projects sessions + checkpoints into RecapSession values
// using local session state. API enrichment happens only when
// opts.EnrichFromAPI is true and a token is available; it never blocks
// the local result.
func LoadRecap(ctx context.Context, opts LoadOpts) (*Recap, error) {
	opts.applyDefaults()

	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("list session states: %w", err)
	}

	out := &Recap{Source: SourceLocal}
	for _, s := range states {
		out.Sessions = append(out.Sessions, projectSession(s))
	}

	// Attach committed checkpoints to the projected sessions. A fresh
	// repo with no metadata branch is a valid state, so errors here
	// degrade to "no checkpoints" rather than fail the whole load.
	committed, err := strategy.ListCheckpoints(ctx)
	if err != nil {
		logging.Warn(ctx, "recap: failed to list committed checkpoints, continuing without them",
			"error", err.Error())
		committed = nil
	}
	bySession := map[string][]RecapCheckpoint{}
	for _, cp := range committed {
		sids := cp.SessionIDs
		if len(sids) == 0 && cp.SessionID != "" {
			// Defensive: should always be populated for current metadata shape,
			// but older or malformed metadata may omit it.
			sids = []string{cp.SessionID}
		}
		// Dedupe defensively — a well-formed CheckpointInfo won't list the
		// same session ID twice, but a malformed one would otherwise cause
		// double-counting in SpanMinutes and len(Checkpoints).
		seen := map[string]bool{}
		for _, sid := range sids {
			if sid == "" || seen[sid] {
				continue
			}
			seen[sid] = true
			bySession[sid] = append(bySession[sid], projectCheckpoint(cp, sid))
		}
	}
	for i := range out.Sessions {
		sid := out.Sessions[i].SessionID
		out.Sessions[i].Checkpoints = bySession[sid]
	}

	// Collect committed checkpoint IDs (skip empty).
	ids := []string{}
	for _, s := range out.Sessions {
		for _, cp := range s.Checkpoints {
			if id := cp.ID.String(); id != "" {
				ids = append(ids, id)
			}
		}
	}
	linked := LookupLinkedCommits(ctx, ids)

	for i := range out.Sessions {
		var sessionLinked []string
		for j, cp := range out.Sessions[i].Checkpoints {
			shas := linked[cp.ID.String()]
			if len(shas) > 0 {
				out.Sessions[i].Checkpoints[j].LinkedCommit = shas[0]
				sessionLinked = append(sessionLinked, shas...)
			}
		}
		out.Sessions[i].LinkedCommits = dedupStrings(sessionLinked)
	}

	// Group sessions by branch (empty-branch sessions form their own group).
	// Then sort each group by StartedAt ascending so priorSameBranch slices
	// are oldest-first.
	sort.Slice(out.Sessions, func(i, j int) bool {
		return out.Sessions[i].StartedAt.Before(out.Sessions[j].StartedAt)
	})
	byBranch := map[string][]int{} // branch → indices into out.Sessions, oldest first
	for i, s := range out.Sessions {
		byBranch[s.Branch] = append(byBranch[s.Branch], i)
	}
	for _, indices := range byBranch {
		for pos, idx := range indices {
			priors := make([]RecapSession, 0, pos)
			for _, pi := range indices[:pos] {
				priors = append(priors, out.Sessions[pi])
			}
			out.Sessions[idx].Badges = ComputeBadges(out.Sessions[idx], priors)
		}
	}

	// Stamp the "<org>/<name>" slug on every session + checkpoint before
	// enrichment so the enricher has a repo to hit. Without this, cp.Repo
	// is empty and EnrichCheckpoint short-circuits with "enrich: empty repo"
	// for every checkpoint — which silently disables all label loading.
	stampRepos(ctx, out.Sessions)

	if opts.EnrichFromAPI && opts.Scope != ScopeLocal {
		enricher := buildEnricher(ctx, opts)
		if enricher != nil {
			enrichSessionsInPlace(ctx, enricher, out.Sessions)
		}
	}
	return out, nil
}

// stampRepos resolves the "<owner>/<name>" slug for each session's worktree
// via git remote, then copies it onto the session and each of its committed
// checkpoints. Cached per worktree path so we only shell out once per
// distinct worktree. Unknown repos leave the field empty (enrichment skips
// those checkpoints, which is the right behavior).
func stampRepos(ctx context.Context, sessions []RecapSession) {
	cache := map[string]string{}
	for si := range sessions {
		wt := sessions[si].WorktreePath
		slug, ok := cache[wt]
		if !ok {
			slug = ResolveRepoFromWorktree(ctx, wt)
			if slug == "unknown" {
				slug = ""
			}
			cache[wt] = slug
		}
		if slug == "" {
			continue
		}
		sessions[si].Repo = slug
		for ci := range sessions[si].Checkpoints {
			sessions[si].Checkpoints[ci].Repo = slug
		}
	}
}

// defaultTokenProvider reads from the OS keyring via auth.LookupCurrentToken.
// Used when LoadOpts.TokenProvider is nil.
func defaultTokenProvider() (string, error) {
	tok, err := auth.LookupCurrentToken()
	if err != nil {
		return "", fmt.Errorf("lookup auth token: %w", err)
	}
	return tok, nil
}

// buildEnricher returns an Enricher if a token is available. Returns
// nil when no token — callers treat that as "enrichment off."
//
// Respects opts.InsecureHTTP: in secure mode, refuses to build against
// a non-HTTPS BaseURL.
func buildEnricher(ctx context.Context, opts LoadOpts) *Enricher {
	provider := opts.TokenProvider
	if provider == nil {
		provider = defaultTokenProvider
	}
	token, err := provider()
	if err != nil {
		logging.Debug(ctx, "recap: token lookup failed; enrichment disabled",
			"error", err.Error())
		return nil
	}
	if token == "" {
		logging.Debug(ctx, "recap: no auth token; enrichment disabled")
		return nil
	}
	if !opts.InsecureHTTP {
		if err := api.RequireSecureURL(api.BaseURL()); err != nil {
			// Security-relevant: refuse plain-HTTP endpoints in production.
			logging.Warn(ctx, "recap: refusing enrichment against non-HTTPS endpoint",
				"base_url", api.BaseURL(), "error", err.Error())
			return nil
		}
	}
	client := api.NewClient(token)
	gitCommonDir, err := strategy.GetGitCommonDir(ctx)
	if err != nil {
		logging.Debug(ctx, "recap: GetGitCommonDir failed; enriching without cache",
			"error", err.Error())
		return NewEnricher(client, nil)
	}
	cache, err := NewAnalysisCache(gitCommonDir)
	if err != nil {
		logging.Debug(ctx, "recap: cache construction failed; enriching without cache",
			"error", err.Error())
		return NewEnricher(client, nil)
	}
	return NewEnricher(client, cache)
}

// enrichSessionsInPlace mutates the checkpoints of each session, setting
// Labels and ToolProfile from the api when available. Each checkpoint
// fetches independently; failures don't block others.
func enrichSessionsInPlace(ctx context.Context, e *Enricher, sessions []RecapSession) {
	for si := range sessions {
		for ci := range sessions[si].Checkpoints {
			enriched, err := e.EnrichCheckpoint(ctx, sessions[si].Checkpoints[ci])
			if err != nil {
				// Non-fatal: EnrichCheckpoint reserves error for future
				// conditions; current failures are logged internally.
				logging.Debug(ctx, "recap: enrichment returned error",
					"checkpoint", sessions[si].Checkpoints[ci].ID.String(),
					"error", err.Error())
			}
			sessions[si].Checkpoints[ci] = enriched
		}
		// Roll session-level Labels up from its checkpoints.
		sessions[si].Labels = rollupLabels(sessions[si].Checkpoints)
		if sessionHasServerData(sessions[si].Checkpoints) {
			sessions[si].Source = SourceMixed
		}
	}
}

func rollupLabels(cps []RecapCheckpoint) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, cp := range cps {
		for _, l := range cp.Labels {
			if seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func sessionHasServerData(cps []RecapCheckpoint) bool {
	for _, cp := range cps {
		if cp.Source == SourceMixed || cp.Source == SourceServer {
			return true
		}
	}
	return false
}

// dedupStrings returns the input slice with duplicates removed while
// preserving insertion order.
func dedupStrings(ss []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// projectSession turns a session.State into an RecapSession projection.
// No i/o; safe to unit-test with in-memory fixtures.
//
// Field sources (verified in session/state.go and strategy/manual_commit_types.go):
//
//	SessionID, BaseCommit, WorktreeID, WorktreePath, StartedAt,
//	EndedAt (*time.Time), Phase (session.Phase), FilesTouched,
//	LastInteractionTime (*time.Time), StepCount, AgentType, ModelName,
//	TokenUsage
//
// Branch is NOT a session.State field — deferred to Chunk 2 when we
// derive it from the shadow-branch name or the checkpoint commit.
func projectSession(s *strategy.SessionState) RecapSession {
	out := RecapSession{
		SessionID:    s.SessionID,
		BaseCommit:   s.BaseCommit,
		WorktreeID:   s.WorktreeID,
		WorktreePath: s.WorktreePath,
		StartedAt:    s.StartedAt,
		EndedAt:      s.EndedAt,
		Phase:        s.Phase,
		FilesTouched: append([]string(nil), s.FilesTouched...),
		TokenUsage:   s.TokenUsage,
		IsActive:     s.EndedAt == nil,
		Source:       SourceLocal,
	}
	if s.LastInteractionTime != nil {
		out.LastInteraction = *s.LastInteractionTime
	}
	if s.AgentType != "" {
		out.AgentsUsed = []string{string(s.AgentType)}
	}
	if s.ModelName != "" {
		out.ModelsUsed = []string{s.ModelName}
	}
	return out
}

// projectCheckpoint turns a strategy.CheckpointInfo into an RecapCheckpoint
// scoped to one session ID. When a checkpoint condensed multiple sessions, the
// caller invokes this once per session ID so each projected session gets its
// own copy. No i/o.
func projectCheckpoint(info strategy.CheckpointInfo, sid string) RecapCheckpoint {
	return RecapCheckpoint{
		ID:           info.CheckpointID,
		SessionID:    sid,
		CreatedAt:    info.CreatedAt,
		FilesTouched: append([]string(nil), info.FilesTouched...),
		Source:       SourceLocal,
		// TokenUsage intentionally omitted: strategy.CheckpointInfo does not
		// carry per-checkpoint token totals. Per-agent token attribution in
		// the Agents view uses RecapSession.TokenUsage (session-scoped) in
		// buildAgentCards — see agents_card.go.
	}
}
