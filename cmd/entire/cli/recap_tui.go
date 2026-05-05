package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

type recapTUIOptions struct {
	Range recap.RangeKey
	View  recap.ViewMode
	Agent string
	Repo  string
	Color bool
}

type recapDataMsg struct {
	requestID int
	resp      *recap.MeRecapResponse
}

type recapErrMsg struct {
	requestID int
	err       error
}

type recapTUIModel struct {
	ctx    context.Context
	client *api.Client
	repo   string

	rangeKey recap.RangeKey
	view     recap.ViewMode
	agent    string
	color    bool

	resp      *recap.MeRecapResponse
	loadErr   error
	loading   bool
	requestID int

	viewport viewport.Model
	width    int
	height   int
	ready    bool
}

func runRecapTUI(ctx context.Context, client *api.Client, opts recapTUIOptions) error {
	p := tea.NewProgram(newRecapTUIModel(ctx, client, opts))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("recap TUI: %w", err)
	}
	return nil
}

func newRecapTUIModel(ctx context.Context, client *api.Client, opts recapTUIOptions) recapTUIModel {
	if opts.Range == "" {
		opts.Range = recap.RangeDay
	}
	if opts.View == "" {
		opts.View = recap.ViewBoth
	}
	if opts.Agent == "" {
		opts.Agent = recap.AgentAll
	}
	return recapTUIModel{
		ctx:       ctx,
		client:    client,
		repo:      opts.Repo,
		rangeKey:  opts.Range,
		view:      opts.View,
		agent:     opts.Agent,
		color:     opts.Color,
		loading:   true,
		requestID: 1,
		width:     recap.DefaultWidth,
		height:    24,
		viewport:  viewport.New(viewport.WithWidth(recap.DefaultWidth), viewport.WithHeight(23)),
	}
}

func (m recapTUIModel) Init() tea.Cmd {
	return m.fetch(m.requestID)
}

func (m recapTUIModel) fetch(requestID int) tea.Cmd {
	return func() tea.Msg {
		start, end := m.rangeKey.Bounds(time.Now())
		resp, err := recap.FetchMeRecap(m.ctx, m.client, start, end, m.repo, 0)
		if err != nil {
			return recapErrMsg{requestID: requestID, err: err}
		}
		return recapDataMsg{requestID: requestID, resp: resp}
	}
}

func (m recapTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // bubbletea interface
	switch msg := msg.(type) {
	case recapDataMsg:
		if msg.requestID != m.requestID {
			return m, nil
		}
		m.loading = false
		m.loadErr = nil
		m.resp = msg.resp
		m = m.withViewport()
		return m, nil

	case recapErrMsg:
		if msg.requestID != m.requestID {
			return m, nil
		}
		m.loading = false
		m.loadErr = msg.err
		return m, nil

	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		m.height = max(msg.Height, 1)
		m = m.withViewport()
		return m, nil

	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Quit) || key.Matches(msg, keys.Back) {
			return m, tea.Quit
		}
		switch msg.String() {
		case "t":
			m.rangeKey = nextRecapRange(m.rangeKey)
			m.requestID++
			m.loading = true
			m.loadErr = nil
			m.resp = nil
			return m.withViewport(), m.fetch(m.requestID)
		case "v":
			m.view = nextRecapView(m.view)
			return m.withViewport(), nil
		case "a":
			m.agent = m.nextAgent()
			return m.withViewport(), nil
		case "r":
			m.requestID++
			m.loading = true
			m.loadErr = nil
			return m, m.fetch(m.requestID)
		}
		if key.Matches(msg, keys.Home) {
			m.viewport.GotoTop()
			return m, nil
		}
		if key.Matches(msg, keys.End) {
			m.viewport.GotoBottom()
			return m, nil
		}
	}

	if m.ready {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m recapTUIModel) View() tea.View {
	v := tea.View{AltScreen: true}
	if m.loadErr != nil {
		v.SetContent(fmt.Sprintf("\n  Failed to load recap: %s\n\n  Press r to retry or q to quit.\n", m.loadErr))
		return v
	}
	if m.loading && m.resp == nil {
		v.SetContent("\n  Loading recap...\n\n  Press q to quit.\n")
		return v
	}
	if !m.ready {
		return v
	}

	var b strings.Builder
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	v.SetContent(b.String())
	return v
}

