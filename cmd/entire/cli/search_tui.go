package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
)

// searchMode tracks whether the user is browsing results or editing the search bar.
type searchMode int

const (
	modeBrowse searchMode = iota
	modeSearch
	modeDetail
)

// searchResultsMsg is sent when a search API call completes.
type searchResultsMsg struct {
	results []search.Result
	total   int
	err     error
}

// searchMoreResultsMsg is sent when a fetch-more-results call completes.
type searchMoreResultsMsg struct {
	results []search.Result
	err     error
}

// searchStyles holds lipgloss styles specific to the search TUI.
// Styles shared with the status TUI (bold, dim, green, red, cyan, agent/id)
// are accessed via the embedded statusStyles.
type searchStyles struct {
	statusStyles

	sectionTitle lipgloss.Style // bold uppercase section headers
	label        lipgloss.Style // dim key labels in detail panel
	selected     lipgloss.Style // highlighted selected row
	helpKey      lipgloss.Style // colored key hints in footer
	helpSep      lipgloss.Style // dim separator dots in footer
	detailTitle  lipgloss.Style // colored title and section headers (orange, bold)
	detailBorder lipgloss.Style // border style for detail card
}

func newSearchStyles(ss statusStyles) searchStyles {
	s := searchStyles{statusStyles: ss}
	if !ss.colorEnabled {
		return s
	}
	s.sectionTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("244"))
	s.label = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	s.selected = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	s.helpKey = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	s.helpSep = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	s.detailTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	s.detailBorder = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("243")).
		Padding(1, 2)
	return s
}

const resultsPerPage = 25

// searchModel is the bubbletea model for interactive search results.
type searchModel struct {
	results      []search.Result
	cursor       int
	page         int // 0-based display page index
	total        int
	width        int
	height       int
	mode         searchMode
	loading      bool
	fetchingMore bool // true while fetching next API page
	searchErr    string
	input        textinput.Model
	searchCfg    search.Config
	apiPage      int // 1-based last-fetched API page
	styles       searchStyles
	detailVP     viewport.Model // full-screen detail view
	browseVP     viewport.Model // scrollable browse view
}

// pageResults returns the slice of results for the current page.
func (m searchModel) pageResults() []search.Result {
	start := m.page * resultsPerPage
	if start >= len(m.results) {
		return nil
	}
	end := start + resultsPerPage
	if end > len(m.results) {
		end = len(m.results)
	}
	return m.results[start:end]
}

// totalPages returns the number of pages based on the API's total result count.
func (m searchModel) totalPages() int {
	if m.total == 0 {
		return 1
	}
	return (m.total + resultsPerPage - 1) / resultsPerPage
}

// selectedResult returns the currently selected result, accounting for pagination.
func (m searchModel) selectedResult() *search.Result {
	pageResults := m.pageResults()
	if m.cursor >= 0 && m.cursor < len(pageResults) {
		return &pageResults[m.cursor]
	}
	return nil
}

func newSearchModel(results []search.Result, query string, total int, cfg search.Config, ss statusStyles) searchModel {
	styles := newSearchStyles(ss)

	ti := textinput.New()
	ti.SetValue(query)
	ti.Prompt = " › "
	ti.Placeholder = "search checkpoints... (author:name date:week branch:main repo:owner/name or repo:*)"
	ti.CharLimit = 200
	ti.Width = max(ss.width-6, 30)
	if ss.colorEnabled {
		ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
		ti.TextStyle = lipgloss.NewStyle()
		ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
		ti.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	}

	var apiPage int
	if results != nil {
		apiPage = 1
	}

	m := searchModel{
		results:   results,
		total:     total,
		width:     ss.width,
		mode:      modeBrowse,
		input:     ti,
		searchCfg: cfg,
		apiPage:   apiPage,
		styles:    styles,
		browseVP:  viewport.New(ss.width, 1), // height set on first WindowSizeMsg
	}
	m = m.refreshBrowseContent()
	return m
}

func (m searchModel) Init() tea.Cmd {
	if m.mode == modeSearch {
		return textinput.Blink
	}
	return nil
}

