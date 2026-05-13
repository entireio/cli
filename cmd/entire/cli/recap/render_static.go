package recap

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

const (
	DefaultWidth = 78
	AgentAll     = "all"
	minWidth     = 60
	noteAnalysis = "Labels require server analysis (may take a few minutes after committing)."
)

type RenderOptions struct {
	Range    RangeKey
	View     ViewMode
	Agent    string
	Width    int
	Color    bool
	Location *time.Location
}

// RenderStaticRecap renders the server-backed static recap view.
func RenderStaticRecap(resp *MeRecapResponse, opts RenderOptions) string {
	if resp == nil {
		resp = &MeRecapResponse{}
	}
	if opts.Range == "" {
		opts.Range = RangeDay
	}
	if opts.View == "" {
		opts.View = ViewBoth
	}
	if opts.Agent == "" {
		opts.Agent = AgentAll
	}
	width := opts.Width
	if width == 0 {
		width = DefaultWidth
	}
	if width < minWidth {
		width = minWidth
	}
	styles := newStaticStyles(opts.Color)

	var b strings.Builder
	b.WriteString(renderControls(opts, styles))
	b.WriteString("\n\n")
	b.WriteString(renderSummary(resp, opts, width, styles))
	if !hasAgentFilter(opts) {
		b.WriteString("\n\n")
		b.WriteString(renderActivity(resp, opts, width, styles))
	}
	b.WriteString("\n\n")
	b.WriteString(renderAgents(resp, opts, width, styles))
	b.WriteString("\n\n  ")
	b.WriteString(styles.info.Render("ℹ"))
	b.WriteString(" ")
	b.WriteString(styles.muted.Render(noteAnalysis))
	return b.String()
}

func renderControls(opts RenderOptions, styles staticStyles) string {
	ranges := []RangeKey{RangeDay, RangeWeek, RangeMonth, Range90d}
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		label := string(r)
		if r == Range90d {
			label = "90d"
		}
		if r == opts.Range {
			label = styles.accent.Render("[" + label + "]")
		}
		parts = append(parts, label)
	}
	agent := opts.Agent
	if agent == "" {
		agent = AgentAll
	}
	return fmt.Sprintf("%s        agent: [%s]        view: %s",
		strings.Join(parts, styles.muted.Render(" · ")), agent, renderViewSelector(opts.View, styles))
}

func renderViewSelector(view ViewMode, styles staticStyles) string {
	labels := []struct {
		mode  ViewMode
		label string
	}{
		{ViewYou, "you"},
		{ViewTeam, "team"},
		{ViewBoth, "both"},
	}
	var parts []string
	for _, item := range labels {
		if item.mode == view {
			parts = append(parts, styles.accent.Render("["+item.label+"]"))
		} else {
			parts = append(parts, item.label)
		}
	}
	return strings.Join(parts, " ")
}

