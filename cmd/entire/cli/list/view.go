package list

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Styles for the TUI
var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("39")).
			MarginBottom(1)

	selectedStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("212"))

	branchStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("39"))

	currentBranchStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("46"))

	sessionStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	activeSessionStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("46"))

	checkpointStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244"))

	mergedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Italic(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginTop(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))
)

// keyMap defines the keybindings for the TUI.
type keyMap struct {
	Up       key.Binding
	Down     key.Binding
	Left     key.Binding
	Right    key.Binding
	Enter    key.Binding
	Open     key.Binding
	Resume   key.Binding
	Rewind   key.Binding
	Help     key.Binding
	Quit     key.Binding
	Collapse key.Binding
	Expand   key.Binding
}

var keys = keyMap{
	Up: key.NewBinding(
		key.WithKeys("up", "k"),
		key.WithHelp("up/k", "move up"),
	),
	Down: key.NewBinding(
		key.WithKeys("down", "j"),
		key.WithHelp("down/j", "move down"),
	),
	Left: key.NewBinding(
		key.WithKeys("left", "h"),
		key.WithHelp("left/h", "collapse"),
	),
	Right: key.NewBinding(
		key.WithKeys("right", "l"),
		key.WithHelp("right/l", "expand"),
	),
	Enter: key.NewBinding(
		key.WithKeys("enter"),
		key.WithHelp("enter", "toggle/select"),
	),
	Open: key.NewBinding(
		key.WithKeys("o"),
		key.WithHelp("o", "open session"),
	),
	Resume: key.NewBinding(
		key.WithKeys("r"),
		key.WithHelp("r", "resume"),
	),
	Rewind: key.NewBinding(
		key.WithKeys("w"),
		key.WithHelp("w", "rewind"),
	),
	Help: key.NewBinding(
		key.WithKeys("?"),
		key.WithHelp("?", "help"),
	),
	Quit: key.NewBinding(
		key.WithKeys("q", "ctrl+c", "esc"),
		key.WithHelp("q", "quit"),
	),
	Collapse: key.NewBinding(
		key.WithKeys("c"),
		key.WithHelp("c", "collapse all"),
	),
	Expand: key.NewBinding(
		key.WithKeys("e"),
		key.WithHelp("e", "expand all"),
	),
}

// Model is the bubbletea model for the list view.
//
//nolint:recvcheck // Mixed receivers required by bubbletea's interface pattern
type Model struct {
	// Tree data
	tree     []*Node
	flatList []*Node
	cursor   int

	// State
	width      int
	height     int
	showHelp   bool
	quitting   bool
	actionDone bool

	// Result of action (if any)
	result *ActionResult
}

// NewModel creates a new list view model.
func NewModel(tree []*Node) Model {
	m := Model{
		tree:   tree,
		cursor: 0,
	}
	m.updateFlatList()
	return m
}

