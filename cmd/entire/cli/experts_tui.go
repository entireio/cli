package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/entireio/cli/cmd/entire/cli/palette"
)

// Layout budget for the experts viewer. The header is a single title line
// followed by one blank line; the footer is a single help line. The remaining
// rows are split between the ranked-agent list (left) and the evidence detail
// (right), separated by a thin vertical rule.
const (
	expertsHeaderHeight = 2
	expertsFooterHeight = 1
	expertsListMinWidth = 22
	expertsListMaxWidth = 36
	expertsPaneGap      = 3 // " │ "
)

// expertsTUIStyles extends the plain-output palette with the few extra styles
// the interactive viewer needs (selection, section headers, footer help).
type expertsTUIStyles struct {
	expertsStyles

	selected lipgloss.Style
	section  lipgloss.Style
	helpKey  lipgloss.Style
	helpDesc lipgloss.Style
	helpSep  lipgloss.Style
	sepBar   lipgloss.Style
}

func newExpertsTUIStyles(useColor bool) expertsTUIStyles {
	s := expertsTUIStyles{expertsStyles: expertsStylesForColor(useColor)}
	if !useColor {
		return s
	}
	s.selected = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true)
	s.section = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Accent)).Bold(true)
	s.helpKey = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Bold(true)
	s.helpDesc = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Faint(true)
	s.helpSep = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Faint(true)
	s.sepBar = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted))
	return s
}

// expertsTUIModel renders a master-detail view over a pre-fetched experts
// response: a ranked list of agent profiles on the left and the selected
// profile's evidence on the right. No API calls happen here — the data is
// fetched once by runExperts and handed in.
type expertsTUIModel struct {
	resp   expertsResponse
	styles expertsTUIStyles

	cursor   int
	expanded bool // expand per-session evidence (matched files, checkpoint ids)

	width  int
	height int
	ready  bool

	vp             viewport.Model
	sectionOffsets []int
	sectionIdx     int
}

func newExpertsTUIModel(resp expertsResponse, useColor bool) expertsTUIModel {
	return expertsTUIModel{
		resp:   resp,
		styles: newExpertsTUIStyles(useColor),
	}
}

func runExpertsTUI(resp expertsResponse, useColor bool) error {
	p := tea.NewProgram(newExpertsTUIModel(resp, useColor))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("experts TUI: %w", err)
	}
	return nil
}

func (m expertsTUIModel) Init() tea.Cmd { return nil }