func renderSummary(resp *MeRecapResponse, opts RenderOptions, width int, styles staticStyles) string {
	me := resp.Summary.Me
	agentFiltered := hasAgentFilter(opts)
	if agentFiltered || me == (SummaryTotals{}) {
		me = sumMe(resp, opts)
	}
	team := resp.Summary.Team
	if agentFiltered {
		if totals := sumTeam(resp, opts); totals != (SummaryTotals{}) {
			team = &totals
		} else {
			team = nil
		}
	} else if team == nil {
		if totals := sumTeam(resp, opts); totals != (SummaryTotals{}) {
			team = &totals
		}
	}
	top := topSignals(resp, opts, styles)
	lines := []string{opts.Range.Title()}
	if window := renderWindow(resp, opts, styles); window != "" {
		lines = append(lines, window)
	}
	lines = append(lines, "")
	if opts.View != ViewTeam {
		lines = append(lines, fmt.Sprintf("%s   %-12s  %-15s  %s",
			styles.accent.Render("you"),
			plural(me.Sessions, "session"), plural(me.Checkpoints, "checkpoint"), formatTokens(me.Tokens)+" tok"))
	}
	if opts.View != ViewYou {
		if team == nil {
			lines = append(lines, styles.team.Render("team")+"  -           -                -")
		} else {
			lines = append(lines, fmt.Sprintf("%s  %-12s  %-15s  %s",
				styles.team.Render("team"),
				plural(team.Sessions, "session"), plural(team.Checkpoints, "checkpoint"), formatTokens(team.Tokens)+" tok"))
		}
	}
	if noteLines := transcriptAvailabilityNote(resp.Summary.Transcripts, opts.View, summaryContentWidth(width)); len(noteLines) > 0 {
		lines = append(lines, "")
		for _, noteLine := range noteLines {
			lines = append(lines, styles.muted.Render(noteLine))
		}
	}
	if len(top) > 0 {
		lines = append(lines, "", styles.muted.Render("top")+"  "+strings.Join(top, styles.muted.Render(" · ")))
	}
	context := []string{plural(len(filteredAgents(resp, opts)), "agent")}
	if repoScope := repoScopeText(resp); repoScope != "" {
		context = append(context, repoScope)
	}
	if !agentFiltered {
		context = append(context, plural(resp.Summary.ActiveDays, "active day"))
	}
	// Wrap on whitespace at the box's content width so long repo names don't
	// tear the border at narrow widths. wrapPlainLine breaks on whitespace
	// only — a single ultra-long repo name (> contentWidth) overflows its
	// line and renderBox truncates it, the same fallback the transcript note
	// relies on.
	lines = append(lines, "")
	for _, line := range wrapPlainLine(strings.Join(context, " · "), summaryContentWidth(width)) {
		lines = append(lines, styles.muted.Render(line))
	}
	return renderBox("", lines, width, styles)
}

// transcriptAvailabilityNote builds the "X unavailable transcripts" hint shown
// inside the summary box. The note is word-wrapped to the available content
// width so it never tears the box at narrow widths or with large counts.
// width is the renderable text width inside the box (already accounting for
// the box borders and the leading two-space indent — see summaryContentWidth).
func transcriptAvailabilityNote(summary TranscriptSummary, view ViewMode, width int) []string {
	status := visibleTranscriptStatus(summary, view)
	total := status.Failed + status.Pending + status.Empty
	if total == 0 {
		return nil
	}
	label := "unavailable transcripts"
	if total == 1 {
		label = "unavailable transcript"
	}
	parts := make([]string, 0, 3)
	if status.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", status.Failed))
	}
	if status.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", status.Pending))
	}
	if status.Empty > 0 {
		parts = append(parts, fmt.Sprintf("%d empty", status.Empty))
	}
	headline := fmt.Sprintf("%d %s", total, label)
	detail := strings.Join(parts, ", ") + "; session totals may be lower"

	out := wrapPlainLine(headline, width)
	out = append(out, wrapPlainLine(detail, width)...)
	return out
}

// visibleTranscriptStatus returns the transcript-availability counts that
// apply to the current view. ViewBoth sums Me + Team so the displayed count
// matches the sessions visible in that view; future ViewMode additions hit
// the default arm and emit a debug log instead of silently zeroing.
func visibleTranscriptStatus(summary TranscriptSummary, view ViewMode) TranscriptStatus {
	switch view {
	case ViewYou:
		return summary.Me
	case ViewTeam:
		if summary.Team == nil {
			return TranscriptStatus{}
		}
		return *summary.Team
	case ViewBoth:
		status := summary.Me
		if summary.Team != nil {
			status.Failed += summary.Team.Failed
			status.Pending += summary.Team.Pending
			status.Empty += summary.Team.Empty
		}
		return status
	default:
		logging.Debug(context.Background(), "recap: unknown view mode for transcript status", slog.String("view", string(view)))
		return TranscriptStatus{}
	}
}

// summaryContentWidth returns the renderable text width inside the summary
// box, accounting for the two border columns and the leading two-space indent
// that renderBox applies to every content line.
func summaryContentWidth(width int) int {
	inner := width - 2 - 2
	if inner < 1 {
		return 1
	}
	return inner
}

