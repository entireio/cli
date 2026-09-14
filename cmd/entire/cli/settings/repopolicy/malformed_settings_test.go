package repopolicy_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings/repopolicy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestClassifyRepoPolicy_MalformedSettingsIsNamedAsSuch pins what a user sees
// when their .entire/settings.json will not parse.
//
// Classification reads repo settings, and .entire's location now routes through
// classification, so a parse failure surfaces during location resolution. That
// is the right place to fail — but not under a message about the DIRECTORY. The
// path is fine; the JSON is not, and "cannot access .entire" sends the reader
// after the wrong thing.
//
// The error must therefore carry ErrRepoSettingsMalformed so presentation can
// name the real cause.
func TestClassifyRepoPolicy_MalformedSettingsIsNamedAsSuch(t *testing.T) {
	// No t.Parallel: t.Chdir.
	root := t.TempDir()
	testutil.InitRepo(t, root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o750))
	require.NoError(t, os.WriteFile(
		filepath.Join(root, ".entire", "settings.json"), []byte(`{"enabled":true`), 0o600))
	t.Chdir(root)

	_, err := repopolicy.ClassifyRepoPolicy(t.Context())
	require.Error(t, err)
	require.ErrorIs(t, err, repopolicy.ErrRepoSettingsMalformed,
		"a settings parse failure must be identifiable, not just a classification failure")
	require.Contains(t, strings.ToLower(err.Error()), "settings",
		"the message must name the settings file, not the directory")
}

// TestErrRepoSettingsMalformed_IsDistinctFromOtherFailures keeps the sentinel
// honest: a classification that fails for some other reason must not match it,
// or presentation would blame the settings file for everything.
func TestErrRepoSettingsMalformed_IsDistinctFromOtherFailures(t *testing.T) {
	t.Parallel()
	require.NotErrorIs(t, errors.New("some other failure"), repopolicy.ErrRepoSettingsMalformed)
}