func (m searchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn,cyclop // bubbletea interface
	switch msg := msg.(type) {
	case searchResultsMsg:
		m.loading = false
		m.fetchingMore = false
		if msg.err != nil {
			m.searchErr = msg.err.Error()
			m = m.refreshBrowseContent()
			return m, nil
		}
		m.searchErr = ""
		m.results = msg.results
		m.total = msg.total
		m.apiPage = 1
		m.cursor = 0
		m.page = 0
		m.browseVP.GotoTop()
		m = m.refreshBrowseContent()
		return m, nil

	case searchMoreResultsMsg:
		m.fetchingMore = false
		if msg.err != nil {
			m.searchErr = msg.err.Error()
			m = m.refreshBrowseContent()
			return m, nil
		}
		m.apiPage++
		if len(msg.results) > 0 {
			m.results = append(m.results, msg.results...)
		} else {
			// API returned no more results — cap total to what we have
			m.total = len(m.results)
		}
		m = m.refreshBrowseContent()
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = max(msg.Width-6, 30)
		m.browseVP.Width = msg.Width
		m.browseVP.Height = max(msg.Height-1, 1) // reserve 1 line for footer
		if m.mode == modeDetail {
			m.detailVP.Width = msg.Width
			m.detailVP.Height = max(msg.Height-2, 1)
		}
		m = m.refreshBrowseContent()
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeSearch:
			return m.updateSearchMode(msg)
		case modeDetail:
			return m.updateDetailMode(msg)
		case modeBrowse:
			return m.updateBrowseMode(msg)
		}
	}
	return m, nil
}

func (m searchModel) updateSearchMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:ireturn // bubbletea pattern
	switch msg.String() {
	case "esc":
		m.mode = modeBrowse
		m.input.Blur()
		m = m.refreshBrowseContent()
		return m, nil
	case "enter":
		raw := strings.TrimSpace(m.input.Value())
		if raw == "" {
			return m, nil
		}
		parsed := search.ParseSearchInput(raw)
		if err := search.ValidateRepoFilters(parsed.Repos); err != nil {
			m.searchErr = err.Error()
			m = m.refreshBrowseContent()
			return m, nil
		}
		m.mode = modeBrowse
		m.input.Blur()
		m.loading = true
		m.searchErr = ""
		cfg := m.searchCfg
		cfg.Query = parsed.Query
		if cfg.Query == "" {
			cfg.Query = search.WildcardQuery
		}
		cfg.Author = parsed.Author
		cfg.Date = parsed.Date
		cfg.Branch = parsed.Branch
		cfg.Repos = parsed.Repos
		m.searchCfg = cfg
		m = m.refreshBrowseContent()
		return m, performSearch(cfg)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m searchModel) updateBrowseMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:ireturn // bubbletea pattern
	pageLen := len(m.pageResults())
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m = m.refreshBrowseContent()
		}
	case "down", "j":
		if m.cursor < pageLen-1 {
			m.cursor++
			m = m.refreshBrowseContent()
		}
	case "n", "right":
		if m.page < m.totalPages()-1 {
			m.page++
			m.cursor = 0
			m.browseVP.GotoTop()
			// Fetch next API page if we've scrolled past loaded results
			start := m.page * resultsPerPage
			if start >= len(m.results) && !m.fetchingMore {
				m.fetchingMore = true
				m = m.refreshBrowseContent()
				return m, fetchMoreResults(m.searchCfg, m.apiPage+1)
			}
			m = m.refreshBrowseContent()
		}
	case "p", "left":
		if m.page > 0 {
			m.page--
			m.cursor = 0
			m.browseVP.GotoTop()
			m = m.refreshBrowseContent()
		}
	case "enter":
		if r := m.selectedResult(); r != nil {
			m.mode = modeDetail
			content := m.renderDetailContent(*r, m.width, true)
			m.detailVP = viewport.New(m.width, max(m.height-2, 1))
			m.detailVP.SetContent(content)
			return m, nil
		}
	case "/":
		m.mode = modeSearch
		m.input.Focus()
		return m, m.input.Cursor.SetMode(cursor.CursorBlink)
	default:
		// Forward unhandled keys (pgup/pgdn/ctrl+u/ctrl+d/g/G/etc.) to viewport for scrolling
		var cmd tea.Cmd
		m.browseVP, cmd = m.browseVP.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m searchModel) updateDetailMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:ireturn // bubbletea pattern
	switch msg.String() {
	case "esc", "backspace":
		m.mode = modeBrowse
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	case "/":
		m.mode = modeSearch
		m.input.Focus()
		return m, m.input.Cursor.SetMode(cursor.CursorBlink)
	}
	var cmd tea.Cmd
	m.detailVP, cmd = m.detailVP.Update(msg)
	return m, cmd
}

