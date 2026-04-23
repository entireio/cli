package recap

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// LabelCount pairs a label with its occurrence count — used for ordered
// distribution rendering in the Labels column + Agents cards.
type LabelCount struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// AgentCard is the per-agent panel data used by the Agents view. Fields
// prefixed Me are the current user's activity; fields prefixed Contrib are
// the whole repo's contributor aggregate (populated by a follow-up commit
// that wires the overview endpoints).
type AgentCard struct {
	Agent string

	// "me" column — from local + per-checkpoint enrichment
	MeSessions    int
	MeCheckpoints int
	MeTokens      int
	MeLabels      []LabelCount
	MeSkills      []string
	MeModels      []string
	MeRepos       []RepoLine
	MeToolMix     map[string]int

	// "contributors" column — from repo-overview endpoints
	// Empty when not logged in / repo not tracked / solo repo.
	ContribSessions    int
	ContribCheckpoints int
	ContribTokens      int
	ContribLabels      []LabelCount
	ContribSkills      []string
	ContribModels      []string
	ContribCount       int // distinct contributors including you
	ContribToolMix     map[string]int
}

// RepoInfo is an alias for RepoLine used in test code and public APIs.
// Both types represent a repo with a session count.
type RepoInfo = RepoLine

// buildAgentCards turns a list of filtered sessions into per-agent cards for
// the "me" side. Contributor fields are left zero — the server-overview
// integration fills them in later.
func buildAgentCards(sessions []RecapSession) []AgentCard {
	type bucket struct {
		card        *AgentCard
		labelCounts map[string]int
		models      map[string]int
		repos       map[string]int
	}
	buckets := map[string]*bucket{}
	for _, s := range sessions {
		for _, agent := range s.AgentsUsed {
			b, ok := buckets[agent]
			if !ok {
				b = &bucket{
					card:        &AgentCard{Agent: agent},
					labelCounts: map[string]int{},
					models:      map[string]int{},
					repos:       map[string]int{},
				}
				buckets[agent] = b
			}
			b.card.MeSessions++
			for _, cp := range s.Checkpoints {
				b.card.MeCheckpoints++
				if cp.TokenUsage != nil {
					b.card.MeTokens += cp.TokenUsage.InputTokens + cp.TokenUsage.OutputTokens
				}
				for _, lbl := range cp.Labels {
					b.labelCounts[lbl]++
				}
			}
			for _, m := range s.ModelsUsed {
				b.models[m]++
			}
			if s.Repo != "" {
				b.repos[s.Repo]++
			}
		}
	}
	cards := make([]AgentCard, 0, len(buckets))
	for _, b := range buckets {
		b.card.MeLabels = labelCountsSorted(b.labelCounts)
		b.card.MeModels = topNByCount(b.models, 3)
		b.card.MeRepos = reposSorted(b.repos)
		cards = append(cards, *b.card)
	}
	sort.Slice(cards, func(i, j int) bool {
		if cards[i].MeSessions != cards[j].MeSessions {
			return cards[i].MeSessions > cards[j].MeSessions
		}
		return cards[i].Agent < cards[j].Agent
	})
	return cards
}

// applyContributors fills AgentCard.Contrib* fields from a ContributorsData
// map fetched from the server. Leaves zero-valued fields alone when no data
// is available for a given agent — the renderer shows "—" as a placeholder.
func applyContributors(cards []AgentCard, data *ContributorsData) {
	if data == nil || len(data.ByAgent) == 0 {
		return
	}
	for i := range cards {
		if a, ok := data.ByAgent[cards[i].Agent]; ok && a != nil {
			cards[i].ContribSessions = a.TotalCount
			cards[i].ContribCheckpoints = a.TotalCount
			cards[i].ContribTokens = a.Tokens
			cards[i].ContribCount = a.DistinctContribs
		}
	}
}

// ViewMode selects what data columns appear in the Agents view.
type ViewMode string

const (
	ViewMe           ViewMode = "me"
	ViewContributors ViewMode = "contributors"
	ViewBoth         ViewMode = "both"
)

