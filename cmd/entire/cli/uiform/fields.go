package uiform

import (
	"errors"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/palette"
)

const keyEnter = "enter"

// FieldHeight returns a compact field height containing its title,
// description, and every option without adding spare viewport rows.
func FieldHeight(optionCount int, description string) int {
	height := optionCount + 1
	if description != "" {
		height += lipgloss.Height(description)
	}
	return height
}

// QuestionTitle renders a bold question with a focus-aware marker.
func QuestionTitle(question string, focused bool) string {
	color := palette.Muted
	if focused {
		color = palette.Warning
	}
	marker := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render("?")
	heading := lipgloss.NewStyle().Bold(true).Render(question)
	return marker + " " + heading
}

// EqualValues compares slices as multisets, so UI option order does not make a
// logically unchanged checklist appear modified.
func EqualValues[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[T]int, len(a))
	for _, value := range a {
		seen[value]++
	}
	for _, value := range b {
		seen[value]--
		if seen[value] < 0 {
			return false
		}
	}
	return true
}

// Radio is a single-choice field whose row cursor is independent from its
// committed value. Arrows move the cursor, Space selects, and Enter continues.
type Radio[T comparable] struct {
	*huh.Select[T]

	value         *T
	committed     T
	highlighted   T
	options       []huh.Option[T]
	refresh       func()
	layoutChanged func()
	title         string
	sectionGap    bool
}

func NewRadio[T comparable](title, description string, options []huh.Option[T], value *T) *Radio[T] {
	field := &Radio[T]{value: value, committed: *value, highlighted: *value, title: title, sectionGap: true}
	field.Select = huh.NewSelect[T]().
		Title(QuestionTitle(title, false)).
		Description(description).
		Height(FieldHeight(len(options), description)).
		Value(value)
	return field.Options(options...)
}

// Options replaces the radio choices and applies the shared selected and
// unselected markers. Callers provide plain labels; Radio owns their styling.
func (field *Radio[T]) Options(options ...huh.Option[T]) *Radio[T] {
	field.options = append(field.options[:0], options...)
	field.renderOptions()
	return field
}

func (field *Radio[T]) renderOptions() {
	options := make([]huh.Option[T], 0, len(field.options))
	for _, option := range field.options {
		marker := lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Muted)).Render("○")
		if option.Value == field.committed {
			marker = lipgloss.NewStyle().Foreground(lipgloss.Color(palette.Success)).Render("●")
		}
		options = append(options, huh.NewOption(marker+" "+option.Key, option.Value))
	}
	field.Select.Options(options...)
}

func (field *Radio[T]) OnRefresh(fn func()) *Radio[T] {
	field.refresh = fn
	return field
}

func (field *Radio[T]) OnLayoutChanged(fn func()) *Radio[T] {
	field.layoutChanged = fn
	return field
}

func (field *Radio[T]) WithSectionGap(enabled bool) *Radio[T] {
	field.sectionGap = enabled
	return field
}

func (field *Radio[T]) Focus() tea.Cmd {
	field.Title(QuestionTitle(field.title, true))
	return field.Select.Focus()
}

func (field *Radio[T]) Blur() tea.Cmd {
	field.Title(QuestionTitle(field.title, false))
	return field.Select.Blur()
}

func (field *Radio[T]) KeyBinds() []key.Binding {
	bindings := field.Select.KeyBinds()
	for i := range bindings {
		help := bindings[i].Help()
		if help.Key == keyEnter && help.Desc == "select" {
			bindings[i].SetHelp(keyEnter, "continue")
		}
	}
	return append(bindings, key.NewBinding(key.WithKeys("space"), key.WithHelp("space", "select")))
}

func (field *Radio[T]) View() string {
	view := field.Select.View()
	if field.sectionGap {
		view += "\n\n"
	}
	return view
}

func (field *Radio[T]) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyPressMsg); ok {
		switch keyMsg.String() {
		case "space", " ":
			field.commitHighlighted()
			return field, nil
		case keyEnter, "tab":
			return field, huh.NextField
		}
	}
	model, cmd := field.Select.Update(msg)
	if updated, ok := model.(*huh.Select[T]); ok {
		field.Select = updated
	}
	_, movedCursor := msg.(tea.KeyPressMsg)
	field.preserveCommittedSelection(movedCursor)
	return field, cmd
}

