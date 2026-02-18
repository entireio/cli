package dashboard

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func TestActiveModel_SetSessionsFiltersEnded(t *testing.T) {
	t.Parallel()

	m := newActiveModel()
	states := testSessionStates(5, true) // last one is ended

	m.setSessions(states)

	// Should have 4 active sessions (5 total - 1 ended)
	assert.Len(t, m.states, 4)
}

func TestActiveModel_SetSessionsAllActive(t *testing.T) {
	t.Parallel()

	m := newActiveModel()
	states := testSessionStates(3, false) // none ended

	m.setSessions(states)
	assert.Len(t, m.states, 3)
}

func TestActiveModel_Navigation(t *testing.T) {
	t.Parallel()

	t.Run("j_moves_cursor_down", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.setSessions(testSessionStates(5, false))

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 1, m.cursor)

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("k_moves_cursor_up", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.setSessions(testSessionStates(5, false))
		m.cursor = 3

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("cursor_stays_at_bounds", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.setSessions(testSessionStates(3, false))

		// At bottom
		m.cursor = 2
		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 2, m.cursor)

		// At top
		m.cursor = 0
		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 0, m.cursor)
	})
}

func TestActiveModel_DetailToggle(t *testing.T) {
	t.Parallel()

	t.Run("enter_opens_detail", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.setSessions(testSessionStates(3, false))

		m, _ = m.update(keyMsg("enter"))
		assert.True(t, m.showDetail)
	})

	t.Run("esc_closes_detail", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.setSessions(testSessionStates(3, false))
		m.showDetail = true
		m.scrollPos = 5

		m, _ = m.update(keyMsg("esc"))
		assert.False(t, m.showDetail)
		assert.Equal(t, 0, m.scrollPos)
	})

	t.Run("enter_on_empty_list", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()

		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.showDetail)
	})
}

func TestActiveModel_ViewStates(t *testing.T) {
	t.Parallel()

	t.Run("error_view", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.err = errors.New("state error")

		view := m.view(80, 20)
		assert.Contains(t, view, "Error loading active sessions")
		assert.Contains(t, view, "state error")
	})

	t.Run("empty_view", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()

		view := m.view(80, 20)
		assert.Contains(t, view, "No active sessions")
	})

	t.Run("list_view", func(t *testing.T) {
		t.Parallel()
		m := newActiveModel()
		m.setSessions(testSessionStates(2, false))

		view := m.view(80, 20)
		assert.Contains(t, view, "Active Sessions")
		assert.Contains(t, view, "(2)")
	})
}

func TestActiveModel_NonKeyMsg(t *testing.T) {
	t.Parallel()

	m := newActiveModel()
	m.setSessions(testSessionStates(3, false))

	updated, cmd := m.update(tea.WindowSizeMsg{Width: 80, Height: 40})
	assert.Equal(t, m.cursor, updated.cursor)
	assert.Nil(t, cmd)
}
