package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"
)

type activityStyles struct {
	colorEnabled bool
	width        int

	bold    lipgloss.Style
	dim     lipgloss.Style
	label   lipgloss.Style
	value   lipgloss.Style
	unit    lipgloss.Style
	desc    lipgloss.Style
	repoNm  lipgloss.Style
	commitH lipgloss.Style
	commitM lipgloss.Style
	add     lipgloss.Style
	del     lipgloss.Style
	muted   lipgloss.Style
}

// getFullTerminalWidth returns the terminal width without the 80-char cap
// used by other commands. Activity benefits from wide output for bar charts.
func getFullTerminalWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 { //nolint:gosec // G115: uintptr->int is safe for fd
			return width
		}
	}
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		if f == nil {
			continue
		}
		if width, _, err := term.GetSize(int(f.Fd())); err == nil && width > 0 { //nolint:gosec // G115: uintptr->int is safe for fd
			return width
		}
	}
	return 80
}

func newActivityStyles(w io.Writer) activityStyles {
	useColor := shouldUseColor(w)
	width := getFullTerminalWidth(w)

	s := activityStyles{
		colorEnabled: useColor,
		width:        width,
	}

	if useColor {
		s.bold = lipgloss.NewStyle().Bold(true)
		s.dim = lipgloss.NewStyle().Faint(true)
		s.label = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true)
		s.value = lipgloss.NewStyle().Bold(true)
		s.unit = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		s.desc = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		s.repoNm = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
		s.commitH = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		s.commitM = lipgloss.NewStyle().Bold(true)
		s.add = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
		s.del = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
		s.muted = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	}

	return s
}

func (s activityStyles) render(style lipgloss.Style, text string) string {
	if !s.colorEnabled {
		return text
	}
	return style.Render(text)
}

func (s activityStyles) renderAgent(agentID, text string) string {
	if !s.colorEnabled {
		return text
	}
	display := agentDisplayMap[agentID]
	return lipgloss.NewStyle().Foreground(lipgloss.Color(display.Color)).Render(text)
}

type agentDisplay struct {
	Label string
	Color string // ANSI 256 color code
	Char  rune   // block character for bar charts
}

// Agent colors match the dark-mode CSS variables from entire.io (Tailwind 400-level).
// Lipgloss resolves hex to the best representation for the terminal's color profile.
var agentDisplayMap = map[string]agentDisplay{
	"claude":   {Label: "Claude Code", Color: "#fb923c", Char: '▓'}, // orange-400
	"gemini":   {Label: "Gemini", Color: "#60a5fa", Char: '▓'},      // blue-400
	"amp":      {Label: "Amp", Color: "#f87171", Char: '▓'},         // red-400
	"codex":    {Label: "Codex", Color: "#818cf8", Char: '▓'},       // indigo-400
	"opencode": {Label: "OpenCode", Color: "#22d3ee", Char: '▓'},    // cyan-400
	"copilot":  {Label: "Copilot", Color: "#a78bfa", Char: '▓'},     // violet-400
	"pi":       {Label: "Pi", Color: "#fbbf24", Char: '▓'},          // amber-400
	"cursor":   {Label: "Cursor", Color: "#38bdf8", Char: '▓'},      // sky-400
	"droid":    {Label: "Droid", Color: "#f472b6", Char: '▓'},       // pink-400
	"kiro":     {Label: "Kiro", Color: "#c084fc", Char: '▓'},        // purple-400
	"unknown":  {Label: "Unknown", Color: "245", Char: '░'},
}

var agentOrder = []string{
	"claude", "codex", "gemini", "amp", "opencode",
	"copilot", "pi", "cursor", "droid", "kiro", "unknown",
}

func renderActivity(w io.Writer, sty activityStyles, stats contributionStats, repos []repoContribution, hourly []hourlyPoint, days []commitDay) {
	fmt.Fprintln(w)
	renderStatCards(w, sty, stats)
	fmt.Fprintln(w)
	renderContributionChart(w, sty, hourly, repos)
	fmt.Fprintln(w)
	renderRepoChart(w, sty, repos)
	fmt.Fprintln(w)
	renderCommitList(w, sty, days)
}

