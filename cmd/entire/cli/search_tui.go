package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	glamour "charm.land/glamour/v2"
	"charm.land/glamour/v2/ansi"
	glamourstyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"github.com/entireio/cli/cmd/entire/cli/codesearch"
	"github.com/entireio/cli/cmd/entire/cli/palette"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/muesli/termenv"
)

// searchTUICommandPath is the command label reported on telemetry emitted from
// inside the TUI. The TUI outlives the cobra command layer that launched it, so
// the path is fixed here rather than read from a *cobra.Command at emit time.
const searchTUICommandPath = "entire search"

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
	counts  *search.TypeCounts
	// countsLowerBound marks facets whose lists were capped upstream, so
	// their counts are "at least N" (rendered as "N+", matching the web).
	countsLowerBound map[string]bool
	warnings         []string
	err              error
}

// searchMoreResultsMsg is sent when a fetch-more-results call completes.
type searchMoreResultsMsg struct {
	results  []search.Result
	warnings []string
	err      error
}

// codeSearchResultsMsg is sent when an async code search call completes.
type codeSearchResultsMsg struct {
	resp *codesearch.SearchResponse
	err  error
	gen  uint64 // generation counter; stale results are discarded
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
	detailTitle  lipgloss.Style // colored title and section headers (accent, bold)
	detailBorder lipgloss.Style // border style for detail card
	tabActive    lipgloss.Style // active type tab
	tabInactive  lipgloss.Style // inactive type tab
}

// Search palette draws from the shared base16 palette: the primary accent
// styles titles/tabs/selection, the detail accent frames the detail card, and
// the commit accent colors commit nodes/type tags in the result list. Snippet
// links use the same Cyan/Blue pair as mdrender so markdown reads identically
// across the CLI.
const (
	searchAccent       = palette.Accent  // primary accent (titles, tabs, selection)
	searchDetailAccent = palette.Accent2 // detail card framing
	searchCommitAccent = palette.Blue    // commit nodes and type tags
)

func newSearchStyles(ss statusStyles) searchStyles {
	s := searchStyles{statusStyles: ss}
	if !ss.colorEnabled {
		return s
	}
	s.sectionTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(searchAccent))
	s.label = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Bold(true)
	s.selected = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(searchAccent))
	s.helpKey = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Bold(true)
	s.helpSep = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Faint(true)
	s.detailTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(searchDetailAccent))
	s.detailBorder = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(searchDetailAccent)).
		Padding(0, 2)
	s.tabActive = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(searchAccent))
	s.tabInactive = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted))
	return s
}

// helpItem renders a "<key> <desc>" pair for a TUI help footer using the
// shared helpKey style. keyLabel may come from a key.Binding's Help().Key or
// be a composite literal like "j/k".
func (s searchStyles) helpItem(keyLabel, desc string) string {
	return s.render(s.helpKey, keyLabel) + " " + desc
}

// resultsPerPage is the page size of the non-interactive (JSON) output modes.
// The TUI itself scrolls continuously and does not page.
const resultsPerPage = 10

// fetchAheadRows is how close to the end of the loaded list the cursor may get
// before the next API page is fetched, so continuous scroll stays smooth.
const fetchAheadRows = 3

// typeFilter represents the active type tab in the TUI.
type typeFilter string

const (
	// typeFilterAll is no longer user-selectable in the TUI (no "All" tab), but
	// remains the internal sentinel for "show every loaded result" used by the
	// fetch-more math that reasons about the grand result total.
	typeFilterAll         typeFilter = ""
	typeFilterCheckpoints typeFilter = typeFilter(search.TypeCheckpoint)
	typeFilterCommits     typeFilter = typeFilter(search.TypeCommit)
	typeFilterSessions    typeFilter = typeFilter(search.TypeSession)
	typeFilterCode        typeFilter = "code"
)

// tabOrder is the left-to-right tab sequence cycled by the ←/→ keys.
var tabOrder = []typeFilter{typeFilterCommits, typeFilterSessions, typeFilterCode}

// searchModel is the bubbletea model for interactive search results.
type searchModel struct {
	results          []search.Result
	cursor           int  // index into the active tab's full loaded list
	pinToEnd         bool // End/G pressed: follow the triggered fetch's rows to the new bottom, then release
	total            int
	width            int
	height           int
	mode             searchMode
	loading          bool
	fetchingMore     bool // true while fetching next API page
	searchErr        string
	input            textinput.Model
	searchCfg        search.Config
	apiPage          int // 1-based last-fetched API page
	styles           searchStyles
	detailVP         viewport.Model     // full-screen detail view
	browseVP         viewport.Model     // scrollable browse view
	filterType       typeFilter         // active type tab filter
	counts           *search.TypeCounts // per-type counts from API
	countsLowerBound map[string]bool    // facets whose counts are lower bounds

	// semanticSearch performs checkpoint searches (initial, re-search, and
	// pagination). The command layer injects its session searcher so every
	// TUI search shares the invocation's discovery cache.
	semanticSearch semanticSearcher

	// warning is the current search's completeness note (partial cell
	// failure, truncated repo index), shown in the status row — the TUI
	// counterpart of the one-shot path's stderr warnings.
	warning string

	// darkBg is captured once before bubbletea takes over the terminal so the
	// snippet renderer never re-queries the terminal via OSC during the Update
	// loop (which would race against bubbletea's stdin reader and stall).
	darkBg bool

	// Code search state.
	codeResults    []codesearch.Result // results from peregrine
	codeStats      codesearch.Stats    // aggregate stats
	codeLoading    bool                // true while async code search runs
	codeSearchErr  string              // error from code search
	codeWarning    string              // code tab's completeness note (failed regions, skipped repos)
	codeSearchOpts codeSearchOpts      // opts for code search (set by caller)
	codeSearchGen  uint64              // generation counter; incremented on each new code search

	// selectionsReported dedupes cli_search_result_selected within one result
	// set, keyed by "<mode>:<rank>". Opening a result, backing out, and opening
	// it again is one act of selection, not two, so an idle enter/esc/enter must
	// not inflate the click-through rate. Distinct ranks still each report —
	// having to open four results before finding the right one is itself the
	// signal. Reset whenever a fresh result set replaces the old one (see
	// resetSelectionReporting); a map field on this value-type model is shared
	// by reference across copies, which is what lets a write in Update persist.
	selectionsReported map[string]bool
}

// codeSearchWarning summarizes a code response's scope loss for the status
// row — the code-tab counterpart of the semantic path's Warnings, so a
// narrowed scope is not invisible in the TUI.
func codeSearchWarning(resp *codesearch.SearchResponse) string {
	var parts []string
	if len(resp.FailedJurisdictions) > 0 {
		parts = append(parts, "some regions failed: "+strings.Join(resp.FailedJurisdictions, ", "))
	}
	if len(resp.SkippedRepos) > 0 {
		parts = append(parts, "skipped repos with no searchable placement: "+strings.Join(resp.SkippedRepos, ", "))
	}
	return strings.Join(parts, "; ")
}

// filteredResults returns results matching the active type filter.
// Returns nil when the Code tab is selected (code results are a different type).
func (m searchModel) filteredResults() []search.Result {
	if m.filterType == typeFilterCode {
		return nil // code results are in codeResults, not here
	}
	if m.filterType == typeFilterAll {
		return m.results
	}
	var out []search.Result
	for _, r := range m.results {
		if typeFilter(r.Type) == m.filterType {
			out = append(out, r)
		}
	}
	return out
}

// visibleCount returns the number of rows in the active tab's loaded list.
func (m searchModel) visibleCount() int {
	if m.filterType == typeFilterCode {
		return len(m.codeResults)
	}
	return len(m.filteredResults())
}

// selectedResult returns the currently selected result.
func (m searchModel) selectedResult() *search.Result {
	filtered := m.filteredResults()
	if m.cursor >= 0 && m.cursor < len(filtered) {
		return &filtered[m.cursor]
	}
	return nil
}