// wrapPlainLine word-wraps a plain (no ANSI) string at width on whitespace
// boundaries. Returns at least one line, even if width is too small to fit a
// single token — that token is emitted as its own (overflowing) line, which
// renderBox will then truncate, rather than silently dropped.
func wrapPlainLine(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}
	if displayLen(s) <= width {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{s}
	}
	var out []string
	current := words[0]
	for _, word := range words[1:] {
		if displayLen(current)+1+displayLen(word) <= width {
			current += " " + word
			continue
		}
		out = append(out, current)
		current = word
	}
	if current != "" {
		out = append(out, current)
	}
	return out
}

// renderWindow formats the recap window using the API's since/until. Parse
// failures fall back to an empty window line (graceful degradation) but the
// underlying error is logged via slog so the failure is diagnosable when
// debug logging is enabled.
func renderWindow(resp *MeRecapResponse, opts RenderOptions, styles staticStyles) string {
	if resp.Since == "" || resp.Until == "" {
		return ""
	}
	since, err := time.Parse(time.RFC3339, resp.Since)
	if err != nil {
		logging.Debug(context.Background(), "recap: failed to parse since", slog.String("value", resp.Since), slog.String("error", err.Error()))
		return ""
	}
	until, err := time.Parse(time.RFC3339, resp.Until)
	if err != nil {
		logging.Debug(context.Background(), "recap: failed to parse until", slog.String("value", resp.Until), slog.String("error", err.Error()))
		return ""
	}
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	return styles.muted.Render("window " + formatWindowTime(since.In(loc)) + " - " + formatWindowTime(until.In(loc)))
}

func formatWindowTime(t time.Time) string {
	return t.Format("Jan 2, 2006 15:04 MST")
}

func repoScopeText(resp *MeRecapResponse) string {
	repos := recapRepoNames(resp)
	switch {
	case len(repos) == 1:
		return "repo " + repos[0]
	case len(repos) > 1:
		limit := min(len(repos), 3)
		text := "repos " + strings.Join(repos[:limit], ", ")
		if extra := len(repos) - limit; extra > 0 {
			text += fmt.Sprintf(" +%d more", extra)
		}
		return text
	case resp.Summary.RepoCount > 0:
		return plural(resp.Summary.RepoCount, "repo")
	default:
		return ""
	}
}

// recapRepoNames returns the deduplicated, trimmed repo names from the
// response. Order is preserved from the API so that repos the server ranked
// as most active appear first in the summary; if we sorted alphabetically
// here, the +N more overflow would hide the user's primary repo.
func recapRepoNames(resp *MeRecapResponse) []string {
	if resp.Repo != nil && strings.TrimSpace(*resp.Repo) != "" {
		return []string{strings.TrimSpace(*resp.Repo)}
	}
	if len(resp.Repos) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	repos := make([]string, 0, len(resp.Repos))
	for _, repo := range resp.Repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		repos = append(repos, repo)
	}
	return repos
}

func hasAgentFilter(opts RenderOptions) bool {
	return opts.Agent != "" && opts.Agent != AgentAll
}

func renderActivity(resp *MeRecapResponse, opts RenderOptions, width int, styles staticStyles) string {
	mostDate, _ := mostActive(resp.Daily)
	header := styles.title.Render("Activity") + styles.muted.Render(" · ") + rangeTag(opts.Range)
	if mostDate != "" {
		header = padRight(header, width-23) + styles.muted.Render("most active: ") + mostDate
	}
	lines := []string{header, renderActivityCells(resp.Daily, width, styles)}
	return strings.Join(lines, "\n")
}

