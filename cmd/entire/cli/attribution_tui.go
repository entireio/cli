package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"

	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// Layout budget for the why viewer. The header is a title line plus a blank
// line; the footer is a marker legend line plus a help line. The remaining rows
// are split between the file-line list (left) and the selected line's
// explanation (right), separated by a thin vertical rule.
const (
	whyHeaderHeight   = 2
	whyFooterHeight   = 2
	whyListMinWidth   = 34
	whyListWidthRatio = 0.55 // list gets slightly more room than the detail
	whyPaneGap        = 3    // " │ "
)

// whyMarkerLegend is the one-line explanation of the attribution tags shown in
// the footer and in the plain-text views. Wording is deliberate — the tags are
// a PER-COMMIT inference, not a per-line truth: [AI] means the commit's
// checkpointed work was fully agent-authored; [MX] means the commit mixed
// agent work with human edits (so any given line may be either); [HU] means no
// agent checkpoint is recorded for the commit. Kept within the blame table's
// 80-column budget (with its 2-space indent); full sentences live in the why
// detail views, and [??]/~/? markers are explained by their own legend line.
const whyMarkerLegend = "per commit: [AI] all agent · [MX] mixed — line may be either · [HU] no agent"

// whyTUIStyles holds the interactive viewer's palette. Empty styles render as
// plain text when color is off, which also keeps tests assertable.
type whyTUIStyles struct {
	colorEnabled bool

	title    lipgloss.Style
	file     lipgloss.Style
	dim      lipgloss.Style
	selected lipgloss.Style
	section  lipgloss.Style
	tagAI    lipgloss.Style
	tagMX    lipgloss.Style
	tagHU    lipgloss.Style
	warn     lipgloss.Style
	helpKey  lipgloss.Style
	helpDesc lipgloss.Style
	sepBar   lipgloss.Style
}

func newWhyTUIStyles(useColor bool) whyTUIStyles {
	s := whyTUIStyles{colorEnabled: useColor}
	if !useColor {
		return s
	}
	s.title = lipgloss.NewStyle().Bold(true)
	s.file = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")).Bold(true)
	s.dim = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	s.selected = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")).Bold(true)
	s.section = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")).Bold(true)
	s.tagAI = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	s.tagMX = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	s.tagHU = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	s.warn = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	s.helpKey = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Bold(true)
	s.helpDesc = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	s.sepBar = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	return s
}

func (s whyTUIStyles) render(style lipgloss.Style, text string) string {
	if !s.colorEnabled {
		return text
	}
	return style.Render(text)
}

// link renders text with an OSC 8 hyperlink when styling is enabled; plain
// styled text otherwise, so dumb terminals and piped output are unaffected.
func (s whyTUIStyles) link(style lipgloss.Style, linkURL, text string) string {
	if !s.colorEnabled || strings.TrimSpace(linkURL) == "" {
		return s.render(style, text)
	}
	return style.Hyperlink(linkURL).Render(text)
}

func (s whyTUIStyles) tagStyle(tag string) lipgloss.Style {
	switch tag {
	case "[AI]":
		return s.tagAI
	case "[MX]":
		return s.tagMX
	default:
		return s.tagHU
	}
}

// whyTUIModel renders a master-detail view over a pre-resolved file
// attribution: the file's lines on the left (with attribution markers) and the
// selected line's full explanation on the right. All data is resolved before
// the program starts — no git or network I/O happens inside the TUI.
type whyTUIModel struct {
	result *fileAttributionResult
	styles whyTUIStyles

	// repoFullName ("owner/repo") enables entire.io session hyperlinks; empty
	// disables them (links are best-effort decoration, never required).
	repoFullName string

	cursor    int
	listTop   int // first visible list row (manual scroll window)
	expanded  bool
	width     int
	height    int
	ready     bool
	vp        viewport.Model
	statusMsg string
}

func newWhyTUIModel(result *fileAttributionResult, repoFullName string, useColor bool, startLine int) whyTUIModel {
	m := whyTUIModel{
		result:       result,
		styles:       newWhyTUIStyles(useColor),
		repoFullName: repoFullName,
	}
	if startLine > 0 {
		for i := range result.Lines {
			if result.Lines[i].LineNumber == startLine {
				m.cursor = i
				break
			}
		}
	}
	return m
}

func runWhyTUI(result *fileAttributionResult, repoFullName string, useColor bool, startLine int) error {
	p := tea.NewProgram(newWhyTUIModel(result, repoFullName, useColor, startLine))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("why TUI: %w", err)
	}
	return nil
}

func (m whyTUIModel) Init() tea.Cmd { return nil }

