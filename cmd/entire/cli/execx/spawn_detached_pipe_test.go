package execx

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A detached child must not depend on this process for its output streams.
// nil Stdout/Stderr makes os/exec open the null device in the child; any
// non-*os.File writer (io.Discard was the regression) makes os/exec create a
// pipe whose read end dies with this process, so a child that outlives a
// short-lived parent (a git hook) is killed by SIGPIPE on its next write.
func TestNewDetachedCmdHoldsNoParentPipes(t *testing.T) {
	t.Parallel()
	cmd := newDetachedCmd("/bin/true", "/tmp", "arg")
	require.Nil(t, cmd.Stdout, "Stdout must stay nil (null device in the child)")
	require.Nil(t, cmd.Stderr, "Stderr must stay nil (null device in the child)")
	require.Nil(t, cmd.Stdin)
	require.Equal(t, "/tmp", cmd.Dir)
}

func TestNewDetachedCmdDefaultsDirOffTheParentWorkingDirectory(t *testing.T) {
	t.Parallel()
	cmd := newDetachedCmd("/bin/true", "")
	require.NotEmpty(t, cmd.Dir, "empty dir must fall back so the child never holds the parent's cwd")
}