func (m expertsTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m = m.layout()
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	if m.ready {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m expertsTUIModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Quit), key.Matches(msg, keys.Back):
		return m, tea.Quit
	case key.Matches(msg, keys.Up):
		if m.cursor > 0 {
			m.cursor--
			m = m.selectionChanged()
		}
		return m, nil
	case key.Matches(msg, keys.Down):
		if m.cursor < len(m.resp.Profiles)-1 {
			m.cursor++
			m = m.selectionChanged()
		}
		return m, nil
	case key.Matches(msg, keys.Home):
		if m.cursor != 0 {
			m.cursor = 0
			m = m.selectionChanged()
		}
		return m, nil
	case key.Matches(msg, keys.End):
		if last := len(m.resp.Profiles) - 1; last >= 0 && m.cursor != last {
			m.cursor = last
			m = m.selectionChanged()
		}
		return m, nil
	case key.Matches(msg, keys.Confirm):
		m.expanded = !m.expanded
		m = m.refreshDetail()
		return m, nil
	case msg.String() == "o":
		if url := m.primarySessionURL(); url != "" {
			return m, openExpertsSessionCmd(url)
		}
		return m, nil
	case msg.String() == "tab":
		m = m.jumpSection(1)
		return m, nil
	case msg.String() == "shift+tab":
		m = m.jumpSection(-1)
		return m, nil
	}

	if m.ready {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

// selectionChanged collapses expanded evidence and rebuilds the detail pane for
// the newly selected agent. Switching agents always starts from a clean,
// top-aligned, collapsed view.
func (m expertsTUIModel) selectionChanged() expertsTUIModel {
	m.expanded = false
	return m.refreshDetail()
}

func (m expertsTUIModel) layout() expertsTUIModel {
	if m.width <= 0 || m.height <= 0 {
		return m
	}
	bodyH := m.bodyHeight()
	rightW := m.rightPaneWidth()
	if !m.ready {
		m.vp = viewport.New(viewport.WithWidth(rightW), viewport.WithHeight(bodyH))
		m.ready = true
	} else {
		m.vp.SetWidth(rightW)
		m.vp.SetHeight(bodyH)
	}
	return m.refreshDetail()
}

func (m expertsTUIModel) refreshDetail() expertsTUIModel {
	if !m.ready {
		return m
	}
	content, offsets := m.renderDetail(m.rightPaneWidth())
	m.sectionOffsets = offsets
	m.sectionIdx = 0
	m.vp.SetContent(content)
	m.vp.GotoTop()
	return m
}

// jumpSection scrolls the detail viewport to the next/previous section header so
// tab cycles through EVIDENCE, SKILLS, TOOLS, MCP, FILES, SESSIONS.
func (m expertsTUIModel) jumpSection(dir int) expertsTUIModel {
	if !m.ready || len(m.sectionOffsets) == 0 {
		return m
	}
	n := len(m.sectionOffsets)
	m.sectionIdx = (m.sectionIdx + dir + n) % n
	m.vp.SetYOffset(m.sectionOffsets[m.sectionIdx])
	return m
}

// primarySessionURL returns the entire.io URL of the selected agent's strongest
// evidence session (the first, since sessions are ranked), or "" when there is
// nothing to open.
func (m expertsTUIModel) primarySessionURL() string {
	if m.cursor < 0 || m.cursor >= len(m.resp.Profiles) {
		return ""
	}
	sessions := m.resp.Profiles[m.cursor].Sessions
	if len(sessions) == 0 {
		return ""
	}
	return expertsSessionURL(m.resp.RepoFullName, sessions[0].SessionID)
}

// openExpertsSessionCmd opens url in the user's browser off the UI thread.
// openBrowser refuses non-HTTP URLs and is a no-op under test.
func openExpertsSessionCmd(url string) tea.Cmd {
	return func() tea.Msg {
		if err := openBrowser(context.Background(), url); err != nil {
			// Browser open is best-effort; the session URL is still visible in the TUI.
			return nil
		}
		return nil
	}
}

func (m expertsTUIModel) bodyHeight() int {
	h := m.height - expertsHeaderHeight - expertsFooterHeight
	if h < 1 {
		h = 1
	}
	return h
}

func (m expertsTUIModel) listPaneWidth() int {
	w := m.width * 32 / 100
	if w < expertsListMinWidth {
		w = expertsListMinWidth
	}
	if w > expertsListMaxWidth {
		w = expertsListMaxWidth
	}
	if maxList := m.width - expertsPaneGap - 10; w > maxList {
		w = max(maxList, 1)
	}
	return w
}

func (m expertsTUIModel) rightPaneWidth() int {
	return max(m.width-m.listPaneWidth()-expertsPaneGap, 1)
}

func (m expertsTUIModel) View() tea.View {
	v := tea.View{AltScreen: true}
	if m.width <= 0 || m.height <= 0 || !m.ready {
		return v
	}
	content := m.renderHeader() + "\n\n" + m.renderBody() + "\n" + m.renderFooter()
	v.SetContent(clampToHeight(content, m.height))
	return v
}

func (m expertsTUIModel) renderHeader() string {
	parts := []string{
		m.styles.render(m.styles.title, "Agent provenance"),
		m.styles.render(m.styles.file, m.resp.RepoFullName),
	}
	if m.resp.Branch != "" {
		parts = append(parts, m.styles.render(m.styles.muted, "("+m.resp.Branch+")"))
	}
	line := strings.Join(parts, " ")
	if scope := m.scopeLabel(); scope != "" {
		line += "  " + m.styles.render(m.styles.muted, "· "+scope)
	}
	return m.fitLine(line, m.width)
}

func (m expertsTUIModel) scopeLabel() string {
	if m.resp.Query != nil && strings.TrimSpace(*m.resp.Query) != "" {
		return *m.resp.Query
	}
	return strings.Join(m.resp.Scopes, ", ")
}

func (m expertsTUIModel) renderBody() string {
	bodyH := m.bodyHeight()
	left := m.renderList(m.listPaneWidth(), bodyH)
	sep := m.verticalSep(bodyH)
	right := m.vp.View()
	return lipgloss.JoinHorizontal(lipgloss.Top, left, sep, right)
}

func (m expertsTUIModel) verticalSep(h int) string {
	bar := " " + m.styles.render(m.styles.sepBar, "│") + " "
	lines := make([]string, h)
	for i := range lines {
		lines[i] = bar
	}
	return strings.Join(lines, "\n")
}

func (m expertsTUIModel) renderList(width, height int) string {
	const profileLines = 2 // label row + summary row per agent
	lines := make([]string, 0, len(m.resp.Profiles)*profileLines)
	for i, p := range m.resp.Profiles {
		caret := "  "
		labelStyle := m.styles.agent
		if i == m.cursor {
			caret = m.styles.render(m.styles.selected, "▸ ")
			labelStyle = m.styles.selected
		}
		summary := fmt.Sprintf("%d sess · %d cp · %d steps", p.SessionCount, p.CheckpointCount, p.StepCount)
		lines = append(lines,
			m.fitLine(caret+m.styles.render(labelStyle, p.AgentLabel), width),
			m.fitLine("  "+m.styles.render(m.styles.muted, summary), width),
		)
	}
	start := listScrollStart(m.cursor, profileLines, height, len(lines))
	end := start + height
	if end > len(lines) {
		end = len(lines)
	}
	window := lines[start:end]
	for len(window) < height {
		window = append(window, strings.Repeat(" ", width))
	}
	return strings.Join(window, "\n")
}

// listScrollStart picks the first visible line in the left list pane so the
// selected profile (cursor) stays in view when the full list exceeds height.
func listScrollStart(cursor, profileLines, height, totalLines int) int {
	if height <= 0 || totalLines <= height {
		return 0
	}
	selStart := cursor * profileLines
	selEnd := selStart + profileLines
	start := 0
	if selEnd > start+height {
		start = selEnd - height
	}
	if selStart < start {
		start = selStart
	}
	maxStart := totalLines - height
	if start > maxStart {
		start = maxStart
	}
	if start < 0 {
		return 0
	}
	return start
}

// renderDetail builds the right-pane content for the selected profile and
// returns the line offsets of each section header (for tab navigation).
func (m expertsTUIModel) renderDetail(width int) (string, []int) {
	if m.cursor < 0 || m.cursor >= len(m.resp.Profiles) {
		return "", nil
	}
	p := m.resp.Profiles[m.cursor]

	var lines []string
	var offsets []int
	section := func(title string) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		offsets = append(offsets, len(lines))
		lines = append(lines, m.styles.render(m.styles.section, title))
	}

	offsets = append(offsets, len(lines))
	lines = append(lines, m.styles.render(m.styles.agent, p.AgentLabel))
	if len(p.Models) > 0 {
		lines = append(lines, m.styles.render(m.styles.muted, "models: "+strings.Join(p.Models, ", ")))
	}

	section("EVIDENCE")
	lines = append(lines, m.detailKV("counts",
		fmt.Sprintf("%d sessions · %d checkpoints · %d steps", p.SessionCount, p.CheckpointCount, p.StepCount)))
	if p.AttributionAgentLines != nil {
		attr := fmt.Sprintf("%d agent-attributed lines", *p.AttributionAgentLines)
		if p.AttributionTotalCommitted != nil && *p.AttributionTotalCommitted > 0 {
			attr += fmt.Sprintf(" of %d committed", *p.AttributionTotalCommitted)
		}
		lines = append(lines, m.detailKV("attribution", attr))
	}
	if p.ExactFileMatches > 0 || p.PrefixFileMatches > 0 {
		lines = append(lines, m.detailKV("file matches",
			fmt.Sprintf("%d exact · %d prefix", p.ExactFileMatches, p.PrefixFileMatches)))
	}
	if p.LastActivityAt != "" {
		lines = append(lines, m.detailKV("last active", formatExpertsTime(p.LastActivityAt)))
	}

	if len(p.Skills) > 0 {
		section("SKILLS")
		lines = append(lines, m.facetLines(p.Skills)...)
	}
	if len(p.ToolMix) > 0 {
		section("TOOLS")
		lines = append(lines, m.facetLines(p.ToolMix)...)
	}
	if len(p.MCPServers) > 0 {
		section("MCP")
		lines = append(lines, m.facetLines(p.MCPServers)...)
	}

	if len(p.MatchedFiles) > 0 {
		section("FILES")
		for _, f := range p.MatchedFiles {
			lines = append(lines, "  "+m.styles.render(m.styles.file, f))
		}
	}

	if len(p.Sessions) > 0 {
		section(fmt.Sprintf("SESSIONS (%d)", len(p.Sessions)))
		for _, sess := range p.Sessions {
			sessURL := expertsSessionURL(m.resp.RepoFullName, sess.SessionID)
			// Clamp the title before attaching the hyperlink so the later
			// width pass never truncates inside the OSC 8 sequence.
			title := sess.DisplayName
			if avail := max(width-4, 1); lipgloss.Width(title) > avail {
				title = xansi.Truncate(title, avail, "…")
			}
			lines = append(lines, "  "+m.styles.render(m.styles.bullet, "•")+" "+m.styles.link(m.styles.facet, sessURL, title))
			meta := fmt.Sprintf("%d checkpoints · %d steps", sess.CheckpointCount, sess.StepCount)
			if sess.AttributionAgentLines != nil {
				meta += fmt.Sprintf(" · %d agent lines", *sess.AttributionAgentLines)
			}
			lines = append(lines, "    "+m.styles.render(m.styles.muted, meta))
			if m.expanded {
				if sessURL != "" {
					lines = append(lines, "    "+m.styles.render(m.styles.muted, "link: "+sessURL))
				}
				if len(sess.MatchedFiles) > 0 {
					lines = append(lines, "    "+m.styles.render(m.styles.muted, "files: ")+m.styles.render(m.styles.file, strings.Join(sess.MatchedFiles, ", ")))
				}
				if len(sess.CheckpointIDs) > 0 {
					lines = append(lines, "    "+m.styles.render(m.styles.muted, "checkpoints: "+strings.Join(sess.CheckpointIDs, ", ")))
				}
			}
		}
	}

	for i, ln := range lines {
		if lipgloss.Width(ln) > width {
			lines[i] = xansi.Truncate(ln, width, "…")
		}
	}
	return strings.Join(lines, "\n"), offsets
}