func (m whyTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m whyTUIModel) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Quit), key.Matches(msg, keys.Back):
		return m, tea.Quit
	case key.Matches(msg, keys.Up):
		return m.moveCursor(m.cursor - 1), nil
	case key.Matches(msg, keys.Down):
		return m.moveCursor(m.cursor + 1), nil
	// Paging is bound to pgup/pgdown explicitly: the shared keymap's
	// NextPage/PrevPage claim n/p, which this viewer uses for the more useful
	// next/previous agent-attributed line jumps.
	case msg.String() == "pgup":
		return m.moveCursor(m.cursor - m.bodyHeight()), nil
	case msg.String() == "pgdown":
		return m.moveCursor(m.cursor + m.bodyHeight()), nil
	case key.Matches(msg, keys.Home):
		return m.moveCursor(0), nil
	case key.Matches(msg, keys.End):
		return m.moveCursor(len(m.result.Lines) - 1), nil
	case key.Matches(msg, keys.Confirm):
		m.expanded = !m.expanded
		return m.refreshDetail(), nil
	case msg.String() == "n":
		return m.jumpAgentLine(1), nil
	case msg.String() == "p", msg.String() == "N":
		return m.jumpAgentLine(-1), nil
	}
	if m.ready {
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m whyTUIModel) moveCursor(to int) whyTUIModel {
	if len(m.result.Lines) == 0 {
		return m
	}
	if to < 0 {
		to = 0
	}
	if to > len(m.result.Lines)-1 {
		to = len(m.result.Lines) - 1
	}
	if to == m.cursor {
		return m
	}
	m.cursor = to
	m.expanded = false
	m = m.scrollListToCursor()
	return m.refreshDetail()
}

// scrollListToCursor adjusts the persisted list window so the cursor stays
// visible. Kept separate from rendering so the scroll state survives value-
// receiver renders.
func (m whyTUIModel) scrollListToCursor() whyTUIModel {
	height := m.bodyHeight()
	if m.cursor < m.listTop {
		m.listTop = m.cursor
	}
	if m.cursor >= m.listTop+height {
		m.listTop = m.cursor - height + 1
	}
	if m.listTop < 0 {
		m.listTop = 0
	}
	return m
}

// jumpAgentLine moves the cursor to the next/previous line with agent
// involvement ([AI] or [MX]) — browsing straight to the interesting lines.
func (m whyTUIModel) jumpAgentLine(dir int) whyTUIModel {
	lines := m.result.Lines
	for i := m.cursor + dir; i >= 0 && i < len(lines); i += dir {
		if lines[i].Authorship == attributionAI || lines[i].Authorship == attributionMixed {
			return m.moveCursor(i)
		}
	}
	m.statusMsg = "no more agent-attributed lines"
	return m
}

func (m whyTUIModel) layout() whyTUIModel {
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

func (m whyTUIModel) bodyHeight() int {
	h := m.height - whyHeaderHeight - whyFooterHeight
	if h < 1 {
		h = 1
	}
	return h
}

func (m whyTUIModel) listWidth() int {
	w := int(float64(m.width) * whyListWidthRatio)
	if w < whyListMinWidth {
		w = whyListMinWidth
	}
	if limit := m.width - whyPaneGap - 20; w > limit {
		w = limit
	}
	if w < 1 {
		w = 1
	}
	return w
}

func (m whyTUIModel) rightPaneWidth() int {
	w := m.width - m.listWidth() - whyPaneGap
	if w < 1 {
		w = 1
	}
	return w
}

func (m whyTUIModel) refreshDetail() whyTUIModel {
	m.statusMsg = ""
	if !m.ready {
		return m
	}
	m.vp.SetContent(m.renderDetail(m.rightPaneWidth()))
	m.vp.GotoTop()
	return m
}

// selectedLine returns the line under the cursor, or nil for an empty file.
func (m whyTUIModel) selectedLine() *attributionLine {
	if m.cursor < 0 || m.cursor >= len(m.result.Lines) {
		return nil
	}
	return &m.result.Lines[m.cursor]
}

// sessionWebURL builds the entire.io session URL for the selected line, or ""
// when there is nothing to link.
func (m whyTUIModel) sessionWebURL(line *attributionLine) string {
	if line == nil || m.repoFullName == "" || line.SessionID == "" {
		return ""
	}
	owner, repo, ok := strings.Cut(m.repoFullName, "/")
	if !ok || owner == "" || repo == "" {
		return ""
	}
	return fmt.Sprintf("https://entire.io/gh/%s/%s/session/%s",
		url.PathEscape(owner), url.PathEscape(repo), url.PathEscape(line.SessionID))
}

func (m whyTUIModel) View() tea.View {
	if !m.ready {
		return tea.NewView("")
	}
	var b strings.Builder
	summary := m.result.Summary
	fmt.Fprintf(&b, "%s %s  %s\n\n",
		m.styles.render(m.styles.title, "why"),
		m.styles.render(m.styles.file, m.result.File),
		m.styles.render(m.styles.dim, fmt.Sprintf("%d lines · %d%% AI · %d%% human · %d%% mixed",
			summary.TotalLines, summary.AIPercentage, summary.HumanPercentage, summary.MixedPercentage)))

	bodyH := m.bodyHeight()
	left := m.renderList(m.listWidth(), bodyH)
	sep := m.verticalSep(bodyH)
	right := m.vp.View()
	// JoinHorizontal is ANSI-aware: it measures each pane's VISIBLE width, so
	// colored list rows stay aligned and the separator column doesn't drift
	// (a raw %-*s pads on byte length and breaks once escape codes are present).
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, sep, right))
	b.WriteString("\n")

	legend := whyMarkerLegend
	if m.statusMsg != "" {
		legend = m.statusMsg
	}
	b.WriteString(m.styles.render(m.styles.dim, legend) + "\n")
	b.WriteString(m.renderHelp())
	return tea.NewView(b.String())
}

