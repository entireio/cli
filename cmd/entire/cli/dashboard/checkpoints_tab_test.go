package dashboard

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckpointsModel_Navigation(t *testing.T) {
	t.Parallel()

	t.Run("j_moves_cursor_down", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(5))

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 1, m.cursor)

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("k_moves_cursor_up", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(5))
		m.cursor = 3

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 2, m.cursor)
	})

	t.Run("cursor_stays_at_bottom_bound", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.cursor = 2

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 2, m.cursor, "cursor should not exceed last item")
	})

	t.Run("cursor_stays_at_top_bound", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.cursor = 0

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 0, m.cursor, "cursor should not go below 0")
	})

	t.Run("down_arrow_works", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))

		m, _ = m.update(keyMsg("down"))
		assert.Equal(t, 1, m.cursor)
	})

	t.Run("up_arrow_works", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.cursor = 2

		m, _ = m.update(keyMsg("up"))
		assert.Equal(t, 1, m.cursor)
	})
}

func TestCheckpointsModel_DetailToggle(t *testing.T) {
	t.Parallel()

	t.Run("enter_opens_detail", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))

		m, _ = m.update(keyMsg("enter"))
		assert.True(t, m.showDetail)
		assert.Equal(t, 0, m.scrollPos)
	})

	t.Run("enter_closes_detail", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.showDetail = true

		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.showDetail)
	})

	t.Run("esc_closes_detail", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.showDetail = true
		m.scrollPos = 5

		m, _ = m.update(keyMsg("esc"))
		assert.False(t, m.showDetail)
		assert.Equal(t, 0, m.scrollPos)
	})

	t.Run("enter_on_empty_list_no_op", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()

		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.showDetail)
	})

	t.Run("j_scrolls_detail_view", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.showDetail = true

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 1, m.scrollPos)
	})
}

func TestCheckpointsModel_FilterMode(t *testing.T) {
	t.Parallel()

	t.Run("slash_enters_filter_mode", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))

		m, _ = m.update(keyMsg("/"))
		assert.True(t, m.filtering)
	})

	t.Run("typing_in_filter_mode", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.filtering = true

		m, _ = m.update(keyMsg("a"))
		assert.Equal(t, "a", m.filter)

		m, _ = m.update(keyMsg("b"))
		assert.Equal(t, "ab", m.filter)
	})

	t.Run("enter_confirms_filter", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.filtering = true
		m.filter = "test"

		m, _ = m.update(keyMsg("enter"))
		assert.False(t, m.filtering)
		assert.Equal(t, "test", m.filter, "filter text should be preserved")
	})

	t.Run("esc_clears_filter", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.filtering = true
		m.filter = "test"

		m, _ = m.update(keyMsg("esc"))
		assert.False(t, m.filtering)
		assert.Empty(t, m.filter)
	})

	t.Run("backspace_removes_last_char", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.filtering = true
		m.filter = "abc"

		m, _ = m.update(keyMsg("backspace"))
		assert.Equal(t, "ab", m.filter)
	})

	t.Run("backspace_on_empty_filter", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.filtering = true
		m.filter = ""

		m, _ = m.update(keyMsg("backspace"))
		assert.Empty(t, m.filter)
	})
}

func TestCheckpointsModel_ApplyFilter(t *testing.T) {
	t.Parallel()

	t.Run("case_insensitive_match_on_ID", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		points := testRewindPoints(5)
		points[2].ID = "SPECIAL-ID"
		m.setCheckpoints(points)

		m.filter = "special"
		m.applyFilter()

		assert.Len(t, m.filtered, 1)
		assert.Equal(t, "SPECIAL-ID", m.filtered[0].ID)
	})

	t.Run("match_on_message", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		points := testRewindPoints(5)
		points[1].Message = "unique fix"
		m.setCheckpoints(points)

		m.filter = "unique"
		m.applyFilter()

		assert.Len(t, m.filtered, 1)
	})

	t.Run("match_on_session_prompt", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		points := testRewindPoints(5)
		points[0].SessionPrompt = "add login feature"
		m.setCheckpoints(points)

		m.filter = "login"
		m.applyFilter()

		assert.Len(t, m.filtered, 1)
	})

	t.Run("empty_filter_shows_all", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(5))

		m.filter = ""
		m.applyFilter()

		assert.Len(t, m.filtered, 5)
	})
}

