package main

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// This is the proposal's single review screen: every onboarding value is
// pre-decided and shown as an editable row; Enter accepts the whole screen and
// builds the folder. ←/→ (or space) change a single-value row inline; the
// Agents row expands to a multi-select; "More options" reveals the advanced
// defaults. Nothing is written until Enter — the model only collects a Config.

type opt struct{ v, t string }

// field is one editable row, backed by getters/setters over the live Config so
// the row is just a view of the resolved decision.
type field struct {
	label, hint string
	multi       bool
	opts        []opt
	cur         func(c *Config) string    // single-select: current value key
	setv        func(c *Config, v string) // single-select: set value
	has         func(c *Config, v string) bool
	toggle      func(c *Config, v string)
	display     func(c *Config) string
}

var (
	stAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	stInk    = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	stLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	stFaint  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	stBoldW  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("255"))
	stFocus  = lipgloss.NewStyle().Background(lipgloss.Color("236"))
	stAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("179"))
	stLink   = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Underline(true)
)

const mirrorDocsURL = "https://docs.entire.io/guides/repositories/mirrors"

type reviewModel struct {
	cfg        Config
	focus      int // 0..len(fields); last index is the "More options" row
	showMore   bool
	agentsOpen bool
	agentFocus int
	confirmed  bool
	cancelled  bool
}

func (m reviewModel) fields() []field {
	var fs []field
	if m.cfg.State == StateEmpty {
		fs = append(fs, field{
			label: "Repository",
			opts: []opt{
				{"local", "New git repo + initial commit"},
				{"github", "+ private GitHub repo (pushed)"},
				{"bare", "Git repo, no commit"},
			},
			cur:  func(c *Config) string { return c.RepoMode },
			setv: func(c *Config, v string) { c.RepoMode = v; c.Connect = c.mirrorable() },
			display: func(c *Config) string {
				return map[string]string{"local": "New git repo + initial commit", "github": "Private GitHub repo (pushed)", "bare": "Git repo, no commit"}[c.RepoMode]
			},
		})
	}
	if m.cfg.State == StateRepoNoOrigin {
		fs = append(fs, field{
			label: "Publish",
			opts:  []opt{{"no", "Local only"}, {"yes", "Publish to GitHub + mirror"}},
			cur:   func(c *Config) string { return boolKey(c.Publish, "yes", "no") },
			setv:  func(c *Config, v string) { c.Publish = v == "yes"; c.Connect = c.mirrorable() },
			display: func(c *Config) string {
				return boolKey(c.Publish, "Publish to GitHub — mirrors to Entire", "Local only (not published)")
			},
		})
	}
	fs = append(fs, field{
		label: "Agents", hint: "detected", multi: true,
		opts:   []opt{{"Claude Code", ""}, {"Gemini CLI", ""}, {"Codex", ""}, {"Cursor", ""}},
		has:    func(c *Config, v string) bool { return contains(c.Agents, v) },
		toggle: func(c *Config, v string) { c.Agents = toggleStr(c.Agents, v) },
		display: func(c *Config) string {
			if len(c.Agents) == 0 {
				return "none selected"
			}
			return strings.Join(c.Agents, ", ")
		},
	})
	if m.cfg.showsWebUIRow() {
		fs = append(fs, field{
			label: "Mirror to Entire", hint: "unlocks code, commits, search & transcripts in entire.io",
			opts:    []opt{{"mirror", "Yes"}, {"local", "No"}},
			cur:     func(c *Config) string { return boolKey(c.Connect, "mirror", "local") },
			setv:    func(c *Config, v string) { c.Connect = v == "mirror" },
			display: func(c *Config) string { return boolKey(c.Connect, "Yes", "No") },
		})
	}
	fs = append(fs, field{
		label: "Telemetry", hint: "anonymous usage",
		opts:    []opt{{"on", "On"}, {"off", "Off"}},
		cur:     func(c *Config) string { return boolKey(c.Telemetry, "on", "off") },
		setv:    func(c *Config, v string) { c.Telemetry = v == "on" },
		display: func(c *Config) string { return boolKey(c.Telemetry, "On", "Off") },
	})
	if m.showMore {
		fs = append(fs, field{
			label: "Checkpoints", hint: "advanced",
			opts: []opt{{"refs", "Git refs (recommended)"}, {"branch", "Shared branch"}},
			cur:  func(c *Config) string { return c.Checkpoints },
			setv: func(c *Config, v string) { c.Checkpoints = v },
			display: func(c *Config) string {
				return boolKey(c.Checkpoints == "refs", "Git refs (recommended)", "Shared branch")
			},
		})
		fs = append(fs, field{
			label: "Import history", hint: fmt.Sprintf("%d past sessions", m.cfg.pastSessions),
			opts: []opt{{"skip", "Skip"}, {"all", "Import all"}},
			cur:  func(c *Config) string { return boolKey(c.ImportAll, "all", "skip") },
			setv: func(c *Config, v string) { c.ImportAll = v == "all" },
			display: func(c *Config) string {
				return boolKey(c.ImportAll, fmt.Sprintf("Import %d sessions", m.cfg.pastSessions), fmt.Sprintf("Skip %d past sessions", m.cfg.pastSessions))
			},
		})
	}
	return fs
}