func renderStatCards(w io.Writer, sty activityStyles, stats contributionStats) {
	cards := []struct {
		label string
		value string
		unit  string
		desc  string
	}{
		{"THROUGHPUT", fmt.Sprintf("%.1f", stats.Throughput), "k", "Avg. tokens/checkpoint"},
		{"ITERATION", fmt.Sprintf("%.1f", stats.Iteration), "x", "Avg sessions/checkpoint"},
		{"CONTINUITY", fmt.Sprintf("%.1f", stats.ContinuityH), "h", "Peak session length"},
		{"STREAK", strconv.Itoa(stats.Streak), " day", fmt.Sprintf("%d current", stats.CurrentStreak)},
	}

	cardWidth := (sty.width - 3) / 4 // 3 separators
	if cardWidth < 16 {
		cardWidth = 16
	}

	var topParts, midParts, botParts []string
	for _, c := range cards {
		lbl := padOrTruncate(c.label, cardWidth)
		dsc := padOrTruncate(c.desc, cardWidth)

		topParts = append(topParts, sty.render(sty.label, lbl))
		midParts = append(midParts, sty.render(sty.value, c.value)+sty.render(sty.unit, c.unit)+
			strings.Repeat(" ", max(0, cardWidth-len(c.value)-len(c.unit))))
		botParts = append(botParts, sty.render(sty.desc, dsc))
	}

	sep := sty.render(sty.dim, " │ ")
	fmt.Fprintln(w, strings.Join(topParts, sep))
	fmt.Fprintln(w, strings.Join(midParts, sep))
	fmt.Fprintln(w, strings.Join(botParts, sep))
}

func renderContributionChart(w io.Writer, sty activityStyles, hourly []hourlyPoint, repos []repoContribution) {
	renderDotChart(w, sty, hourly, repos)
}

