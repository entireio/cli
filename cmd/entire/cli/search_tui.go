package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
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
	detailTitle  lipgloss.Style // colored title inside detail card
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
	mode         searchMode
	loading      bool
	fetchingMore bool // true while fetching next API page
	searchErr    string
	input        textinput.Model
	searchCfg    search.Config
	apiPage      int // 1-based last-fetched API page
	styles       searchStyles
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
	ti.Placeholder = "search checkpoints... (author:name date:week)"
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

	return searchModel{
		results:   results,
		total:     total,
		width:     ss.width,
		mode:      modeBrowse,
		input:     ti,
		searchCfg: cfg,
		apiPage:   apiPage,
		styles:    styles,
	}
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
			return m, nil
		}
		m.searchErr = ""
		m.results = msg.results
		m.total = msg.total
		m.apiPage = 1
		m.cursor = 0
		m.page = 0
		return m, nil

	case searchMoreResultsMsg:
		m.fetchingMore = false
		if msg.err != nil {
			m.searchErr = msg.err.Error()
			return m, nil
		}
		m.apiPage++
		if len(msg.results) > 0 {
			m.results = append(m.results, msg.results...)
		} else {
			// API returned no more results — cap total to what we have
			m.total = len(m.results)
		}
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.input.Width = max(msg.Width-6, 30)
		return m, nil

	case tea.KeyMsg:
		if m.mode == modeSearch {
			return m.updateSearchMode(msg)
		}
		return m.updateBrowseMode(msg)
	}
	return m, nil
}

func (m searchModel) updateSearchMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) { //nolint:ireturn // bubbletea pattern
	switch msg.String() {
	case "esc":
		m.mode = modeBrowse
		m.input.Blur()
		return m, nil
	case "enter":
		raw := strings.TrimSpace(m.input.Value())
		if raw == "" {
			return m, nil
		}
		m.mode = modeBrowse
		m.input.Blur()
		m.loading = true
		m.searchErr = ""
		cfg := m.searchCfg
		parsed := search.ParseSearchInput(raw)
		cfg.Query = parsed.Query
		if cfg.Query == "" {
			cfg.Query = search.WildcardQuery
		}
		cfg.Author = parsed.Author
		cfg.Date = parsed.Date
		m.searchCfg = cfg
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
		}
	case "down", "j":
		if m.cursor < pageLen-1 {
			m.cursor++
		}
	case "n", "right":
		if m.page < m.totalPages()-1 {
			m.page++
			m.cursor = 0
			// Fetch next API page if we've scrolled past loaded results
			start := m.page * resultsPerPage
			if start >= len(m.results) && !m.fetchingMore {
				m.fetchingMore = true
				return m, fetchMoreResults(m.searchCfg, m.apiPage+1)
			}
		}
	case "p", "left":
		if m.page > 0 {
			m.page--
			m.cursor = 0
		}
	case "/":
		m.mode = modeSearch
		m.input.Focus()
		return m, m.input.Cursor.SetMode(cursor.CursorBlink)
	}
	return m, nil
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

	var b strings.Builder
	pad := " "

	// Section: SEARCH
	b.WriteString("\n")
	b.WriteString(pad + m.styles.render(m.styles.sectionTitle, "SEARCH"))
	b.WriteString("\n\n")

	// Search input
	if m.mode == modeSearch {
		b.WriteString(pad + m.input.View())
		b.WriteString("\n\n")
		b.WriteString(pad + m.styles.render(m.styles.dim, "  Filters: author:<name>  date:<week|month>"))
		b.WriteString("\n")
	} else {
		query := m.input.Value()
		b.WriteString(pad + m.styles.render(m.styles.agent, "›") + " " + m.styles.render(m.styles.bold, query))
	}
	b.WriteString("\n\n")

	// Loading / error / empty states
	if m.loading {
		b.WriteString(pad + m.styles.render(m.styles.dim, "Searching...") + "\n")
		b.WriteString(m.viewHelp())
		return b.String()
	}
	if m.searchErr != "" {
		b.WriteString(pad + m.styles.render(m.styles.red, "Error: "+m.searchErr) + "\n")
		b.WriteString(m.viewHelp())
		return b.String()
	}
	if len(m.results) == 0 {
		b.WriteString(pad + m.styles.render(m.styles.dim, "No results found.") + "\n")
		b.WriteString(m.viewHelp())
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

	// Detail card
	if r := m.selectedResult(); r != nil {
		b.WriteString(m.viewDetailCard(*r))
		b.WriteString("\n")
	}

	// Footer
	b.WriteString(m.viewHelp())

	return b.String()
}