func performSearch(cfg search.Config) tea.Cmd {
	return func() tea.Msg {
		resp, err := search.Search(context.Background(), cfg)
		if err != nil {
			return searchResultsMsg{err: err}
		}
		return searchResultsMsg{results: resp.Results, total: resp.Total}
	}
}

func fetchMoreResults(cfg search.Config, page int) tea.Cmd {
	return func() tea.Msg {
		cfg.Page = page
		resp, err := search.Search(context.Background(), cfg)
		if err != nil {
			return searchMoreResultsMsg{err: err}
		}
		return searchMoreResultsMsg{results: resp.Results}
	}
}

// ─── View ────────────────────────────────────────────────────────────────────

func (m searchModel) View() string {
	if m.width == 0 {
		return ""
	}

	if m.mode == modeDetail {
		return m.viewDetailFull()
	}

	if m.mode == modeSearch {
		return m.viewSearchMode()
	}

	// Browse mode: scrollable viewport + fixed footer.
	return m.browseVP.View() + "\n" + m.viewHelp()
}

func (m searchModel) viewSearchHeader(b *strings.Builder) {
	pad := " "
	b.WriteString("\n")
	b.WriteString(pad + m.styles.render(m.styles.sectionTitle, "SEARCH"))
	b.WriteString("\n\n")
}

func (m searchModel) viewSearchMode() string {
	var b strings.Builder
	m.viewSearchHeader(&b)
	b.WriteString(" " + m.input.View())
	b.WriteString("\n\n")
	if m.searchErr != "" {
		b.WriteString(" " + m.styles.render(m.styles.red, "Error: "+m.searchErr))
		b.WriteString("\n\n")
	}
	b.WriteString(" " + m.styles.render(m.styles.dim, "  Filters: author:<name>  date:<week|month>  branch:<name>  repo:<owner/name|*>"))
	b.WriteString("\n")
	b.WriteString(" " + m.styles.render(m.styles.dim, "  repo:* searches all accessible repos"))
	b.WriteString("\n\n")
	b.WriteString(m.viewHelp())
	return b.String()
}

// renderBrowseContent builds the scrollable content for browse mode (everything except the footer).
func (m searchModel) renderBrowseContent() string {
	var b strings.Builder
	pad := " "

	m.viewSearchHeader(&b)

	query := m.input.Value()
	b.WriteString(pad + m.styles.render(m.styles.agent, "›") + " " + m.styles.render(m.styles.bold, query))
	b.WriteString("\n\n")

	// Loading / error / empty states
	if m.loading {
		b.WriteString(pad + m.styles.render(m.styles.dim, "Searching..."))
		return b.String()
	}
	if m.searchErr != "" {
		b.WriteString(pad + m.styles.render(m.styles.red, "Error: "+m.searchErr))
		return b.String()
	}
	if len(m.results) == 0 {
		b.WriteString(pad + m.styles.render(m.styles.dim, "No results found."))
		return b.String()
	}

	// Section: RESULTS
	b.WriteString(pad + m.styles.render(m.styles.sectionTitle, "RESULTS"))
	b.WriteString("\n\n")

	// Table (current page only)
	if m.fetchingMore && m.pageResults() == nil {
		b.WriteString(pad + m.styles.render(m.styles.dim, "Loading more results...") + "\n")
	} else {
		b.WriteString(m.viewTable())
	}
	b.WriteString("\n")

	// Detail card (no truncation — viewport handles overflow)
	if r := m.selectedResult(); r != nil {
		b.WriteString(m.viewDetailCard(*r))
	}

	return strings.TrimRight(b.String(), "\n")
}

// refreshBrowseContent rebuilds the browse viewport content from current state.
func (m searchModel) refreshBrowseContent() searchModel {
	m.browseVP.SetContent(m.renderBrowseContent())
	return m
}

