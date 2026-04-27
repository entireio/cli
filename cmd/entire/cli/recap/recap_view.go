package recap

import (
	"sort"
	"strconv"
	"time"
)

// RangeKey names a pre-baked time range selectable via CLI flag or keyboard
// toggle. Day is the default; Month is calendar-aligned, 90d is a rolling window.
type RangeKey string

const (
	RangeDay     RangeKey = "day"
	RangeWeek    RangeKey = "week"
	RangeMonth   RangeKey = "month"
	Range90d     RangeKey = "90d"
	rangeDefault          = RangeWeek // Agents view reads better over a week
)

// Title returns the human-readable label shown in the summary panel header.
func (r RangeKey) Title() string {
	switch r {
	case RangeDay:
		return "Today"
	case RangeWeek:
		return "Last 7 days"
	case RangeMonth:
		return "This month"
	case Range90d:
		return "Last 90 days"
	}
	return string(r)
}

// Bounds returns the half-open [start, end) interval for a RangeKey relative
// to a reference time (usually time.Now()). Local-time midnight boundaries
// so "today" starts where the user's clock starts.
func (r RangeKey) Bounds(now time.Time) (time.Time, time.Time) {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)
	switch r {
	case RangeDay:
		return dayStart, dayEnd
	case RangeWeek:
		return dayEnd.AddDate(0, 0, -7), dayEnd
	case RangeMonth:
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		return monthStart, monthStart.AddDate(0, 1, 0)
	case Range90d:
		return dayEnd.AddDate(0, 0, -90), dayEnd
	}
	return dayStart, dayEnd
}

// BuildOpts controls how the view is assembled.
type BuildOpts struct {
	Range        RangeKey
	AgentFilter  string            // empty = all agents
	Mode         ViewMode          // defaults to ViewBoth
	ServerMe     *ContributorsData // optional; overrides me-side metrics with server truth
	Contributors *ContributorsData // optional; fills the contributors columns
	ServerDaily  []DailyCount      // optional; per-day server activity for the strip
	Now          time.Time         // injectable for deterministic tests
}

// View is everything a renderer needs. Pure data — no styling, no I/O.
// The Footer string is left to the renderer; the view exposes the raw bits.
type View struct {
	Title      string
	Range      RangeKey
	Mode       ViewMode // rendering path dispatches on this
	Summary    SummaryBand
	Activity   []int // heatmap buckets; length depends on Range
	Sessions   []SessionRow
	AgentCards []AgentCard
	Repos      []RepoLine // nil when all sessions share one repo
	Worktrees  []WorktreeRollup
	Labels     []LabelLine
	// Notes are diagnostic hints rendered at the bottom of the view — things
	// like "not logged in" or "repo not tracked" that explain why a column
	// might be empty. Plain strings; the renderer styles them.
	Notes []string
}

// SummaryBand holds the headline facts at the top of every view.
type SummaryBand struct {
	// Legacy fields — still populated by BuildView for backward compat.
	TopAgent         string
	TopAgentCount    int
	TopModel         string
	TopModelCount    int
	DominantLabel    string
	DominantLabelPct float64 // 0-1
	SessionCount     int
	CheckpointCount  int
	TokenTotal       int
	CommitCount      int
	AgentFilter      string // non-empty when filtered — rendered as chip

	// New fields for the you/team/top/context redesign (spec 2026-04-22).
	RangeLabel string // e.g. "Last 90 days", "Today"

	// "you" (current user) side.
	YouSessions    int
	YouCheckpoints int
	YouTokens      int

	// "team" (contributors) side.
	TeamSessions    int
	TeamCheckpoints int
	TeamTokens      int

	// Top signals — empty strings omit the slot.
	TopSkill string
	TopLabel string

	// Context line counts.
	AgentCount int
	RepoCount  int
	ActiveDays int
}

// SessionRow is a row in the middle panel's chronological list.
type SessionRow struct {
	Badge       string // ● live, ◌ idle
	Agent       string
	Repo        string
	Span        string // "2h", "30m"
	Label       string // session's dominant label
	Checkpoints int
	Hint        ActionHint
	StartedAt   time.Time // for chronological sort
}

// RepoLine is a row in the Repos bottom-panel column.
type RepoLine struct {
	Repo         string
	SessionCount int
}

// LabelLine is a row in the Labels bottom-panel column.
type LabelLine struct {
	Label string
	Count int
	Pct   int // 0-100, pre-rounded for rendering
}