func (m searchModel) viewTable() string {
	contentWidth := m.width - 2 // 1 char padding each side
	cols := computeColumns(contentWidth)
	pad := " "

	var b strings.Builder

	// Column headers
	hdr := fmt.Sprintf("%-*s %-*s %-*s %-*s %-*s",
		cols.age, "Age",
		cols.id, "ID",
		cols.branch, "Branch",
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
	prompt := fmt.Sprintf("%-*s", cols.prompt, stringutil.TruncateRunes(
		stringutil.CollapseWhitespace(r.Data.Prompt), cols.prompt-1, "…",
	))
	authorName := derefStr(r.Data.AuthorUsername, r.Data.Author)
	author := fmt.Sprintf("%-*s", cols.author, stringutil.TruncateRunes(authorName, cols.author-1, "…"))

	return fmt.Sprintf("%s %s %s %s %s", age, id, branch, prompt, author)
}

func (m searchModel) viewDetailCard(r search.Result) string {
	const labelWidth = 12
	innerWidth := m.width - 8 // border + padding eats ~6-8 chars

	var content strings.Builder

	// Title
	content.WriteString(m.styles.render(m.styles.detailTitle, "Checkpoint Detail"))
	content.WriteString("\n\n")

	writeField := func(label, value string) {
		lbl := fmt.Sprintf("%-*s", labelWidth, label+":")
		content.WriteString(m.styles.render(m.styles.label, lbl) + " " + value + "\n")
	}

	writeField("ID", r.Data.ID)
	writeField("Prompt", r.Data.Prompt)

	writeField("Commit", formatCommit(r.Data.CommitSHA, r.Data.CommitMessage))

	writeField("Branch", r.Data.Branch)
	writeField("Repo", r.Data.Org+"/"+r.Data.Repo)
	writeField("Author", formatAuthor(r.Data.Author, r.Data.AuthorUsername))
	writeField("Created", formatCreatedAt(r.Data.CreatedAt))
	writeField("Match", formatMatch(r.Meta))

	if r.Meta.Snippet != "" {
		content.WriteString("\n")
		content.WriteString(m.styles.render(m.styles.label, "Snippet:") + "\n")
		content.WriteString(r.Meta.Snippet + "\n")
	}

	if len(r.Data.FilesTouched) > 0 {
		content.WriteString("\n")
		content.WriteString(m.styles.render(m.styles.label, "Files:") + "\n")
		for _, f := range r.Data.FilesTouched {
			content.WriteString(f + "\n")
		}
	}

	cardContent := strings.TrimRight(content.String(), "\n")

	card := cardContent
	if m.styles.colorEnabled {
		card = m.styles.detailBorder.Width(max(innerWidth, 40)).Render(cardContent)
	}

	return indentLines(card, " ")
}

func (m searchModel) viewHelp() string {
	dot := m.styles.render(m.styles.helpSep, " · ")

	if m.mode == modeSearch {
		return m.styles.render(m.styles.helpKey, "enter") + " search" + dot +
			m.styles.render(m.styles.helpKey, "esc") + " cancel" + "\n"
	}

	pages := m.totalPages()

	left := m.styles.render(m.styles.helpKey, "/") + " search" + dot +
		m.styles.render(m.styles.helpKey, "j/k") + " navigate"
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

// ─── Column Layout ───────────────────────────────────────────────────────────

// columnLayout holds computed column widths for the search results table.
type columnLayout struct {
	age    int
	id     int
	branch int
	prompt int
	author int
}

// computeColumns calculates column widths from terminal width.
func computeColumns(width int) columnLayout {
	const (
		ageWidth    = 10
		idWidth     = 12
		authorWidth = 14
		gaps        = 4 // spaces between columns
	)

	remaining := width - ageWidth - idWidth - authorWidth - gaps
	if remaining < 20 {
		remaining = 20
	}

	branchWidth := max(remaining*20/100, 8)
	promptWidth := remaining - branchWidth

	return columnLayout{
		age:    ageWidth,
		id:     idWidth,
		branch: branchWidth,
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

// formatAuthor renders username with display name, e.g. "dipree (Daniel Adams)".
func formatAuthor(author string, username *string) string {
	if username != nil && *username != "" {
		return *username + " (" + author + ")"
	}
	return author
}

// formatCreatedAt renders a timestamp with relative time.
func formatCreatedAt(createdAt string) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return createdAt
	}
	return t.Format("Jan 02, 2006") + " (" + timeAgo(t) + ")"
}

// formatMatch renders match type and score.
func formatMatch(meta search.Meta) string {
	s := meta.MatchType
	if meta.Score > 0 {
		s += fmt.Sprintf(" (score: %.3f)", meta.Score)
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

	fmt.Fprintf(w, "%-*s  %-*s  %-*s  %-*s  %-*s\n",
		cols.age, "AGE",
		cols.id, "ID",
		cols.branch, "BRANCH",
		cols.prompt, "PROMPT",
		cols.author, "AUTHOR",
	)

	for _, r := range results {
		age := formatSearchAge(r.Data.CreatedAt)
		id := stringutil.TruncateRunes(r.Data.ID, cols.id, "")
		branch := stringutil.TruncateRunes(r.Data.Branch, cols.branch, "...")
		prompt := stringutil.TruncateRunes(
			stringutil.CollapseWhitespace(r.Data.Prompt), cols.prompt, "...",
		)
		author := stringutil.TruncateRunes(derefStr(r.Data.AuthorUsername, r.Data.Author), cols.author, "...")

		fmt.Fprintf(w, "%-*s  %-*s  %-*s  %-*s  %-*s\n",
			cols.age, age,
			cols.id, id,
			cols.branch, branch,
			cols.prompt, prompt,
			cols.author, author,
		)
	}
}