func (m searchModel) viewTable() string {
	contentWidth := max(m.width-2, 0) // 1 char padding each side
	cols := computeColumns(contentWidth)
	pad := " "

	var b strings.Builder

	// Column headers
	hdr := fmt.Sprintf("%-*s %-*s %-*s %-*s %-*s %-*s",
		cols.age, "Age",
		cols.id, "ID",
		cols.branch, "Branch",
		cols.repo, "Repo",
		cols.prompt, "Prompt",
		cols.author, "Author",
	)
	b.WriteString(pad + m.styles.render(m.styles.dim, hdr) + "\n")

	// Header separator
	b.WriteString(pad + m.styles.render(m.styles.dim, strings.Repeat("─", contentWidth)) + "\n")

	// Rows
	for i, r := range m.pageResults() {
		row := m.viewRow(r, cols)
		if i == m.cursor && m.styles.colorEnabled {
			b.WriteString(pad + m.styles.selected.Render(row))
		} else {
			b.WriteString(pad + row)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func (m searchModel) viewRow(r search.Result, cols columnLayout) string {
	age := fmt.Sprintf("%-*s", cols.age, stringutil.TruncateRunes(formatSearchAge(r.Data.CreatedAt), cols.age, ""))
	id := fmt.Sprintf("%-*s", cols.id, stringutil.TruncateRunes(r.Data.ID, cols.id-1, "…"))
	branch := fmt.Sprintf("%-*s", cols.branch, stringutil.TruncateRunes(r.Data.Branch, cols.branch-1, "…"))
	repo := fmt.Sprintf("%-*s", cols.repo, stringutil.TruncateRunes(
		r.Data.Org+"/"+r.Data.Repo, cols.repo-1, "…",
	))
	prompt := fmt.Sprintf("%-*s", cols.prompt, stringutil.TruncateRunes(
		stringutil.CollapseWhitespace(r.Data.Prompt), cols.prompt-1, "…",
	))
	authorName := derefStr(r.Data.AuthorUsername, r.Data.Author)
	author := fmt.Sprintf("%-*s", cols.author, stringutil.TruncateRunes(authorName, cols.author-1, "…"))

	return fmt.Sprintf("%s %s %s %s %s %s", age, id, branch, repo, prompt, author)
}

// renderDetailContent builds the text content for a checkpoint detail (no border/card chrome).
func (m searchModel) renderDetailContent(r search.Result, contentWidth int, showSections bool) string {
	const labelWidth = 12
	// Available width for field values: content width minus label minus space.
	valueWidth := contentWidth - labelWidth - 1
	if valueWidth < 20 {
		valueWidth = 0 // disable wrapping on very narrow terminals
	}

	var content strings.Builder

	content.WriteString(m.styles.render(m.styles.detailTitle, "Checkpoint Detail"))
	content.WriteString("\n")

	formatLabel := func(label string) string {
		return m.styles.render(m.styles.label, fmt.Sprintf("%-*s", labelWidth, label+":"))
	}

	writeField := func(label, value string) {
		content.WriteString(formatLabel(label) + " " + value + "\n")
	}

	// writeWrappedField word-wraps a long value, indenting continuation lines to align with the value column.
	writeWrappedField := func(label, value string) {
		if valueWidth == 0 || len(value) <= valueWidth {
			writeField(label, value)
			return
		}
		indent := strings.Repeat(" ", labelWidth+1) // align with value column
		wrapped := wrapText(value, valueWidth)
		lines := strings.Split(wrapped, "\n")
		content.WriteString(formatLabel(label) + " " + lines[0] + "\n")
		for _, line := range lines[1:] {
			content.WriteString(indent + line + "\n")
		}
	}

	writeSection := func(title string) {
		if showSections {
			content.WriteString("\n" + m.styles.render(m.styles.detailTitle, title) + "\n")
		} else {
			content.WriteString("\n")
		}
	}

	// ── OVERVIEW ──
	writeSection("OVERVIEW")
	writeField("ID", r.Data.ID)
	writeWrappedField("Prompt", stringutil.CollapseWhitespace(r.Data.Prompt))
	matchType := r.Meta.MatchType
	if r.Meta.Score > 0 {
		matchType += " " + m.styles.render(m.styles.dim, fmt.Sprintf("(score: %.3f)", r.Meta.Score))
	}
	writeField("Match", matchType)

	// ── SOURCE ──
	writeSection("SOURCE")
	writeWrappedField("Commit", formatCommit(r.Data.CommitSHA, r.Data.CommitMessage))
	writeField("Branch", r.Data.Branch)
	writeField("Repo", r.Data.Org+"/"+r.Data.Repo)
	authorStr := r.Data.Author
	if r.Data.AuthorUsername != nil && *r.Data.AuthorUsername != "" {
		authorStr = *r.Data.AuthorUsername + " " + m.styles.render(m.styles.dim, "("+r.Data.Author+")")
	}
	writeField("Author", authorStr)
	createdStr := formatDetailCreatedAt(r.Data.CreatedAt, m.styles)
	writeField("Created", createdStr)

	// ── SNIPPET ──
	if r.Meta.Snippet != "" {
		writeSection("SNIPPET")
		if valueWidth > 0 {
			content.WriteString(wrapText(r.Meta.Snippet, contentWidth) + "\n")
		} else {
			content.WriteString(r.Meta.Snippet + "\n")
		}
	}

	// ── FILES ──
	if len(r.Data.FilesTouched) > 0 {
		content.WriteString("\n")
		if showSections {
			content.WriteString(m.styles.render(m.styles.detailTitle, "FILES") + "\n")
		} else {
			content.WriteString(m.styles.render(m.styles.label, "Files:") + "\n")
		}
		for _, f := range r.Data.FilesTouched {
			content.WriteString("  " + f + "\n")
		}
	}

	return strings.TrimRight(content.String(), "\n")
}

// formatDetailCreatedAt renders date (default) + relative time (dim) for the detail view.
func formatDetailCreatedAt(createdAt string, styles searchStyles) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return createdAt
	}
	return t.Format("Jan 02, 2006") + " " + styles.render(styles.dim, "("+timeAgo(t)+")")
}

// maxCardContentLines is the maximum number of content lines shown in the
// inline detail card. Longer content is truncated with a "enter for more" hint.
// The full content is always available via the detail view (enter key).
const maxCardContentLines = 15

func (m searchModel) viewDetailCard(r search.Result) string {
	var contentWidth int
	var borderWidth int
	if m.styles.colorEnabled {
		// lipgloss .Width(W) includes padding but excludes border:
		//   text wraps at W - padding(4), rendered = W + border(2), + indent(1) = W + 3
		borderWidth = max(m.width-3, 0)
		contentWidth = max(borderWidth-4, 0)
	} else {
		// No border/padding in NO_COLOR mode, only indent(1)
		contentWidth = max(m.width-1, 0)
	}
	cardContent := m.renderDetailContent(r, contentWidth, false)

	lines := strings.Split(cardContent, "\n")
	if len(lines) > maxCardContentLines {
		lines = lines[:maxCardContentLines]
		hint := m.styles.render(m.styles.dim, "▼ enter for more")
		hintWidth := lipgloss.Width(hint)
		lines = append(lines, "", strings.Repeat(" ", max(contentWidth-hintWidth, 0))+hint)
		cardContent = strings.Join(lines, "\n")
	}

	card := cardContent
	if m.styles.colorEnabled {
		card = m.styles.detailBorder.Width(borderWidth).Render(cardContent)
	}

	return indentLines(card, " ")
}

func (m searchModel) viewDetailFull() string {
	var b strings.Builder
	b.WriteString(m.detailVP.View())
	b.WriteString("\n")

	// Scroll indicator + help
	scrollPct := m.styles.render(m.styles.dim, fmt.Sprintf("%3.f%%", m.detailVP.ScrollPercent()*100))
	help := m.styles.render(m.styles.helpKey, "j/k") + " scroll" +
		m.styles.render(m.styles.helpSep, " · ") +
		m.styles.render(m.styles.helpKey, "esc") + " back" +
		m.styles.render(m.styles.helpSep, " · ") +
		m.styles.render(m.styles.helpKey, "q") + " quit"

	gap := m.width - lipgloss.Width(help) - lipgloss.Width(scrollPct) - 2
	if gap < 1 {
		gap = 1
	}
	b.WriteString(help + strings.Repeat(" ", gap) + scrollPct + "\n")

	return b.String()
}

func (m searchModel) viewHelp() string {
	dot := m.styles.render(m.styles.helpSep, " · ")

	if m.mode == modeSearch {
		return m.styles.render(m.styles.helpKey, "enter") + " search" + dot +
			m.styles.render(m.styles.helpKey, "esc") + " cancel" + "\n"
	}

	pages := m.totalPages()

	left := m.styles.render(m.styles.helpKey, "/") + " search" + dot +
		m.styles.render(m.styles.helpKey, "j/k") + " navigate" + dot +
		m.styles.render(m.styles.helpKey, "enter") + " detail"
	if pages > 1 {
		left += dot + m.styles.render(m.styles.helpKey, "n/p") + " page"
	}
	left += dot + m.styles.render(m.styles.helpKey, "q") + " quit"

	right := fmt.Sprintf("%d results", m.total)
	if pages > 1 {
		right = fmt.Sprintf("page %d/%d · %d results", m.page+1, pages, m.total)
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}

	return left + strings.Repeat(" ", gap) + m.styles.render(m.styles.dim, right) + "\n"
}

// indentLines prefixes every line of text with the given prefix.
func indentLines(text, prefix string) string {
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(prefix + line + "\n")
	}
	return b.String()
}