// selectedCodeResult returns the currently selected code result, or nil.
func (m searchModel) selectedCodeResult() *codesearch.Result {
	if m.cursor >= 0 && m.cursor < len(m.codeResults) {
		return &m.codeResults[m.cursor]
	}
	return nil
}

// computeTypeCounts calculates per-tab counts from the loaded results,
// falling back to API-provided counts when available. Checkpoints have no tab
// (they fold into sessions server-side), so they are not counted here.
func (m searchModel) computeTypeCounts() (commits, sessions int) {
	if m.counts != nil {
		return m.counts.Commits, m.counts.Sessions
	}
	for _, r := range m.results {
		switch typeFilter(r.Type) {
		case typeFilterCommits:
			commits++
		case typeFilterSessions:
			sessions++
		case typeFilterAll, typeFilterCode, typeFilterCheckpoints:
			// not shown as a tab; skip
		}
	}
	return
}

// tabTotal returns the active tab's server-side result count and whether it is
// a lower bound. Falls back to the loaded count where no per-type total exists.
func (m searchModel) tabTotal() (total int, lowerBound bool) {
	cm, ss := m.computeTypeCounts()
	switch m.filterType {
	case typeFilterCommits:
		return cm, m.countsLowerBound["commits"]
	case typeFilterSessions:
		return ss, m.countsLowerBound["sessions"] || m.countsLowerBound["checkpoints"]
	case typeFilterAll:
		return max(m.total, len(m.results)), false
	case typeFilterCode, typeFilterCheckpoints:
		return m.visibleCount(), false
	default:
		return m.visibleCount(), false
	}
}

func newSearchModel(results []search.Result, query string, total int, cfg search.Config, ss statusStyles, codeOpts *codeSearchOpts) searchModel {
	styles := newSearchStyles(ss)

	ti := textinput.New()
	ti.SetValue(query)
	ti.Prompt = " › "
	ti.Placeholder = "search sessions, commits, code... (author:name date:week branch:main repo:owner/name or repo:*)"
	ti.CharLimit = 200
	ti.SetWidth(max(ss.width-6, 30))
	ti.SetVirtualCursor(true)
	if ss.colorEnabled {
		s := ti.Styles()
		focused := s.Focused
		focused.Prompt = lipgloss.NewStyle().Foreground(lipgloss.Color(searchAccent)).Bold(true)
		focused.Text = lipgloss.NewStyle()
		focused.Placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Faint(true)
		s.Focused = focused
		s.Cursor.Color = lipgloss.Color(searchAccent)
		ti.SetStyles(s)
	}

	var apiPage int
	if results != nil {
		apiPage = 1
	}

	m := searchModel{
		results:    results,
		total:      total,
		width:      ss.width,
		mode:       modeBrowse,
		input:      ti,
		searchCfg:  cfg,
		apiPage:    apiPage,
		styles:     styles,
		browseVP:   viewport.New(viewport.WithWidth(ss.width), viewport.WithHeight(1)), // height set on first WindowSizeMsg
		darkBg:     termenv.HasDarkBackground(),
		filterType: typeFilterCommits, // default to the leftmost tab (Commits), matching the web UI order
		// Command layer overrides with its session searcher; instrumented
		// anyway so a forgotten override never silently drops telemetry.
		semanticSearch: instrumentSemanticSearcher(searchTUICommandPath, newSemanticSearcher(false)),
	}
	if codeOpts != nil {
		m.codeSearchOpts = *codeOpts
		if codeOpts.query != "" {
			m.codeLoading = true
			m.codeSearchGen = 1
		}
	}
	m = m.refreshBrowseContent()
	return m
}

// resetSelectionReporting clears the per-result-set selection dedupe so the
// next result set reports its own selections. Called wherever a fresh set
// replaces the previous one — never on pagination, which appends to the set
// the user is already looking at and whose earlier ranks are unchanged.
func (m searchModel) resetSelectionReporting() searchModel {
	m.selectionsReported = nil
	return m
}

// reportSelection emits one cli_search_result_selected for the row the user
// just opened, at most once per (mode, rank) per result set. Called from the
// Confirm key handler — the single seam both tabs' detail views open through,
// so this is the CLI's equivalent of a search-result click.
//
// Content-free: the result's type enum, its rank, and the size of the list it
// came from. Nothing derived from the query or from what the result says.
func (m searchModel) reportSelection(mode, resultType string, rank, resultCount int) searchModel {
	key := mode + ":" + strconv.Itoa(rank)
	if m.selectionsReported[key] {
		return m
	}
	if m.selectionsReported == nil {
		m.selectionsReported = make(map[string]bool)
	}
	m.selectionsReported[key] = true

	// context.Background matches the other telemetry/search call sites in this
	// file: bubbletea's Update carries no context, and the emit is detached and
	// best-effort, so there is nothing for a cancellable context to cancel.
	emitSearchSelection(context.Background(), telemetry.SearchSelection{
		Command:     searchTUICommandPath,
		Mode:        mode,
		ResultType:  resultType,
		Rank:        rank,
		ResultCount: resultCount,
	})
	return m
}

func (m searchModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.mode == modeSearch {
		cmds = append(cmds, textinput.Blink)
	}
	if m.codeLoading {
		cmds = append(cmds, performCodeSearch(m.codeSearchOpts, m.codeSearchGen))
	}
	return tea.Batch(cmds...)
}

