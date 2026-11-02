//go:build linux || darwin

package strategy

import (
	"os/exec"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/stretchr/testify/require"
)

// liveNonAncestorOwner starts a long-lived child and returns its identity: a
// live process that is provably not an ancestor of this test process, standing
// in for another agent that is running while somebody else commits.
func liveNonAncestorOwner(t *testing.T) *proclive.Identity {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Wait() }) //nolint:errcheck // killed by the test context
	id, ok := proclive.IdentityOf(cmd.Process.Pid)
	require.True(t, ok, "IdentityOf(child) must resolve on supported platforms")
	return &id
}

// Not parallel: clears and sets caller-session environment variables.
func TestCommitProvenance_ForeignSessions(t *testing.T) {
	clearCallerSessionEnv(t)

	dead := *selfAncestorOwner(t)
	dead.Start = "not-this-process"

	foreign := &SessionState{SessionID: "other-agent", Owner: liveNonAncestorOwner(t)}
	claimed := &SessionState{SessionID: "claimed", Owner: liveNonAncestorOwner(t)}
	states := []*SessionState{
		foreign,
		claimed,
		{SessionID: "committing-agent", Owner: selfAncestorOwner(t)},
		{SessionID: "no-owner"},
		{SessionID: "dead-owner", Owner: &dead},
		{SessionID: "no-host", Owner: func() *proclive.Identity {
			id := *foreign.Owner
			id.Host = ""
			return &id
		}()},
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", claimed.SessionID)

	provenance := newCommitProvenance(t.Context(), states)

	require.True(t, provenance.isForeign("other-agent"), "a live agent outside the commit's ancestry did not make the commit")
	for _, id := range []string{"claimed", "committing-agent", "no-owner", "dead-owner", "no-host"} {
		require.False(t, provenance.isForeign(id), "%s must keep today's linking: nothing proves it foreign", id)
	}
}
