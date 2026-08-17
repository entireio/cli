package uiform

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

func TestRadioArrowsMoveWithoutCommitting(t *testing.T) {
	selected := "eu"
	field := NewRadio("Region", "", []huh.Option[string]{
		huh.NewOption("US", "us"),
		huh.NewOption("EU", "eu"),
	}, &selected)
	field.WithKeyMap(huh.NewDefaultKeyMap())
	field.Focus()
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd != nil {
		t.Fatal("down arrow emitted a field transition")
	}
	if selected != "eu" {
		t.Fatalf("down arrow committed %q, want eu", selected)
	}
	field.Update(tea.KeyPressMsg{Code: ' '})
	if selected != "us" {
		t.Fatalf("space committed %q, want highlighted us", selected)
	}
}

func TestRadioEnterAdvances(t *testing.T) {
	selected := "us"
	field := NewRadio("Region", "", []huh.Option[string]{huh.NewOption("US", "us")}, &selected)
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not advance")
	}
}

func TestChecklistReportsSelectionChanges(t *testing.T) {
	selected := []string{"claude-code"}
	changed := false
	field := NewChecklist("Agents", "", []huh.Option[string]{
		huh.NewOption("Claude Code", "claude-code"),
		huh.NewOption("Codex", "codex"),
	}, &selected, false).OnSelectionChanged(func() { changed = true })
	field.WithKeyMap(huh.NewDefaultKeyMap())
	field.Focus()
	_, cmd := field.Update(tea.KeyPressMsg{Code: ' '})
	if cmd != nil {
		t.Fatal("checklist toggle scheduled an extra render command")
	}
	if !changed {
		t.Fatal("checklist did not report a changed selection")
	}
}

func TestActionSelectEnterSubmits(t *testing.T) {
	choice := "save"
	field := NewActionSelect("Save", "", []huh.Option[string]{huh.NewOption("Save", "save")}, &choice)
	field.WithKeyMap(huh.NewDefaultKeyMap())
	field.WithPosition(huh.FieldPosition{Field: 0, FirstField: 0, LastField: 0, LastGroup: 0})
	field.Focus()
	_, cmd := field.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter did not submit action")
	}
}

func TestQuestionTitleChangesWithFocus(t *testing.T) {
	if QuestionTitle("Agents", true) == QuestionTitle("Agents", false) {
		t.Fatal("focused and blurred question titles are identical")
	}
}

func TestFocusDoesNotScheduleAnExtraRender(t *testing.T) {
	choice := "save"
	field := NewActionSelect("Save", "", []huh.Option[string]{huh.NewOption("Save", choice)}, &choice)
	if cmd := field.Focus(); cmd != nil {
		t.Fatal("focus scheduled an extra render")
	}
}