func (m searchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:cyclop // bubbletea interface
	switch msg := msg.(type) {
	case searchResultsMsg:
		m.loading = false
		m.fetchingMore = false
		if msg.err != nil {
			m.searchErr = RenderUserFacingError(msg.err).Error()
			m = m.refreshBrowseContent()
			return m, nil
		}
		m.searchErr = ""
		// A fresh set: previously reported ranks now index different rows.
		m = m.resetSelectionReporting()
		m.results = msg.results
		m.total = msg.total
		m.counts = msg.counts
		m.countsLowerBound = msg.countsLowerBound
		m.warning = strings.Join(msg.warnings, "; ")
		m.apiPage = 1
		m.cursor = 0
		m.pinToEnd = false
		m.browseVP.GotoTop()
		m = m.refreshBrowseContent()
		return m, nil

	case searchMoreResultsMsg:
		m.fetchingMore = false
		if msg.err != nil {
			m.searchErr = RenderUserFacingError(msg.err).Error()
			m.pinToEnd = false
			m = m.refreshBrowseContent()
			return m, nil
		}
		if len(msg.warnings) > 0 {
			m.warning = strings.Join(msg.warnings, "; ")
		}
		m.apiPage++
		if len(msg.results) > 0 {
			m.results = append(m.results, msg.results...)
		} else {
			// API returned no more results — cap total so fetch-ahead stops asking.
			m.total = len(m.results)
		}
		// End/G follows its triggered fetch to the new bottom, then releases:
		// the pin repositions once and must not chain further fetches, or one
		// keypress would download every remaining page. While the tab is still
		// empty (n == 0) the pin stays armed so the sparse-tab chain below can
		// keep hunting for the tab's first rows.
		if m.pinToEnd {
			if n := m.visibleCount(); n > 0 {
				m.cursor = n - 1
				m.pinToEnd = false
				m = m.refreshBrowseContent()
				m.browseVP.GotoBottom()
				return m, nil
			}
		}
		m = m.refreshBrowseContent()
		// A fetched page may add nothing to the active tab (pages interleave
		// types); keep fetching while the cursor still sits at the end and the
		// server has more, so one keypress scrolls through sparse tabs too.
		return m.withFetchAhead()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(max(msg.Width-6, 30))
		m.browseVP.SetWidth(msg.Width)
		m.browseVP.SetHeight(max(msg.Height-1, 1)) // reserve 1 line for footer
		if m.mode == modeDetail {
			m.detailVP.SetWidth(msg.Width)
			m.detailVP.SetHeight(max(msg.Height-2, 1))
		}
		m = m.refreshBrowseContent()
		return m, nil

	case codeSearchResultsMsg:
		if msg.gen != m.codeSearchGen {
			return m, nil // stale result from a superseded search
		}
		m.codeLoading = false
		if msg.err != nil {
			m.codeSearchErr = RenderUserFacingError(msg.err).Error()
		} else if msg.resp != nil {
			m.codeResults = msg.resp.Results
			m.codeStats = msg.resp.Stats
			m.codeSearchErr = ""
			m.codeWarning = codeSearchWarning(msg.resp)
		}
		if m.filterType == typeFilterCode {
			m.cursor = 0
			m.browseVP.GotoTop()
		}
		m = m.refreshBrowseContent()
		return m, nil

	case tea.KeyPressMsg:
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

func (m searchModel) updateSearchMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Back):
		m.mode = modeBrowse
		m.input.Blur()
		m = m.refreshBrowseContent()
		return m, nil
	case key.Matches(msg, keys.Confirm):
		raw := strings.TrimSpace(m.input.Value())
		if raw == "" {
			return m, nil
		}
		// Checkpoint search: ParseSearchInput extracts author:/date:/branch:/repo:.
		// ValidateRepoFilters only checks each repo value's shape; both semantic
		// and code search accept multiple repos and fan out across cells.
		parsed := search.ParseSearchInput(raw)
		checkpointRepoErr := search.ValidateRepoFilters(parsed.Repos)

		m.searchErr = ""

		var cmds []tea.Cmd
		willFireCodeSearch := false

		// Code search uses extractInlineRepoFilters (not ParseSearchInput)
		// so author:/date:/branch: tokens are preserved as literal search
		// text, matching the --code CLI path.
		codeQuery, inlineRepos := extractInlineRepoFilters(raw)
		if codeQuery != "" {
			willFireCodeSearch = true
			opts := m.codeSearchOpts
			opts.query = codeQuery
			// Always reset to the model's default repo scope, then apply
			// inline overrides. This prevents a stale repo list from a
			// previous query leaking into the next one.
			opts.repoFilters = m.codeSearchOpts.repoFilters
			if len(inlineRepos) > 0 {
				if hasAllReposFilter(inlineRepos) {
					opts.repoFilters = nil
				} else {
					opts.repoFilters = filterRepoWildcards(inlineRepos)
				}
			}
			m.codeSearchGen++
			m = m.resetSelectionReporting()
			m.codeLoading = true
			m.codeResults = nil
			m.codeSearchErr = ""
			m.codeWarning = ""
			cmds = append(cmds, performCodeSearch(opts, m.codeSearchGen))
		} else {
			// No code query (e.g. repo-only input) — clear stale code
			// results and bump the generation so any in-flight search
			// from a prior query is discarded when it completes. Nothing
			// will arrive to overwrite the warning, so clear it here too.
			m.codeSearchGen++
			m = m.resetSelectionReporting()
			m.codeLoading = false
			m.codeResults = nil
			m.codeSearchErr = ""
			m.codeWarning = ""
		}

		// Checkpoint search (only if repo filters are valid for the checkpoint API).
		if checkpointRepoErr != nil {
			m.searchErr = checkpointRepoErr.Error()
			if !willFireCodeSearch {
				// Neither search will fire — stay in search mode so the
				// user can correct the input without pressing / again.
				m = m.refreshBrowseContent()
				return m, nil
			}
		} else {
			m.loading = true
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
			cmds = append(cmds, m.performSearch(cfg))
		}

		m.mode = modeBrowse
		m.input.Blur()
		m = m.refreshBrowseContent()
		return m, tea.Batch(cmds...)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// switchTab activates a type tab and resets the list position.
func (m searchModel) switchTab(f typeFilter) searchModel {
	m.filterType = f
	m.cursor = 0
	m.pinToEnd = false
	m.browseVP.GotoTop()
	return m.refreshBrowseContent()
}

// cycleTab moves the active tab left (delta -1) or right (+1) through
// tabOrder, wrapping at the ends.
func (m searchModel) cycleTab(delta int) searchModel {
	for i, f := range tabOrder {
		if f == m.filterType {
			return m.switchTab(tabOrder[(i+delta+len(tabOrder))%len(tabOrder)])
		}
	}
	// Internal filters (all/checkpoints) have no tab; snap to the first one.
	return m.switchTab(tabOrder[0])
}

// withFetchAhead fires the next API page fetch when the cursor is near the end
// of the loaded list and the server may have more results — the continuous
// scroll counterpart of explicit paging.
//
// ponytail: the semantic total spans all types while tabs filter client-side,
// so on a sparse tab this can fetch pages that add no visible rows (bounded by
// the server total). The upgrade path is per-type server pagination.
func (m searchModel) withFetchAhead() (tea.Model, tea.Cmd) {
	if m.filterType == typeFilterCode || m.fetchingMore || m.loading {
		return m, nil
	}
	if len(m.results) >= m.total {
		return m, nil
	}
	if m.cursor < m.visibleCount()-fetchAheadRows {
		return m, nil
	}
	m.fetchingMore = true
	m = m.refreshBrowseContent()
	return m, m.fetchMoreResults(m.searchCfg, m.apiPage+1)
}

func (m searchModel) updateBrowseMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Quit), key.Matches(msg, keys.Back), msg.String() == "h":
		return m, tea.Quit
	case key.Matches(msg, keys.NextTab):
		// Fetch-ahead so a sparse tab loads rows instead of sitting on
		// "No results for this type." while later pages may hold some.
		return m.cycleTab(1).withFetchAhead()
	case key.Matches(msg, keys.PrevTab):
		return m.cycleTab(-1).withFetchAhead()
	case key.Matches(msg, keys.Up):
		m.pinToEnd = false
		if m.cursor > 0 {
			m.cursor--
			m = m.refreshBrowseContent()
		}
	case key.Matches(msg, keys.Down):
		m.pinToEnd = false
		if m.cursor < m.visibleCount()-1 {
			m.cursor++
			m = m.refreshBrowseContent()
		}
		return m.withFetchAhead()
	case key.Matches(msg, keys.Home):
		m.cursor = 0
		m.pinToEnd = false
		m = m.refreshBrowseContent()
		m.browseVP.GotoTop()
	case key.Matches(msg, keys.End):
		m.pinToEnd = true
		if n := m.visibleCount(); n > 0 {
			m.cursor = n - 1
			m = m.refreshBrowseContent()
			m.browseVP.GotoBottom()
		}
		return m.withFetchAhead()
	case key.Matches(msg, keys.Confirm):
		// Leaving browse mode releases the End pin so a fetch landing while
		// the detail view is open can't move the highlight underneath it.
		if m.filterType == typeFilterCode {
			if r := m.selectedCodeResult(); r != nil {
				m = m.reportSelection(telemetry.SearchModeCode, telemetry.SearchSelectionTypeCode, m.cursor, len(m.codeResults))
				m.mode = modeDetail
				m.pinToEnd = false
				content := m.renderCodeDetail(*r, m.width, true)
				m.detailVP = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(max(m.height-2, 1)))
				m.detailVP.SetContent(content)
				return m, nil
			}
		} else if r := m.selectedResult(); r != nil {
			m = m.reportSelection(telemetry.SearchModeCheckpoint, searchSelectionResultType(r.Type), m.cursor, m.visibleCount())
			m.mode = modeDetail
			m.pinToEnd = false
			content := m.renderDetailContent(*r, m.width, true)
			m.detailVP = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(max(m.height-2, 1)))
			m.detailVP.SetContent(content)
			return m, nil
		}
	case key.Matches(msg, keys.Search):
		m.mode = modeSearch
		m.pinToEnd = false
		m.input.Focus()
		return m, textinput.Blink
	default:
		// Forward unhandled keys (pgup/pgdn/ctrl+u/ctrl+d/g/G/etc.) to viewport for scrolling
		var cmd tea.Cmd
		m.browseVP, cmd = m.browseVP.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m searchModel) updateDetailMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.Quit):
		return m, tea.Quit
	case key.Matches(msg, keys.Back), msg.String() == "backspace":
		m.mode = modeBrowse
		return m, nil
	case key.Matches(msg, keys.Search):
		m.mode = modeSearch
		m.input.Focus()
		return m, textinput.Blink
	}
	var cmd tea.Cmd
	m.detailVP, cmd = m.detailVP.Update(msg)
	return m, cmd
}