func (m reviewModel) Init() tea.Cmd { return nil }

func (m reviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	fs := m.fields()
	moreIdx := len(fs)

	if key.Mod == tea.ModCtrl && key.Code == 'c' {
		m.cancelled = true
		return m, tea.Quit
	}

	if m.agentsOpen {
		af := agentsFieldIndex(fs)
		n := len(fs[af].opts)
		switch {
		case key.Code == tea.KeyUp:
			m.agentFocus = (m.agentFocus - 1 + n) % n
		case key.Code == tea.KeyDown:
			m.agentFocus = (m.agentFocus + 1) % n
		case key.Code == ' ':
			fs[af].toggle(&m.cfg, fs[af].opts[m.agentFocus].v)
		case key.Code == tea.KeyEnter || key.Code == tea.KeyEscape:
			m.agentsOpen = false
		}
		return m, nil
	}

	switch {
	case key.Code == tea.KeyUp:
		if m.focus > 0 {
			m.focus--
		}
	case key.Code == tea.KeyDown:
		if m.focus < moreIdx {
			m.focus++
		}
	case key.Code == tea.KeyEnter:
		m.confirmed = true
		return m, tea.Quit
	case key.Code == tea.KeyEscape || key.Code == 'q':
		m.cancelled = true
		return m, tea.Quit
	case key.Code == tea.KeyRight || key.Code == ' ':
		if m.focus == moreIdx {
			m.showMore = !m.showMore
			return m, nil
		}
		f := fs[m.focus]
		if f.multi {
			m.agentsOpen = true
			m.agentFocus = 0
		} else {
			cycle(&m.cfg, f, +1)
		}
	case key.Code == tea.KeyLeft:
		if m.focus < moreIdx && !fs[m.focus].multi {
			cycle(&m.cfg, fs[m.focus], -1)
		}
	}
	return m, nil
}

