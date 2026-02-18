package dashboard

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func TestSessionsModel_Navigation(t *testing.T) {
	t.Parallel()

	t.Run("j_moves_cursor_down", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(5))

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 1, m.cursor)

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("k_moves_cursor_up", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(5))
		m.cursor = 3

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("cursor_stays_at_bottom_bound", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.cursor = 2

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("cursor_stays_at_top_bound", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.cursor = 0

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 0, m.cursor)
	})
}

func TestSessionsModel_DetailToggle(t *testing.T) {
	t.Parallel()

	t.Run("enter_opens_detail", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))

		m, _ = m.update(keyMsg("enter"))
		assert.True(t, m.showDetail)
		assert.Equal(t, 0, m.scrollPos)
	})

	t.Run("esc_closes_detail", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.showDetail = true
		m.scrollPos = 5

		m, _ = m.update(keyMsg("esc"))
		assert.False(t, m.showDetail)
		assert.Equal(t, 0, m.scrollPos)
	})

	t.Run("enter_on_empty_list", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()

		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.showDetail)
	})

	t.Run("scrollPos_resets_on_toggle", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.showDetail = true
		m.scrollPos = 10

		// Close detail
		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.showDetail)
		assert.Equal(t, 0, m.scrollPos)
	})
}

func TestSessionsModel_FilterMode(t *testing.T) {
	t.Parallel()

	t.Run("slash_enters_filter", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))

		m, _ = m.update(keyMsg("/"))
		assert.True(t, m.filtering)
	})

	t.Run("type_filter_text", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(5))
		m.filtering = true

		m, _ = m.update(keyMsg("t"))
		m, _ = m.update(keyMsg("e"))
		assert.Equal(t, "te", m.filter)
	})

	t.Run("enter_confirms_filter", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.filtering = true
		m.filter = "xyz"

		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.filtering)
		assert.Equal(t, "xyz", m.filter)
	})

	t.Run("esc_clears_filter", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.filtering = true
		m.filter = "xyz"

		m, _ = m.update(keyMsg("esc"))
		assert.False(t, m.filtering)
		assert.Empty(t, m.filter)
	})

	t.Run("backspace_removes_char", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(testSessions(3))
		m.filtering = true
		m.filter = "abc"

		m, _ = m.update(keyMsg("backspace"))
		assert.Equal(t, "ab", m.filter)
	})

	t.Run("filter_matches_description", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		sessions := testSessions(5)
		sessions[2].Description = "unique description"
		m.setSessions(sessions)

		m.filter = "unique"
		m.applyFilter()
		assert.Len(t, m.filtered, 1)
	})
}

func TestSessionsModel_ViewStates(t *testing.T) {
	t.Parallel()

	t.Run("error_view", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.err = errors.New("load failed")

		view := m.view(80, 20)
		assert.Contains(t, view, "Error loading sessions")
		assert.Contains(t, view, "load failed")
	})

	t.Run("empty_list", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		m.setSessions(nil)

		view := m.view(80, 20)
		assert.Contains(t, view, "No sessions found")
	})

	t.Run("no_description", func(t *testing.T) {
		t.Parallel()
		m := newSessionsModel()
		sessions := testSessions(1)
		sessions[0].Description = strategy.NoDescription
		m.setSessions(sessions)

		view := m.view(80, 20)
		assert.Contains(t, view, "(no description)")
	})
}

func TestSessionsModel_NonKeyMsg(t *testing.T) {
	t.Parallel()

	m := newSessionsModel()
	m.setSessions(testSessions(3))

	updated, cmd := m.update(tea.WindowSizeMsg{Width: 80, Height: 40})
	assert.Equal(t, m.cursor, updated.cursor)
	assert.Nil(t, cmd)
}