func renderDotChart(w io.Writer, sty activityStyles, hourly []hourlyPoint, repos []repoContribution) {
	agentTotals := make(map[string]int)
	total := 0
	for _, r := range repos {
		total += r.Total
		for agent, count := range r.Agents {
			agentTotals[agent] += count
		}
	}

	totalLabel := ""
	if total > 0 {
		totalLabel = sty.render(sty.muted, fmt.Sprintf("  %d checkpoints", total))
	}
	fmt.Fprintf(w, "%s%s\n", sty.render(sty.label, "CONTRIBUTIONS"), totalLabel)

	if len(hourly) == 0 {
		fmt.Fprintln(w, sty.render(sty.muted, "  No activity data"))
		return
	}

	now := time.Now().Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	numDays := 30

	// 4 hour bands: 0-5, 6-11, 12-17, 18-23
	type hourBand struct {
		label  string
		lo, hi int
	}
	bands := []hourBand{
		{" 0", 0, 5},
		{" 6", 6, 11},
		{"12", 12, 17},
		{"18", 18, 23},
	}

	// Grid: [band][day] -> best point
	type cell struct {
		value   int
		agentID string
	}
	grid := make([][]cell, len(bands))
	for i := range grid {
		grid[i] = make([]cell, numDays)
	}

	for _, p := range hourly {
		pt, err := time.ParseInLocation("2006-01-02", p.Date, time.Local)
		if err != nil {
			continue
		}
		dayIdx := int(today.Sub(pt).Hours() / 24)
		col := numDays - 1 - dayIdx
		if col < 0 || col >= numDays {
			continue
		}
		band := -1
		for i, hb := range bands {
			if p.Hour >= hb.lo && p.Hour <= hb.hi {
				band = i
				break
			}
		}
		if band < 0 {
			continue
		}
		if p.Value > grid[band][col].value {
			grid[band][col] = cell{value: p.Value, agentID: p.AgentID}
		}
	}

	sizeChars := []string{" ", "·", "•", "●", "⬤"}
	sizeFor := func(v int) int {
		switch {
		case v == 0:
			return 0
		case v <= 2:
			return 1
		case v <= 5:
			return 2
		case v <= 9:
			return 3
		default:
			return 4
		}
	}

	labelWidth := 3
	availCols := sty.width - labelWidth - 2
	colWidth := availCols / numDays
	if colWidth < 1 {
		colWidth = 1
	}

	// Date axis
	fmt.Fprint(w, strings.Repeat(" ", labelWidth+1))
	startDate := today.AddDate(0, 0, -(numDays - 1))
	lastMonth := ""
	for d := 0; d < numDays; d++ {
		date := startDate.AddDate(0, 0, d)
		month := date.Format("Jan")
		if month != lastMonth {
			fmt.Fprint(w, sty.render(sty.muted, month))
			labelCols := int(math.Ceil(float64(len(month)) / float64(max(colWidth, 1))))
			if labelCols < 1 {
				labelCols = 1
			}
			d += labelCols - 1
			lastMonth = month
		} else {
			fmt.Fprint(w, strings.Repeat(" ", colWidth))
		}
	}
	fmt.Fprintln(w)

	// Grid rows
	for band, hb := range bands {
		fmt.Fprint(w, sty.render(sty.dim, hb.label)+" ")
		for col := range numDays {
			c := grid[band][col]
			sz := sizeFor(c.value)
			if sz == 0 {
				fmt.Fprint(w, strings.Repeat(" ", colWidth))
			} else {
				fmt.Fprint(w, sty.renderAgent(c.agentID, sizeChars[sz])+strings.Repeat(" ", colWidth-1))
			}
		}
		fmt.Fprintln(w)
	}

	// Size legend
	fmt.Fprintln(w, sty.render(sty.dim, "     · 1-2   • 3-5   ● 6-9   ⬤ 10+"))

	// Agent legend
	if total > 0 {
		var parts []string
		for _, id := range agentOrder {
			count, ok := agentTotals[id]
			if !ok || count == 0 {
				continue
			}
			pct := float64(count) / float64(total) * 100
			display := agentDisplayMap[id]
			parts = append(parts, sty.renderAgent(id, fmt.Sprintf("● %s %d%%", display.Label, int(math.Round(pct)))))
		}
		fmt.Fprintln(w, strings.Join(parts, sty.render(sty.dim, "  ")))
	}
}

// renderBrailleChart is an alternative contribution chart using Unicode braille
// characters for higher resolution. Swap renderDotChart → renderBrailleChart in
// renderContributionChart to enable it.
var _ = renderBrailleChart // keep compiled while inactive

