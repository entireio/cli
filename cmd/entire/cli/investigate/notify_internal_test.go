package investigate

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Not parallel: uses t.Chdir().
// An always_prompt configured in the committed project file is dropped by the
// settings trust gate, and the run path must say so: a preamble that silently
// stops applying is indistinguishable from one nobody wrote.
func TestNotifyDroppedInvestigatePrompt_NamesFieldAndRemedy(t *testing.T) {
	tmp := t.TempDir()
	testutil.InitRepo(t, tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"investigate":{"agents":["claude-code"],"always_prompt":"be skeptical"}}`),
		0o644))
	t.Chdir(tmp)

	s, err := settings.Load(context.Background())
	require.NoError(t, err)
	require.Empty(t, s.Investigate.AlwaysPrompt, "the gate drops the project-file prompt")

	var buf bytes.Buffer
	notifyDroppedInvestigatePrompt(&buf, s)
	out := buf.String()
	assert.Contains(t, out, "investigate.always_prompt", "the notice names the dropped field")
	assert.Contains(t, out, "settings.local.json", "the notice names the remedy")
}