func TestCheckpointsModel_FilterCursorAdjustment(t *testing.T) {
	t.Parallel()

	m := newCheckpointsModel()
	m.setCheckpoints(testRewindPoints(10))
	m.cursor = 8

	// Apply a filter that matches only 2 items
	m.filter = "prompt 0"
	m.applyFilter()

	assert.LessOrEqual(t, m.cursor, len(m.filtered)-1, "cursor should be clamped to filtered length")
}

func TestCheckpointsModel_RewindConfirmation(t *testing.T) {
	t.Parallel()

	t.Run("r_enters_confirmation", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))

		m, _ = m.update(keyMsg("r"))
		assert.True(t, m.confirming)
	})

	t.Run("y_confirms_rewind", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.confirming = true

		m, cmd := m.update(keyMsg("y"))
		assert.False(t, m.confirming)
		require.NotNil(t, cmd)

		msg := cmd()
		req, ok := msg.(rewindRequestMsg)
		require.True(t, ok)
		assert.Equal(t, m.filtered[0].ID, req.pointID)
	})

	t.Run("n_cancels_confirmation", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.confirming = true

		m, _ = m.update(keyMsg("n"))
		assert.False(t, m.confirming)
	})

	t.Run("esc_cancels_confirmation", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.confirming = true

		m, _ = m.update(keyMsg("esc"))
		assert.False(t, m.confirming)
	})
}

func TestCheckpointsModel_RewindGuards(t *testing.T) {
	t.Parallel()

	t.Run("r_blocked_in_detail_view", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(testRewindPoints(3))
		m.showDetail = true

		m, _ = m.update(keyMsg("r"))
		assert.False(t, m.confirming)
	})

	t.Run("r_blocked_on_empty_list", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()

		m, _ = m.update(keyMsg("r"))
		assert.False(t, m.confirming)
	})
}

func TestCheckpointsModel_ViewStates(t *testing.T) {
	t.Parallel()

	t.Run("error_view", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.err = errors.New("test error")

		view := m.view(80, 20)
		assert.Contains(t, view, "Error loading checkpoints")
		assert.Contains(t, view, "test error")
	})

	t.Run("empty_list", func(t *testing.T) {
		t.Parallel()
		m := newCheckpointsModel()
		m.setCheckpoints(nil)

		view := m.view(80, 20)
		assert.Contains(t, view, "No checkpoints found")
	})

	t.Run("checkpoint_type_labels", func(t *testing.T) {
		t.Parallel()

		// Task checkpoint
		m := newCheckpointsModel()
		points := testRewindPoints(1)
		points[0].IsTaskCheckpoint = true
		m.setCheckpoints(points)

		view := m.view(80, 20)
		assert.Contains(t, view, cpTypeTask)

		// Logs-only (committed) checkpoint
		m = newCheckpointsModel()
		points = testRewindPoints(1)
		points[0].IsLogsOnly = true
		m.setCheckpoints(points)

		view = m.view(80, 20)
		assert.Contains(t, view, cpTypeCommitted)
	})
}

func TestCheckpointsModel_NonKeyMsg(t *testing.T) {
	t.Parallel()

	m := newCheckpointsModel()
	m.setCheckpoints(testRewindPoints(3))

	// A non-key message should be a no-op
	updated, cmd := m.update(tea.WindowSizeMsg{Width: 80, Height: 40})
	assert.Equal(t, m.cursor, updated.cursor)
	assert.Nil(t, cmd)
}