func renderBrailleChart(w io.Writer, sty activityStyles, hourly []hourlyPoint, repos []repoContribution) {
	// Agent breakdown header + total
	agentTotals := make(map[string]int)
	total := 0
	for _, r := range repos {
		total += r.Total
		for agent, count := range r.Agents {
			agentTotals[agent] += count
		}
	}

	// Header line: CONTRIBUTIONS  N checkpoints
	totalLabel := ""
	if total > 0 {
		totalLabel = sty.render(sty.muted, fmt.Sprintf("  %d checkpoints", total))
	}
	fmt.Fprintf(w, "%s%s\n", sty.render(sty.label, "CONTRIBUTIONS"), totalLabel)

	if len(hourly) == 0 {
		fmt.Fprintln(w, sty.render(sty.muted, "  No activity data"))
		return
	}

	// Determine date range
	now := time.Now().Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	numDays := 30

	labelWidth := 3
	chartCols := sty.width - labelWidth - 2
	if chartCols < 20 {
		chartCols = 20
	}

	// 6 braille rows × 4 dots = 24 hours
	chartRows := 6
	dotsX := chartCols * 2
	dotsY := chartRows * 4

	type dotInfo struct {
		set     bool
		agentID string
		value   int
	}
	dotGrid := make([][]dotInfo, dotsY)
	for i := range dotGrid {
		dotGrid[i] = make([]dotInfo, dotsX)
	}

	for _, p := range hourly {
		pt, err := time.ParseInLocation("2006-01-02", p.Date, time.Local)
		if err != nil {
			continue
		}
		dayIdx := int(today.Sub(pt).Hours() / 24)
		dayFromStart := numDays - 1 - dayIdx
		if dayFromStart < 0 || dayFromStart >= numDays {
			continue
		}

		dx := int(float64(dayFromStart) / float64(numDays) * float64(dotsX))
		if dx >= dotsX {
			dx = dotsX - 1
		}

		dy := p.Hour
		if dy >= dotsY {
			dy = dotsY - 1
		}

		// Bubble radius scales with value
		radius := 0
		if p.Value >= 3 {
			radius = 1
		}
		if p.Value >= 8 {
			radius = 2
		}

		for oy := -radius; oy <= radius; oy++ {
			for ox := -radius; ox <= radius; ox++ {
				if ox*ox+oy*oy > radius*radius+1 {
					continue
				}
				ny, nx := dy+oy, dx+ox
				if ny >= 0 && ny < dotsY && nx >= 0 && nx < dotsX {
					if p.Value > dotGrid[ny][nx].value {
						dotGrid[ny][nx] = dotInfo{set: true, agentID: p.AgentID, value: p.Value}
					}
				}
			}
		}
	}

	// Date axis labels
	fmt.Fprint(w, strings.Repeat(" ", labelWidth+1))
	startDate := today.AddDate(0, 0, -(numDays - 1))
	lastMonth := ""
	for col := 0; col < chartCols; col++ {
		dayFrac := float64(col) / float64(chartCols) * float64(numDays)
		date := startDate.AddDate(0, 0, int(dayFrac))
		month := date.Format("Jan")
		if month != lastMonth {
			fmt.Fprint(w, sty.render(sty.muted, month))
			col += len(month) - 1
			lastMonth = month
		} else {
			fmt.Fprint(w, " ")
		}
	}
	fmt.Fprintln(w)

	// Render braille grid
	hourLabels := []string{" 0", " 4", " 8", "12", "16", "20"}

	for cy := range chartRows {
		fmt.Fprint(w, sty.render(sty.dim, hourLabels[cy])+" ")

		for cx := range chartCols {
			var brailleCode rune = 0x2800
			dominantAgent := ""
			maxVal := 0

			// Braille dot layout: each char is 2 wide × 4 tall
			//   dot1 dot4     bit0 bit3
			//   dot2 dot5     bit1 bit4
			//   dot3 dot6     bit2 bit5
			//   dot7 dot8     bit6 bit7
			dotBits := [8][2]int{
				{0, 0}, {1, 0}, {2, 0}, {0, 1},
				{1, 1}, {2, 1}, {3, 0}, {3, 1},
			}

			for bit, offset := range dotBits {
				dy := cy*4 + offset[0]
				dx := cx*2 + offset[1]
				if dy < dotsY && dx < dotsX && dotGrid[dy][dx].set {
					brailleCode |= 1 << bit
					if dotGrid[dy][dx].value > maxVal {
						maxVal = dotGrid[dy][dx].value
						dominantAgent = dotGrid[dy][dx].agentID
					}
				}
			}

			if brailleCode == 0x2800 {
				fmt.Fprint(w, " ")
			} else {
				fmt.Fprint(w, sty.renderAgent(dominantAgent, string(brailleCode)))
			}
		}
		fmt.Fprintln(w)
	}

	// Agent legend
	if total > 0 {
		var parts []string
		for _, id := range agentOrder {
			count, ok := agentTotals[id]
			if !ok || count == 0 {
				continue
			}
			pct := float64(count) / float64(total) * 100
			display := agentDisplayMap[id]
			parts = append(parts, sty.renderAgent(id, fmt.Sprintf("● %s %d%%", display.Label, int(math.Round(pct)))))
		}
		fmt.Fprintln(w, strings.Join(parts, sty.render(sty.dim, "  ")))
	}
}