func (m searchModel) performSearch(cfg search.Config) tea.Cmd {
	searcher := m.semanticSearch
	return func() tea.Msg {
		resp, err := searcher(context.Background(), cfg)
		if err != nil {
			return searchResultsMsg{err: err}
		}
		return searchResultsMsg{results: resp.Results, total: resp.Total, counts: resp.Counts, countsLowerBound: resp.CountsLowerBound, warnings: resp.Warnings}
	}
}

func performCodeSearch(opts codeSearchOpts, gen uint64) tea.Cmd {
	return func() tea.Msg {
		resp, err := searchAllCells(context.Background(), opts)
		return codeSearchResultsMsg{resp: resp, err: err, gen: gen}
	}
}

func (m searchModel) fetchMoreResults(cfg search.Config, page int) tea.Cmd {
	// Pages server-side: the fan-out forwards Page to every cell (each cell's
	// page N is interleaved by the merge), so results past the first fetch
	// stay reachable.
	searcher := m.semanticSearch
	return func() tea.Msg {
		cfg.Page = page
		resp, err := searcher(context.Background(), cfg)
		if err != nil {
			return searchMoreResultsMsg{err: err}
		}
		return searchMoreResultsMsg{results: resp.Results, warnings: resp.Warnings}
	}
}

// ─── View ────────────────────────────────────────────────────────────────────

func (m searchModel) View() tea.View {
	v := tea.View{AltScreen: true}
	if m.width == 0 {
		return v
	}

	switch m.mode {
	case modeDetail:
		v.SetContent(m.viewDetailFull())
	case modeSearch:
		v.SetContent(m.viewSearchMode())
	case modeBrowse:
		v.SetContent(m.viewBrowse())
	}
	return v
}

// viewBrowse composes the browse screen as a fixed master-detail layout:
//
//	┌ header  (search title, query, tabs, RESULTS) — pinned top
//	│ list    (scrollable result rows in browseVP)
//	│ detail  (bordered card for the selected row) — pinned bottom
//	└ footer  (help line) — pinned very bottom
//
// Heights are budgeted from the terminal height so the detail card is always
// fully visible and can never be clipped, regardless of how the list scrolls.
func (m searchModel) viewBrowse() string {
	footer := strings.TrimRight(m.viewHelp(), "\n")
	header, showList := m.viewBrowseHeader()

	var body string
	switch {
	case !showList:
		// Loading / error / empty states: header is the whole body; pin the
		// footer to the bottom of the screen.
		body = padToHeight(header, max(m.height-1, 0)) + "\n" + footer
	default:
		paneH := m.detailPaneHeight(lipgloss.Height(header))
		// browseVP height is kept in sync by refreshBrowseContent; render
		// whatever window it currently exposes.
		list := m.browseVP.View()
		// The reserved row beneath the list shows the page/results count (and a
		// scroll affordance when rows are cut off).
		gap := m.viewListStatusRow()
		if paneH <= 0 {
			body = header + "\n" + list + "\n" + gap + "\n" + footer
		} else {
			body = header + "\n" + list + "\n" + gap + "\n" + m.viewDetailPane(paneH) + "\n" + footer
		}
	}

	// Final safety net: on a terminal too short to fit even the pinned chrome,
	// clamp so we never emit more rows than the screen (which would scroll the
	// alt-screen and clip the top). Exactly-budgeted layouts are unaffected.
	return clampToHeight(body, m.height)
}

// padToHeight pads s with blank lines so it occupies exactly n lines (or leaves
// it unchanged when already taller). Used to push a pinned footer to the bottom.
func padToHeight(s string, n int) string {
	h := lipgloss.Height(s)
	if h >= n {
		return s
	}
	return s + strings.Repeat("\n", n-h)
}