// wrapText wraps text to the given width, breaking at word boundaries.
// Existing newlines in the input are preserved.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	var result strings.Builder
	for i, paragraph := range strings.Split(text, "\n") {
		if i > 0 {
			result.WriteByte('\n')
		}
		wrapParagraph(&result, paragraph, width)
	}
	return result.String()
}

func wrapParagraph(b *strings.Builder, text string, width int) {
	words := strings.Fields(text)
	if len(words) == 0 {
		return
	}
	lineLen := 0
	for i, w := range words {
		wLen := len(w)
		if i == 0 {
			b.WriteString(w)
			lineLen = wLen
			continue
		}
		if lineLen+1+wLen > width {
			b.WriteByte('\n')
			b.WriteString(w)
			lineLen = wLen
		} else {
			b.WriteByte(' ')
			b.WriteString(w)
			lineLen += 1 + wLen
		}
	}
}

// ─── Column Layout ───────────────────────────────────────────────────────────

// columnLayout holds computed column widths for the search results table.
type columnLayout struct {
	age    int
	id     int
	branch int
	repo   int
	prompt int
	author int
}

// computeColumns calculates column widths from terminal width.
func computeColumns(width int) columnLayout {
	const (
		ageWidth    = 10
		idWidth     = 12
		repoMin     = 10
		authorWidth = 14
		gaps        = 5 // spaces between columns
	)

	remaining := width - ageWidth - idWidth - authorWidth - gaps
	if remaining < 20 {
		remaining = 20
	}

	branchWidth := max(remaining*18/100, 8)
	repoWidth := max(remaining*18/100, repoMin)
	promptWidth := remaining - branchWidth - repoWidth
	if promptWidth < 12 {
		reclaim := 12 - promptWidth
		repoWidth = max(repoWidth-reclaim, repoMin)
		promptWidth = remaining - branchWidth - repoWidth
	}

	return columnLayout{
		age:    ageWidth,
		id:     idWidth,
		branch: branchWidth,
		repo:   repoWidth,
		prompt: promptWidth,
		author: authorWidth,
	}
}

