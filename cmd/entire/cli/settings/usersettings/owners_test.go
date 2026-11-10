package usersettings

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// NormalizeOwner keys a push URL's owner the way `repos` keys a repository,
// and fails closed: a transport that is not a forge's, or a URL that names no
// owner, normalizes to "" and so matches no owner list entry.
func TestNormalizeOwner(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]string{
		"https://github.com/EntireIO/cli.git":               "gh/entireio",
		"git@github.com:entireio/cli.git":                   "gh/entireio",
		"ssh://git@github.com/entireio/cli.git":             "gh/entireio",
		"entire://aws-us-east-2.entire.io/gh/entireio/cli":  "gh/entireio",
		"entire://aws-us-east-2.entire.io/et/acme/widgets":  "et/acme",
		"https://gitlab.com/acme/widgets.git":               "gitlab.com/acme",
		"git@gitlab.com:grp/sub/widgets.git":                "gitlab.com/grp",
		"https://gitlab.com/grp/sub/deeper/widgets.git":     "gitlab.com/grp",
		"git://git.corp.example/acme/widgets.git":           "git.corp.example/acme",
		"http://git.corp.example/acme/widgets.git":          "git.corp.example/acme",
		"file:///srv/git/acme/widgets.git":                  "",
		"/srv/git/acme/widgets.git":                         "",
		"ext::ssh -o ProxyCommand=evil github.com/acme/app": "",
		"https://github.com/acme":                           "",
		"":                                                  "",
	} {
		t.Run(url, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, want, NormalizeOwner(url))
		})
	}
}

// --owner takes exactly <forge token or host>/<owner>, lowercased; GitHub's
// host is spelled as its forge token so it matches what push URLs normalize
// to. Wildcards, URLs and a GitLab subgroup are refused: the list matches the
// top-level owner only.
func TestValidateOwner(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"gh/entireio":           "gh/entireio",
		"GH/EntireIO":           "gh/entireio",
		"github.com/entireio":   "gh/entireio",
		"et/acme":               "et/acme",
		"gitlab.com/grp":        "gitlab.com/grp",
		"git.corp.example/acme": "git.corp.example/acme",
	} {
		got, err := ValidateOwner(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	trimmed, err := ValidateOwner("  gh/acme\t")
	require.NoError(t, err)
	assert.Equal(t, "gh/acme", trimmed)
	for _, in := range []string{
		"", "entireio", "gh/*", "*/acme", "gh/entireio/cli", "gitlab.com/grp/sub",
		"https://github.com/entireio", "gh/..", "gh/", "/acme", "gh/a b",
	} {
		_, err := ValidateOwner(in)
		assert.Error(t, err, in)
	}
}

// Adding owners keeps the ones already listed, in order, without repeats.
func TestAddGlobalEnableOwners(t *testing.T) {
	writeUserFile(t, `{"global_enable": true, "global_enable_owners": ["gh/acme"]}`)

	owners, err := AddGlobalEnableOwners(t.Context(), []string{"et/acme", "gh/acme"})
	require.NoError(t, err)
	assert.Equal(t, []string{"gh/acme", "et/acme"}, owners)

	data, err := os.ReadFile(filepath.Join(os.Getenv(userdirs.EnvConfigDir), FileName))
	require.NoError(t, err)
	assert.JSONEq(t, `{"global_enable": true, "global_enable_owners": ["gh/acme", "et/acme"]}`, string(data))
}

// A list that is not one is refused rather than replaced.
func TestAddGlobalEnableOwners_RefusesAMalformedList(t *testing.T) {
	const content = `{"global_enable_owners": "gh/acme"}`
	writeUserFile(t, content)

	_, err := AddGlobalEnableOwners(t.Context(), []string{"et/acme"})
	require.Error(t, err)
	data, readErr := os.ReadFile(filepath.Join(os.Getenv(userdirs.EnvConfigDir), FileName))
	require.NoError(t, readErr)
	assert.JSONEq(t, content, string(data))
}