// clampToHeight returns at most the first n lines of s.
func clampToHeight(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
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

// viewTypeTabs renders the type filter tabs with counts.
func (m searchModel) viewTypeTabs() string {
	cmCount, ssCount := m.computeTypeCounts()

	renderTab := func(label string, filter typeFilter, count int, lowerBound bool) string {
		suffix := ""
		if lowerBound {
			// The count is a lower bound — that facet's list was capped
			// upstream. Same "+" the web renders (ENT-1777).
			suffix = "+"
		}
		text := fmt.Sprintf("%s %d%s", label, count, suffix)
		if m.filterType == filter {
			return m.styles.render(m.styles.tabActive, "‹ "+text+" ›")
		}
		return m.styles.render(m.styles.tabInactive, text)
	}

	// No Checkpoints tab: the search service folds checkpoint hits into their
	// owning sessions (ENT-1595), so a checkpoint surfaces as its session. This
	// matches the web, which dropped its Checkpoints tab. Raw checkpoints stay
	// reachable by ID via `entire checkpoint explain <id>` (folded session rows
	// route there).
	// Sessions ORs the checkpoints flag like the web: session rows fold in
	// checkpoint hits, so a capped checkpoints list bounds sessions too.
	tabs := []string{
		renderTab("Commits", typeFilterCommits, cmCount, m.countsLowerBound["commits"]),
		renderTab("Sessions", typeFilterSessions, ssCount, m.countsLowerBound["sessions"] || m.countsLowerBound["checkpoints"]),
		renderTab("Code", typeFilterCode, len(m.codeResults), false),
	}

	return strings.Join(tabs, "  ")
}

// viewBrowseHeader builds the pinned top chrome (search title, query, type
// tabs, RESULTS heading). The second return value reports whether the
// scrolling list + detail regions should render; it is false for the
// loading / error / empty states, where the returned string is the whole body.
func (m searchModel) viewBrowseHeader() (string, bool) {
	var b strings.Builder
	pad := " "

	m.viewSearchHeader(&b)

	query := m.input.Value()
	b.WriteString(pad + m.styles.render(m.styles.sectionTitle, "›") + " " + m.styles.render(m.styles.bold, query))
	b.WriteString("\n\n")

	// Always show type tabs so the user can switch to the Code tab even when
	// the semantic search is loading/errored/empty.
	resultsBlocked := m.loading || m.searchErr != "" || len(m.results) == 0

	// Type tabs
	b.WriteString(pad + m.viewTypeTabs())
	b.WriteString("\n\n")

	// Loading/error/empty state for the semantic-result tabs (Sessions,
	// Commits). Code has its own state below and is exempt.
	if resultsBlocked && m.filterType != typeFilterCode {
		switch {
		case m.loading:
			b.WriteString(pad + m.styles.render(m.styles.dim, "Searching..."))
		case m.searchErr != "":
			b.WriteString(pad + m.styles.render(m.styles.red, "Error: "+m.searchErr))
		default:
			b.WriteString(pad + m.styles.render(m.styles.dim, "No results found."))
		}
		return b.String(), false
	}

	// Section: RESULTS
	b.WriteString(pad + m.styles.render(m.styles.sectionTitle, "RESULTS"))
	b.WriteString("\n")

	// Code tab has its own loading/empty state.
	if m.filterType == typeFilterCode {
		if m.codeLoading {
			b.WriteString("\n" + pad + m.styles.render(m.styles.dim, "Searching code..."))
			return b.String(), false
		}
		if m.codeSearchErr != "" {
			b.WriteString("\n" + pad + m.styles.render(m.styles.red, "Code search error: "+m.codeSearchErr))
			return b.String(), false
		}
		if len(m.codeResults) == 0 {
			b.WriteString("\n" + pad + m.styles.render(m.styles.dim, "No code results found."))
			return b.String(), false
		}
		return b.String(), true
	}

	filtered := m.filteredResults()
	if len(filtered) == 0 {
		if m.fetchingMore {
			b.WriteString("\n" + pad + m.styles.render(m.styles.dim, "Loading more results..."))
		} else {
			b.WriteString("\n" + pad + m.styles.render(m.styles.dim, "No results for this type."))
		}
		return b.String(), false
	}

	return b.String(), true
}

// refreshBrowseContent rebuilds the scrollable list viewport and re-applies the
// master-detail layout (list height + cursor visibility) for the current state.
func (m searchModel) refreshBrowseContent() searchModel {
	// Trim the trailing newline so the viewport's line count reflects the real
	// number of rows (a phantom blank line would keep "scroll down" lit at the
	// bottom of the list).
	m.browseVP.SetContent(strings.TrimRight(m.viewResultList(), "\n"))

	if m.height <= 0 || m.width == 0 {
		return m
	}
	header, showList := m.viewBrowseHeader()
	if !showList {
		return m
	}
	headerLines := lipgloss.Height(header)
	paneH := m.detailPaneHeight(headerLines)
	// Always reserve the gap/scroll-hint row and the footer row.
	listH := max(m.height-headerLines-paneH-detailGap-1, 1)
	m.browseVP.SetHeight(listH)
	return m.ensureCursorVisible()
}

// detailPaneHeight returns the number of terminal rows reserved for the pinned
// detail pane, or 0 when there is no selection or the screen is too short to
// pin one (in which case the list takes the full area and detail is reachable
// via the full-screen view). headerLines is the height of the pinned top chrome.
func (m searchModel) detailPaneHeight(headerLines int) int {
	hasSelection := m.selectedResult() != nil
	if m.filterType == typeFilterCode {
		hasSelection = m.selectedCodeResult() != nil
	}
	if m.height <= 0 || !hasSelection {
		return 0
	}
	avail := m.height - 1 - headerLines - detailGap // footer + gap rows
	if avail < minListHeight+minDetailPaneHeight {
		return 0
	}
	h := m.height * 2 / 5 // ~40% of the screen
	h = min(max(h, minDetailPaneHeight), maxDetailPaneHeight)
	if h > avail-minListHeight {
		h = avail - minListHeight
	}
	return h
}

// ensureCursorVisible scrolls the list viewport so the selected row stays
// within the visible window as the cursor moves.
func (m searchModel) ensureCursorVisible() searchModel {
	listH := m.browseVP.Height()
	if listH <= 0 {
		return m
	}
	top := m.cursor * linesPerResult // first line of the selected item
	bottom := top + 1                // each item occupies 2 lines
	yo := m.browseVP.YOffset()
	switch {
	case top < yo:
		yo = top
	case bottom > yo+listH-1:
		yo = bottom - listH + 1
	}
	m.browseVP.SetYOffset(max(yo, 0))
	return m
}

// viewListStatusRow renders the single reserved row directly beneath the result
// list: a "more results" scroll affordance on the left (only when rows are
// scrolled out of view) and the page / total-results indicator on the right.
// It is exactly one line wide (never wraps) so the layout budget holds.
func (m searchModel) viewListStatusRow() string {
	contentWidth := max(m.width-2, 0)

	// Left: scroll affordance when the list viewport can't show every row.
	left := ""
	if listH := m.browseVP.Height(); listH > 0 {
		if total := m.browseVP.TotalLineCount(); total > listH {
			yo := m.browseVP.YOffset()
			switch {
			case yo > 0 && yo+listH < total:
				left = "↑↓ more results"
			case yo+listH < total:
				left = "↓ more results"
			default:
				left = "↑ more results"
			}
		}
	}

	// Right: loaded count, with the tab's server-side total when more exist
	// ("N of M results"; "+" marks a lower-bound count, matching the tabs).
	n := m.visibleCount()
	tabTotal, lb := m.tabTotal()
	right := fmt.Sprintf("%d results", n)
	if tabTotal > n || lb {
		suffix := ""
		if lb {
			suffix = "+"
		}
		right = fmt.Sprintf("%d of %d%s results", n, tabTotal, suffix)
	}
	if m.fetchingMore {
		right = "loading more… · " + right
	}
	warning := m.warning
	if m.filterType == typeFilterCode {
		warning = m.codeWarning
	}
	if warning != "" {
		right = "⚠ " + warning + " · " + right
	}
	if lipgloss.Width(right) > contentWidth {
		right = stringutil.TruncateRunes(right, contentWidth, "…")
	}

	// Drop the scroll hint if both can't fit on one row.
	if lipgloss.Width(left)+1+lipgloss.Width(right) > contentWidth {
		left = ""
	}
	gap := max(contentWidth-lipgloss.Width(left)-lipgloss.Width(right), 1)

	return " " + m.styles.render(m.styles.dim, left) +
		strings.Repeat(" ", gap) + m.styles.render(m.styles.dim, right)
}

// gutterWidth is the fixed-width left rail of the result list: a selection
// caret, the graph node glyph, and trailing space. The metadata line is
// indented to align with the title (col gutterWidth).
const gutterWidth = 4

// Layout constants for the master-detail browse view.
const (
	// linesPerResult is the vertical stride of one result in the list: 2 content
	// lines (title + meta) plus 1 separator rule between items.
	linesPerResult = 3

	minListHeight       = 4  // never shrink the list below this to fit the detail pane
	minDetailPaneHeight = 6  // smallest worthwhile pinned detail pane (border + a few rows)
	maxDetailPaneHeight = 24 // cap so the detail pane never dominates a tall terminal

	// detailGap is the blank line between the result list and the pinned detail
	// card, for a little breathing room.
	detailGap = 1
)

// viewResultList renders the active tab's loaded results as a two-line list:
// a type-colored graph node + bold title with a right-aligned relative age,
// then a dim metadata line (type · repo · ⎇ branch · author). Items are
// separated by a thin rule, mirroring the web activity list.
func (m searchModel) viewResultList() string {
	contentWidth := max(m.width-2, 0) // 1 char padding each side
	pad := " "

	var b strings.Builder
	rule := pad + m.styles.render(m.styles.dim, strings.Repeat("─", contentWidth)) + "\n"

	if m.filterType == typeFilterCode {
		for i, r := range m.codeResults {
			if i > 0 {
				b.WriteString(rule)
			}
			b.WriteString(m.viewCodeResultItem(r, i == m.cursor, contentWidth))
		}
		return b.String()
	}

	results := m.filteredResults()
	for i, r := range results {
		if i > 0 {
			b.WriteString(rule)
		}
		b.WriteString(m.viewResultItem(r, i == m.cursor, contentWidth))
	}

	return b.String()
}

// viewResultItem renders a single two-line result entry (title line + meta line)
// with a leading 1-char pad on each line, matching the surrounding browse layout.
func (m searchModel) viewResultItem(r search.Result, selected bool, contentWidth int) string {
	pad := " "
	var b strings.Builder

	// ── Title line: gutter + bold title …… right-aligned age ──
	node, caret := "◇", " "
	if selected {
		node, caret = "◆", "▸"
	}
	gutter := caret + " " + m.styles.render(resultNodeStyle(m.styles, r.Type, selected), node) + " "

	age := formatSearchAge(r.ResultCreatedAt())
	ageW := lipgloss.Width(age)

	titleMax := max(contentWidth-gutterWidth-ageW-1, 8)
	title := stringutil.TruncateRunes(stringutil.CollapseWhitespace(r.ResultTitle()), titleMax, "…")
	titleStyle := m.styles.bold
	if selected {
		titleStyle = m.styles.selected
	}

	gap := max(contentWidth-gutterWidth-lipgloss.Width(title)-ageW, 1)
	b.WriteString(pad + gutter + m.styles.render(titleStyle, title) +
		strings.Repeat(" ", gap) + m.styles.render(m.styles.dim, age) + "\n")

	// ── Meta line: type · repo · ⎇ branch · author (indented to title col) ──
	indent := strings.Repeat(" ", gutterWidth)
	// The search type constants ("checkpoint", "commit", "session") double as
	// the lowercase type word shown on the metadata line.
	typeWord := r.Type
	typeTag := m.styles.render(resultNodeStyle(m.styles, r.Type, false), typeWord)

	var meta strings.Builder
	meta.WriteString(r.ResultOrg() + "/" + r.ResultRepo())
	if br := r.ResultBranch(); br != "" {
		meta.WriteString("  ⎇ " + br)
	}
	if author := r.ResultAuthor(); author != "" {
		meta.WriteString("  · " + author)
	}
	metaMax := max(contentWidth-gutterWidth-lipgloss.Width(typeWord)-2, 8)
	metaStr := stringutil.TruncateRunes(meta.String(), metaMax, "…")
	b.WriteString(pad + indent + typeTag + "  " + m.styles.render(m.styles.dim, metaStr) + "\n")

	return b.String()
}

// viewCodeResultItem renders a single two-line code search result (file:line + context).
func (m searchModel) viewCodeResultItem(r codesearch.Result, selected bool, contentWidth int) string {
	pad := " "
	var b strings.Builder

	// ── Title line: gutter + repo:path:line ──
	node, caret := "◇", " "
	if selected {
		node, caret = "◆", "▸"
	}
	nodeStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Green))
	if !m.styles.colorEnabled {
		nodeStyle = lipgloss.NewStyle()
	}
	if selected {
		nodeStyle = m.styles.selected
	}
	gutter := caret + " " + m.styles.render(nodeStyle, node) + " "

	location := fmt.Sprintf("%s:%s:%d", r.Repo, r.Path, r.Line)
	titleMax := max(contentWidth-gutterWidth, 8)
	location = stringutil.TruncateRunes(location, titleMax, "…")
	titleStyle := m.styles.bold
	if selected {
		titleStyle = m.styles.selected
	}
	b.WriteString(pad + gutter + m.styles.render(titleStyle, location) + "\n")

	// ── Context line: the matching source line, truncated ──
	indent := strings.Repeat(" ", gutterWidth)
	ctx := r.ContextLine
	runes := []rune(ctx)
	ctxMax := max(contentWidth-gutterWidth, 8)
	if len(runes) > ctxMax {
		ctx = string(runes[:ctxMax]) + "…"
	}
	b.WriteString(pad + indent + m.styles.render(m.styles.dim, ctx) + "\n")

	return b.String()
}

