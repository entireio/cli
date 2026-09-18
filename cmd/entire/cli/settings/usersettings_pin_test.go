package settings

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
)

// usersettings validates prompt_default against its own copy of these values,
// because settings imports usersettings and the dependency cannot run the
// other way. Two lists that must agree need a test that fails when they drift,
// or a user file and a project file would disagree about what is a legal
// prompt_default.
func TestOPFPromptDefaultsMatchSettings(t *testing.T) {
	t.Parallel()
	for _, valid := range []string{"", OPFPromptAsk, OPFPromptNever, OPFPromptAlways} {
		require.NoError(t, usersettings.ValidateOPFRunSettings(0, valid),
			"settings accepts prompt_default %q, so the user file must too", valid)
	}
	assert.Error(t, usersettings.ValidateOPFRunSettings(0, "sometimes"),
		"a value settings rejects must not be accepted from the user file")
}
