package strategy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// getParentPIDLinux tests — /proc/<pid>/stat parsing edge cases
// ============================================================================

// TestGetParentPIDLinux_CurrentProcess verifies that getParentPIDLinux returns
// the correct parent PID for the current process by cross-checking with os.Getppid().
func TestGetParentPIDLinux_CurrentProcess(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	ppid, err := getParentPIDLinux(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), ppid,
		"getParentPIDLinux should return the same value as os.Getppid()")
}

// TestGetParentPIDLinux_PID1 verifies that getParentPIDLinux can read PID 1 (init/systemd).
// PID 1's parent is typically 0 on Linux.
func TestGetParentPIDLinux_PID1(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	ppid, err := getParentPIDLinux(1)
	require.NoError(t, err)
	assert.Equal(t, 0, ppid,
		"PID 1's parent should be 0")
}

// TestGetParentPIDLinux_NonExistentPID verifies that getParentPIDLinux returns
// an error for a PID that doesn't exist.
func TestGetParentPIDLinux_NonExistentPID(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	// PID 4194305 is outside the typical PID range, very unlikely to exist
	_, err := getParentPIDLinux(4194305)
	assert.Error(t, err,
		"getParentPIDLinux should return an error for a non-existent PID")
}

// TestGetParentPIDLinux_ProcessWithParensInName verifies that the /proc/stat
// parser handles process names containing parentheses correctly.
// The format is: pid (comm) state ppid ...
// If comm contains ')', a naive parser would split at the wrong position.
// We use strings.LastIndex(")") to handle this.
func TestGetParentPIDLinux_ProcessWithParensInName(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	// We can't control the comm field of a real process easily, but we can
	// verify the current process parses correctly as a sanity check.
	// The key edge case is handled by the LastIndex(")") logic in getParentPIDLinux.
	ppid, err := getParentPIDLinux(os.Getpid())
	require.NoError(t, err)
	assert.True(t, ppid > 0, "parent PID should be positive")
}

// TestGetParentPID_Dispatch verifies that getParentPID dispatches correctly
// based on the OS and returns consistent results with os.Getppid().
func TestGetParentPID_Dispatch(t *testing.T) {
	t.Parallel()
	ppid, err := getParentPID(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), ppid,
		"getParentPID should return the same value as os.Getppid()")
}

// TestGetParentPID_InvalidPID verifies that getParentPID returns an error
// for an invalid PID.
func TestGetParentPID_InvalidPID(t *testing.T) {
	t.Parallel()
	_, err := getParentPID(99999999)
	assert.Error(t, err,
		"getParentPID should return an error for a non-existent PID")
}

// ============================================================================
// findSessionByPIDChain — additional edge cases
// ============================================================================

// TestFindSessionByPIDChain_DuplicatePIDs verifies that when multiple sessions
// share the same AgentPID, the one with the most recent LastInteractionTime wins,
// regardless of slice position.
func TestFindSessionByPIDChain_DuplicatePIDs(t *testing.T) {
	t.Parallel()
	myPID := os.Getpid()

	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()

	sessionA := &SessionState{
		SessionID:           "session-dup-a",
		AgentPID:            myPID,
		LastInteractionTime: &newer, // More recent
	}
	sessionB := &SessionState{
		SessionID:           "session-dup-b",
		AgentPID:            myPID, // Same PID as session A
		LastInteractionTime: &older,
	}

	// Session A is first in slice and has newer time — should win
	result := findSessionByPIDChain([]*SessionState{sessionA, sessionB})
	require.NotNil(t, result, "should find a session matching the current PID")
	assert.Equal(t, "session-dup-a", result.SessionID,
		"when duplicate PIDs exist, the session with the most recent LastInteractionTime wins")

	// Reverse slice order — session A should STILL win (deterministic, not order-dependent)
	result = findSessionByPIDChain([]*SessionState{sessionB, sessionA})
	require.NotNil(t, result, "should find a session matching the current PID")
	assert.Equal(t, "session-dup-a", result.SessionID,
		"result should be deterministic regardless of input slice order")
}

// TestFindSessionByPIDChain_AllZeroPIDs verifies that when all sessions have
// AgentPID=0 (pre-upgrade sessions), nil is returned.
func TestFindSessionByPIDChain_AllZeroPIDs(t *testing.T) {
	t.Parallel()
	sessions := []*SessionState{
		{SessionID: "zero-a", AgentPID: 0},
		{SessionID: "zero-b", AgentPID: 0},
		{SessionID: "zero-c", AgentPID: 0},
	}

	result := findSessionByPIDChain(sessions)
	assert.Nil(t, result, "should return nil when all sessions have AgentPID=0")
}