// renderCodeDetail builds the detail content for a code search result.
func (m searchModel) renderCodeDetail(r codesearch.Result, contentWidth int, showSections bool) string {
	w := m.newDetailWriter("Code Match", contentWidth, showSections)

	w.section("LOCATION")
	w.field("Repo", r.Repo)
	w.field("Path", r.Path)
	w.field("Line", strconv.Itoa(r.Line))
	if r.Column > 0 {
		w.field("Column", strconv.Itoa(r.Column))
	}
	if r.Score > 0 {
		w.field("Score", fmt.Sprintf("%.3f", r.Score))
	}

	w.section("CONTEXT")
	for _, line := range r.ContextBefore {
		w.b.WriteString(m.styles.render(m.styles.dim, "  "+line) + "\n")
	}
	w.b.WriteString("▸ " + r.ContextLine + "\n")
	for _, line := range r.ContextAfter {
		w.b.WriteString(m.styles.render(m.styles.dim, "  "+line) + "\n")
	}

	return w.String()
}

// resultNodeStyle returns the accent style for a result's graph node and type
// tag: accent (magenta) for checkpoints, bright magenta for sessions, blue for
// commits. The
// selected node is always rendered in the shared selection accent.
func resultNodeStyle(s searchStyles, resultType string, selected bool) lipgloss.Style {
	if !s.colorEnabled {
		return lipgloss.NewStyle()
	}
	if selected {
		return s.selected
	}
	switch resultType {
	case search.TypeSession:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(searchDetailAccent))
	case search.TypeCommit:
		return lipgloss.NewStyle().Foreground(lipgloss.Color(searchCommitAccent))
	default: // checkpoint and unknown
		return lipgloss.NewStyle().Foreground(lipgloss.Color(searchAccent))
	}
}

// typeLabel returns a short type badge for display in the static table.
func typeLabel(resultType string) string {
	switch resultType {
	case search.TypeCheckpoint:
		return "CP"
	case search.TypeCommit:
		return "CM"
	case search.TypeSession:
		return "SS"
	default:
		return strings.ToUpper(resultType[:min(2, len(resultType))])
	}
}

// formatResultID returns a display-friendly ID for a result.
func formatResultID(r search.Result) string {
	if r.Type == search.TypeCommit && r.Commit != nil && len(r.Commit.CommitSHA) > 7 {
		return r.Commit.CommitSHA[:7]
	}
	return r.ResultID()
}

// renderDetailContent builds the text content for a result detail (no border/card chrome).
func (m searchModel) renderDetailContent(r search.Result, contentWidth int, showSections bool) string {
	switch r.Type {
	case search.TypeCheckpoint:
		return m.renderCheckpointDetail(r, contentWidth, showSections)
	case search.TypeCommit:
		return m.renderCommitDetail(r, contentWidth, showSections)
	case search.TypeSession:
		return m.renderSessionDetail(r, contentWidth, showSections)
	default:
		return m.renderCheckpointDetail(r, contentWidth, showSections)
	}
}

// detailWriter accumulates a label/value detail body. It owns the shared
// layout (label column width, value wrap width, section spacing) so the
// per-type renderers below differ only in which fields they emit.
type detailWriter struct {
	b            strings.Builder
	styles       searchStyles
	labelWidth   int
	valueWidth   int
	showSections bool
}

func (m searchModel) newDetailWriter(title string, contentWidth int, showSections bool) *detailWriter {
	const labelWidth = 12
	valueWidth := contentWidth - labelWidth - 1
	if valueWidth < 20 {
		valueWidth = 0
	}
	w := &detailWriter{
		styles:       m.styles,
		labelWidth:   labelWidth,
		valueWidth:   valueWidth,
		showSections: showSections,
	}
	w.b.WriteString(w.styles.render(w.styles.detailTitle, title) + "\n")
	return w
}

func (w *detailWriter) label(label string) string {
	return w.styles.render(w.styles.label, fmt.Sprintf("%-*s", w.labelWidth, label+":"))
}

func (w *detailWriter) field(label, value string) {
	w.b.WriteString(w.label(label) + " " + value + "\n")
}

func (w *detailWriter) wrappedField(label, value string) {
	if w.valueWidth == 0 || len(value) <= w.valueWidth {
		w.field(label, value)
		return
	}
	indent := strings.Repeat(" ", w.labelWidth+1)
	lines := strings.Split(wrapText(value, w.valueWidth), "\n")
	w.b.WriteString(w.label(label) + " " + lines[0] + "\n")
	for _, line := range lines[1:] {
		w.b.WriteString(indent + line + "\n")
	}
}

func (w *detailWriter) section(title string) {
	if w.showSections {
		w.b.WriteString("\n" + w.styles.render(w.styles.detailTitle, title) + "\n")
	} else {
		w.b.WriteString("\n")
	}
}

// matchField emits the "Match" row, appending the relevance score when present.
func (w *detailWriter) matchField(meta search.Meta) {
	value := meta.MatchType
	if meta.Score > 0 {
		value += " " + w.styles.render(w.styles.dim, fmt.Sprintf("(score: %.3f)", meta.Score))
	}
	w.field("Match", value)
}

