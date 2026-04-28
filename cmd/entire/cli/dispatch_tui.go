package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	glamourstyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/muesli/termenv"
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
var newDispatchProgram = func(model tea.Model, outW io.Writer, altScreen bool) dispatchProgram {
	options := []tea.ProgramOption{tea.WithOutput(outW)}
	if altScreen {
		options = append(options, tea.WithAltScreen())
	}
	return tea.NewProgram(model, options...)
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
	clearDispatchInlineView(outW, finished.View())
	if finished.result.err != nil {
		return "", finished.result.err
	}
	return finished.result.markdown, nil
}

func defaultRenderTerminalMarkdown(w io.Writer, markdown string) (string, error) {
	renderer, err := glamour.NewTermRenderer(
		glamour.WithStyles(dispatchMarkdownStyles()),
		glamour.WithWordWrap(getTerminalWidth(w)),
		glamour.WithPreservedNewLines(),
	)
	if err != nil {
		return "", fmt.Errorf("initialize markdown renderer: %w", err)
	}

	rendered, err := renderer.Render(markdown)
	if err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return rendered, nil
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

func dispatchMarkdownStyles() ansi.StyleConfig {
	return dispatchMarkdownStylesForBackground(termenv.HasDarkBackground())
}

func dispatchMarkdownStylesForBackground(darkBackground bool) ansi.StyleConfig {
	var styles ansi.StyleConfig
	if darkBackground {
		styles = glamourstyles.DarkStyleConfig
	} else {
		styles = glamourstyles.LightStyleConfig
	}

	if darkBackground {
		styles.Document.Color = stringPtr("252")
		styles.Heading.Color = stringPtr("252")
		styles.Code.BackgroundColor = stringPtr("236")
		styles.CodeBlock.Color = stringPtr("252")
	} else {
		styles.Document.Color = stringPtr("234")
		styles.Heading.Color = stringPtr("234")
		styles.Code.BackgroundColor = stringPtr("254")
		styles.CodeBlock.Color = stringPtr("242")
	}
	styles.Heading.Bold = boolPtr(true)

	styles.H1.Prefix = "# "
	styles.H1.Suffix = ""
	styles.H1.Color = stringPtr("#fb923c")
	styles.H1.BackgroundColor = nil
	styles.H1.Bold = boolPtr(true)

	styles.H2.Color = stringPtr("#22d3ee")
	styles.H2.Bold = boolPtr(true)
	styles.H3.Color = stringPtr("#818cf8")
	styles.H3.Bold = boolPtr(true)
	styles.H4.Color = stringPtr("252")
	styles.H4.Bold = boolPtr(true)
	styles.H5.Color = stringPtr("245")
	styles.H5.Bold = boolPtr(true)
	styles.H6.Color = stringPtr("245")
	styles.H6.Bold = boolPtr(false)

	styles.HorizontalRule.Color = stringPtr("240")
	styles.Item.Color = stringPtr("#fb923c")
	styles.Enumeration.Color = stringPtr("#818cf8")
	styles.BlockQuote.Color = stringPtr("245")

	styles.Link.Color = stringPtr("#22d3ee")
	styles.Link.Underline = boolPtr(true)
	styles.LinkText.Color = stringPtr("#818cf8")
	styles.LinkText.Bold = boolPtr(true)

	styles.Code.Color = stringPtr("#fb923c")
	if darkBackground {
		styles.CodeBlock.Chroma = &ansi.Chroma{
			Text: ansi.StylePrimitive{
				Color: stringPtr("252"),
			},
			Error: ansi.StylePrimitive{
				Color: stringPtr("252"),
			},
			Comment: ansi.StylePrimitive{
				Color:  stringPtr("245"),
				Italic: boolPtr(true),
			},
			Keyword: ansi.StylePrimitive{
				Color: stringPtr("#818cf8"),
				Bold:  boolPtr(true),
			},
			KeywordReserved: ansi.StylePrimitive{
				Color: stringPtr("#818cf8"),
				Bold:  boolPtr(true),
			},
			Name: ansi.StylePrimitive{
				Color: stringPtr("252"),
			},
			NameFunction: ansi.StylePrimitive{
				Color: stringPtr("#22d3ee"),
			},
			NameBuiltin: ansi.StylePrimitive{
				Color: stringPtr("#818cf8"),
			},
			Literal: ansi.StylePrimitive{
				Color: stringPtr("#fbbf24"),
			},
			LiteralString: ansi.StylePrimitive{
				Color: stringPtr("#fbbf24"),
			},
			LiteralNumber: ansi.StylePrimitive{
				Color: stringPtr("#fbbf24"),
			},
			Operator: ansi.StylePrimitive{
				Color: stringPtr("244"),
			},
			Punctuation: ansi.StylePrimitive{
				Color: stringPtr("244"),
			},
			GenericDeleted: ansi.StylePrimitive{
				Color: stringPtr("1"),
			},
			GenericInserted: ansi.StylePrimitive{
				Color: stringPtr("2"),
			},
			Background: ansi.StylePrimitive{
				BackgroundColor: stringPtr("236"),
			},
		}
	} else {
		styles.CodeBlock.Chroma = &ansi.Chroma{
			Text: ansi.StylePrimitive{
				Color: stringPtr("#2A2A2A"),
			},
			Error: ansi.StylePrimitive{
				Color: stringPtr("#2A2A2A"),
			},
			Comment: ansi.StylePrimitive{
				Color:  stringPtr("#8D8D8D"),
				Italic: boolPtr(true),
			},
			Keyword: ansi.StylePrimitive{
				Color: stringPtr("#818cf8"),
				Bold:  boolPtr(true),
			},
			KeywordReserved: ansi.StylePrimitive{
				Color: stringPtr("#818cf8"),
				Bold:  boolPtr(true),
			},
			Name: ansi.StylePrimitive{
				Color: stringPtr("#2A2A2A"),
			},
			NameFunction: ansi.StylePrimitive{
				Color: stringPtr("#22d3ee"),
			},
			NameBuiltin: ansi.StylePrimitive{
				Color: stringPtr("#818cf8"),
			},
			Literal: ansi.StylePrimitive{
				Color: stringPtr("#fbbf24"),
			},
			LiteralString: ansi.StylePrimitive{
				Color: stringPtr("#fbbf24"),
			},
			LiteralNumber: ansi.StylePrimitive{
				Color: stringPtr("#fbbf24"),
			},
			Operator: ansi.StylePrimitive{
				Color: stringPtr("#7A7A7A"),
			},
			Punctuation: ansi.StylePrimitive{
				Color: stringPtr("#7A7A7A"),
			},
			GenericDeleted: ansi.StylePrimitive{
				Color: stringPtr("1"),
			},
			GenericInserted: ansi.StylePrimitive{
				Color: stringPtr("2"),
			},
			Background: ansi.StylePrimitive{
				BackgroundColor: stringPtr("254"),
			},
		}
	}

	styles.Table.Color = stringPtr("245")
	styles.Table.CenterSeparator = stringPtr(" ")
	styles.Table.ColumnSeparator = stringPtr(" ")
	styles.Table.RowSeparator = stringPtr("-")

	return styles
}

func boolPtr(v bool) *bool {
	return &v
}

func stringPtr(v string) *string {
	return &v
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

//nolint:ireturn // tea.Model interface contract
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
	case tea.KeyMsg:
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

func (m dispatchStatusModel) View() string {
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

	return "\n" + m.styles.card.Width(cardWidth).Render(strings.Join(lines, "\n"))
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