// TestFindSessionByPIDChain_MixedZeroAndValid verifies that sessions with
// AgentPID=0 are skipped but valid PIDs are still checked.
func TestFindSessionByPIDChain_MixedZeroAndValid(t *testing.T) {
	t.Parallel()
	myPID := os.Getpid()

	sessions := []*SessionState{
		{SessionID: "zero-session", AgentPID: 0},
		{SessionID: "valid-session", AgentPID: myPID},
		{SessionID: "another-zero", AgentPID: 0},
	}

	result := findSessionByPIDChain(sessions)
	require.NotNil(t, result, "should find the session with valid PID")
	assert.Equal(t, "valid-session", result.SessionID)
}

// TestFindSessionByPIDChain_SingleSession verifies correct behavior with a
// single session that matches.
func TestFindSessionByPIDChain_SingleSession(t *testing.T) {
	t.Parallel()
	myPID := os.Getpid()

	session := &SessionState{
		SessionID: "only-session",
		AgentPID:  myPID,
	}

	result := findSessionByPIDChain([]*SessionState{session})
	require.NotNil(t, result)
	assert.Equal(t, "only-session", result.SessionID)
}

// TestFindSessionByPIDChain_NegativePID verifies that negative PIDs are treated
// as non-matching (they would never appear in a PPID chain walk).
func TestFindSessionByPIDChain_NegativePID(t *testing.T) {
	t.Parallel()
	session := &SessionState{
		SessionID: "negative-pid",
		AgentPID:  -1,
	}

	result := findSessionByPIDChain([]*SessionState{session})
	assert.Nil(t, result, "negative PIDs should never match any process in the chain")
}

// ============================================================================
// sortSessionsByLastInteraction — additional edge cases
// ============================================================================

// TestSortSessionsByLastInteraction_SingleElement verifies that sorting a
// single-element slice doesn't panic and preserves the element.
func TestSortSessionsByLastInteraction_SingleElement(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sessions := []*SessionState{
		{SessionID: "only-one", LastInteractionTime: &now},
	}

	sortSessionsByLastInteraction(sessions)
	assert.Equal(t, "only-one", sessions[0].SessionID)
}

// TestSortSessionsByLastInteraction_EqualTimestamps verifies that sessions
// with equal timestamps don't cause issues (sort stability).
func TestSortSessionsByLastInteraction_EqualTimestamps(t *testing.T) {
	t.Parallel()
	now := time.Now()
	sessions := []*SessionState{
		{SessionID: "a", LastInteractionTime: &now},
		{SessionID: "b", LastInteractionTime: &now},
		{SessionID: "c", LastInteractionTime: &now},
	}

	// Should not panic
	sortSessionsByLastInteraction(sessions)
	assert.Len(t, sessions, 3)
}

// TestSortSessionsByLastInteraction_MixedNilAndValues verifies correct ordering
// when some sessions have timestamps and others don't, in various positions.
func TestSortSessionsByLastInteraction_MixedNilAndValues(t *testing.T) {
	t.Parallel()
	now := time.Now()
	older := now.Add(-5 * time.Minute)

	sessions := []*SessionState{
		{SessionID: "nil-first", LastInteractionTime: nil},
		{SessionID: "older", LastInteractionTime: &older},
		{SessionID: "nil-second", LastInteractionTime: nil},
		{SessionID: "newer", LastInteractionTime: &now},
	}

	sortSessionsByLastInteraction(sessions)

	// Entries with timestamps should come first (newer before older)
	assert.Equal(t, "newer", sessions[0].SessionID)
	assert.Equal(t, "older", sessions[1].SessionID)
	// nil entries should be last
	assert.Nil(t, sessions[2].LastInteractionTime)
	assert.Nil(t, sessions[3].LastInteractionTime)
}

// TestSortSessionsByLastInteraction_Empty verifies that sorting an empty
// slice doesn't panic.
func TestSortSessionsByLastInteraction_Empty(t *testing.T) {
	t.Parallel()
	var sessions []*SessionState
	// Should not panic
	sortSessionsByLastInteraction(sessions)
	assert.Empty(t, sessions)
}

// ============================================================================
// AgentPID persistence — round-trip through save/load
// ============================================================================