func (m recapTUIModel) withViewport() recapTUIModel {
	vpHeight := m.height - 1
	if vpHeight < 1 {
		vpHeight = 1
	}
	if !m.ready {
		m.viewport = viewport.New(viewport.WithWidth(m.width), viewport.WithHeight(vpHeight))
		m.ready = true
	} else {
		m.viewport.SetWidth(m.width)
		m.viewport.SetHeight(vpHeight)
	}
	if m.resp != nil {
		m.viewport.SetContent(recap.RenderStaticRecap(m.resp, recap.RenderOptions{
			Range: m.rangeKey,
			View:  m.view,
			Agent: m.agent,
			Width: m.width,
			Color: m.color,
		}))
	}
	return m
}

func (m recapTUIModel) renderFooter() string {
	choices := []string{
		recapFooterLine(m.color, []recapHelpItem{
			{"t", "range"},
			{"v", "view"},
			{"a", "agent"},
			{"r", "refresh"},
			{"↑/↓", "scroll"},
			{"q", "quit"},
		}),
		recapFooterLine(m.color, []recapHelpItem{
			{"t", "range"},
			{"v", "view"},
			{"a", "agent"},
			{"q", "quit"},
		}),
		recapFooterLine(m.color, []recapHelpItem{
			{"t", "range"},
			{"v", "view"},
			{"q", "quit"},
		}),
		recapFooterLine(m.color, []recapHelpItem{
			{"q", "quit"},
		}),
	}
	for _, choice := range choices {
		if m.width <= 0 || lipgloss.Width(choice) <= m.width {
			return choice
		}
	}
	return ""
}

type recapHelpItem struct {
	key  string
	desc string
}

func recapFooterLine(color bool, items []recapHelpItem) string {
	if !color {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, item.key+" "+item.desc)
		}
		return strings.Join(parts, " · ")
	}
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Bold(true)
	item := func(k, desc string) string {
		return keyStyle.Render(k) + helpStyle.Render(" "+desc)
	}
	parts := make([]string, 0, len(items))
	for _, helpItem := range items {
		parts = append(parts, item(helpItem.key, helpItem.desc))
	}
	return strings.Join(parts, helpStyle.Render(" · "))
}

func nextRecapRange(current recap.RangeKey) recap.RangeKey {
	switch current {
	case recap.RangeDay:
		return recap.RangeWeek
	case recap.RangeWeek:
		return recap.RangeMonth
	case recap.RangeMonth:
		return recap.Range90d
	case recap.Range90d:
		return recap.RangeDay
	default:
		return recap.RangeDay
	}
}

func nextRecapView(current recap.ViewMode) recap.ViewMode {
	switch current {
	case recap.ViewBoth:
		return recap.ViewYou
	case recap.ViewYou:
		return recap.ViewTeam
	case recap.ViewTeam:
		return recap.ViewBoth
	default:
		return recap.ViewBoth
	}
}

func (m recapTUIModel) nextAgent() string {
	agents := m.agentIDs()
	if len(agents) == 0 {
		return recap.AgentAll
	}
	if m.agent == "" || m.agent == recap.AgentAll {
		return agents[0]
	}
	for i, agentID := range agents {
		if agentID == m.agent {
			if i == len(agents)-1 {
				return recap.AgentAll
			}
			return agents[i+1]
		}
	}
	return recap.AgentAll
}

func (m recapTUIModel) agentIDs() []string {
	if m.resp == nil {
		return nil
	}
	type agentItem struct {
		id    string
		label string
	}
	items := make([]agentItem, 0, len(m.resp.Agents))
	for key, entry := range m.resp.Agents {
		id := entry.AgentID
		if id == "" {
			id = key
		}
		if id == "" {
			continue
		}
		label := entry.AgentLabel
		if label == "" {
			label = id
		}
		items = append(items, agentItem{id: id, label: label})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].label != items[j].label {
			return items[i].label < items[j].label
		}
		return items[i].id < items[j].id
	})
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.id)
	}
	return ids
}