// renderAgentsView composes the Agents panel body: optional legend row
// + sorted agent cards separated by blank lines.
// innerWidth is threaded from RenderStatic so narrow terminals cascade
// down to the bar-rendering level. The caller computes it as
// `terminalWidth - 4` (2 chars border + 1 char padding each side).
func renderAgentsView(cards []AgentCard, mode ViewMode, innerWidth int, styles Styles) string {
	if len(cards) == 0 {
		return styles.muted.Render("(no agent activity in range)")
	}

	sorted := append([]AgentCard(nil), cards...)
	sort.SliceStable(sorted, func(i, j int) bool {
		iSum := sorted[i].MeSessions + sorted[i].ContribSessions
		jSum := sorted[j].MeSessions + sorted[j].ContribSessions
		if iSum != jSum {
			return iSum > jSum
		}
		return sorted[i].Agent < sorted[j].Agent
	})

	var parts []string
	if mode == ViewBoth {
		parts = append(parts, fmt.Sprintf("%s   %s",
			styles.accent.Render("you ███"),
			styles.team.Render("team ▒")))
	}
	for _, c := range sorted {
		parts = append(parts, renderAgentCard(c, mode, innerWidth, styles))
	}
	return strings.Join(parts, "\n\n")
}

// renderAgentCard renders a single agent block. Field groups render in
// fixed order: comparison bars → team qualitative → your qualitative.
// Rows where both sides are zero are dropped. Blocks are skipped when
// the view mode or data makes them irrelevant.
func renderAgentCard(c AgentCard, mode ViewMode, innerWidth int, styles Styles) string {
	var b strings.Builder
	b.WriteString(styles.accent.Render(c.Agent))
	b.WriteString("\n")

	bars := renderBarRows(c, mode, innerWidth, styles)
	if bars != "" {
		b.WriteString(bars)
	}

	if teamBlock := renderTeamBlock(c, mode, styles); teamBlock != "" {
		b.WriteString("\n")
		b.WriteString(teamBlock)
	}
	if yourBlock := renderYourBlock(c, mode, styles); yourBlock != "" {
		b.WriteString("\n")
		b.WriteString(yourBlock)
	}
	return b.String()
}

// renderBarRows emits up to 3 bar rows (tokens, sessions, checkpoints).
// Drops rows where both sides are zero.
func renderBarRows(c AgentCard, mode ViewMode, innerWidth int, styles Styles) string {
	rows := []struct {
		label string
		you   int
		team  int
	}{
		{"tokens", c.MeTokens, c.ContribTokens},
		{"sessions", c.MeSessions, c.ContribSessions},
		{"checkpoints", c.MeCheckpoints, c.ContribCheckpoints},
	}
	barWidth := innerWidth - 12 /*label*/ - 14 /*readout*/ - 4 /*padding*/
	var lines []string
	for _, r := range rows {
		if r.you == 0 && r.team == 0 {
			continue
		}
		bar := renderComparisonBar(singleSide(mode, r.you, r.team, true),
			singleSide(mode, r.you, r.team, false), barWidth, styles)
		readout := formatBarReadout(r.you, r.team, r.label, mode)
		lines = append(lines, fmt.Sprintf("  %-12s %s  %s", r.label, bar, readout))
	}
	return strings.Join(lines, "\n")
}

// singleSide returns the you or team value, respecting view mode. In
// ViewMe we return 0 for the team slot so the bar renders amber-only;
// in ViewContributors we flip. In ViewBoth both sides pass through.
func singleSide(mode ViewMode, you, team int, isYou bool) int {
	switch mode {
	case ViewMe:
		if isYou {
			return you
		}
		return 0
	case ViewContributors:
		if isYou {
			return 0
		}
		return team
	case ViewBoth:
		if isYou {
			return you
		}
		return team
	}
	if isYou {
		return you
	}
	return team
}

// formatBarReadout produces the right-side numeric text. In both mode
// it's "<you> / <team>"; in single-side views it's just one value.
func formatBarReadout(you, team int, metric string, mode ViewMode) string {
	fmtVal := func(v int) string {
		if metric == "tokens" {
			return formatTokens(v)
		}
		return strconv.Itoa(v)
	}
	switch mode {
	case ViewMe:
		return fmtVal(you)
	case ViewContributors:
		return fmtVal(team)
	case ViewBoth:
		return fmt.Sprintf("%s / %s", fmtVal(you), fmtVal(team))
	}
	return fmt.Sprintf("%s / %s", fmtVal(you), fmtVal(team))
}