// authorField emits the "Author" row, preferring the username with the raw
// author dimmed in parentheses when both are available.
func (w *detailWriter) authorField(author string, username *string) {
	value := author
	if username != nil && *username != "" {
		value = *username + " " + w.styles.render(w.styles.dim, "("+author+")")
	}
	w.field("Author", value)
}

func (w *detailWriter) String() string {
	return strings.TrimRight(w.b.String(), "\n")
}

func (m searchModel) renderCheckpointDetail(r search.Result, contentWidth int, showSections bool) string {
	w := m.newDetailWriter("Checkpoint Detail", contentWidth, showSections)
	cp := r.Checkpoint
	if cp == nil {
		return w.String()
	}

	// ── OVERVIEW ──
	w.section("OVERVIEW")
	w.field("ID", cp.ID)
	w.wrappedField("Prompt", stringutil.CollapseWhitespace(cp.Prompt))
	w.matchField(r.Meta)

	// ── SOURCE ──
	w.section("SOURCE")
	w.wrappedField("Commit", formatCommit(cp.CommitSHA, cp.CommitMessage))
	w.field("Branch", cp.Branch)
	w.field("Repo", cp.Org+"/"+cp.Repo)
	w.authorField(cp.Author, cp.AuthorUsername)
	w.field("Created", formatDetailCreatedAt(cp.CreatedAt, m.styles))

	// ── SNIPPET ──
	if r.Meta.Snippet != "" {
		w.section("SNIPPET")
		switch {
		case showSections:
			w.b.WriteString(renderSnippetMarkdown(r.Meta.Snippet, contentWidth, m.darkBg) + "\n")
		case w.valueWidth > 0:
			w.b.WriteString(wrapText(r.Meta.Snippet, contentWidth) + "\n")
		default:
			w.b.WriteString(r.Meta.Snippet + "\n")
		}
	}

	// ── FILES ──
	if len(cp.FilesTouched) > 0 {
		w.b.WriteString("\n")
		if showSections {
			w.b.WriteString(m.styles.render(m.styles.detailTitle, "FILES") + "\n")
		} else {
			w.b.WriteString(m.styles.render(m.styles.label, "Files:") + "\n")
		}
		for _, f := range cp.FilesTouched {
			w.b.WriteString("  " + f + "\n")
		}
	}

	return w.String()
}

func (m searchModel) renderCommitDetail(r search.Result, contentWidth int, showSections bool) string {
	w := m.newDetailWriter("Commit Detail", contentWidth, showSections)
	cm := r.Commit
	if cm == nil {
		return w.String()
	}

	w.section("OVERVIEW")
	sha := cm.CommitSHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	w.field("SHA", sha)
	w.wrappedField("Subject", cm.CommitSubject)
	if cm.CommitMessage != cm.CommitSubject {
		w.wrappedField("Message", stringutil.CollapseWhitespace(cm.CommitMessage))
	}
	w.matchField(r.Meta)

	w.section("SOURCE")
	w.field("Branch", cm.Branch)
	w.field("Repo", cm.Org+"/"+cm.Repo)
	w.authorField(cm.Author, cm.AuthorUsername)
	w.field("Created", formatDetailCreatedAt(cm.CreatedAt, m.styles))

	w.section("STATS")
	w.field("Additions", fmt.Sprintf("+%d", cm.Additions))
	w.field("Deletions", fmt.Sprintf("-%d", cm.Deletions))
	w.field("Files", fmt.Sprintf("%d changed", cm.FilesChanged))

	if cm.HTMLUrl != nil && *cm.HTMLUrl != "" {
		w.field("URL", *cm.HTMLUrl)
	}

	return w.String()
}

func (m searchModel) renderSessionDetail(r search.Result, contentWidth int, showSections bool) string {
	w := m.newDetailWriter("Session Detail", contentWidth, showSections)
	ss := r.Session
	if ss == nil {
		return w.String()
	}

	w.section("OVERVIEW")
	w.field("Session ID", ss.SessionID)
	w.wrappedField("Name", ss.DisplayName)
	if ss.Prompt != nil && *ss.Prompt != "" {
		w.wrappedField("Prompt", stringutil.CollapseWhitespace(*ss.Prompt))
	}
	if ss.Agent != nil && *ss.Agent != "" {
		w.field("Agent", *ss.Agent)
	}
	if ss.Model != nil && *ss.Model != "" {
		w.field("Model", *ss.Model)
	}
	w.field("Steps", strconv.Itoa(ss.StepCount))
	w.matchField(r.Meta)

	w.section("SOURCE")
	w.field("Repo", ss.Org+"/"+ss.Repo)
	if ss.Branch != nil && *ss.Branch != "" {
		w.field("Branch", *ss.Branch)
	}
	// Session author is the username only (no raw-author fallback to dim).
	if ss.AuthorUsername != nil && *ss.AuthorUsername != "" {
		w.field("Author", *ss.AuthorUsername)
	}
	w.field("Created", formatDetailCreatedAt(ss.CreatedAt, m.styles))

	return w.String()
}

// formatDetailCreatedAt renders date (default) + relative time (dim) for the detail view.
func formatDetailCreatedAt(createdAt string, styles searchStyles) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return createdAt
	}
	return t.Format("Jan 02, 2006") + " " + styles.render(styles.dim, "("+timeAgo(t)+")")
}

// viewDetailPane renders the pinned detail card for the selected result,
// sized to exactly paneH terminal rows. Content taller than the pane is
// truncated with a "▼ enter for more" hint (the full content is always
// available via the full-screen detail view). Shorter content is padded so
// the pane occupies its full allotment and the footer stays pinned.
func (m searchModel) viewDetailPane(paneH int) string {
	if paneH <= 0 {
		return strings.TrimSuffix(padToHeight("", paneH), "\n")
	}

	// Code tab uses codeResults; other tabs use selectedResult().
	var r *search.Result
	if m.filterType == typeFilterCode {
		if m.selectedCodeResult() == nil {
			return strings.TrimSuffix(padToHeight("", paneH), "\n")
		}
	} else {
		r = m.selectedResult()
		if r == nil {
			return strings.TrimSuffix(padToHeight("", paneH), "\n")
		}
	}

	var contentWidth, borderWidth, chrome int
	if m.styles.colorEnabled {
		// lipgloss v2 .Width(W) is the outer width: it absorbs horizontal padding
		// (2+2) and the rounded border (1+1), so the inner text area is W-6.
		// .Height(H) yields H + vertical-padding(0) + border(2) rendered rows, so
		// vertical chrome is 2. Wrapping content to W-6 keeps the card at its
		// budgeted height.
		borderWidth = max(m.width-3, 0)
		contentWidth = max(borderWidth-6, 0)
		chrome = 2
	} else {
		// NO_COLOR: a leading dim rule stands in for the top border.
		contentWidth = max(m.width-1, 0)
		chrome = 1
	}

	contentLines := max(paneH-chrome, 1)
	var detailContent string
	if m.filterType == typeFilterCode {
		if cr := m.selectedCodeResult(); cr != nil {
			detailContent = m.renderCodeDetail(*cr, contentWidth, false)
		}
	} else {
		detailContent = m.renderDetailContent(*r, contentWidth, false)
	}
	lines := strings.Split(detailContent, "\n")
	if len(lines) > contentLines {
		lines = lines[:contentLines]
		hint := m.styles.render(m.styles.dim, "▼ enter for more")
		lines[contentLines-1] = strings.Repeat(" ", max(contentWidth-lipgloss.Width(hint), 0)) + hint
	}
	// Hard-cap each line to the inner width (ANSI-aware) so no line wraps inside
	// the bordered box and inflates the card past its budgeted height. This keeps
	// the pane exactly paneH rows tall regardless of detail content.
	for i, ln := range lines {
		if lipgloss.Width(ln) > contentWidth {
			lines[i] = xansi.Truncate(ln, contentWidth, "…")
		}
	}
	body := strings.Join(lines, "\n")

	if m.styles.colorEnabled {
		card := m.styles.detailBorder.Width(borderWidth).Height(contentLines).Render(body)
		return strings.TrimSuffix(indentLines(card, " "), "\n")
	}

	pane := " " + m.styles.horizontalRule(contentWidth) + "\n" + strings.TrimSuffix(indentLines(body, " "), "\n")
	return padToHeight(pane, paneH)
}