// updateFlatList rebuilds the flat list based on expansion state.
func (m *Model) updateFlatList() {
	m.flatList = FlattenTree(m.tree)
	// Ensure cursor stays in bounds
	if m.cursor >= len(m.flatList) {
		m.cursor = len(m.flatList) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
//
//nolint:ireturn // Required by tea.Model interface
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// If action was done, any key quits
		if m.actionDone {
			m.quitting = true
			return m, tea.Quit
		}

		switch {
		case key.Matches(msg, keys.Quit):
			m.quitting = true
			return m, tea.Quit

		case key.Matches(msg, keys.Up):
			if m.cursor > 0 {
				m.cursor--
			}

		case key.Matches(msg, keys.Down):
			if m.cursor < len(m.flatList)-1 {
				m.cursor++
			}

		case key.Matches(msg, keys.Left):
			// Collapse current node or go to parent
			if len(m.flatList) > 0 {
				node := m.flatList[m.cursor]
				if node.Expanded && len(node.Children) > 0 {
					node.Expanded = false
					m.updateFlatList()
				} else if node.Parent != nil {
					// Find parent in flat list and move cursor there
					for i, n := range m.flatList {
						if n == node.Parent {
							m.cursor = i
							break
						}
					}
				}
			}

		case key.Matches(msg, keys.Right), key.Matches(msg, keys.Enter):
			// Expand current node
			if len(m.flatList) > 0 {
				node := m.flatList[m.cursor]
				if len(node.Children) > 0 {
					node.Expanded = !node.Expanded
					m.updateFlatList()
				} else if key.Matches(msg, keys.Enter) {
					// If no children, perform default action
					m.performDefaultAction()
				}
			}

		case key.Matches(msg, keys.Open):
			if len(m.flatList) > 0 {
				node := m.flatList[m.cursor]
				if node.Type == NodeTypeSession || node.Type == NodeTypeCheckpoint {
					m.result = PerformOpen(node)
					m.actionDone = true
					return m, tea.Quit
				}
			}

		case key.Matches(msg, keys.Resume):
			if len(m.flatList) > 0 {
				node := m.flatList[m.cursor]
				m.result = PerformResume(node)
				m.actionDone = true
				return m, tea.Quit
			}

		case key.Matches(msg, keys.Rewind):
			if len(m.flatList) > 0 {
				node := m.flatList[m.cursor]
				if node.Type == NodeTypeCheckpoint {
					m.result = PerformRewind(node)
					m.actionDone = true
					return m, tea.Quit
				}
			}

		case key.Matches(msg, keys.Help):
			m.showHelp = !m.showHelp

		case key.Matches(msg, keys.Collapse):
			// Collapse all nodes
			for _, node := range m.tree {
				collapseAll(node)
			}
			m.updateFlatList()

		case key.Matches(msg, keys.Expand):
			// Expand all nodes
			for _, node := range m.tree {
				expandAll(node)
			}
			m.updateFlatList()
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	}

	return m, nil
}

// performDefaultAction performs the default action for the current node.
func (m *Model) performDefaultAction() {
	if len(m.flatList) == 0 {
		return
	}

	node := m.flatList[m.cursor]
	switch node.Type {
	case NodeTypeBranch:
		// For branches, resume
		m.result = PerformResume(node)
		m.actionDone = true
	case NodeTypeSession:
		// For sessions, open
		m.result = PerformOpen(node)
		m.actionDone = true
	case NodeTypeCheckpoint:
		// For checkpoints, rewind
		m.result = PerformRewind(node)
		m.actionDone = true
	}
}

// collapseAll recursively collapses all nodes.
func collapseAll(node *Node) {
	node.Expanded = false
	for _, child := range node.Children {
		collapseAll(child)
	}
}

// expandAll recursively expands all nodes.
func expandAll(node *Node) {
	node.Expanded = true
	for _, child := range node.Children {
		expandAll(child)
	}
}

// View implements tea.Model.
func (m Model) View() string {
	if m.quitting {
		if m.result != nil {
			return m.renderResult()
		}
		return ""
	}

	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("Entire Checkpoints"))
	b.WriteString("\n\n")

	// Tree view
	if len(m.flatList) == 0 {
		b.WriteString("No checkpoints found.\n")
	} else {
		for i, node := range m.flatList {
			line := m.renderNode(node, i == m.cursor)
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	// Help
	if m.showHelp {
		b.WriteString("\n")
		b.WriteString(m.renderFullHelp())
	} else {
		b.WriteString("\n")
		b.WriteString(helpStyle.Render("[o]pen  [r]esume  [w]ind  [enter]toggle  [?]help  [q]uit"))
	}

	return b.String()
}

// renderNode renders a single node in the tree.
func (m Model) renderNode(node *Node, selected bool) string {
	depth := GetNodeDepth(node)
	indent := strings.Repeat("  ", depth)

	// Expansion indicator
	var expander string
	if len(node.Children) > 0 {
		if node.Expanded {
			expander = "v "
		} else {
			expander = "> "
		}
	} else {
		expander = "  "
	}

	// Node icon based on type
	var icon string
	switch node.Type {
	case NodeTypeBranch:
		icon = ""
	case NodeTypeSession:
		if node.IsActive {
			icon = "* "
		} else {
			icon = "o "
		}
	case NodeTypeCheckpoint:
		if node.IsTaskCheckpoint {
			icon = "t "
		} else {
			icon = "- "
		}
	}

	// Build label
	label := m.formatNodeLabel(node)

	// Apply styling
	var style lipgloss.Style
	switch node.Type {
	case NodeTypeBranch:
		switch {
		case node.IsCurrent:
			style = currentBranchStyle
		case node.IsMerged:
			style = mergedStyle
		default:
			style = branchStyle
		}
	case NodeTypeSession:
		if node.IsActive {
			style = activeSessionStyle
		} else {
			style = sessionStyle
		}
	case NodeTypeCheckpoint:
		style = checkpointStyle
	}

	text := indent + expander + icon + style.Render(label)

	if selected {
		return selectedStyle.Render("> ") + text
	}
	return "  " + text
}

// formatNodeLabel formats the label for a node.
func (m Model) formatNodeLabel(node *Node) string {
	switch node.Type {
	case NodeTypeBranch:
		label := node.ID
		if node.IsCurrent {
			label += " *"
		}
		if node.IsMerged {
			label += " (merged)"
		}
		if len(node.Children) > 0 {
			label += fmt.Sprintf("  [%d checkpoints]", len(node.Children))
		}
		return label

	case NodeTypeCheckpoint:
		// Show commit hash and message (this is the main item now)
		var parts []string

		// Commit hash
		if node.CommitHash != "" {
			parts = append(parts, node.CommitHash)
		}

		// Commit message (truncated)
		if node.CommitMsg != "" {
			msg := node.CommitMsg
			if len(msg) > 40 {
				msg = msg[:37] + "..."
			}
			parts = append(parts, msg)
		}

		// Steps count
		if node.StepsCount > 0 {
			parts = append(parts, fmt.Sprintf("(%d steps)", node.StepsCount))
		}

		label := strings.Join(parts, " ")

		if node.IsActive {
			label += " <active>"
		}

		if node.IsTaskCheckpoint {
			label += " [task]"
		}
		return label

	case NodeTypeSession:
		// Session shown under checkpoint - display session ID and description
		displayID := node.SessionID
		if len(displayID) > 19 {
			displayID = displayID[:19]
		}
		label := "Session: " + displayID

		if node.Description != "" && node.Description != "No description" {
			desc := node.Description
			if len(desc) > 35 {
				desc = desc[:32] + "..."
			}
			label += " - " + desc
		}

		return label
	}

	return node.Label
}

// renderResult renders the action result.
func (m Model) renderResult() string {
	if m.result == nil {
		return ""
	}

	var b strings.Builder

	if m.result.Error != nil {
		b.WriteString(errorStyle.Render(fmt.Sprintf("Error: %v", m.result.Error)))
		b.WriteString("\n")
		return b.String()
	}

	if m.result.Message != "" {
		b.WriteString(m.result.Message)
		b.WriteString("\n")
	}

	if m.result.ResumeCommand != "" {
		b.WriteString("\nTo continue this session, run:\n")
		b.WriteString(fmt.Sprintf("  %s\n", m.result.ResumeCommand))
	}

	return b.String()
}

// renderFullHelp renders the full help text.
func (m Model) renderFullHelp() string {
	return helpStyle.Render(`Keybindings:
  up/k, down/j   Navigate
  left/h         Collapse / go to parent
  right/l        Expand
  enter          Toggle expand / perform action
  o              Open session logs
  r              Resume (switch branch + restore)
  w              Rewind to checkpoint
  c              Collapse all
  e              Expand all
  ?              Toggle help
  q/esc          Quit

Actions:
  Open    - Copy session logs, show resume command
  Resume  - Switch to branch and restore session
  Rewind  - Restore code state to checkpoint`)
}

// GetResult returns the action result after the model finishes.
func (m Model) GetResult() *ActionResult {
	return m.result
}