// renderTeamBlock renders the three team-qualitative rows. Skipped in
// ViewMe or when no data. Prefix "team " only appears in ViewBoth.
func renderTeamBlock(c AgentCard, mode ViewMode, styles Styles) string {
	if mode == ViewMe {
		return ""
	}
	if len(c.ContribLabels) == 0 && len(c.ContribSkills) == 0 && len(c.ContribToolMix) == 0 {
		return ""
	}
	var lines []string
	lbl := func(name string) string {
		if mode == ViewBoth {
			return "  " + styles.team.Render("team "+name)
		}
		return "  " + styles.label.Render(name)
	}
	if len(c.ContribLabels) > 0 {
		lines = append(lines, lbl("labels")+"    "+formatLabelList(c.ContribLabels, styles))
	}
	if len(c.ContribSkills) > 0 {
		lines = append(lines, lbl("skills")+"    "+formatList(c.ContribSkills, styles.info))
	}
	if len(c.ContribToolMix) > 0 {
		lines = append(lines, lbl("tool mix")+"  "+formatToolMix(c.ContribToolMix))
	}
	return strings.Join(lines, "\n")
}

// renderYourBlock renders the two your-qualitative rows. Skipped in
// ViewContributors or when no data.
func renderYourBlock(c AgentCard, mode ViewMode, styles Styles) string {
	if mode == ViewContributors {
		return ""
	}
	if len(c.MeModels) == 0 && len(c.MeRepos) == 0 {
		return ""
	}
	var lines []string
	lbl := func(name string) string {
		if mode == ViewBoth {
			return "  " + styles.accent.Render("your "+name)
		}
		return "  " + styles.label.Render(name)
	}
	if len(c.MeModels) > 0 {
		lines = append(lines, lbl("models")+"    "+formatList(c.MeModels, styles.value))
	}
	if len(c.MeRepos) > 0 {
		lines = append(lines, lbl("repos")+"     "+formatRepoList(c.MeRepos))
	}
	return strings.Join(lines, "\n")
}

// format helpers ------------------------------------------------------------

// formatLabelList renders top-3 labels with percentage breakdowns.
func formatLabelList(labels []LabelCount, styles Styles) string {
	if len(labels) == 0 {
		return styles.muted.Render("—")
	}
	n := len(labels)
	if n > 3 {
		n = 3
	}
	total := 0
	for _, l := range labels {
		total += l.Count
	}
	parts := make([]string, 0, n)
	for i := range n {
		pct := 0
		if total > 0 {
			pct = (labels[i].Count * 100) / total
		}
		parts = append(parts, fmt.Sprintf("%s %d%%", labels[i].Label, pct))
	}
	return strings.Join(parts, "  ")
}

// formatList renders a slice of strings joined with commas using the given style.
func formatList(items []string, style interface{ Render(s ...string) string }) string {
	if len(items) == 0 {
		return ""
	}
	return style.Render(strings.Join(items, ", "))
}

// formatToolMix renders a tool-mix map as "key pct%  key pct%" sorted by value desc.
// Values are treated as percentages directly (e.g. fileOps:61 → "fileOps 61%").
func formatToolMix(mix map[string]int) string {
	if len(mix) == 0 {
		return ""
	}
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(mix))
	for k, v := range mix {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	n := len(all)
	if n > 3 {
		n = 3
	}
	parts := make([]string, 0, n)
	for i := range n {
		parts = append(parts, fmt.Sprintf("%s %d%%", all[i].k, all[i].v))
	}
	return strings.Join(parts, "  ")
}

// formatRepoList renders a list of RepoLine entries as "repo (count), ..." .
func formatRepoList(repos []RepoLine) string {
	if len(repos) == 0 {
		return ""
	}
	parts := make([]string, 0, len(repos))
	for _, r := range repos {
		parts = append(parts, fmt.Sprintf("%s (%d)", r.Repo, r.SessionCount))
	}
	return strings.Join(parts, ", ")
}

// helpers --------------------------------------------------------------------

func labelCountsSorted(m map[string]int) []LabelCount {
	out := make([]LabelCount, 0, len(m))
	for k, v := range m {
		out = append(out, LabelCount{Label: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	return out
}

func topNByCount(m map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	out := make([]string, 0, len(all))
	for _, e := range all {
		out = append(out, e.k)
	}
	return out
}

func reposSorted(m map[string]int) []RepoLine {
	out := make([]RepoLine, 0, len(m))
	for k, v := range m {
		out = append(out, RepoLine{Repo: k, SessionCount: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionCount != out[j].SessionCount {
			return out[i].SessionCount > out[j].SessionCount
		}
		return out[i].Repo < out[j].Repo
	})
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
