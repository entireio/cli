package review

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Not parallel: uses t.Chdir().
// A prompt configured in the committed project file is dropped by the settings
// trust gate, and the run path must say so: a preamble that silently stops
// applying is indistinguishable from one nobody wrote.
func TestNotifyDroppedReviewPrompts_NamesFieldAndRemedy(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"review_profiles":{"general":{"agents":{"codex":{"prompt":"focus on security"}}},"other":{"agents":{"pi":{"prompt":"x"}}}}}`),
		0o644))
	t.Chdir(tmp)

	s, err := settings.Load(context.Background())
	require.NoError(t, err)

	var buf bytes.Buffer
	notifyDroppedReviewPrompts(&buf, s, "general")
	out := buf.String()
	assert.Contains(t, out, "review_profiles.general.agents.codex.prompt",
		"the notice names the dropped field")
	assert.Contains(t, out, "settings.local.json", "the notice names the remedy")
	assert.NotContains(t, out, "review_profiles.other",
		"the notice is scoped to the profile about to run")
}

// Not parallel: uses t.Chdir().
// A committed profile whose only worker is prompt-only (the Pi shape) must
// survive the trust gate: the worker runs on the profile task, and selection
// succeeds so the drop notice can actually print instead of the run dying on
// "no configured agents" with no explanation.
func TestSelectReviewProfile_KeepsPromptOnlyWorkerAfterGate(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"review_profiles":{"general":{"agents":{"pi":{"prompt":"focus on security"}}}}}`), 0o644))
	t.Chdir(tmp)

	s, err := settings.Load(context.Background())
	require.NoError(t, err)

	name, profile, err := selectReviewProfile(s, "general")
	require.NoError(t, err)
	assert.Equal(t, "general", name)
	require.Contains(t, profile.Agents, "pi")
	assert.Empty(t, profile.Agents["pi"].Prompt, "the untrusted prompt stays dropped")

	var buf bytes.Buffer
	notifyDroppedReviewPrompts(&buf, s, name)
	assert.Contains(t, buf.String(), "review_profiles.general.agents.pi.prompt",
		"the drop is reported for the surviving worker")
}

// Not parallel: uses t.Chdir().
// The non-interactive first-run setup persists the built-in task text into the
// committed project file. The gate drops it, but the fallback produces the
// same text, so the notice must stay quiet for that no-effect drop while still
// firing for a custom task.
func TestNotifyDroppedReviewPrompts_SkipsDefaultEqualTask(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	defaultTask := profileTask("general", settings.ReviewProfileConfig{})
	projectJSON, err := json.Marshal(map[string]any{
		"enabled": true,
		"review_profiles": map[string]any{
			"general": map[string]any{"task": defaultTask, "agents": map[string]any{"codex": map[string]any{"model": "gpt-5"}}},
			"audit":   map[string]any{"task": "Audit the dependencies.", "agents": map[string]any{"codex": map[string]any{"model": "gpt-5"}}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".entire", "settings.json"), projectJSON, 0o644))
	t.Chdir(tmp)

	s, err := settings.Load(context.Background())
	require.NoError(t, err)

	var buf bytes.Buffer
	notifyDroppedReviewPrompts(&buf, s, "general")
	assert.Empty(t, buf.String(),
		"a dropped task equal to the built-in default changes nothing and must not nag")

	buf.Reset()
	notifyDroppedReviewPrompts(&buf, s, "audit")
	assert.Contains(t, buf.String(), "review_profiles.audit.task",
		"a dropped custom task changes behavior and must be reported")
}
