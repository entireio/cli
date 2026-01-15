package list

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
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

	// Brighter style for checkpoint messages (SHA + message before the em dash)
	checkpointMessageStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("252")) // Bright gray, close to white

	mergedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			Italic(true)

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginTop(1)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196"))

	// Additional styles for checkpoint/session details
	diffAddStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("42")) // Green for additions

	diffDelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("196")) // Red for deletions

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("244")) // Dim gray for metadata

	agentBadgeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("99")).
			Bold(true) // Purple for agent badge

	// Key hint styles
	keyAvailableStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("255")) // Bright white for available actions

	keyUnavailableStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("241")) // Dim for unavailable actions
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

// PendingAction represents an action awaiting confirmation.
type PendingAction struct {
	Action      Action
	Node        *Node
	Title       string
	Description string
}

// GetPendingAction returns the pending action that needs confirmation.
func (m Model) GetPendingAction() *PendingAction {
	return m.pendingAction
}

// Heights for fixed UI elements
const (
	titleHeight   = 3 // Title + blank line
	helpBarHeight = 1 // Help bar at bottom
	footerPadding = 1 // Extra padding
	minListHeight = 5 // Minimum visible list items
)

// Labels for file counts
const (
	fileSingular = "file"
	filePlural   = "files"
)

// Model is the bubbletea model for the list view.
//
//nolint:recvcheck // Mixed receivers required by bubbletea's interface pattern
type Model struct {
	// Tree data
	tree     []*Node
	flatList []*Node
	cursor   int

	// Viewport for scrolling
	viewport viewport.Model
	ready    bool

	// State
	width      int
	height     int
	showHelp   bool
	quitting   bool
	actionDone bool

	// Confirmation dialog
	pendingAction *PendingAction

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
	// Update viewport content
	m.updateViewportContent()
}

