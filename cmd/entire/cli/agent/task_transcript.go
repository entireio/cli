package agent

import (
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// TaskTranscriptMatcher is implemented by agents whose subagent (task)
// transcripts follow a predictable file layout. Callers holding a task
// transcript path from session state use it to confirm that the path names the
// expected task before reading it.
type TaskTranscriptMatcher interface {
	Agent

	// TaskTranscriptMatches reports whether path is where the agent writes the
	// transcript of task agentID in session sessionID. parentPath is the
	// session's own transcript, or "" when it is not known; a layout relative
	// to the parent then checks the file name only. All paths are absolute
	// and clean. TaskTranscriptMatches does no I/O.
	TaskTranscriptMatches(parentPath, sessionID, agentID, path string) bool
}

// AsTaskTranscriptMatcher returns the agent as a TaskTranscriptMatcher if it is
// a built-in agent that implements the interface.
func AsTaskTranscriptMatcher(ag Agent) (TaskTranscriptMatcher, bool) {
	return builtinCapability[TaskTranscriptMatcher](ag)
}

// TaskTranscriptBesideParent reports whether path lies in the directory of the
// session transcript parentPath, or in that session's subagents directory as
// named by paths.SubagentsDir. An empty parentPath matches any directory. The
// comparison is lexical and folds case on Windows.
func TaskTranscriptBesideParent(parentPath, sessionID, path string) bool {
	if parentPath == "" {
		return true
	}
	dir := filepath.Dir(path)
	parentDir := filepath.Dir(parentPath)
	return sameDir(dir, parentDir) || sameDir(dir, paths.SubagentsDir(parentDir, sessionID))
}

// sameDir reports whether a and b name the same directory, with the case
// folding pathHasDirPrefix applies: a path of equal length inside a directory
// is that directory.
func sameDir(a, b string) bool {
	return len(a) == len(b) && pathHasDirPrefix(a, b)
}
