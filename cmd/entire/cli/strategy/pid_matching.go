package strategy

import (
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// findSessionByPIDChain walks the current process's PPID chain and returns
// the session whose AgentPID matches any ancestor PID.
//
// The prepare-commit-msg hook runs as a subprocess of git, which is itself a
// subprocess of the agent process. By walking up the process tree from the hook
// process, we can deterministically identify which session initiated the commit.
//
// Sessions with AgentPID=0 are skipped (pre-upgrade sessions that don't have
// PID tracking yet).
//
// The walk is capped at 20 levels to avoid infinite loops in unusual process
// hierarchies (e.g., PID 1 pointing to itself on some systems).
//
// Returns nil if no session matches any ancestor PID.
func findSessionByPIDChain(sessions []*SessionState) *SessionState {
	// Build a set of agent PIDs for O(1) lookup
	pidToSession := make(map[int]*SessionState)
	for _, s := range sessions {
		if s.AgentPID == 0 {
			continue // Skip pre-upgrade sessions
		}
		pidToSession[s.AgentPID] = s
	}
	if len(pidToSession) == 0 {
		return nil
	}

	// Walk the PPID chain from the current process up to 20 levels
	pid := os.Getpid()
	const maxDepth = 20
	for i := 0; i < maxDepth; i++ {
		if s, ok := pidToSession[pid]; ok {
			return s
		}

		ppid, err := getParentPID(pid)
		if err != nil || ppid == pid || ppid <= 0 {
			break // Reached init or error
		}
		pid = ppid
	}

	return nil
}

// getParentPID returns the parent process ID for the given PID.
// Uses platform-specific methods: /proc on Linux, ps command on macOS.
func getParentPID(pid int) (int, error) {
	if runtime.GOOS == "linux" {
		return getParentPIDLinux(pid)
	}
	return getParentPIDDarwin(pid)
}

// getParentPIDLinux reads /proc/<pid>/stat to get the parent PID (field 4).
func getParentPIDLinux(pid int) (int, error) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}

	// /proc/<pid>/stat format: pid (comm) state ppid ...
	// comm can contain spaces and parentheses, so find the last ')' first
	content := string(data)
	lastParen := strings.LastIndex(content, ")")
	if lastParen < 0 || lastParen+2 >= len(content) {
		return 0, os.ErrNotExist
	}

	// After the last ')' we have: " state ppid ..."
	fields := strings.Fields(content[lastParen+2:])
	if len(fields) < 2 {
		return 0, os.ErrNotExist
	}

	// fields[0] = state, fields[1] = ppid
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

// getParentPIDDarwin uses the ps command to get the parent PID on macOS.
func getParentPIDDarwin(pid int) (int, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

// sortSessionsByLastInteraction sorts sessions by LastInteractionTime descending
// (most recently interacted first). Sessions with nil LastInteractionTime sort last.
//
// This is used as a fallback when PID-based matching is unavailable (e.g.,
// pre-upgrade sessions with AgentPID=0).
func sortSessionsByLastInteraction(sessions []*SessionState) {
	sort.Slice(sessions, func(i, j int) bool {
		ti := sessions[i].LastInteractionTime
		tj := sessions[j].LastInteractionTime
		if ti == nil && tj == nil {
			return false
		}
		if ti == nil {
			return false // nil sorts last
		}
		if tj == nil {
			return true // nil sorts last
		}
		return ti.After(*tj)
	})
}
