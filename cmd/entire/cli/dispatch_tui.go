package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/muesli/termenv"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/mdrender"
)

type dispatchRenderResult struct {
	markdown string
	err      error
}

type dispatchStatusModel struct {
	ctx      context.Context
	cancel   context.CancelFunc
	spinner  spinner.Model
	styles   dispatchStatusStyles
	title    string
	subtitle string
	details  []string
	footer   string
	width    int
	height   int
	run      func(context.Context) (string, error)
	result   dispatchRenderResult
}

type dispatchStatusStyles struct {
	card     lipgloss.Style
	title    lipgloss.Style
	subtitle lipgloss.Style
	detail   lipgloss.Style
	footer   lipgloss.Style
	spinner  lipgloss.Style
}

type dispatchProgram interface {
	Run() (tea.Model, error)
}

// newDispatchProgram is overridden by tests via assignment. Tests that mutate
// it cannot use t.Parallel() — they would race each other's factory.
// altScreen is unused in v2 (set on tea.View instead) but retained for backward
// compatibility with existing test fakes.
var newDispatchProgram = func(model tea.Model, outW io.Writer, _ bool) dispatchProgram {
	return tea.NewProgram(model, tea.WithOutput(outW))
}

func defaultRunInteractiveDispatch(ctx context.Context, outW io.Writer, opts dispatchpkg.Options) (string, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	model := newDispatchStatusModel(outW, opts, func(runCtx context.Context) (string, error) {
		result, err := runDispatch(runCtx, opts)
		if err != nil {
			return "", err
		}
		return renderDispatchMarkdown(result), nil
	})
	model.ctx = runCtx
	model.cancel = cancel

	program := newDispatchProgram(model, outW, false)
	finalModel, err := program.Run()
	if err != nil {
		return "", fmt.Errorf("run dispatch tui: %w", err)
	}

	finished, ok := finalModel.(dispatchStatusModel)
	if !ok {
		return "", errors.New("unexpected dispatch loading state")
	}
	clearDispatchInlineView(outW, finished.View().Content)
	if finished.result.err != nil {
		return "", finished.result.err
	}
	return finished.result.markdown, nil
}

// defaultRenderTerminalMarkdown renders dispatch's LLM markdown output via
// the shared mdrender palette. Always renders (no TTY check) — dispatch's
// existing behavior is to emit ANSI codes even when redirected so that
// `entire dispatch | less -R` still shows colors.
func defaultRenderTerminalMarkdown(w io.Writer, markdown string) (string, error) {
	return mdrender.Render(markdown, getTerminalWidth(w), termenv.HasDarkBackground()) //nolint:wrapcheck // mdrender already wraps glamour's errors with package context
}

func newDispatchStatusModel(
	w io.Writer,
	opts dispatchpkg.Options,
	run func(context.Context) (string, error),
) dispatchStatusModel {
	ss := newStatusStyles(w)
	styles := newDispatchStatusStyles(ss)
	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	if ss.colorEnabled {
		sp.Style = styles.spinner
	}

	title := "Generating dispatch"
	subtitle := "This can take a moment."

	return dispatchStatusModel{
		spinner:  sp,
		styles:   styles,
		title:    title,
		subtitle: subtitle,
		details:  dispatchStatusDetails(opts),
		footer:   "Press ctrl+c to cancel",
		width:    ss.width,
		height:   12,
		run:      run,
	}
}

func newDispatchStatusStyles(ss statusStyles) dispatchStatusStyles {
	styles := dispatchStatusStyles{
		card:     lipgloss.NewStyle(),
		title:    lipgloss.NewStyle().Bold(true),
		subtitle: lipgloss.NewStyle(),
		detail:   lipgloss.NewStyle(),
		footer:   lipgloss.NewStyle(),
		spinner:  lipgloss.NewStyle().Bold(true),
	}
	if !ss.colorEnabled {
		return styles
	}

	styles.title = styles.title.Foreground(lipgloss.Color("#fb923c"))
	styles.subtitle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styles.detail = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styles.footer = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styles.spinner = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c")).Bold(true)
	return styles
}

func dispatchStatusDetails(opts dispatchpkg.Options) []string {
	scope := "Scope: current repo"
	if len(opts.RepoPaths) > 0 {
		scope = "Scope: " + strings.Join(opts.RepoPaths, ", ")
	}

	var branches string
	switch {
	case opts.AllBranches:
		branches = "Branches: all local branches"
	case opts.Mode == dispatchpkg.ModeLocal:
		branches = "Branches: current branch"
	default:
		branches = "Branches: default branches"
	}

	window := "Window: " + strings.TrimSpace(opts.Since)
	if strings.TrimSpace(opts.Until) != "" {
		window += " → " + strings.TrimSpace(opts.Until)
	}

	return []string{scope, branches, window}
}

func (m dispatchStatusModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.runDispatch())
}

func (m dispatchStatusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	case dispatchRenderResult:
		m.result = msg
		return m, tea.Quit
	case tea.KeyPressMsg:
		if key.Matches(msg, keys.Quit) || key.Matches(msg, keys.Back) {
			if m.cancel != nil {
				m.cancel()
			}
			m.result.err = errDispatchCancelled
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m dispatchStatusModel) View() tea.View {
	cardWidth := min(max(m.width-8, 44), 76)

	lines := []string{
		m.styles.spinner.Render(m.spinner.View()) + " " + m.styles.title.Render(m.title),
		m.styles.subtitle.Render(m.subtitle),
		"",
	}
	for _, detail := range m.details {
		lines = append(lines, m.styles.detail.Render(detail))
	}
	lines = append(lines, "", m.styles.footer.Render(m.footer))

	return tea.NewView("\n" + m.styles.card.Width(cardWidth).Render(strings.Join(lines, "\n")))
}

func (m dispatchStatusModel) runDispatch() tea.Cmd {
	return func() tea.Msg {
		markdown, err := m.run(m.ctx)
		return dispatchRenderResult{markdown: markdown, err: err}
	}
}

func clearDispatchInlineView(w io.Writer, view string) {
	lineCount := renderedLineCount(view)
	for range lineCount {
		_, _ = io.WriteString(w, "\x1b[1A\x1b[2K\r") //nolint:errcheck // terminal escape sequence, ignore write errors
	}
}

func renderedLineCount(view string) int {
	if view == "" {
		return 0
	}
	return strings.Count(view, "\n") + 1
}
