package ticket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	got, err := ParsePlatform("linear")
	require.NoError(t, err)
	assert.Equal(t, PlatformLinear, got)

	_, err = ParsePlatform("jira")
	require.Error(t, err, "jira is not supported yet")
	assert.Contains(t, err.Error(), "linear", "error lists supported platforms")
}

func TestPlatform_DisplayName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "Linear", PlatformLinear.DisplayName())
	assert.Equal(t, "acme", Platform("acme").DisplayName())
}