func renderRepoChart(w io.Writer, sty activityStyles, repos []repoContribution) {
	if len(repos) == 0 {
		return
	}

	fmt.Fprintln(w, sty.render(sty.label, "REPOSITORIES"))

	maxRepos := 5
	if len(repos) < maxRepos {
		maxRepos = len(repos)
	}
	display := repos[:maxRepos]

	maxCount := display[0].Total

	maxNameLen := 0
	for _, r := range display {
		if len(r.Repo) > maxNameLen {
			maxNameLen = len(r.Repo)
		}
	}
	if maxNameLen > 30 {
		maxNameLen = 30
	}

	countWidth := len(strconv.Itoa(maxCount))
	barWidth := sty.width - maxNameLen - countWidth - 4
	if barWidth < 10 {
		barWidth = 10
	}

	for _, r := range display {
		name := r.Repo
		if len(name) > maxNameLen {
			name = name[:maxNameLen-1] + "…"
		}
		name = fmt.Sprintf("%-*s", maxNameLen, name)

		bar := renderAgentBar(sty, r.Agents, maxCount, barWidth)
		count := fmt.Sprintf("%*d", countWidth, r.Total)

		fmt.Fprintf(w, "%s %s %s\n",
			sty.render(sty.repoNm, name),
			bar,
			sty.render(sty.muted, count))
	}
}

func renderAgentBar(sty activityStyles, agents map[string]int, maxCount, barWidth int) string {
	if maxCount == 0 {
		return strings.Repeat(" ", barWidth)
	}

	var b strings.Builder

	filled := 0
	for _, id := range agentOrder {
		count, ok := agents[id]
		if !ok || count == 0 {
			continue
		}
		segWidth := int(math.Round(float64(count) / float64(maxCount) * float64(barWidth)))
		if segWidth < 1 && count > 0 {
			segWidth = 1
		}
		if filled+segWidth > barWidth {
			segWidth = barWidth - filled
		}
		if segWidth <= 0 {
			continue
		}

		display := agentDisplayMap[id]
		seg := strings.Repeat(string(display.Char), segWidth)
		b.WriteString(sty.renderAgent(id, seg))
		filled += segWidth
	}

	if filled < barWidth {
		b.WriteString(sty.render(sty.dim, strings.Repeat("░", barWidth-filled)))
	}

	return b.String()
}

func renderCommitList(w io.Writer, sty activityStyles, days []commitDay) {
	renderCommitListN(w, sty, days, 3)
}