// updateViewportContent updates the viewport with current tree content.
func (m *Model) updateViewportContent() {
	if !m.ready {
		return
	}
	var lines []string
	for i, node := range m.flatList {
		lines = append(lines, m.renderNode(node, i == m.cursor))
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
}

// ensureCursorVisible scrolls the viewport to keep the cursor visible.
func (m *Model) ensureCursorVisible() {
	if !m.ready {
		return
	}
	// Calculate visible range
	viewportHeight := m.viewport.Height
	yOffset := m.viewport.YOffset

	// If cursor is above visible area, scroll up
	if m.cursor < yOffset {
		m.viewport.SetYOffset(m.cursor)
	}
	// If cursor is below visible area, scroll down
	if m.cursor >= yOffset+viewportHeight {
		m.viewport.SetYOffset(m.cursor - viewportHeight + 1)
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

		// If confirmation dialog is showing, handle it
		if m.pendingAction != nil {
			return m.handleConfirmation(msg)
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
					if m.performDefaultAction() {
						return m, tea.Quit
					}
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
				m.pendingAction = m.buildResumeConfirmation(node)
			}

		case key.Matches(msg, keys.Rewind):
			if len(m.flatList) > 0 {
				node := m.flatList[m.cursor]
				// For checkpoints, rewind directly
				// For sessions, rewind to their parent checkpoint
				// For branches, do nothing (no checkpoint to rewind to)
				switch node.Type {
				case NodeTypeCheckpoint:
					m.pendingAction = m.buildRewindConfirmation(node)
				case NodeTypeSession:
					if node.Parent != nil && node.Parent.Type == NodeTypeCheckpoint {
						m.pendingAction = m.buildRewindConfirmation(node.Parent)
					}
				case NodeTypeBranch:
					// No action for branches
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

		// Calculate viewport height (total height minus fixed elements)
		viewportHeight := msg.Height - titleHeight - helpBarHeight - footerPadding
		if viewportHeight < minListHeight {
			viewportHeight = minListHeight
		}

		if !m.ready {
			// First time initialization
			m.viewport = viewport.New(msg.Width, viewportHeight)
			m.viewport.YPosition = titleHeight
			m.ready = true
			m.updateViewportContent()
		} else {
			// Resize existing viewport
			m.viewport.Width = msg.Width
			m.viewport.Height = viewportHeight
		}
	}

	// After cursor movement, ensure it's visible and update content
	m.updateViewportContent()
	m.ensureCursorVisible()

	return m, nil
}

// performDefaultAction performs the default action for the current node.
// Returns true if the TUI should quit immediately.
func (m *Model) performDefaultAction() bool {
	if len(m.flatList) == 0 {
		return false
	}

	node := m.flatList[m.cursor]
	switch node.Type {
	case NodeTypeBranch:
		// For branches, show resume confirmation
		m.pendingAction = m.buildResumeConfirmation(node)
		return false // Stay in TUI to show confirmation
	case NodeTypeSession:
		// For sessions, open (no confirmation needed)
		m.result = PerformOpen(node)
		m.actionDone = true
		return true
	case NodeTypeCheckpoint:
		// For checkpoints, show rewind confirmation
		m.pendingAction = m.buildRewindConfirmation(node)
		return false // Stay in TUI to show confirmation
	}
	return false
}

// handleConfirmation handles key presses when a confirmation dialog is shown.
//
//nolint:ireturn // Required by tea.Model interface
func (m Model) handleConfirmation(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		// Execute the confirmed action
		switch m.pendingAction.Action {
		case ActionResume:
			m.result = PerformResume(m.pendingAction.Node)
		case ActionRewind:
			m.result = PerformRewind(m.pendingAction.Node)
		case ActionOpen:
			m.result = PerformOpen(m.pendingAction.Node)
		}
		m.pendingAction = nil
		m.actionDone = true
		return m, tea.Quit
	case "n", "N", "esc", "q":
		// Cancel - go back to list
		m.pendingAction = nil
	}
	return m, nil
}

// buildResumeConfirmation creates a confirmation dialog for the resume action.
func (m Model) buildResumeConfirmation(node *Node) *PendingAction {
	var title, desc string

	switch node.Type {
	case NodeTypeBranch:
		title = "Resume session on this branch?"
		desc = fmt.Sprintf("Session logs will be restored for branch '%s'.", node.ID)
	case NodeTypeCheckpoint:
		shortHash := node.CommitHash
		if len(shortHash) > 7 {
			shortHash = shortHash[:7]
		}
		title = fmt.Sprintf("Resume session from %s?", shortHash)
		desc = "Session logs will be restored."
	case NodeTypeSession:
		title = "Resume this session?"
		desc = "Session logs will be restored."
	}

	return &PendingAction{
		Action:      ActionResume,
		Node:        node,
		Title:       title,
		Description: desc,
	}
}

// buildRewindConfirmation creates a confirmation dialog for the rewind action.
func (m Model) buildRewindConfirmation(node *Node) *PendingAction {
	shortHash := node.CommitHash
	if len(shortHash) > 7 {
		shortHash = shortHash[:7]
	}

	// Match messaging from `entire rewind`
	title := fmt.Sprintf("Reset to %s?", shortHash)
	desc := fmt.Sprintf("This will reset to: %s\nChanges after this point may be lost!", node.CommitMsg)

	return &PendingAction{
		Action:      ActionRewind,
		Node:        node,
		Title:       title,
		Description: desc,
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

	// Tree view using viewport for scrolling
	switch {
	case len(m.flatList) == 0:
		b.WriteString("No checkpoints found.\n")
	case m.ready:
		b.WriteString(m.viewport.View())
	default:
		// Fallback while viewport not ready
		for i, node := range m.flatList {
			line := m.renderNode(node, i == m.cursor)
			b.WriteString(line)
			b.WriteString("\n")
		}
	}

	// Pad to push footer to bottom
	if m.ready && m.height > 0 {
		contentHeight := titleHeight + m.viewport.Height
		remainingHeight := m.height - contentHeight - helpBarHeight
		if remainingHeight > 0 {
			b.WriteString(strings.Repeat("\n", remainingHeight))
		}
	}

	// Confirmation dialog or help (pinned to bottom)
	b.WriteString("\n")
	switch {
	case m.pendingAction != nil:
		b.WriteString(m.renderConfirmation())
	case m.showHelp:
		b.WriteString(m.renderFullHelp())
	default:
		b.WriteString(m.renderHelpBar())
	}

	return b.String()
}

// renderConfirmation renders the confirmation dialog.
func (m Model) renderConfirmation() string {
	var b strings.Builder

	// Title in bold
	titleText := selectedStyle.Render(m.pendingAction.Title)
	b.WriteString(titleText)
	b.WriteString("\n")

	// Description
	b.WriteString(m.pendingAction.Description)
	b.WriteString("\n\n")

	// Options
	b.WriteString(helpStyle.Render("[y/enter] Confirm  [n/esc] Cancel"))

	return b.String()
}

// renderHelpBar renders the help bar with available actions highlighted.
func (m Model) renderHelpBar() string {
	// Determine which actions are available for the current node
	canOpen := false
	canResume := false
	canRewind := false

	if len(m.flatList) > 0 && m.cursor < len(m.flatList) {
		node := m.flatList[m.cursor]
		switch node.Type {
		case NodeTypeBranch:
			canResume = true
		case NodeTypeCheckpoint:
			canOpen = true
			canResume = true
			canRewind = true
		case NodeTypeSession:
			canOpen = true
			canResume = true
			// Can rewind if parent is a checkpoint
			canRewind = node.Parent != nil && node.Parent.Type == NodeTypeCheckpoint
		}
	}

	// Helper to style a key hint
	styleKey := func(hint string, available bool) string {
		if available {
			return keyAvailableStyle.Render(hint)
		}
		return keyUnavailableStyle.Render(hint)
	}

	parts := []string{
		styleKey("[o]pen", canOpen),
		styleKey("[r]esume", canResume),
		styleKey("[w]ind", canRewind),
		keyAvailableStyle.Render("[enter]toggle"),
		keyAvailableStyle.Render("[?]help"),
		keyAvailableStyle.Render("[q]uit"),
	}

	return strings.Join(parts, "  ")
}

// renderNode renders a single node in the tree.
func (m Model) renderNode(node *Node, selected bool) string {
	depth := GetNodeDepth(node)
	indent := strings.Repeat(" ", depth) // 1 space per level for tighter layout

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

	// Node icon based on type (may be styled separately)
	var icon, styledIcon string
	switch node.Type {
	case NodeTypeBranch:
		icon = ""
		styledIcon = ""
	case NodeTypeSession:
		if node.IsActive {
			icon = "* "
			styledIcon = activeSessionStyle.Render("*") + " " // Only the * is green
		} else {
			icon = "o "
			styledIcon = sessionStyle.Render("o ") // Dim
		}
	case NodeTypeCheckpoint:
		if node.IsTaskCheckpoint {
			icon = "t "
		} else {
			icon = "- "
		}
		styledIcon = icon
	}

	// Selection prefix
	selPrefix := "  "
	if selected {
		selPrefix = selectedStyle.Render("> ")
	}

	// For checkpoints, use two-column layout with right-aligned metadata
	if node.Type == NodeTypeCheckpoint {
		return m.renderCheckpointNode(node, selPrefix, indent, expander, icon)
	}

	// Build label for non-checkpoint nodes
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
		// Sessions always use sessionStyle - the active indicator is in the icon
		style = sessionStyle
	case NodeTypeCheckpoint:
		// Checkpoints are handled by renderCheckpointNode above
		style = checkpointStyle
	}

	return selPrefix + indent + expander + styledIcon + style.Render(label)
}

// renderCheckpointNode renders a checkpoint with metadata following the message.
func (m Model) renderCheckpointNode(node *Node, selPrefix, indent, expander, icon string) string {
	// Build left-side content (SHA + message)
	var leftParts []string
	if node.IsUncommitted {
		leftParts = append(leftParts, "       ") // 7 spaces for alignment
	} else if node.CommitHash != "" {
		leftParts = append(leftParts, node.CommitHash)
	}

	// Add commit message (sanitize newlines to prevent UI issues)
	if node.CommitMsg != "" {
		msg := strings.ReplaceAll(node.CommitMsg, "\n", " ")
		msg = strings.ReplaceAll(msg, "\r", "")
		// Collapse multiple spaces that might result from newline replacement
		for strings.Contains(msg, "  ") {
			msg = strings.ReplaceAll(msg, "  ", " ")
		}
		leftParts = append(leftParts, strings.TrimSpace(msg))
	}

	leftText := strings.Join(leftParts, " ")

	// Build right-side metadata
	var rightParts []string

	// Time ago (no parentheses - em dash is separator enough)
	if !node.Timestamp.IsZero() {
		rightParts = append(rightParts, formatTimeAgo(node.Timestamp))
	}

	// Diff stats combined with file count: "[+167 -54 (16 files)]"
	if node.Insertions > 0 || node.Deletions > 0 || node.FileCount > 0 {
		var statParts []string
		if node.Insertions > 0 {
			statParts = append(statParts, fmt.Sprintf("+%d", node.Insertions))
		}
		if node.Deletions > 0 {
			statParts = append(statParts, fmt.Sprintf("-%d", node.Deletions))
		}
		stat := strings.Join(statParts, " ")
		if node.FileCount > 0 {
			fileLabel := fileSingular
			if node.FileCount > 1 {
				fileLabel = filePlural
			}
			stat += fmt.Sprintf(" (%d %s)", node.FileCount, fileLabel)
		}
		rightParts = append(rightParts, "["+stat+"]")
	}

	// Task indicator
	if node.IsTaskCheckpoint {
		rightParts = append(rightParts, "[task]")
	}

	// Style the parts - use brighter style for the message (before em dash)
	styledLeft := checkpointMessageStyle.Render(leftText)

	// Style right metadata with colors, preceded by em dash separator
	var styledRight string
	if len(rightParts) > 0 {
		separator := dimStyle.Render(" — ")
		styledRight = separator + m.styleRightMetadata(rightParts, node)
	}

	return selPrefix + indent + expander + icon + styledLeft + styledRight
}

// styleRightMetadata applies colors to the right-side metadata.
func (m Model) styleRightMetadata(parts []string, node *Node) string {
	var styledParts []string
	for _, part := range parts {
		switch {
		case strings.HasPrefix(part, "[") && strings.Contains(part, "+"):
			// This is the bracketed stats part: [+159 -67 (1 file)]
			styledParts = append(styledParts, m.styleStats(node))
		default:
			// Time ago, [task], and other metadata - dim
			styledParts = append(styledParts, dimStyle.Render(part))
		}
	}
	return strings.Join(styledParts, " ")
}

// styleStats applies colors to the diff stats string with brackets.
func (m Model) styleStats(node *Node) string {
	var parts []string
	if node.Insertions > 0 {
		parts = append(parts, diffAddStyle.Render(fmt.Sprintf("+%d", node.Insertions)))
	}
	if node.Deletions > 0 {
		parts = append(parts, diffDelStyle.Render(fmt.Sprintf("-%d", node.Deletions)))
	}
	result := strings.Join(parts, " ")
	if node.FileCount > 0 {
		fileLabel := fileSingular
		if node.FileCount > 1 {
			fileLabel = filePlural
		}
		result += dimStyle.Render(fmt.Sprintf(" (%d %s)", node.FileCount, fileLabel))
	}
	return dimStyle.Render("[") + result + dimStyle.Render("]")
}

// formatNodeLabel formats the label for a node (branches and sessions only).
// Checkpoints are handled separately in renderCheckpointNode for two-column layout.
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
		// Checkpoints are rendered by renderCheckpointNode, but we need this case
		// for exhaustive switch. Return the commit message as fallback.
		return node.CommitMsg

	case NodeTypeSession:
		// Format: prompt [agent badge] [N steps]
		var parts []string

		// Prompt/description (main content, no truncation, sanitize newlines)
		if node.Description != "" && node.Description != "No description" {
			desc := strings.ReplaceAll(node.Description, "\n", " ")
			desc = strings.ReplaceAll(desc, "\r", "")
			for strings.Contains(desc, "  ") {
				desc = strings.ReplaceAll(desc, "  ", " ")
			}
			parts = append(parts, strings.TrimSpace(desc))
		} else {
			// Fallback to session ID if no description (ASCII, so byte truncation is safe)
			displayID := node.SessionID
			if len(displayID) > 19 {
				displayID = displayID[:19]
			}
			parts = append(parts, displayID)
		}

		// Agent badge
		if node.Agent != "" {
			parts = append(parts, agentBadgeStyle.Render("["+node.Agent+"]"))
		}

		// Step count
		if node.SessionStep > 0 {
			parts = append(parts, dimStyle.Render(fmt.Sprintf("(%d steps)", node.SessionStep)))
		}

		return strings.Join(parts, " ")
	}

	return node.Label
}

// formatTimeAgo formats a time as a human-readable "time ago" string.
func formatTimeAgo(t time.Time) string {
	now := time.Now()
	diff := now.Sub(t)

	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		mins := int(diff.Minutes())
		if mins == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", mins)
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case diff < 7*24*time.Hour:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	case diff < 30*24*time.Hour:
		weeks := int(diff.Hours() / (24 * 7))
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	default:
		months := int(diff.Hours() / (24 * 30))
		if months == 1 {
			return "1 month ago"
		}
		return fmt.Sprintf("%d months ago", months)
	}
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
