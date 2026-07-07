package ticket

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCommand_Shape(t *testing.T) {
	t.Parallel()

	cmd := NewCommand(Deps{})
	require.NotNil(t, cmd)
	assert.Equal(t, "ticket", cmd.Use)
	assert.True(t, cmd.Hidden, "ticket group is hidden while the feature matures")
	assert.NotEmpty(t, cmd.Short)
	assert.NotNil(t, cmd.PersistentPreRunE, "group gates on a git repository")
}