func renderCommitListN(w io.Writer, sty activityStyles, days []commitDay, maxDays int) {
	if len(days) == 0 {
		return
	}

	if maxDays <= 0 || maxDays > len(days) {
		maxDays = len(days)
	}

	for _, day := range days[:maxDays] {
		displayDate := formatCommitDate(day.Date)
		commitWord := "commits"
		if len(day.Commits) == 1 {
			commitWord = "commit"
		}

		fmt.Fprintf(w, "%s  %s\n",
			sty.render(sty.bold, displayDate),
			sty.render(sty.muted, fmt.Sprintf("%d %s", len(day.Commits), commitWord)))

		for _, c := range day.Commits {
			sha := c.CommitSHA
			if len(sha) > 7 {
				sha = sha[:7]
			}

			msg := "(no message)"
			if c.CommitMsg != nil && *c.CommitMsg != "" {
				msg = firstLine(*c.CommitMsg)
			}

			var badges []string
			for _, a := range uniqueCommitAgents(c) {
				display := agentDisplayMap[a]
				badges = append(badges, sty.renderAgent(a, display.Label))
			}

			fileStats := fmt.Sprintf("%d files", c.FilesChanged)
			if c.FilesChanged == 1 {
				fileStats = "1 file"
			}

			cpCount := len(c.Checkpoints)
			cpStr := ""
			if cpCount == 1 {
				cpStr = "1 checkpoint"
			} else if cpCount > 1 {
				cpStr = fmt.Sprintf("%d checkpoints", cpCount)
			}

			// Build right-aligned stats: +N / -N  M files  [K checkpoints]
			var statParts []string
			statParts = append(statParts,
				sty.render(sty.add, fmt.Sprintf("+%d", c.Additions))+
					sty.render(sty.muted, " / ")+
					sty.render(sty.del, fmt.Sprintf("-%d", c.Deletions)))
			statParts = append(statParts, sty.render(sty.muted, fileStats))
			if cpStr != "" {
				statParts = append(statParts, sty.render(sty.muted, cpStr))
			}
			rightSide := strings.Join(statParts, sty.render(sty.dim, "  "))
			// Plain length for padding (ANSI codes don't count)
			rightPlain := fmt.Sprintf("+%d / -%d  %s", c.Additions, c.Deletions, fileStats)
			if cpStr != "" {
				rightPlain += "  " + cpStr
			}

			// Build left side: hash  message  repo  [badges]
			left := sty.render(sty.commitH, sha) + " " +
				sty.render(sty.commitM, msg) + " " +
				sty.render(sty.muted, c.RepoFullName)
			leftPlain := sha + " " + msg + " " + c.RepoFullName
			var leftSb359 strings.Builder
			for _, badge := range badges {
				leftSb359.WriteString("  " + badge)
			}
			left += leftSb359.String()
			var leftPlainSb362 strings.Builder
			for _, a := range uniqueCommitAgents(c) {
				leftPlainSb362.WriteString("  " + agentDisplayMap[a].Label)
			}
			leftPlain += leftPlainSb362.String()

			// Truncate message if line would exceed width
			maxMsg := sty.width - (lipgloss.Width(leftPlain) - lipgloss.Width(msg)) - lipgloss.Width(rightPlain) - 2
			if maxMsg < 10 {
				maxMsg = 10
			}
			if lipgloss.Width(msg) > maxMsg {
				msg = truncateDisplayWidth(msg, maxMsg, "...")
				// Rebuild left side with truncated message
				left = sty.render(sty.commitH, sha) + " " +
					sty.render(sty.commitM, msg) + " " +
					sty.render(sty.muted, c.RepoFullName)
				leftPlain = sha + " " + msg + " " + c.RepoFullName
				var leftSb378 strings.Builder
				for _, badge := range badges {
					leftSb378.WriteString("  " + badge)
				}
				left += leftSb378.String()
				var leftPlainSb381 strings.Builder
				for _, a := range uniqueCommitAgents(c) {
					leftPlainSb381.WriteString("  " + agentDisplayMap[a].Label)
				}
				leftPlain += leftPlainSb381.String()
			}

			gap := sty.width - lipgloss.Width(leftPlain) - lipgloss.Width(rightPlain)
			if gap < 2 {
				gap = 2
			}
			fmt.Fprintf(w, "%s%s%s\n", left, strings.Repeat(" ", gap), rightSide)
		}
		fmt.Fprintln(w)
	}
}

func uniqueCommitAgents(c userCommit) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, cp := range c.Checkpoints {
		agents := cp.Agents
		// Fall back to singular Agent field when Agents slice is empty
		if len(agents) == 0 && cp.Agent != "" {
			agents = []string{cp.Agent}
		}
		for _, a := range agents {
			id := normalizeAgentString(a)
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				result = append(result, id)
			}
		}
	}
	sort.Strings(result)
	return result
}

func formatCommitDate(dateStr string) string {
	t, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
	if err != nil {
		return dateStr
	}
	now := time.Now().Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	days := int(today.Sub(t).Hours() / 24)

	switch days {
	case 0:
		return t.Format("Monday 2 Jan") + " (today)"
	case 1:
		return t.Format("Monday 2 Jan") + " (yesterday)"
	default:
		return t.Format("Monday 2 Jan")
	}
}

func padOrTruncate(s string, width int) string {
	runes := []rune(s)
	if len(runes) > width {
		return string(runes[:width-1]) + "…"
	}
	return s + strings.Repeat(" ", width-len(runes))
}

func truncateDisplayWidth(s string, width int, tail string) string {
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(tail) >= width {
		return tail[:width]
	}

	var b strings.Builder
	remaining := width - lipgloss.Width(tail)
	for _, r := range s {
		if lipgloss.Width(b.String()+string(r)) > remaining {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + tail
}