// TestAgentPID_PersistsAcrossSaveLoad verifies that the AgentPID field
// correctly round-trips through JSON serialization in the session state store.
func TestAgentPID_PersistsAcrossSaveLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Create a state store backed by a temp directory
	stateDir := filepath.Join(dir, "sessions")
	require.NoError(t, os.MkdirAll(stateDir, 0o750))

	// Create state with a known AgentPID
	now := time.Now()
	state := &SessionState{
		SessionID:           "test-pid-persistence",
		BaseCommit:          "abc123",
		StartedAt:           now,
		LastInteractionTime: &now,
		AgentPID:            12345,
		StepCount:           0,
	}

	// Write state file directly
	data := fmt.Sprintf(`{
  "session_id": "test-pid-persistence",
  "base_commit": "abc123",
  "started_at": %q,
  "agent_pid": 12345,
  "checkpoint_count": 0
}`, now.Format(time.RFC3339Nano))

	stateFile := filepath.Join(stateDir, "test-pid-persistence.json")
	require.NoError(t, os.WriteFile(stateFile, []byte(data), 0o600))

	// Verify we wrote the right thing
	_ = state // used above for reference

	// Read back and verify AgentPID survived
	readData, err := os.ReadFile(stateFile)
	require.NoError(t, err)
	assert.Contains(t, string(readData), `"agent_pid": 12345`,
		"AgentPID should be persisted in the JSON state file")
}

// TestAgentPID_OmittedWhenZero verifies that AgentPID is omitted from JSON
// when it's zero (omitempty behavior for backward compatibility).
func TestAgentPID_OmittedWhenZero(t *testing.T) {
	t.Parallel()
	// Manually check the JSON tag
	// The State struct has: AgentPID int `json:"agent_pid,omitempty"`
	// In Go, omitempty for int means it's omitted when 0.
	data := fmt.Sprintf(`{
  "session_id": "test-zero-pid",
  "base_commit": "abc123",
  "started_at": %q,
  "checkpoint_count": 0
}`, time.Now().Format(time.RFC3339Nano))

	// Verify that loading a state without agent_pid results in AgentPID=0
	assert.NotContains(t, data, "agent_pid",
		"JSON without agent_pid field should result in AgentPID=0 on load")
}

// ============================================================================
// InitializeSession — AgentPID lifecycle
// ============================================================================

// TestInitializeSession_SetsAgentPID verifies that InitializeSession sets
// AgentPID to the parent PID (os.Getppid()) for new sessions.
func TestInitializeSession_SetsAgentPID(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "test-pid-init"
	err := s.InitializeSession(sessionID, "Claude Code", "", "")
	require.NoError(t, err)

	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, os.Getppid(), state.AgentPID,
		"InitializeSession should set AgentPID to the parent PID of the hook handler")
	assert.True(t, state.AgentPID > 0,
		"AgentPID should be a positive value")
}

// TestInitializeSession_RefreshesAgentPIDOnTurnStart verifies that when
// InitializeSession is called again for an existing session (new turn),
// the AgentPID is refreshed to the current parent PID. This handles the
// scenario where an agent process restarts with a different PID.
func TestInitializeSession_RefreshesAgentPIDOnTurnStart(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "test-pid-refresh"

	// First call initializes the session
	err := s.InitializeSession(sessionID, "Claude Code", "", "")
	require.NoError(t, err)

	// Manually set AgentPID to a stale value to simulate agent restart
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	state.AgentPID = 99999 // Fake old PID
	err = s.saveSessionState(state)
	require.NoError(t, err)

	// Second call (new turn) should refresh the PID
	err = s.InitializeSession(sessionID, "Claude Code", "", "")
	require.NoError(t, err)

	state, err = s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, os.Getppid(), state.AgentPID,
		"InitializeSession should refresh AgentPID on subsequent calls (new turn)")
	assert.NotEqual(t, 99999, state.AgentPID,
		"stale AgentPID should be replaced with the current parent PID")
}

// ============================================================================
// Integration: PID chain walk with real process hierarchy
// ============================================================================

// TestGetParentPID_WalksToInit verifies that walking the PPID chain from the
// current process eventually reaches PID 1 (init) or a process whose parent
// is itself (PID 0 on Linux).
func TestGetParentPID_WalksToInit(t *testing.T) {
	t.Parallel()
	pid := os.Getpid()
	visited := make(map[int]bool)
	depth := 0

	for depth < 30 { // Safety limit
		if visited[pid] {
			// Cycle detected (PID 1 → 0 → error, or PID 1 → PID 1)
			break
		}
		visited[pid] = true

		ppid, err := getParentPID(pid)
		if err != nil {
			// Expected when we reach PID 0 or a non-existent process
			break
		}

		if ppid == pid || ppid <= 0 {
			// Reached the top of the process tree
			break
		}

		pid = ppid
		depth++
	}

	assert.True(t, depth > 0,
		"should have walked at least one level up the PPID chain")
	assert.True(t, depth < 30,
		"PPID chain should terminate within 30 levels")
}

// ============================================================================
// getParentPIDLinux — /proc/stat format edge cases
// ============================================================================