func (m expertsTUIModel) detailKV(label, value string) string {
	return "  " + m.styles.render(m.styles.label, label+":") + " " + value
}

func (m expertsTUIModel) facetLines(facets []expertsFacetCount) []string {
	out := make([]string, 0, len(facets))
	for _, f := range facets {
		out = append(out, "  "+m.styles.render(m.styles.facet, f.Name)+" "+m.styles.render(m.styles.muted, fmt.Sprintf("(%d)", f.Count)))
	}
	return out
}

func (m expertsTUIModel) renderFooter() string {
	expand := "expand"
	if m.expanded {
		expand = "collapse"
	}
	items := []string{
		m.footerItem("↑/↓ j/k", "agent"),
		m.footerItem("tab", "section"),
		m.footerItem("enter", expand),
		m.footerItem("o", "open ↗"),
		m.footerItem("pgup/pgdn", "scroll"),
		m.footerItem("q", "quit"),
	}
	return m.fitLine(strings.Join(items, m.styles.render(m.styles.helpSep, " · ")), m.width)
}

func (m expertsTUIModel) footerItem(k, desc string) string {
	return m.styles.render(m.styles.helpKey, k) + " " + m.styles.render(m.styles.helpDesc, desc)
}

// fitLine truncates s to width (ANSI-aware) and right-pads with spaces so the
// returned line occupies exactly width display cells. This keeps the two panes
// aligned when joined horizontally.
func (m expertsTUIModel) fitLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	out := xansi.Truncate(s, width, "…")
	if pad := width - lipgloss.Width(out); pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	return out
}

func formatExpertsTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if t, err = time.Parse(time.RFC3339Nano, s); err != nil {
			return s
		}
	}
	return t.Local().Format("Jan 02, 2006")
}