// ─── Formatting Helpers ──────────────────────────────────────────────────────

// formatSearchAge parses an RFC3339 timestamp and returns a relative time string.
func formatSearchAge(createdAt string) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return createdAt
	}
	return timeAgo(t)
}

// formatCommit renders commit SHA + message, handling nil pointers.
func formatCommit(sha, message *string) string {
	s := derefStr(sha, "—")
	if sha != nil && len(*sha) > 7 {
		s = (*sha)[:7]
	}
	msg := derefStr(message, "")
	if msg != "" {
		s += "  " + msg
	}
	return s
}

// derefStr returns the dereferenced string pointer, or fallback if nil.
func derefStr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// ─── Static Fallback ─────────────────────────────────────────────────────────

// renderSearchStatic writes a non-interactive table for accessible mode.
func renderSearchStatic(w io.Writer, results []search.Result, query string, total int, styles statusStyles) {
	fmt.Fprintf(w, "Found %d checkpoints matching %q\n\n", total, query)

	cols := computeColumns(styles.width)

	fmt.Fprintf(w, "%-*s %-*s %-*s %-*s %-*s %-*s\n",
		cols.age, "AGE",
		cols.id, "ID",
		cols.branch, "BRANCH",
		cols.repo, "REPO",
		cols.prompt, "PROMPT",
		cols.author, "AUTHOR",
	)

	for _, r := range results {
		age := formatSearchAge(r.Data.CreatedAt)
		id := stringutil.TruncateRunes(r.Data.ID, cols.id, "")
		branch := stringutil.TruncateRunes(r.Data.Branch, cols.branch, "...")
		repo := stringutil.TruncateRunes(r.Data.Org+"/"+r.Data.Repo, cols.repo, "...")
		prompt := stringutil.TruncateRunes(
			stringutil.CollapseWhitespace(r.Data.Prompt), cols.prompt, "...",
		)
		author := stringutil.TruncateRunes(derefStr(r.Data.AuthorUsername, r.Data.Author), cols.author, "...")

		fmt.Fprintf(w, "%-*s %-*s %-*s %-*s %-*s %-*s\n",
			cols.age, age,
			cols.id, id,
			cols.branch, branch,
			cols.repo, repo,
			cols.prompt, prompt,
			cols.author, author,
		)
	}
}
