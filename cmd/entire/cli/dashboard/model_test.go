package dashboard

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestNewModel_DefaultState(t *testing.T) {
	t.Parallel()

	m := newModel()

	assert.Equal(t, tabSessions, m.activeTab)
	assert.False(t, m.showHelp)
	assert.False(t, m.quitting)
	assert.Nil(t, m.rewindReq)
	require.NoError(t, m.err)
	assert.NotNil(t, m.dataLoaded)
	assert.Empty(t, m.dataLoaded)
}

func TestModel_TabSwitching(t *testing.T) {
	t.Parallel()

	m := newModel()

	// tab forward: Sessions -> Checkpoints
	m, _ = updateModel(t, m, keyMsg("tab"))
	assert.Equal(t, tabCheckpoints, m.activeTab)

	// tab forward: Checkpoints -> Active
	m, _ = updateModel(t, m, keyMsg("tab"))
	assert.Equal(t, tabActive, m.activeTab)

	// tab forward: Active -> Settings
	m, _ = updateModel(t, m, keyMsg("tab"))
	assert.Equal(t, tabSettings, m.activeTab)

	// tab forward wraps: Settings -> Sessions
	m, _ = updateModel(t, m, keyMsg("tab"))
	assert.Equal(t, tabSessions, m.activeTab)

	// shift+tab backward wraps: Sessions -> Settings
	m, _ = updateModel(t, m, keyMsg("shift+tab"))
	assert.Equal(t, tabSettings, m.activeTab)

	// shift+tab backward: Settings -> Active
	m, _ = updateModel(t, m, keyMsg("shift+tab"))
	assert.Equal(t, tabActive, m.activeTab)
}

func TestModel_QuitKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{name: "q", key: "q"},
		{name: "ctrl+c", key: "ctrl+c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newModel()
			m, cmd := updateModel(t, m, keyMsg(tt.key))

			assert.True(t, m.quitting)
			require.NotNil(t, cmd)
			// tea.Quit returns a tea.Msg; calling the Cmd should produce a quit message
			msg := cmd()
			_, isQuit := msg.(tea.QuitMsg)
			assert.True(t, isQuit, "expected tea.QuitMsg from quit command")
		})
	}
}

func TestModel_ToggleHelp(t *testing.T) {
	t.Parallel()

	m := newModel()

	assert.False(t, m.showHelp)

	m, _ = updateModel(t, m, keyMsg("?"))
	assert.True(t, m.showHelp)

	m, _ = updateModel(t, m, keyMsg("?"))
	assert.False(t, m.showHelp)
}

func TestModel_DataMessages(t *testing.T) {
	t.Parallel()

	t.Run("sessionsMsg", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		sessions := testSessions(3)

		m, _ = updateModel(t, m, sessionsMsg{sessions: sessions})

		assert.True(t, m.dataLoaded[tabSessions])
		assert.Len(t, m.sessions.filtered, 3)
	})

	t.Run("checkpointsMsg", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		points := testRewindPoints(5)

		m, _ = updateModel(t, m, checkpointsMsg{points: points})

		assert.True(t, m.dataLoaded[tabCheckpoints])
		assert.Len(t, m.checkpoints.filtered, 5)
	})

	t.Run("activeSessionsMsg", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		states := testSessionStates(2, false)

		m, _ = updateModel(t, m, activeSessionsMsg{states: states})

		assert.True(t, m.dataLoaded[tabActive])
		assert.Len(t, m.active.states, 2)
	})

	t.Run("settingsDataMsg", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		s := &settings.EntireSettings{Enabled: true, Strategy: "manual-commit"}

		m, _ = updateModel(t, m, settingsDataMsg{settings: s})

		assert.True(t, m.dataLoaded[tabSettings])
		assert.NotNil(t, m.settings.data)
	})
}

func TestModel_DataMessagesWithError(t *testing.T) {
	t.Parallel()

	testErr := errors.New("load failed")

	t.Run("sessionsMsg_error", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m, _ = updateModel(t, m, sessionsMsg{err: testErr})
		assert.Equal(t, testErr, m.sessions.err)
	})

	t.Run("checkpointsMsg_error", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m, _ = updateModel(t, m, checkpointsMsg{err: testErr})
		assert.Equal(t, testErr, m.checkpoints.err)
	})

	t.Run("activeSessionsMsg_error", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m, _ = updateModel(t, m, activeSessionsMsg{err: testErr})
		assert.Equal(t, testErr, m.active.err)
	})

	t.Run("settingsDataMsg_error", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m, _ = updateModel(t, m, settingsDataMsg{err: testErr})
		assert.Equal(t, testErr, m.settings.err)
	})
}

func TestModel_RewindRequestMsg(t *testing.T) {
	t.Parallel()

	m := newModel()
	m, cmd := updateModel(t, m, rewindRequestMsg{pointID: "abc123"})

	require.NotNil(t, m.rewindReq)
	assert.Equal(t, "abc123", m.rewindReq.PointID)
	assert.True(t, m.quitting)
	require.NotNil(t, cmd)
}

func TestModel_ViewWhileQuitting(t *testing.T) {
	t.Parallel()

	m := newModel()
	m.quitting = true

	assert.Empty(t, m.View())
}

func TestModel_WindowSizeMsg(t *testing.T) {
	t.Parallel()

	m := newModel()
	m, _ = updateModel(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})

	assert.Equal(t, 120, m.width)
	assert.Equal(t, 40, m.height)
}

func TestModel_IsTabCapturingInput(t *testing.T) {
	t.Parallel()

	t.Run("sessions_filtering_blocks_tab", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.activeTab = tabSessions
		m.sessions.filtering = true

		assert.True(t, m.isTabCapturingInput())
	})

	t.Run("checkpoints_filtering_blocks_tab", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.activeTab = tabCheckpoints
		m.checkpoints.filtering = true

		assert.True(t, m.isTabCapturingInput())
	})

	t.Run("checkpoints_confirming_blocks_tab", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.activeTab = tabCheckpoints
		m.checkpoints.confirming = true

		assert.True(t, m.isTabCapturingInput())
	})

	t.Run("active_tab_does_not_capture", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.activeTab = tabActive

		assert.False(t, m.isTabCapturingInput())
	})
}

func TestModel_ContentHeight(t *testing.T) {
	t.Parallel()

	t.Run("normal", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.height = 30
		assert.Equal(t, 27, m.contentHeight()) // 30 - 3 overhead
	})

	t.Run("with_help", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.height = 30
		m.showHelp = true
		assert.Equal(t, 20, m.contentHeight()) // 30 - 10 overhead
	})

	t.Run("minimum", func(t *testing.T) {
		t.Parallel()
		m := newModel()
		m.height = 2
		assert.Equal(t, 5, m.contentHeight()) // minimum is 5
	})
}
