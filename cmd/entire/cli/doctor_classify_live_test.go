//go:build linux || darwin

package cli

import (
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Owner liveness is only introspectable on linux/darwin (proclive returns
// LivenessUnknown elsewhere), so these live-process classifications are
// platform-gated. An owner with a mismatched start fingerprint reads as a
// reused — i.e. dead — PID, which is deterministic on both platforms.

func TestClassifySession_IdlePhaseOwnerExited_Stuck(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// An agent that finished its last turn and then quit without firing a
	// session-end hook leaves the session IDLE with a dead owner. Reported
	// regardless of how recently it interacted — process death is unambiguous,
	// so no inactivity timeout has to elapse first.
	state := &strategy.SessionState{
		SessionID:  "test-idle-exited",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseIdle,
		StepCount:  2,
		Owner:      &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"},
	}

	result := classifySession(state, repo, time.Now())

	require.NotNil(t, result, "IDLE session with an exited owner should be stuck")
	assert.Contains(t, result.Reason, "exited (no longer running)")
	assert.Equal(t, 2, result.CheckpointCount)
}

func TestClassifySession_IdlePhaseOwnerAlive_Healthy(t *testing.T) {
	dir := setupGitRepoForPhaseTest(t)
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	owner, ok := proclive.ResolveOwner()
	if !ok {
		t.Skip("no stable process owner resolvable in this environment")
	}

	// The common case: an agent sitting at its prompt between turns. Extending
	// the exited-owner check to IDLE must not make these look stuck.
	state := &strategy.SessionState{
		SessionID:  "test-idle-alive",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseIdle,
		StepCount:  2,
		Owner:      &owner,
	}

	assert.Nil(t, classifySession(state, repo, time.Now()),
		"IDLE session with a live owner should be healthy")
}