func (m reviewModel) View() tea.View {
	fs := m.fields()
	moreIdx := len(fs)
	var b strings.Builder

	title := "Enable Entire in this repo"
	if m.cfg.State == StateEmpty {
		title = "Set up this folder with Entire"
	}
	b.WriteString(stBoldW.Render(title) + "\n")
	b.WriteString(stFaint.Render("  "+m.cfg.State.found(m.cfg.Slug)) + "\n")
	b.WriteString(stFaint.Render("  Everything's set — press enter, or change anything first.") + "\n\n")

	for i, f := range fs {
		focused := i == m.focus && !m.agentsOpen
		caret := "  "
		if focused {
			caret = stAccent.Render("› ")
		}
		label := stLabel.Render(pad(f.label, 18))
		val := stAccent.Render("● ") + stInk.Render(f.display(&m.cfg))
		hint := ""
		if f.hint != "" {
			hint = "  " + stFaint.Render(f.hint)
		}
		line := caret + label + val + hint
		if focused {
			line = stFocus.Render(line)
		}
		b.WriteString(line + "\n")

		if m.agentsOpen && f.multi {
			for j, o := range f.opts {
				g := "◻"
				if f.has(&m.cfg, o.v) {
					g = stAccent.Render("◼")
				}
				pointer := "  "
				if j == m.agentFocus {
					pointer = stAccent.Render("› ")
				}
				b.WriteString("      " + pointer + g + " " + stInk.Render(o.v) + "\n")
			}
			b.WriteString("      " + stFaint.Render("space toggles · enter closes") + "\n")
		}
	}

	// More options row.
	moreLabel := "▸ More options  (checkpoints · import)"
	if m.showMore {
		moreLabel = "▾ Fewer options"
	}
	moreLine := "  " + stFaint.Render(moreLabel)
	if m.focus == moreIdx {
		moreLine = stFocus.Render(stAccent.Render("› ") + stFaint.Render(moreLabel))
	}
	b.WriteString(moreLine + "\n\n")

	// Without a mirror entire.io shows only an onboarding page for the repo —
	// lead with what stays locked, then the pitch + docs.
	if !(m.cfg.mirrorable() && m.cfg.Connect) {
		b.WriteString(stAmber.Render("  Local-only: entire.io shows just an onboarding page for this repo.") + "\n")
		b.WriteString(stAmber.Render("  Mirror this repo to unleash the full power of entire.io.") + "\n")
		b.WriteString(stFaint.Render("  Docs: ") + stLink.Render(mirrorDocsURL) + "\n")
	}
	// Spell out every consequential action Enter takes — publishing a GitHub
	// repo and mirroring are not obvious from the rows alone.
	var actions []string
	if m.cfg.State == StateEmpty {
		actions = append(actions, "inits a git repo")
	}
	actions = append(actions, "installs hooks & config")
	if m.cfg.publishesGitHub() {
		actions = append(actions, "publishes to GitHub")
	}
	if m.cfg.Connect && m.cfg.mirrorable() {
		actions = append(actions, "mirrors to Entire")
	}
	b.WriteString(stAccent.Render("▸ Press enter") + stFaint.Render(" — "+humanJoin(actions)) + "\n")
	b.WriteString(stFaint.Render("  ↑↓ move · ←→/space change · enter enable · esc cancel") + "\n")

	// Inline (not AltScreen): the review draws in place below the prompt and
	// the run output flows underneath, instead of taking over the terminal.
	return tea.NewView(b.String())
}

// runReview runs the review screen and returns the reviewed config, or ok=false
// if the user cancelled.
func runReview(ctx context.Context, cfg Config) (Config, bool, error) {
	m, err := tea.NewProgram(reviewModel{cfg: cfg}, tea.WithContext(ctx)).Run()
	if err != nil {
		return cfg, false, err
	}
	rm := m.(reviewModel)
	return rm.cfg, rm.confirmed && !rm.cancelled, nil
}

// --- helpers ---

func cycle(c *Config, f field, dir int) {
	cur := f.cur(c)
	idx := 0
	for i, o := range f.opts {
		if o.v == cur {
			idx = i
			break
		}
	}
	n := len(f.opts)
	f.setv(c, f.opts[((idx+dir)%n+n)%n].v)
}

func agentsFieldIndex(fs []field) int {
	for i, f := range fs {
		if f.multi {
			return i
		}
	}
	return 0
}

// humanJoin renders a list as "a", "a and b", or "a, b, and c".
func humanJoin(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	case 2:
		return xs[0] + " and " + xs[1]
	default:
		return strings.Join(xs[:len(xs)-1], ", ") + ", and " + xs[len(xs)-1]
	}
}

func pad(s string, w int) string {
	for len(s) < w {
		s += " "
	}
	return s
}

func boolKey(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func toggleStr(xs []string, v string) []string {
	if contains(xs, v) {
		out := xs[:0:0]
		for _, x := range xs {
			if x != v {
				out = append(out, x)
			}
		}
		return out
	}
	return append(append([]string{}, xs...), v)
}
