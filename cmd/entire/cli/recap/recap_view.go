package recap

import (
	"sort"
	"strconv"
	"time"
)

// RangeKey names a pre-baked time range selectable via CLI flag or keyboard
// toggle. Day is the default; Month is calendar-aligned, 30d and 90d are
// rolling windows.
type RangeKey string

const (
	RangeDay     RangeKey = "day"
	RangeWeek    RangeKey = "week"
	RangeMonth   RangeKey = "month"
	Range30d     RangeKey = "30d"
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
	case Range30d:
		return "Last 30 days"
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
	case Range30d:
		return dayEnd.AddDate(0, 0, -30), dayEnd
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
	Contributors *ContributorsData // optional; fills the contributors columns
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
	var cps []RecapCheckpoint
	tokens := 0
	commits := map[string]bool{}
	for _, s := range filtered {
		for _, cp := range s.Checkpoints {
			if cp.CreatedAt.Before(start) || !cp.CreatedAt.Before(end) {
				continue
			}
			cps = append(cps, cp)
			if cp.TokenUsage != nil {
				tokens += cp.TokenUsage.InputTokens + cp.TokenUsage.OutputTokens
			}
			if cp.LinkedCommit != "" {
				commits[cp.LinkedCommit] = true
			}
		}
	}

	// Summary band.
	agentCounts := map[string]int{}
	modelCounts := map[string]int{}
	labelCounts := map[string]int{}
	for _, s := range filtered {
		for _, a := range s.AgentsUsed {
			agentCounts[a]++
		}
		for _, m := range s.ModelsUsed {
			modelCounts[m]++
		}
	}
	for _, cp := range cps {
		for _, lbl := range cp.Labels {
			labelCounts[lbl]++
		}
	}
	topAgent, topAgentN := pickTop(agentCounts)
	topModel, topModelN := pickTop(modelCounts)
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
	view.Summary = SummaryBand{
		TopAgent:         topAgent,
		TopAgentCount:    topAgentN,
		TopModel:         topModel,
		TopModelCount:    topModelN,
		DominantLabel:    domLabel,
		DominantLabelPct: domPct,
		SessionCount:     len(filtered),
		CheckpointCount:  len(cps),
		TokenTotal:       tokens,
		CommitCount:      len(commits),
		AgentFilter:      opts.AgentFilter,
	}

	// Activity buckets: shape depends on range. Day = 24 hourly; others =
	// daily cells across the span.
	view.Activity = buildActivityBuckets(cps, opts.Range, start, end)

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
	applyContributors(view.AgentCards, opts.Contributors)

	// Label distribution lines (for the bottom-panel Labels column).
	totalLabels := 0
	for _, c := range labelCounts {
		totalLabels += c
	}
	view.Labels = make([]LabelLine, 0, len(labelCounts))
	for lbl, n := range labelCounts {
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
