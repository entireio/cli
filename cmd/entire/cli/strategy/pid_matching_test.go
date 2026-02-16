package strategy

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFindSessionByPIDChain_MatchesCurrentProcess creates a session with AgentPID
// matching a known ancestor PID of the test process and verifies it is returned.
func TestFindSessionByPIDChain_MatchesCurrentProcess(t *testing.T) {
	t.Parallel()
	// The test process's own PID is an ancestor from the hook's perspective.
	// In production, the hook process walks up to find the agent PID.
	// Here, we set AgentPID to the current process's PID which will be found
	// when walking the PPID chain (since the current process IS in its own chain).
	myPID := os.Getpid()

	sessionA := &SessionState{
		SessionID: "session-a",
		AgentPID:  99999999, // Non-matching PID
	}
	sessionB := &SessionState{
		SessionID: "session-b",
		AgentPID:  myPID, // Matches current process
	}

	result := findSessionByPIDChain([]*SessionState{sessionA, sessionB})
	require.NotNil(t, result, "should find a session matching the current process PID")
	assert.Equal(t, "session-b", result.SessionID)
}

// TestFindSessionByPIDChain_MatchesParentPID verifies that the walker matches
// against parent PIDs, not just the current process.
func TestFindSessionByPIDChain_MatchesParentPID(t *testing.T) {
	t.Parallel()
	parentPID := os.Getppid()

	session := &SessionState{
		SessionID: "parent-match",
		AgentPID:  parentPID,
	}

	result := findSessionByPIDChain([]*SessionState{session})
	require.NotNil(t, result, "should find a session matching the parent PID")
	assert.Equal(t, "parent-match", result.SessionID)
}

// TestFindSessionByPIDChain_NoMatch creates sessions with non-matching PIDs
// and verifies nil is returned.
func TestFindSessionByPIDChain_NoMatch(t *testing.T) {
	t.Parallel()
	sessionA := &SessionState{
		SessionID: "session-a",
		AgentPID:  99999998, // Unlikely to match any real ancestor
	}
	sessionB := &SessionState{
		SessionID: "session-b",
		AgentPID:  99999999,
	}

	result := findSessionByPIDChain([]*SessionState{sessionA, sessionB})
	assert.Nil(t, result, "should return nil when no session matches any ancestor PID")
}

// TestFindSessionByPIDChain_SkipsZeroPID verifies that sessions with AgentPID=0
// (pre-upgrade sessions) are skipped by the PID chain walker.
func TestFindSessionByPIDChain_SkipsZeroPID(t *testing.T) {
	t.Parallel()
	sessionA := &SessionState{
		SessionID: "pre-upgrade-session",
		AgentPID:  0, // Pre-upgrade, should be skipped
	}

	result := findSessionByPIDChain([]*SessionState{sessionA})
	assert.Nil(t, result, "should skip sessions with AgentPID=0")
}

// TestFindSessionByPIDChain_EmptySessions verifies that an empty session list
// returns nil without error.
func TestFindSessionByPIDChain_EmptySessions(t *testing.T) {
	t.Parallel()
	result := findSessionByPIDChain(nil)
	assert.Nil(t, result, "should return nil for empty session list")

	result = findSessionByPIDChain([]*SessionState{})
	assert.Nil(t, result, "should return nil for empty session list")
}

// TestSortSessionsByLastInteraction verifies that sessions are sorted by
// LastInteractionTime in descending order (most recent first).
func TestSortSessionsByLastInteraction(t *testing.T) {
	t.Parallel()
	now := time.Now()
	older := now.Add(-10 * time.Minute)
	oldest := now.Add(-20 * time.Minute)

	sessions := []*SessionState{
		{SessionID: "oldest", LastInteractionTime: &oldest},
		{SessionID: "newest", LastInteractionTime: &now},
		{SessionID: "middle", LastInteractionTime: &older},
	}

	sortSessionsByLastInteraction(sessions)

	assert.Equal(t, "newest", sessions[0].SessionID)
	assert.Equal(t, "middle", sessions[1].SessionID)
	assert.Equal(t, "oldest", sessions[2].SessionID)
}

// TestSortSessionsByLastInteraction_NilTimestamps verifies that sessions
// with nil LastInteractionTime sort last.
func TestSortSessionsByLastInteraction_NilTimestamps(t *testing.T) {
	t.Parallel()
	now := time.Now()
	older := now.Add(-10 * time.Minute)

	sessions := []*SessionState{
		{SessionID: "nil-time", LastInteractionTime: nil},
		{SessionID: "newer", LastInteractionTime: &now},
		{SessionID: "older", LastInteractionTime: &older},
	}

	sortSessionsByLastInteraction(sessions)

	assert.Equal(t, "newer", sessions[0].SessionID)
	assert.Equal(t, "older", sessions[1].SessionID)
	assert.Equal(t, "nil-time", sessions[2].SessionID)
}

// TestSortSessionsByLastInteraction_AllNil verifies sort stability when
// all sessions have nil LastInteractionTime.
func TestSortSessionsByLastInteraction_AllNil(t *testing.T) {
	t.Parallel()
	sessions := []*SessionState{
		{SessionID: "a", LastInteractionTime: nil},
		{SessionID: "b", LastInteractionTime: nil},
	}

	// Should not panic
	sortSessionsByLastInteraction(sessions)
	assert.Len(t, sessions, 2)
}