func renderAgents(resp *MeRecapResponse, opts RenderOptions, width int, styles staticStyles) string {
	agents := filteredAgents(resp, opts)
	if len(agents) == 0 {
		return renderBox("Agents · "+strings.ToLower(opts.Range.Title()), []string{styles.muted.Render("(no agent activity in range)")}, width, styles)
	}
	lines := []string{}
	if opts.View == ViewBoth {
		lines = append(lines, "                                      "+styles.accent.Render("you ███")+"   "+styles.team.Render("team ▒"), "")
	}
	for i, entry := range agents {
		lines = append(lines, renderAgent(entry, opts, width-4, styles)...)
		if i < len(agents)-1 {
			lines = append(lines, "")
		}
	}
	return renderBox("Agents · "+strings.ToLower(opts.Range.Title()), lines, width, styles)
}

func renderAgent(entry AgentEntry, opts RenderOptions, width int, styles staticStyles) []string {
	label := entry.AgentLabel
	if label == "" {
		label = entry.AgentID
	}
	lines := []string{styles.title.Render(label)}
	metrics := []struct {
		name string
		me   int
		team int
	}{
		{"tokens", entry.Me.Tokens, teamValue(entry.Contributors, func(a AgentAggregate) int { return a.Tokens })},
		{"sessions", entry.Me.Sessions, teamValue(entry.Contributors, func(a AgentAggregate) int { return a.Sessions })},
		{"checkpoints", entry.Me.Checkpoints, teamValue(entry.Contributors, func(a AgentAggregate) int { return a.Checkpoints })},
	}
	for _, metric := range metrics {
		if metric.me == 0 && metric.team == 0 {
			continue
		}
		lines = append(lines, "  "+
			padRight(styles.muted.Render(metric.name), 12)+" "+
			padRight(comparisonBar(metric.me, metric.team, opts.View, 32, styles), 32)+" "+
			styles.value.Render(metricReadout(metric.name, metric.me, metric.team, opts.View)))
	}
	if opts.View != ViewYou && entry.Contributors != nil {
		lines = append(lines, qualitativeRows("team", *entry.Contributors, styles)...)
	}
	if opts.View != ViewTeam {
		lines = append(lines, qualitativeRows("your", entry.Me, styles)...)
	}
	return fitLines(lines, width)
}

func qualitativeRows(prefix string, agg AgentAggregate, styles staticStyles) []string {
	var rows []string
	if len(agg.Labels) > 0 {
		rows = append(rows, "  "+styles.muted.Render(prefix+" labels")+"    "+formatLabels(agg.Labels, styles))
	}
	if len(agg.Skills) > 0 {
		rows = append(rows, "  "+styles.muted.Render(prefix+" skills")+"    "+formatSkills(agg.Skills, styles))
	}
	if mix := formatToolMix(agg.ToolMix); mix != "" {
		rows = append(rows, "  "+styles.muted.Render(prefix+" tool mix")+"  "+mix)
	}
	return rows
}

func filteredAgents(resp *MeRecapResponse, opts RenderOptions) []AgentEntry {
	agents := make([]AgentEntry, 0, len(resp.Agents))
	for key, entry := range resp.Agents {
		if entry.AgentID == "" {
			entry.AgentID = key
		}
		if opts.Agent != "" && opts.Agent != AgentAll && opts.Agent != entry.AgentID {
			continue
		}
		agents = append(agents, entry)
	}
	sort.SliceStable(agents, func(i, j int) bool {
		iScore := agents[i].Me.Sessions + agents[i].Me.Checkpoints
		jScore := agents[j].Me.Sessions + agents[j].Me.Checkpoints
		if iScore != jScore {
			return iScore > jScore
		}
		return agents[i].AgentLabel < agents[j].AgentLabel
	})
	return agents
}

func sumMe(resp *MeRecapResponse, opts RenderOptions) SummaryTotals {
	var out SummaryTotals
	for _, entry := range filteredAgents(resp, opts) {
		out.Sessions += entry.Me.Sessions
		out.Checkpoints += entry.Me.Checkpoints
		out.Tokens += entry.Me.Tokens
	}
	return out
}