// renderList renders the visible window of file lines as a single block (rows
// joined by newlines), each row fit to width and the block padded to height.
// Lines are windowed manually (a file can be thousands of lines). Rows are
// width-fit with fitLine (ANSI-aware) so JoinHorizontal keeps the columns and
// separator aligned once color escape codes are present.
func (m whyTUIModel) renderList(width, height int) string {
	lines := m.result.Lines
	if len(lines) == 0 {
		return m.fitLine(m.styles.render(m.styles.dim, "(empty file)"), width)
	}

	// Read-only clamp: the persisted window is maintained by
	// scrollListToCursor; this only guards against a resize shrinking the
	// window after the last cursor move.
	top := m.listTop
	if m.cursor < top {
		top = m.cursor
	}
	if m.cursor >= top+height {
		top = m.cursor - height + 1
	}
	if top < 0 {
		top = 0
	}

	numW := len(strconv.Itoa(lines[len(lines)-1].LineNumber))
	rows := make([]string, 0, height)
	for i := top; i < len(lines) && len(rows) < height; i++ {
		line := lines[i]
		tag := attributionTag(line.Authorship)
		marker := attributionLineMarker(line)
		if marker == "" {
			marker = " "
		}
		content := strings.ReplaceAll(line.Content, "\t", "    ")
		var row string
		if i == m.cursor {
			// One outer style over the whole row so the selection reads as a
			// single unit (no nested tag styling to unbalance the escapes).
			row = m.styles.render(m.styles.selected,
				fmt.Sprintf("%*d %s%s %s", numW, line.LineNumber, tag, marker, content))
		} else {
			row = fmt.Sprintf("%*d %s%s %s", numW, line.LineNumber,
				m.styles.render(m.styles.tagStyle(tag), tag), marker, content)
		}
		rows = append(rows, m.fitLine(row, width))
	}
	// Pad to a full-height block so JoinHorizontal aligns the panes.
	for len(rows) < height {
		rows = append(rows, strings.Repeat(" ", width))
	}
	return strings.Join(rows, "\n")
}

// fitLine truncates s to width and right-pads it to exactly width, measuring
// VISIBLE width (xansi.Truncate / lipgloss.Width ignore escape codes) so
// colored rows occupy the same column span as plain ones.
func (m whyTUIModel) fitLine(s string, width int) string {
	if width <= 0 {
		return ""
	}
	out := xansi.Truncate(s, width, "…")
	if pad := width - lipgloss.Width(out); pad > 0 {
		out += strings.Repeat(" ", pad)
	}
	return out
}

// verticalSep is the " │ " column between the list and detail panes, h rows tall
// so JoinHorizontal has a full-height middle block to align against.
func (m whyTUIModel) verticalSep(h int) string {
	bar := " " + m.styles.render(m.styles.sepBar, "│") + " "
	lines := make([]string, h)
	for i := range lines {
		lines[i] = bar
	}
	return strings.Join(lines, "\n")
}

