package cli

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// These tests mutate the process-wide --context override, the process
// environment and the export's inherited-value snapshot, so none of them may
// call t.Parallel().

// resetContextExportForTest starts a test from "nothing exported yet" and puts
// the captured values back afterwards, so the order tests run in cannot leak
// one test's inherited value into another's restore. The restored snapshot is
// un-fired rather than already-captured, because a sync.Once cannot be put
// back into the fired state: a later test that skips this helper therefore
// re-reads the environment on its first export instead of inheriting whatever
// the previous test happened to capture.
func resetContextExportForTest(t *testing.T) {
	t.Helper()
	prevValue, prevPresent := inheritedContextEnv.value, inheritedContextEnv.present
	inheritedContextEnv = inheritedEnv{}
	t.Cleanup(func() { inheritedContextEnv = inheritedEnv{value: prevValue, present: prevPresent} })
}

// TestContextFlag_ExportsToChildEnvironment: parsing --context publishes the
// selection as ENTIRE_CONTEXT, the only channel git's `entire` remote helper
// (and any other child that resolves a login itself) reads (COR-1630). The
// flag outranks an inherited value, matching in-process precedence.
func TestContextFlag_ExportsToChildEnvironment(t *testing.T) {
	t.Setenv(contexts.EnvContextVar, "eu.auth.partial.to")
	contexts.SetFlagOverrideForTest(t, "")
	resetContextExportForTest(t)

	require.NoError(t, (&contextFlagValue{}).Set("us.auth.partial.to"))

	assert.Equal(t, "us.auth.partial.to", os.Getenv(contexts.EnvContextVar), "children must see the flag's choice")
	sel, err := (&contexts.File{Contexts: []*contexts.Context{{Name: "us.auth.partial.to", CoreURL: "https://us.auth.partial.to"}}}).Active()
	require.NoError(t, err)
	assert.Equal(t, "--context", sel.Source, "in-process resolution still attributes the choice to the flag")
}

// TestContextFlag_BlankRestoresInheritedEnvironment: a blank --context clears
// the in-process override AND puts the environment back — after an earlier
// non-blank Set as well, so `--context us --context ""` does not leave `us`
// behind for the env tier to pick up.
func TestContextFlag_BlankRestoresInheritedEnvironment(t *testing.T) {
	t.Run("inherited value comes back", func(t *testing.T) {
		t.Setenv(contexts.EnvContextVar, "eu.auth.partial.to")
		contexts.SetFlagOverrideForTest(t, "")
		resetContextExportForTest(t)

		v := &contextFlagValue{}
		require.NoError(t, v.Set("us.auth.partial.to"))
		require.NoError(t, v.Set("  "))

		assert.Equal(t, "eu.auth.partial.to", os.Getenv(contexts.EnvContextVar))
		f := &contexts.File{Contexts: []*contexts.Context{{Name: "eu.auth.partial.to", CoreURL: "https://eu.auth.partial.to"}}}
		sel, err := f.Active()
		require.NoError(t, err)
		assert.Equal(t, "$"+contexts.EnvContextVar, sel.Source, "the user's own variable, not a stale flag")
	})

	t.Run("absent stays absent", func(t *testing.T) {
		t.Setenv(contexts.EnvContextVar, "placeholder") // registers the restore
		os.Unsetenv(contexts.EnvContextVar)
		contexts.SetFlagOverrideForTest(t, "")
		resetContextExportForTest(t)

		v := &contextFlagValue{}
		require.NoError(t, v.Set("us.auth.partial.to"))
		require.NoError(t, v.Set(""))

		_, present := os.LookupEnv(contexts.EnvContextVar)
		assert.False(t, present, "a blank flag must not leave an exported value behind")
	})
}

// TestContextFlag_FailedExportRecordsNoOverride: when the environment refuses
// the value (a NUL byte is the one thing os.Setenv rejects), Set fails without
// recording the in-process override, so the parent cannot act as an identity
// its children never received.
func TestContextFlag_FailedExportRecordsNoOverride(t *testing.T) {
	t.Setenv(contexts.EnvContextVar, "from-shell")
	contexts.SetFlagOverrideForTest(t, "")
	resetContextExportForTest(t)

	v := &contextFlagValue{}
	require.Error(t, v.Set("bad\x00name"))

	assert.Empty(t, v.String())
	assert.Equal(t, "from-shell", os.Getenv(contexts.EnvContextVar))
	sel, err := (&contexts.File{Contexts: []*contexts.Context{{Name: "from-shell", CoreURL: "https://c.example"}}}).Active()
	require.NoError(t, err)
	assert.Equal(t, "$"+contexts.EnvContextVar, sel.Source, "no flag override may be recorded after a failed export")
}

// TestValidateContextFlag: a --context naming no saved login is refused in the
// parent, attributed to the flag, before any child could report it as
// $ENTIRE_CONTEXT. Without the flag nothing is checked, so an inherited
// ENTIRE_CONTEXT keeps being validated where it is consumed.
func TestValidateContextFlag(t *testing.T) {
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv(contexts.EnvContextVar, "")
	os.Unsetenv(contexts.EnvContextVar)
	contexts.SetFlagOverrideForTest(t, "")
	resetContextExportForTest(t)
	require.NoError(t, contexts.Save(os.Getenv("ENTIRE_CONFIG_DIR"), &contexts.File{
		Contexts: []*contexts.Context{{Name: "saved", CoreURL: "https://core.example", Handle: "me", KeychainService: "kc:saved"}},
	}))

	newCmd := func() *cobra.Command {
		cmd := &cobra.Command{Use: "root", Run: func(*cobra.Command, []string) {}}
		addContextFlag(cmd)
		return cmd
	}

	t.Run("unknown name is the flag's error and the export is undone", func(t *testing.T) {
		t.Setenv(contexts.EnvContextVar, "from-shell")
		contexts.SetFlagOverrideForTest(t, "")
		resetContextExportForTest(t)
		cmd := newCmd()
		require.NoError(t, cmd.ParseFlags([]string{"--context", "typo"}))
		require.Equal(t, "typo", os.Getenv(contexts.EnvContextVar), "parsing exports before validation can run")

		err := validateContextFlag(cmd)
		var unknown *contexts.UnknownContextError
		require.ErrorAs(t, err, &unknown)
		assert.Equal(t, "--context", unknown.Source)
		assert.Equal(t, "typo", unknown.Name)
		assert.Equal(t, "from-shell", os.Getenv(contexts.EnvContextVar), "a refused name must not stay exported for whatever spawns on the way out")
	})

	t.Run("saved name passes", func(t *testing.T) {
		cmd := newCmd()
		require.NoError(t, cmd.ParseFlags([]string{"--context", "saved"}))
		require.NoError(t, validateContextFlag(cmd))
	})

	t.Run("blank flag, nothing checked", func(t *testing.T) {
		contexts.SetFlagOverrideForTest(t, "")
		t.Setenv(contexts.EnvContextVar, "dangling-from-shell")
		cmd := newCmd()
		require.NoError(t, cmd.ParseFlags([]string{"--context", "us", "--context", ""}))
		require.NoError(t, validateContextFlag(cmd), "a blank flag clears the override; it must not turn the check on the user's variable")
	})

	t.Run("no flag, nothing checked", func(t *testing.T) {
		contexts.SetFlagOverrideForTest(t, "")
		t.Setenv(contexts.EnvContextVar, "dangling-from-shell")
		cmd := newCmd()
		require.NoError(t, cmd.ParseFlags(nil))
		require.NoError(t, validateContextFlag(cmd), "an exported variable is the user's own and is not the flag's to refuse")
	})
}