func sumTeam(resp *MeRecapResponse, opts RenderOptions) SummaryTotals {
	var out SummaryTotals
	for _, entry := range filteredAgents(resp, opts) {
		if entry.Contributors == nil {
			continue
		}
		out.Sessions += entry.Contributors.Sessions
		out.Checkpoints += entry.Contributors.Checkpoints
		out.Tokens += entry.Contributors.Tokens
	}
	return out
}

func topSignals(resp *MeRecapResponse, opts RenderOptions, styles staticStyles) []string {
	agents := filteredAgents(resp, opts)
	var parts []string
	if len(agents) > 0 {
		label := agents[0].AgentLabel
		if label == "" {
			label = agents[0].AgentID
		}
		parts = append(parts, styles.accent.Render(label))
	}
	if skill := topSkill(agents); skill != "" {
		parts = append(parts, styles.skill.Render(skill))
	}
	if label := topLabel(agents); label != "" {
		parts = append(parts, labelStyle(label, styles).Render(label))
	}
	return parts
}

func topSkill(agents []AgentEntry) string {
	counts := map[string]int{}
	for _, agent := range agents {
		for _, skill := range agent.Me.Skills {
			counts[skill.Skill] += skill.Count
		}
	}
	return topCount(counts)
}

func topLabel(agents []AgentEntry) string {
	counts := map[string]int{}
	for _, agent := range agents {
		for _, label := range agent.Me.Labels {
			counts[label.Label] += label.Count
		}
	}
	return topCount(counts)
}

func topCount(counts map[string]int) string {
	var keys []string
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	best := ""
	bestN := 0
	for _, key := range keys {
		if counts[key] > bestN {
			best = key
			bestN = counts[key]
		}
	}
	return best
}

func teamValue(agg *AgentAggregate, f func(AgentAggregate) int) int {
	if agg == nil {
		return 0
	}
	return f(*agg)
}

func metricReadout(metric string, me, team int, view ViewMode) string {
	format := strconv.Itoa
	if metric == "tokens" {
		format = formatTokens
	}
	switch view {
	case ViewYou:
		return format(me)
	case ViewTeam:
		return format(team)
	case ViewBoth:
		if team == 0 {
			return format(me) + " / -"
		}
		return format(me) + " / " + format(team)
	default:
		return format(me)
	}
}

func comparisonBar(me, team int, view ViewMode, width int, styles staticStyles) string {
	switch view {
	case ViewYou:
		return strings.Repeat(styles.accent.Render("█"), scaledWidth(me, me, width))
	case ViewTeam:
		return strings.Repeat(styles.team.Render("▒"), scaledWidth(team, team, width))
	case ViewBoth:
		total := me + team
		if total == 0 {
			return ""
		}
		meWidth := scaledWidth(me, total, width)
		teamWidth := scaledWidth(team, total, width)
		return strings.Repeat(styles.accent.Render("█"), meWidth) + strings.Repeat(styles.team.Render("▒"), teamWidth)
	default:
		return ""
	}
}

func scaledWidth(value, total, width int) int {
	if value <= 0 || total <= 0 || width <= 0 {
		return 0
	}
	n := int(math.Round(float64(value) * float64(width) / float64(total)))
	if n == 0 {
		return 1
	}
	if n > width {
		return width
	}
	return n
}

func formatLabels(labels []LabelCount, styles staticStyles) string {
	limit := min(len(labels), 3)
	parts := make([]string, 0, limit)
	for i := range limit {
		parts = append(parts, labelStyle(labels[i].Label, styles).Render("● "+labels[i].Label))
	}
	return strings.Join(parts, "  ")
}

func formatSkills(skills []SkillCount, styles staticStyles) string {
	limit := min(len(skills), 3)
	parts := make([]string, 0, limit)
	for i := range limit {
		parts = append(parts, styles.skill.Render(skills[i].Skill))
	}
	return strings.Join(parts, ", ")
}

func labelStyle(name string, styles staticStyles) interface{ Render(s ...string) string } {
	switch name {
	case "feature_build", "enhancement":
		return styles.labelFeature
	case "bug_fix", "security_fix":
		return styles.labelFix
	case "refactor", "optimization":
		return styles.labelRefactor
	case "testing":
		return styles.labelTesting
	case "configuration", "dependencies", "documentation", "investigation":
		return styles.labelInfo
	case "performance":
		return styles.labelPerf
	default:
		return styles.value
	}
}

