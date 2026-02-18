package dashboard

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

func TestSettingsModel_SetData(t *testing.T) {
	t.Parallel()

	m := newSettingsModel()
	m.setData(settingsDataMsg{
		settings: &settings.EntireSettings{
			Enabled:  true,
			Strategy: "manual-commit",
		},
	})

	assert.NotNil(t, m.data)
	assert.NotEmpty(t, m.lines)
}

func TestSettingsModel_Scroll(t *testing.T) {
	t.Parallel()

	t.Run("j_scrolls_down", func(t *testing.T) {
		t.Parallel()
		m := newSettingsModel()
		m.height = 5
		// Build some lines to scroll through
		m.setData(settingsDataMsg{
			settings: &settings.EntireSettings{
				Enabled:  true,
				Strategy: "manual-commit",
			},
		})

		m, _ = m.update(keyMsg("j"))
		assert.Equal(t, 1, m.scrollPos)
	})

	t.Run("k_scrolls_up", func(t *testing.T) {
		t.Parallel()
		m := newSettingsModel()
		m.height = 5
		m.setData(settingsDataMsg{
			settings: &settings.EntireSettings{
				Enabled:  true,
				Strategy: "manual-commit",
			},
		})
		m.scrollPos = 3

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 2, m.scrollPos)
	})

	t.Run("k_stays_at_top", func(t *testing.T) {
		t.Parallel()
		m := newSettingsModel()
		m.scrollPos = 0

		m, _ = m.update(keyMsg("k"))
		assert.Equal(t, 0, m.scrollPos)
	})
}

func TestSettingsModel_ViewStates(t *testing.T) {
	t.Parallel()

	t.Run("error_view", func(t *testing.T) {
		t.Parallel()
		m := newSettingsModel()
		m.err = errors.New("settings error")

		view := m.view(80, 20)
		assert.Contains(t, view, "Error loading settings")
		assert.Contains(t, view, "settings error")
	})

	t.Run("no_data_view", func(t *testing.T) {
		t.Parallel()
		m := newSettingsModel()

		view := m.view(80, 20)
		assert.Contains(t, view, "No settings data available")
	})

	t.Run("with_data", func(t *testing.T) {
		t.Parallel()
		m := newSettingsModel()
		m.height = 30
		m.setData(settingsDataMsg{
			settings: &settings.EntireSettings{
				Enabled:  true,
				Strategy: "auto-commit",
			},
		})

		view := m.view(80, 30)
		assert.Contains(t, view, "Configuration")
		assert.Contains(t, view, "auto-commit")
	})
}

func TestSettingsModel_NonKeyMsg(t *testing.T) {
	t.Parallel()

	m := newSettingsModel()

	updated, cmd := m.update(tea.WindowSizeMsg{Width: 80, Height: 40})
	assert.Equal(t, m.scrollPos, updated.scrollPos)
	assert.Nil(t, cmd)
}
