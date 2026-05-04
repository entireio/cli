package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
)

// activityDataMsg is sent when API data has been fetched.
type activityDataMsg struct {
	stats  contributionStats
	repos  []repoContribution
	hourly []hourlyPoint
	days   []commitDay
}

// activityErrMsg is sent when fetching fails.
type activityErrMsg struct{ err error }

type activityModel struct {
	// Data (nil until loaded)
	stats  *contributionStats
	repos  []repoContribution
	hourly []hourlyPoint
	days   []commitDay

	// Loading state
	loading bool
	loadErr error
	spinner spinner.Model

	// Fetch context
	ctx    context.Context
	client *api.Client

	// View state
	viewport viewport.Model
	sty      activityStyles
	useColor bool
	width    int
	height   int
	ready    bool
}

func runActivityTUI(ctx context.Context, client *api.Client) error {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	m := activityModel{
		loading:  true,
		spinner:  sp,
		ctx:      ctx,
		client:   client,
		useColor: shouldUseColor(os.Stdout),
	}
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("activity TUI: %w", err)
	}
	return nil
}

func (m activityModel) fetchData() tea.Msg { //nolint:ireturn // bubbletea Cmd signature requires tea.Msg return
	activity, commits, err := fetchActivityData(m.ctx, m.client)
	if err != nil {
		return activityErrMsg{err: err}
	}

	return activityDataMsg{
		stats: contributionStats{
			Tasks:         activity.Stats.Tasks,
			Throughput:    activity.Stats.Throughput,
			Iteration:     activity.Stats.Iteration,
			ContinuityH:   activity.Stats.ContinuityHours,
			Streak:        activity.Stats.LifetimeStreak,
			CurrentStreak: activity.Stats.LifetimeCurrentStreak,
		},
		repos:  activity.Repos,
		hourly: activity.HourlyContributions,
		days:   groupCommitsByDay(commits),
	}
}

func (m activityModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.fetchData)
}

func (m activityModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) { //nolint:ireturn // bubbletea interface
	switch msg := msg.(type) {
	case activityDataMsg:
		m.loading = false
		m.stats = &msg.stats
		m.repos = msg.repos
		m.hourly = msg.hourly
		m.days = msg.days
		if m.width > 0 {
			m = m.withViewport()
		}
		return m, nil

	case activityErrMsg:
		m.loading = false
		m.loadErr = msg.err
		return m, nil

	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Quit) || key.Matches(msg, keys.Back) {
			return m, tea.Quit
		}
		if key.Matches(msg, keys.Home) {
			if m.ready {
				m.viewport.GotoTop()
			}
			return m, nil
		}
		if key.Matches(msg, keys.End) {
			if m.ready {
				m.viewport.GotoBottom()
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.sty = newActivityStylesWithWidth(m.width, m.useColor)
		if m.stats != nil {
			m = m.withViewport()
		}
		return m, nil

	case spinner.TickMsg:
		if m.loading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			return m, cmd
		}
	}

	if m.ready {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m activityModel) withViewport() activityModel {
	headerHeight := m.headerLineCount()
	vpHeight := m.height - headerHeight - 1
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
	m.viewport.SetContent(m.renderCommits())
	return m
}

func (m activityModel) View() tea.View {
	v := tea.View{AltScreen: true}
	if m.loadErr != nil {
		v.SetContent(fmt.Sprintf("\n  Failed to load activity: %s\n\n  Press q to quit.\n", m.loadErr))
		return v
	}

	if m.loading {
		v.SetContent(fmt.Sprintf("\n  %s Loading activity...\n", m.spinner.View()))
		return v
	}

	if !m.ready {
		return v
	}

	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	b.WriteString(m.renderFooter())
	v.SetContent(b.String())
	return v
}

func (m activityModel) renderHeader() string {
	if m.stats == nil {
		return ""
	}
	var buf bytes.Buffer
	buf.WriteString("\n")
	renderStatCards(&buf, m.sty, *m.stats)
	buf.WriteString("\n")
	renderContributionChart(&buf, m.sty, m.hourly, m.repos)
	buf.WriteString("\n")
	renderRepoChart(&buf, m.sty, m.repos)
	buf.WriteString("\n")
	return buf.String()
}

func (m activityModel) headerLineCount() int {
	return strings.Count(m.renderHeader(), "\n")
}

func (m activityModel) renderCommits() string {
	var buf bytes.Buffer
	renderCommitListN(&buf, m.sty, m.days, -1)
	return buf.String()
}

func (m activityModel) renderFooter() string {
	if !m.sty.colorEnabled {
		return ""
	}
	helpStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Bold(true)
	sep := helpStyle.Render(" · ")

	fullHelp := keyStyle.Render("↑/↓, j/k") + helpStyle.Render(" scroll") +
		sep + keyStyle.Render("home/end, g/G") + helpStyle.Render(" top/bottom") +
		sep + keyStyle.Render(keys.Quit.Help().Key) + helpStyle.Render(" "+keys.Quit.Help().Desc)
	standardHelp := keyStyle.Render("↑/↓") + helpStyle.Render(" scroll") +
		sep + keyStyle.Render("home/end") + helpStyle.Render(" top/bottom") +
		sep + keyStyle.Render(keys.Quit.Help().Key) + helpStyle.Render(" "+keys.Quit.Help().Desc)
	shortHelp := keyStyle.Render("↑/↓") + helpStyle.Render(" scroll")
	quitHelp := keyStyle.Render(keys.Quit.Help().Key) + helpStyle.Render(" "+keys.Quit.Help().Desc)
	helpChoices := []string{fullHelp, standardHelp, shortHelp, quitHelp}

	if m.viewport.TotalLineCount() <= m.viewport.Height() {
		for _, help := range helpChoices {
			if m.width <= 0 || lipgloss.Width(help) <= m.width {
				return help
			}
		}
		return ""
	}

	pct := helpStyle.Render(padLeft(int(m.viewport.ScrollPercent()*100)) + "%")
	for _, help := range helpChoices {
		gap := m.width - lipgloss.Width(help) - lipgloss.Width(sep) - lipgloss.Width(pct)
		if gap >= 1 {
			return help + sep + helpStyle.Render(strings.Repeat(" ", gap)) + pct
		}
	}

	pctWidth := lipgloss.Width(pct)
	if m.width < pctWidth {
		return helpStyle.Render(strings.Repeat(" ", max(m.width, 0)))
	}
	return helpStyle.Render(strings.Repeat(" ", m.width-pctWidth)) + pct
}

func newActivityStylesWithWidth(width int, useColor bool) activityStyles {
	return activityStyles{
		colorEnabled: useColor,
		width:        width,
		bold:         lipgloss.NewStyle().Bold(true),
		dim:          lipgloss.NewStyle().Faint(true),
		label:        lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Bold(true),
		value:        lipgloss.NewStyle().Bold(true),
		unit:         lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		desc:         lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		repoNm:       lipgloss.NewStyle().Foreground(lipgloss.Color("7")),
		commitH:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		commitM:      lipgloss.NewStyle().Bold(true),
		add:          lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		del:          lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		muted:        lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
}

func padLeft(n int) string {
	s := strings.Builder{}
	if n < 10 {
		s.WriteString("  ")
	} else if n < 100 {
		s.WriteString(" ")
	}
	fmt.Fprintf(&s, "%d", n)
	return s.String()
}