func formatToolMix(mix ToolMix) string {
	values := []struct {
		name  string
		count int
	}{
		{"fileOps", mix.FileOps},
		{"search", mix.Search},
		{"shell", mix.Shell},
		{"mcp", mix.MCP},
		{"agent", mix.Agent},
		{"other", mix.Other},
	}
	total := 0
	for _, value := range values {
		total += value.count
	}
	if total == 0 {
		return ""
	}
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].count != values[j].count {
			return values[i].count > values[j].count
		}
		return values[i].name < values[j].name
	})
	limit := min(len(values), 3)
	parts := make([]string, 0, limit)
	for i := range limit {
		if values[i].count == 0 {
			continue
		}
		pct := int(math.Round(float64(values[i].count) * 100 / float64(total)))
		parts = append(parts, fmt.Sprintf("%s %d%%", values[i].name, pct))
	}
	return strings.Join(parts, " · ")
}

func renderActivityCells(daily []DailyCount, width int, styles staticStyles) string {
	if len(daily) == 0 {
		return "(no activity in range)"
	}
	inner := width - 2
	if inner < 1 {
		inner = 1
	}
	if len(daily) > inner {
		daily = daily[len(daily)-inner:]
	}
	maxCount := 0
	for _, day := range daily {
		if day.Count > maxCount {
			maxCount = day.Count
		}
	}
	var b strings.Builder
	for _, day := range daily {
		b.WriteString(activityCell(day.Count, maxCount, styles))
	}
	return b.String()
}

func activityCell(count, maxCount int, styles staticStyles) string {
	r := activityRune(count, maxCount)
	if count <= 0 || maxCount <= 0 {
		return styles.activityEmpty.Render(string(r))
	}
	ratio := float64(count) / float64(maxCount)
	switch {
	case ratio >= 0.75:
		return styles.activityHigh.Render(string(r))
	case ratio >= 0.5:
		return styles.activityMid.Render(string(r))
	default:
		return styles.activityLow.Render(string(r))
	}
}

func activityRune(count, maxCount int) rune {
	if count <= 0 || maxCount <= 0 {
		return '░'
	}
	ratio := float64(count) / float64(maxCount)
	switch {
	case ratio >= 0.75:
		return '█'
	case ratio >= 0.5:
		return '▓'
	case ratio >= 0.25:
		return '▒'
	default:
		return '░'
	}
}

func mostActive(daily []DailyCount) (string, int) {
	bestDate := ""
	bestCount := 0
	for _, day := range daily {
		if day.Count > bestCount {
			bestDate = day.Date
			bestCount = day.Count
		}
	}
	return bestDate, bestCount
}

func renderBox(title string, lines []string, width int, styles staticStyles) string {
	inner := width - 2
	top := "╭" + strings.Repeat("─", inner) + "╮"
	if title != "" {
		titleText := "─ " + styles.title.Render(title) + " "
		top = "╭" + titleText + strings.Repeat("─", max(inner-displayLen(titleText), 0)) + "╮"
	}
	out := []string{styles.border.Render(top)}
	for _, line := range lines {
		out = append(out, styles.border.Render("│")+padRight("  "+line, inner)+styles.border.Render("│"))
	}
	out = append(out, styles.border.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

func fitLines(lines []string, width int) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, truncateRunes(line, width))
	}
	return out
}

func padRight(s string, width int) string {
	if displayLen(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-displayLen(s))
}

func truncateRunes(s string, width int) string {
	if displayLen(s) <= width {
		return s
	}
	if width <= 1 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

func displayLen(s string) int {
	return ansi.StringWidth(s)
}

func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return strconv.Itoa(n)
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func rangeTag(r RangeKey) string {
	if r == Range90d {
		return "90d"
	}
	return string(r)
}