// BuildView projects a slice of sessions into a View for the
// given range + agent filter. Pure — no I/O, no git — so it's easy to
// fixture-test and safe to call from TUI update loops.
func BuildView(sessions []RecapSession, opts BuildOpts) View {
	if opts.Range == "" {
		opts.Range = rangeDefault
	}
	if opts.Mode == "" {
		opts.Mode = ViewBoth
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	start, end := opts.Range.Bounds(opts.Now)

	// Filter by range first, then by agent. Order matters for performance
	// but not for correctness — both filters are commutative.
	filtered := make([]RecapSession, 0, len(sessions))
	for _, s := range sessions {
		if !inRange(s, start, end) {
			continue
		}
		if opts.AgentFilter != "" && !sessionUsesAgent(s, opts.AgentFilter) {
			continue
		}
		filtered = append(filtered, s)
	}

	view := View{
		Title: opts.Range.Title(),
		Range: opts.Range,
		Mode:  opts.Mode,
	}

	// Collect checkpoints in-range for aggregations that work at the cp level.
	// Tokens live at the session level (cp.TokenUsage is always nil — see
	// projectCheckpoint in load.go), so we sum them once per filtered session.
	var cps []RecapCheckpoint
	tokens := 0
	commits := map[string]bool{}
	for _, s := range filtered {
		if s.TokenUsage != nil {
			tokens += s.TokenUsage.InputTokens + s.TokenUsage.OutputTokens
		}
		for _, cp := range s.Checkpoints {
			if cp.CreatedAt.Before(start) || !cp.CreatedAt.Before(end) {
				continue
			}
			cps = append(cps, cp)
			if cp.LinkedCommit != "" {
				commits[cp.LinkedCommit] = true
			}
		}
	}

	view.Summary = buildSummaryBand(filtered, cps, tokens, commits, opts)

	// Activity buckets: shape depends on range. Day = 24 hourly; others =
	// daily cells across the span.
	view.Activity = buildActivityBuckets(cps, opts.Range, start, end)
	// When the server returned per-day counts, prefer them — they cover
	// cross-repo work and stay consistent with /me/recap me-side totals.
	// Local cps only see this worktree's branch, so the strip would be
	// sparse for users with multi-repo or multi-machine work otherwise.
	if len(opts.ServerDaily) > 0 && opts.Range != RangeDay {
		view.Activity = serverDailyToBuckets(opts.ServerDaily, start, end)
	}

	// Session rows in chronological order (oldest first).
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].StartedAt.Before(filtered[j].StartedAt)
	})
	view.Sessions = make([]SessionRow, 0, len(filtered))
	for _, s := range filtered {
		label, _ := DominantLabel(perSessionLabelCounts(s))
		view.Sessions = append(view.Sessions, SessionRow{
			Badge:       badgeFor(s),
			Agent:       firstAgent(s),
			Repo:        s.Repo,
			Span:        spanOf(s),
			Label:       label,
			Checkpoints: len(s.Checkpoints),
			Hint:        NextAction(s),
			StartedAt:   s.StartedAt,
		})
	}

	// Repo list (hidden when only one distinct repo to keep Day uncluttered).
	repoCounts := map[string]int{}
	for _, s := range filtered {
		if s.Repo != "" {
			repoCounts[s.Repo]++
		}
	}
	if len(repoCounts) > 1 {
		view.Repos = make([]RepoLine, 0, len(repoCounts))
		for r, n := range repoCounts {
			view.Repos = append(view.Repos, RepoLine{Repo: r, SessionCount: n})
		}
		sort.Slice(view.Repos, func(i, j int) bool {
			if view.Repos[i].SessionCount != view.Repos[j].SessionCount {
				return view.Repos[i].SessionCount > view.Repos[j].SessionCount
			}
			return view.Repos[i].Repo < view.Repos[j].Repo
		})
	}

	view.Worktrees = AggregateByWorktree(filtered)
	view.AgentCards = buildAgentCards(filtered)
	// Server is source of truth for me-side metrics — apply BEFORE contributors
	// so applyContributors doesn't accidentally stomp on them.
	view.AgentCards = applyServerMe(view.AgentCards, opts.ServerMe)
	applyContributors(view.AgentCards, opts.Contributors)

	// Populate team side of SummaryBand from ContributorsData if available.
	if opts.Contributors != nil {
		for _, contrib := range opts.Contributors.ByAgent {
			if contrib == nil {
				continue
			}
			view.Summary.TeamSessions += contrib.TotalCount
			view.Summary.TeamCheckpoints += contrib.TotalCount
			view.Summary.TeamTokens += contrib.Tokens
		}
	}

	// Label distribution lines (for the bottom-panel Labels column).
	cpLabelCounts := map[string]int{}
	for _, cp := range cps {
		for _, lbl := range cp.Labels {
			cpLabelCounts[lbl]++
		}
	}
	totalLabels := 0
	for _, c := range cpLabelCounts {
		totalLabels += c
	}
	view.Labels = make([]LabelLine, 0, len(cpLabelCounts))
	for lbl, n := range cpLabelCounts {
		pct := 0
		if totalLabels > 0 {
			pct = (n * 100) / totalLabels
		}
		view.Labels = append(view.Labels, LabelLine{Label: lbl, Count: n, Pct: pct})
	}
	sort.Slice(view.Labels, func(i, j int) bool {
		if view.Labels[i].Count != view.Labels[j].Count {
			return view.Labels[i].Count > view.Labels[j].Count
		}
		return view.Labels[i].Label < view.Labels[j].Label
	})

	return view
}