func (field *Radio[T]) preserveCommittedSelection(movedCursor bool) {
	if field.value == nil {
		return
	}
	if movedCursor {
		field.highlighted = *field.value
	}
	*field.value = field.committed
}

func (field *Radio[T]) commitHighlighted() {
	if field.value == nil {
		return
	}
	field.committed = field.highlighted
	*field.value = field.committed
	if field.refresh != nil {
		field.refresh()
	}
	field.renderOptions()
	if field.layoutChanged != nil {
		field.layoutChanged()
	}
}

// Checklist is a checkbox field with shared focus rendering and change hooks.
type Checklist[T comparable] struct {
	*huh.MultiSelect[T]

	value            *[]T
	selectionChanged func()
	showSectionGap   func() bool
	title            string
}

func NewChecklist[T comparable](title, description string, options []huh.Option[T], selected *[]T, requireOne bool) *Checklist[T] {
	field := &Checklist[T]{value: selected, title: title}
	multi := huh.NewMultiSelect[T]().
		Title(QuestionTitle(title, false)).
		Description(description).
		Options(options...).
		Height(FieldHeight(len(options), description)).
		Value(selected)
	if requireOne {
		multi = multi.Validate(func(values []T) error {
			if len(values) == 0 {
				return errors.New("select at least one option")
			}
			return nil
		})
	}
	field.MultiSelect = multi
	return field
}

func (field *Checklist[T]) OnSelectionChanged(fn func()) *Checklist[T] {
	field.selectionChanged = fn
	return field
}

func (field *Checklist[T]) ShowSectionGapWhen(fn func() bool) *Checklist[T] {
	field.showSectionGap = fn
	return field
}

func (field *Checklist[T]) Focus() tea.Cmd {
	field.Title(QuestionTitle(field.title, true))
	return field.MultiSelect.Focus()
}

func (field *Checklist[T]) Blur() tea.Cmd {
	field.Title(QuestionTitle(field.title, false))
	return field.MultiSelect.Blur()
}

func (field *Checklist[T]) View() string {
	view := field.MultiSelect.View()
	if field.showSectionGap != nil && field.showSectionGap() {
		view += "\n\n"
	}
	return view
}

func (field *Checklist[T]) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	var before []T
	if field.value != nil {
		before = append(before, (*field.value)...)
	}
	model, cmd := field.MultiSelect.Update(msg)
	if updated, ok := model.(*huh.MultiSelect[T]); ok {
		field.MultiSelect = updated
	}
	if _, keyPress := msg.(tea.KeyPressMsg); keyPress && field.value != nil && !EqualValues(before, *field.value) {
		if field.selectionChanged != nil {
			field.selectionChanged()
		}
	}
	return field, cmd
}

// ActionSelect is a plain action list with the same focus behavior as the
// radio and checklist primitives.
type ActionSelect[T comparable] struct {
	*huh.Select[T]

	title string
}

func NewActionSelect[T comparable](title, description string, options []huh.Option[T], value *T) *ActionSelect[T] {
	field := &ActionSelect[T]{title: title}
	field.Select = huh.NewSelect[T]().
		Title(QuestionTitle(title, false)).
		Description(description).
		Options(options...).
		Height(FieldHeight(len(options), description)).
		Value(value)
	return field
}

func (field *ActionSelect[T]) Focus() tea.Cmd {
	field.Title(QuestionTitle(field.title, true))
	return field.Select.Focus()
}

func (field *ActionSelect[T]) Blur() tea.Cmd {
	field.Title(QuestionTitle(field.title, false))
	return field.Select.Blur()
}

func (field *ActionSelect[T]) Update(msg tea.Msg) (huh.Model, tea.Cmd) {
	model, cmd := field.Select.Update(msg)
	if updated, ok := model.(*huh.Select[T]); ok {
		field.Select = updated
	}
	return field, cmd
}
