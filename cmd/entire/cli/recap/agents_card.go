package recap

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// LabelCount pairs a label with its occurrence count — used for ordered
// distribution rendering in the Labels column + Agents cards.
type LabelCount struct {
	Label string
	Count int
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

	// "contributors" column — from repo-overview endpoints
	// Empty when not logged in / repo not tracked / solo repo.
	ContribSessions    int
	ContribCheckpoints int
	ContribTokens      int
	ContribLabels      []LabelCount
	ContribSkills      []string
	ContribModels      []string
	ContribCount       int // distinct contributors including you
}

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

// renderAgentsView draws the Agents view: one AgentCard panel per agent.
// When mode is "me" only, the contributors column is suppressed; "contributors"
// hides the me column; "both" shows side-by-side.
func renderAgentsView(cards []AgentCard, mode ViewMode, styles Styles) string {
	if len(cards) == 0 {
		return styles.muted.Render("(no agents in range)")
	}
	var blocks []string
	for _, c := range cards {
		blocks = append(blocks, renderAgentCard(c, mode, styles))
	}
	return strings.Join(blocks, "\n\n")
}

func renderAgentCard(c AgentCard, mode ViewMode, styles Styles) string {
	var b strings.Builder
	b.WriteString(styles.title.Render(c.Agent))
	b.WriteString("\n")

	showMe := mode == ViewMe || mode == ViewBoth
	showContrib := mode == ViewContributors || mode == ViewBoth

	// Column headers.
	switch mode {
	case ViewBoth:
		fmt.Fprintf(&b, "  %-24s %-14s %-14s\n",
			"", styles.label.Render("me"), styles.label.Render("contributors"))
	case ViewMe:
		fmt.Fprintf(&b, "  %-24s %-14s\n", "", styles.label.Render("me"))
	case ViewContributors:
		fmt.Fprintf(&b, "  %-24s %-14s\n", "", styles.label.Render("contributors"))
	}

	// Numeric rows.
	row := func(label, meVal, contribVal string) string {
		switch mode {
		case ViewBoth:
			return fmt.Sprintf("  %-24s %s %s\n",
				styles.muted.Render(label),
				padValue(meVal, styles.value),
				padValue(contribVal, styles.value))
		case ViewMe:
			return fmt.Sprintf("  %-24s %s\n",
				styles.muted.Render(label),
				padValue(meVal, styles.value))
		case ViewContributors:
			return fmt.Sprintf("  %-24s %s\n",
				styles.muted.Render(label),
				padValue(contribVal, styles.value))
		}
		return ""
	}

	meN := func(n int) string {
		if !showMe {
			return ""
		}
		return strconv.Itoa(n)
	}
	contribN := func(n int) string {
		if !showContrib {
			return ""
		}
		if n == 0 {
			return styles.muted.Render("—")
		}
		return strconv.Itoa(n)
	}

	b.WriteString(row("Sessions", meN(c.MeSessions), contribN(c.ContribSessions)))
	b.WriteString(row("Checkpoints", meN(c.MeCheckpoints), contribN(c.ContribCheckpoints)))
	b.WriteString(row("Tokens", tokensIfAny(c.MeTokens, showMe), tokensIfAny(c.ContribTokens, showContrib)))
	if mode == ViewBoth || mode == ViewContributors {
		contribCount := "—"
		if c.ContribCount > 0 {
			contribCount = strconv.Itoa(c.ContribCount)
		}
		b.WriteString(row("Distinct contributors", "", styles.muted.Render(contribCount)))
	}

	// Labels: show sorted top-3, compact bars for me; contributors renders the
	// server-provided sorted list when available.
	if showMe || showContrib {
		b.WriteString("\n")
		b.WriteString(styles.muted.Render("  Top labels") + "\n")
		meLabels := topLabelsInline(c.MeLabels, styles)
		contribLabels := topLabelsInline(c.ContribLabels, styles)
		switch mode {
		case ViewBoth:
			fmt.Fprintf(&b, "    %-30s   %-30s\n", meLabels, contribLabels)
		case ViewMe:
			b.WriteString("    " + meLabels + "\n")
		case ViewContributors:
			b.WriteString("    " + contribLabels + "\n")
		}
	}

	if showMe && (len(c.MeSkills) > 0 || len(c.MeModels) > 0 || len(c.MeRepos) > 0) {
		b.WriteString("\n")
		if len(c.MeSkills) > 0 {
			fmt.Fprintf(&b, "  %s %s\n",
				styles.muted.Render("Top skills"),
				styles.info.Render(strings.Join(c.MeSkills, ", ")))
		}
		if len(c.MeModels) > 0 {
			fmt.Fprintf(&b, "  %s %s\n",
				styles.muted.Render("Top models"),
				styles.value.Render(strings.Join(c.MeModels, ", ")))
		}
		if len(c.MeRepos) > 0 {
			var repoLines []string
			for _, r := range c.MeRepos {
				repoLines = append(repoLines, fmt.Sprintf("%s (%d)", r.Repo, r.SessionCount))
			}
			fmt.Fprintf(&b, "  %s %s\n",
				styles.muted.Render("Top repos "),
				strings.Join(repoLines, ", "))
		}
	}

	return b.String()
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

func topLabelsInline(labels []LabelCount, styles Styles) string {
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
		parts = append(parts, fmt.Sprintf("%s %d%%", styles.accent.Render(labels[i].Label), pct))
	}
	return strings.Join(parts, "  ")
}

func tokensIfAny(n int, show bool) string {
	if !show {
		return ""
	}
	if n == 0 {
		return "—"
	}
	return formatTokens(n)
}

const cardValueWidth = 14

// padValue renders a styled value padded to cardValueWidth. Uses a plain
// left-pad since our values are ASCII-safe (digits + token suffixes).
func padValue(s string, style lipgloss.Style) string {
	if len(s) < cardValueWidth {
		s += strings.Repeat(" ", cardValueWidth-len(s))
	}
	return style.Render(s)
}