// renderDetail builds the explanation pane for the selected line — the same
// facts as the plain-text `why <file>:<line>` output, formatted for a pane.
func (m whyTUIModel) renderDetail(width int) string {
	line := m.selectedLine()
	if line == nil {
		return m.styles.render(m.styles.dim, "No lines.")
	}
	wrap := func(s string) string {
		return lipgloss.NewStyle().Width(width).Render(s)
	}
	var b strings.Builder
	sec := func(title string) { b.WriteString(m.styles.render(m.styles.section, title) + "\n") }

	sec(fmt.Sprintf("LINE %d  %s", line.LineNumber, attributionTag(line.Authorship)))
	b.WriteString(wrap(m.authorshipSentence(line)) + "\n\n")

	if line.ShortCommitSHA != "" {
		commit := "Commit: " + line.ShortCommitSHA
		if line.Author != "" {
			commit += "  by " + line.Author
		}
		if line.AuthorTime != nil {
			commit += "  " + line.AuthorTime.Format("2006-01-02 15:04")
		}
		b.WriteString(wrap(commit) + "\n")
	}
	if line.Agent != "" {
		agentLine := "Agent: " + line.Agent
		if line.Model != "" {
			agentLine += " · " + line.Model
		}
		b.WriteString(wrap(agentLine) + "\n")
	}
	if line.SessionID != "" {
		sessionText := "Session: " + shortSessionID(line.SessionID)
		if u := m.sessionWebURL(line); u != "" {
			sessionText = m.link(m.styles.dim, u, sessionText) // dim styled + clickable
		}
		b.WriteString(wrap(sessionText) + "\n")
	}
	if line.CheckpointID != "" {
		b.WriteString(wrap("Checkpoint: "+line.CheckpointID) + "\n")
	}
	b.WriteString("\n")

	if line.Prompt != "" {
		label := "PROMPT"
		if line.PromptSessionLevel {
			label = "SESSION PROMPT"
		}
		sec(label)
		prompt := line.Prompt
		if !m.expanded {
			prompt = stringutil.TruncateRunes(stringutil.CollapseWhitespace(prompt), 240, "… (enter to expand)")
		}
		b.WriteString(wrap(prompt) + "\n")
		if line.PromptSessionLevel {
			b.WriteString(wrap(m.styles.render(m.styles.dim, "session-level prompt — may not appear in this checkpoint's transcript")) + "\n")
		}
		b.WriteString("\n")
	}
	if line.Intent != "" && line.Intent != line.Prompt {
		sec("INTENT")
		b.WriteString(wrap(line.Intent) + "\n\n")
	}

	if line.MetadataMissing {
		msg := "Checkpoint metadata was not found locally; showing trailer-level attribution only."
		if line.MetadataMissingReason != "" {
			msg = line.MetadataMissingReason
		}
		b.WriteString(wrap(m.styles.render(m.styles.warn, msg)) + "\n\n")
	}
	if line.SessionFallback {
		b.WriteString(wrap(m.styles.render(m.styles.warn,
			"~ best-effort: this file is not in the checkpoint session's recorded paths; the agent and prompt shown are a guess")) + "\n\n")
	}

	if len(line.Candidates) > 1 {
		sec(fmt.Sprintf("CANDIDATE CHECKPOINTS (%d)", len(line.Candidates)))
		for _, c := range line.Candidates {
			row := "- " + c.CheckpointID
			if c.Agent != "" {
				row += " · " + c.Agent
			}
			if m.expanded && c.Prompt != "" {
				row += " · " + stringutil.TruncateRunes(stringutil.CollapseWhitespace(c.Prompt), 120, "…")
			}
			b.WriteString(wrap(row) + "\n")
		}
		b.WriteString("\n")
	}

	if line.CheckpointID != "" && !line.MetadataMissing {
		b.WriteString(wrap(m.styles.render(m.styles.dim, "Full context: entire checkpoint explain "+line.CheckpointID)) + "\n")
	}
	return b.String()
}

// authorshipSentence spells out what the tag means for THIS line, with the
// wording the attribution actually supports: [AI] = the commit's checkpointed
// work was fully agent-authored; [MX] = agent work with human edits mixed in;
// [HU] = no agent checkpoint recorded for the commit.
func (m whyTUIModel) authorshipSentence(line *attributionLine) string {
	switch line.Authorship {
	case attributionAI:
		return "Agent-authored: the checkpoint work behind this commit was fully agent-authored."
	case attributionMixed:
		return "Mixed: this commit combined agent work with human edits, so this line may be either."
	case attributionUncommitted:
		return "Uncommitted: this line has no commit yet."
	case attributionHuman:
		return "Human: no agent checkpoint is recorded for this commit."
	default:
		return string(line.Authorship)
	}
}

// link is a tiny alias so renderDetail reads naturally.
func (m whyTUIModel) link(style lipgloss.Style, url, text string) string {
	return m.styles.link(style, url, text)
}

func (m whyTUIModel) renderHelp() string {
	parts := []struct{ k, d string }{
		{"↑/↓", "line"}, {"n/p", "next/prev agent line"}, {"enter", "expand"},
		{"pgup/pgdn", "page"}, {"g/G", "top/bottom"}, {"q", "quit"},
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(m.styles.render(m.styles.helpDesc, " · "))
		}
		b.WriteString(m.styles.render(m.styles.helpKey, p.k) + " " + m.styles.render(m.styles.helpDesc, p.d))
	}
	return b.String()
}