// buildActivityBuckets returns the per-range strip data. Day = 24 hourly;
// Week = 7 daily; Month = days-in-current-month; 30d/90d = N daily.
func buildActivityBuckets(cps []RecapCheckpoint, r RangeKey, start, end time.Time) []int {
	if r == RangeDay {
		buckets := make([]int, 24)
		for _, cp := range cps {
			h := cp.CreatedAt.Hour()
			buckets[h]++
		}
		return buckets
	}
	days := int(end.Sub(start).Hours()/24) + 0 // half-open; end exclusive
	if days <= 0 {
		days = 1
	}
	buckets := make([]int, days)
	for _, cp := range cps {
		offset := int(cp.CreatedAt.Sub(start).Hours() / 24)
		if offset < 0 || offset >= days {
			continue
		}
		buckets[offset]++
	}
	return buckets
}

// serverDailyToBuckets maps the response's daily array onto a buckets slice
// indexed the same way buildActivityBuckets is — one bucket per day in
// [start, end). Days the server didn't return stay zero.
func serverDailyToBuckets(daily []DailyCount, start, end time.Time) []int {
	days := int(end.Sub(start).Hours()/24) + 0
	if days <= 0 {
		days = 1
	}
	buckets := make([]int, days)
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	for _, d := range daily {
		t, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			continue
		}
		offset := int(t.Sub(startDay).Hours() / 24)
		if offset < 0 || offset >= days {
			continue
		}
		buckets[offset] += d.Count
	}
	return buckets
}

// helpers (trivial, inlined per the plan's "inline trivial helpers" rule) ----

func inRange(s RecapSession, start, end time.Time) bool {
	// A session overlaps the range if either its StartedAt or its
	// LastInteraction timestamp lands in [start, end).
	for _, t := range []time.Time{s.StartedAt, s.LastInteraction} {
		if t.IsZero() {
			continue
		}
		if !t.Before(start) && t.Before(end) {
			return true
		}
	}
	return false
}

func sessionUsesAgent(s RecapSession, agent string) bool {
	for _, a := range s.AgentsUsed {
		if a == agent {
			return true
		}
	}
	return false
}

func pickTop(counts map[string]int) (string, int) {
	var (
		best  string
		bestN int
	)
	// Sort keys for determinism when counts tie.
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if counts[k] > bestN {
			bestN = counts[k]
			best = k
		}
	}
	return best, bestN
}

func perSessionLabelCounts(s RecapSession) map[string]int {
	out := map[string]int{}
	for _, cp := range s.Checkpoints {
		for _, lbl := range cp.Labels {
			out[lbl]++
		}
	}
	return out
}

func badgeFor(s RecapSession) string {
	if s.IsActive {
		return "●"
	}
	return "◌"
}

func firstAgent(s RecapSession) string {
	if len(s.AgentsUsed) == 0 {
		return ""
	}
	return s.AgentsUsed[0]
}