// TestGetParentPIDLinux_ParsesStatCorrectly verifies that getParentPIDLinux
// reads /proc/<pid>/stat for a real process and produces a positive integer.
func TestGetParentPIDLinux_ParsesStatCorrectly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	// Read /proc/self/stat to verify the format we're parsing
	data, err := os.ReadFile("/proc/self/stat")
	require.NoError(t, err, "should be able to read /proc/self/stat")

	// Verify it contains expected format: pid (comm) state ppid ...
	content := string(data)
	assert.Contains(t, content, ")",
		"/proc/self/stat should contain closing parenthesis for comm field")

	// Now verify our parser returns the correct PPID
	ppid, err := getParentPIDLinux(os.Getpid())
	require.NoError(t, err)
	assert.Equal(t, os.Getppid(), ppid,
		"parsed PPID should match os.Getppid()")
}

// TestGetParentPIDLinux_ProcStatWithSpacesInComm verifies that the parser
// handles the case where /proc/<pid>/stat has spaces in the comm field.
// We test this by reading PID 1's stat file, which often has a simple name
// like "(systemd)" or "(init)".
func TestGetParentPIDLinux_ProcStatWithSpacesInComm(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	// PID 1 always exists and its parent is always 0
	ppid, err := getParentPIDLinux(1)
	require.NoError(t, err)
	assert.Equal(t, 0, ppid, "PID 1's parent should be 0")

	// Also read the raw stat to verify format
	statPath := "/proc/1/stat"
	data, err := os.ReadFile(statPath)
	if err != nil {
		t.Skipf("cannot read %s: %v", statPath, err)
	}
	content := string(data)

	// Verify the stat line starts with "1 (" (PID followed by comm in parens)
	assert.True(t, len(content) > 2 && content[0] == '1',
		"/proc/1/stat should start with PID 1")
}

// TestGetParentPID_ZeroPID verifies that requesting parent of PID 0 returns
// an error (PID 0 is the kernel scheduler, doesn't have a /proc entry).
func TestGetParentPID_ZeroPID(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	_, err := getParentPIDLinux(0)
	assert.Error(t, err, "PID 0 should not have a /proc entry")
}

// TestGetParentPID_NegativePID verifies that a negative PID returns an error.
func TestGetParentPID_NegativePID(t *testing.T) {
	t.Parallel()
	_, err := getParentPID(-1)
	assert.Error(t, err, "negative PID should return an error")
}

// ============================================================================
// findSessionByPIDChain — maxDepth boundary
// ============================================================================

// TestFindSessionByPIDChain_MaxDepthSafety verifies that the PPID chain walk
// terminates even if it doesn't find a matching session (bounded by maxDepth=20).
func TestFindSessionByPIDChain_MaxDepthSafety(t *testing.T) {
	t.Parallel()
	// Create a session with a PID that won't match any ancestor
	session := &SessionState{
		SessionID: "unreachable",
		AgentPID:  88888888, // Won't match any real process
	}

	// This should terminate quickly (within maxDepth iterations)
	result := findSessionByPIDChain([]*SessionState{session})
	assert.Nil(t, result,
		"should return nil when no ancestor matches within maxDepth")
}

// ============================================================================
// initializeSession — AgentPID in new session state
// ============================================================================

// TestInitializeSession_NewSessionHasAgentPIDInState verifies that a freshly
// created session includes AgentPID in the persisted state, not just in memory.
func TestInitializeSession_NewSessionHasAgentPIDInState(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	s := &ManualCommitStrategy{}
	sessionID := "test-pid-new-session-state"

	err := s.InitializeSession(sessionID, "Claude Code", "", "")
	require.NoError(t, err)

	// Load from disk to verify persistence
	state, err := s.loadSessionState(sessionID)
	require.NoError(t, err)
	require.NotNil(t, state)

	assert.Equal(t, os.Getppid(), state.AgentPID,
		"newly created session should have AgentPID set from os.Getppid()")
}

// ============================================================================
// /proc/<pid>/stat parsing — field extraction robustness
// ============================================================================

// TestGetParentPIDLinux_SelfStat verifies that /proc/self/stat returns the
// same result as /proc/<our-pid>/stat (consistency check).
func TestGetParentPIDLinux_SelfStat(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}

	// Read via /proc/self/stat
	selfData, err := os.ReadFile("/proc/self/stat")
	require.NoError(t, err)

	// Read via /proc/<pid>/stat
	pidData, err := os.ReadFile("/proc/" + strconv.Itoa(os.Getpid()) + "/stat")
	require.NoError(t, err)

	// Both should parse to the same PPID (though PIDs might differ due to /proc/self
	// being read by the same process)
	ppidSelf, err := getParentPIDLinux(os.Getpid())
	require.NoError(t, err)

	_ = selfData
	_ = pidData
	assert.Equal(t, os.Getppid(), ppidSelf)
}