func (m searchModel) viewDetailFull() string {
	var b strings.Builder
	b.WriteString(m.detailVP.View())
	b.WriteString("\n")

	// Scroll indicator + help
	scrollPct := m.styles.render(m.styles.dim, fmt.Sprintf("%3.f%%", m.detailVP.ScrollPercent()*100))
	dot := m.styles.render(m.styles.helpSep, " · ")
	help := m.styles.helpItem("j/k", "scroll") + dot +
		m.styles.helpItem(keys.Back.Help().Key, keys.Back.Help().Desc) + dot +
		m.styles.helpItem(keys.Quit.Help().Key, keys.Quit.Help().Desc)

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
		return m.styles.helpItem(keys.Confirm.Help().Key, "search") + dot +
			m.styles.helpItem(keys.Back.Help().Key, "cancel") + "\n"
	}

	left := m.styles.helpItem(keys.Search.Help().Key, keys.Search.Help().Desc) + dot +
		m.styles.helpItem("↑/↓, j/k", "scroll") + dot +
		m.styles.helpItem("home/end, g/G", "top/bottom") + dot +
		m.styles.helpItem("←/→", "type") + dot +
		m.styles.helpItem(keys.Quit.Help().Key, keys.Quit.Help().Desc)

	// The results count lives on the status row beneath the list
	// (viewListStatusRow), so the footer holds only the key hints.
	return left + "\n"
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
	typeCol int
	age     int
	id      int
	branch  int
	repo    int
	prompt  int
	author  int
}

// computeColumns calculates column widths from terminal width.
func computeColumns(width int) columnLayout {
	const (
		typeWidth   = 5
		ageWidth    = 10
		idWidth     = 12
		repoMin     = 10
		authorWidth = 14
		gaps        = 6 // spaces between columns (one more than before for type col)
	)

	remaining := width - typeWidth - ageWidth - idWidth - authorWidth - gaps
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
		typeCol: typeWidth,
		age:     ageWidth,
		id:      idWidth,
		branch:  branchWidth,
		repo:    repoWidth,
		prompt:  promptWidth,
		author:  authorWidth,
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

// ─── Snippet Markdown ────────────────────────────────────────────────────────

// renderSnippetMarkdown renders a search snippet as markdown using glamour v2.
// It is used in the full-screen checkpoint detail view where the snippet has
// room to breathe; the inline detail card keeps plain word-wrapping. On any
// renderer error or impractically narrow widths it falls back to wrapText.
//
// dark must be detected before bubbletea owns the terminal — querying termenv
// inside the Update loop races against bubbletea's stdin reader and stalls.
//
// A fresh TermRenderer is built per call. *TermRenderer carries shared mutable
// state via ansi.RenderContext.blockStack, so caching the renderer would
// require serialising every Render call; construction is cheap (just goldmark
// + ANSI option setup, no chroma init unless a fenced code block forces it),
// so we just rebuild and avoid the concurrency hazard altogether.
func renderSnippetMarkdown(snippet string, width int, dark bool) string {
	if width < 20 {
		return wrapText(snippet, width)
	}
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(snippetMarkdownStyles(dark)),
		glamour.WithWordWrap(width),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return wrapText(snippet, width)
	}
	rendered, err := renderer.Render(snippet)
	if err != nil {
		return wrapText(snippet, width)
	}
	return strings.TrimRight(rendered, "\n")
}

// snippetMarkdownStyles returns a glamour style config tailored for inline
// snippets. Foreground colours are nilled across every text-bearing element
// so the snippet inherits the terminal's default foreground colour. ANSI
// palette numbers like "234" embedded in glamour's stock styles get remapped
// by terminal themes and produce unreadable colours on cream / Solarized
// backgrounds — letting the terminal pick the colour avoids that entirely.
//
// IMPORTANT: this function copies a package-level glamourstyles var by value,
// then re-assigns its pointer fields. *Re-assigning* (`= nil`, `= &x`) is
// safe — it rebinds the local field. *Dereferencing* through the pointer
// (`*s.Document.Color = "x"`) would mutate the shared global and pollute
// every other glamour caller in the process. Don't do that.
func snippetMarkdownStyles(dark bool) ansi.StyleConfig {
	var s ansi.StyleConfig
	if dark {
		s = glamourstyles.DarkStyleConfig
	} else {
		s = glamourstyles.LightStyleConfig
	}
	zero := uint(0)
	s.Document.Margin = &zero
	s.Document.BlockPrefix = ""
	s.Document.BlockSuffix = ""

	// Null foreground on every primitive that contributes to flowing text so
	// nothing relies on theme-remappable ANSI palette numbers. Code/CodeBlock
	// keep their styling because BackgroundColor is enough to differentiate
	// them visually.
	s.Document.Color = nil
	s.Paragraph.Color = nil
	s.Text.Color = nil
	s.BlockQuote.Color = nil
	s.Strong.Color = nil
	s.Emph.Color = nil
	s.Strikethrough.Color = nil
	s.Heading.Color = nil
	s.H1.Color = nil
	s.H2.Color = nil
	s.H3.Color = nil
	s.H4.Color = nil
	s.H5.Color = nil
	s.H6.Color = nil
	s.Item.Color = nil
	s.Enumeration.Color = nil
	s.List.Color = nil

	// Links are the one place we *want* a colour: an underline alone is easy
	// to miss inline. Match mdrender's link pair (Cyan URL, Blue link text).
	linkColor := palette.Cyan
	linkTextColor := palette.Blue
	s.Link.Color = &linkColor
	s.LinkText.Color = &linkTextColor

	return s
}

// ─── Static Fallback ─────────────────────────────────────────────────────────

// renderSearchStatic writes a non-interactive table for accessible mode.
func renderSearchStatic(w io.Writer, results []search.Result, query string, total int, lowerBound bool, styles statusStyles) {
	// "+" marks a lower bound — some type's list was capped, so more matches
	// exist than are counted (the web renders the same suffix; ENT-1777).
	suffix := ""
	if lowerBound {
		suffix = "+"
	}
	fmt.Fprintf(w, "Found %d%s results matching %q\n\n", total, suffix, query)

	cols := computeColumns(styles.width)

	fmt.Fprintf(w, "%-*s %-*s %-*s %-*s %-*s %-*s %-*s\n",
		cols.typeCol, "TYPE",
		cols.age, "AGE",
		cols.id, "ID",
		cols.branch, "BRANCH",
		cols.repo, "REPO",
		cols.prompt, "TITLE",
		cols.author, "AUTHOR",
	)

	for _, r := range results {
		typeBadge := typeLabel(r.Type)
		age := formatSearchAge(r.ResultCreatedAt())
		id := stringutil.TruncateRunes(formatResultID(r), cols.id, "")
		branch := stringutil.TruncateRunes(r.ResultBranch(), cols.branch, "...")
		repo := stringutil.TruncateRunes(r.ResultOrg()+"/"+r.ResultRepo(), cols.repo, "...")
		title := stringutil.TruncateRunes(
			stringutil.CollapseWhitespace(r.ResultTitle()), cols.prompt, "...",
		)
		author := stringutil.TruncateRunes(r.ResultAuthor(), cols.author, "...")

		fmt.Fprintf(w, "%-*s %-*s %-*s %-*s %-*s %-*s %-*s\n",
			cols.typeCol, typeBadge,
			cols.age, age,
			cols.id, id,
			cols.branch, branch,
			cols.repo, repo,
			cols.prompt, title,
			cols.author, author,
		)
	}
}