// buildSummaryBand computes all SummaryBand fields from filtered sessions,
// in-range checkpoints, token totals, and commit set. Extracted from BuildView
// to reduce cyclomatic complexity.
func buildSummaryBand(
	filtered []RecapSession,
	cps []RecapCheckpoint,
	tokens int,
	commits map[string]bool,
	opts BuildOpts,
) SummaryBand {
	// Weight top-agent by sessions + checkpoints so the agent doing the
	// most *work* wins, not just the one with the most started sessions.
	// A session with 0 checkpoints contributes 1; a session with 50
	// checkpoints contributes 51. Matches the Agents panel sort.
	agentCounts := map[string]int{}
	modelCounts := map[string]int{}
	labelCounts := map[string]int{}
	// Weight every "top" signal by the same "work done" metric so they're
	// comparable: 1 + len(Checkpoints) per session. Without this, TopModel
	// (session-count) could pick a model from zero-checkpoint sessions
	// while TopAgent (checkpoint-weighted) picks a different one, giving
	// a confusing "top agent X / top model Y" where X and Y aren't related.
	for _, s := range filtered {
		w := 1 + len(s.Checkpoints)
		for _, a := range s.AgentsUsed {
			agentCounts[a] += w
		}
		for _, m := range s.ModelsUsed {
			modelCounts[m] += w
		}
	}
	for _, cp := range cps {
		for _, lbl := range cp.Labels {
			labelCounts[lbl]++
		}
	}

	topAgent, topAgentN := pickTop(agentCounts)
	topModel, topModelN := pickTop(modelCounts)
	topSkill, _ := pickTop(skillCounts(cps))
	domLabel, domOK := DominantLabel(labelCounts)
	domPct := 0.0
	if domOK {
		total := 0
		for _, c := range labelCounts {
			total += c
		}
		if total > 0 {
			domPct = float64(labelCounts[domLabel]) / float64(total)
		}
	}

	distinctRepos := map[string]bool{}
	for _, s := range filtered {
		if s.Repo != "" {
			distinctRepos[s.Repo] = true
		}
	}
	activeDaySet := map[string]bool{}
	for _, cp := range cps {
		key := cp.CreatedAt.Format("2006-01-02")
		activeDaySet[key] = true
	}

	topLabel := ""
	if domOK {
		topLabel = domLabel
	}

	// Prefer server totals when available — this is what keeps CLI totals
	// in sync with the entire.io dashboard (both read the same server query).
	// When opts.ServerMe is nil (offline / not logged in), fall back to the
	// local sums computed above.
	youCheckpoints := len(cps)
	youTokens := tokens
	if opts.ServerMe != nil && len(opts.ServerMe.ByAgent) > 0 {
		srvCp, srvTok := 0, 0
		for _, a := range opts.ServerMe.ByAgent {
			if a == nil {
				continue
			}
			srvCp += a.TotalCount // holds checkpoint count per MeFromMeRecap
			srvTok += a.Tokens
		}
		if srvCp > 0 {
			youCheckpoints = srvCp
		}
		if srvTok > 0 {
			youTokens = srvTok
		}
	}

	return SummaryBand{
		// Legacy fields.
		TopAgent:         topAgent,
		TopAgentCount:    topAgentN,
		TopModel:         topModel,
		TopModelCount:    topModelN,
		DominantLabel:    domLabel,
		DominantLabelPct: domPct,
		SessionCount:     len(filtered),
		CheckpointCount:  youCheckpoints,
		TokenTotal:       youTokens,
		CommitCount:      len(commits),
		AgentFilter:      opts.AgentFilter,

		// New you/team/top/context fields.
		RangeLabel:     opts.Range.Title(),
		YouSessions:    len(filtered),
		YouCheckpoints: youCheckpoints,
		YouTokens:      youTokens,
		TopSkill:       topSkill,
		TopLabel:       topLabel,
		AgentCount:     len(agentCounts),
		RepoCount:      len(distinctRepos),
		ActiveDays:     len(activeDaySet),
	}
}

// skillCounts tallies skills across a slice of checkpoints.
func skillCounts(cps []RecapCheckpoint) map[string]int {
	out := map[string]int{}
	for _, cp := range cps {
		for _, sk := range cp.SkillsUsed {
			out[sk]++
		}
	}
	return out
}

func spanOf(s RecapSession) string {
	mins := s.SpanMinutes()
	switch {
	case mins < 1:
		return "<1m"
	case mins < 60:
		return strconv.Itoa(int(mins)) + "m"
	case mins < 24*60:
		return strconv.Itoa(int(mins/60)) + "h"
	default:
		return strconv.Itoa(int(mins/(24*60))) + "d"
	}
}
