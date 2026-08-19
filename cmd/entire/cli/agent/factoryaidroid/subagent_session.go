package factoryaidroid

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// droidSessionStart is the first line of a Droid session transcript. A session
// spawned by the Task tool records the invoking session and tool-use ID here;
// a top-level session leaves both empty.
type droidSessionStart struct {
	Type             string `json:"type"`
	CallingSessionID string `json:"callingSessionId"`
	CallingToolUseID string `json:"callingToolUseId"`
	SessionTitle     string `json:"sessionTitle"`
}

// sessionStartScanLimit bounds how far into a transcript we look for the
// session_start entry. Droid writes it first; the allowance only covers a
// future format that prepends a line or two, and keeps a large rollout from
// being read end-to-end on every turn.
const sessionStartScanLimit = 8

// ResolveSubagentSession reports whether this transcript belongs to a Worker
// session spawned by a parent task invocation.
//
// Droid runs Workers as detached sessions: the parent's PostToolUse hook fires
// when the Worker is dispatched, before it has touched the worktree, so only
// the Worker's own session boundary can delimit its work. Its transcript's
// session_start line carries callingSessionId/callingToolUseId, which survives
// independently of hook ordering and of the synthetic tool-use IDs the CLI mints
// when Droid's tool hooks omit one.
func (f *FactoryAIDroidAgent) ResolveSubagentSession(sessionRef string) (agent.SubagentSessionLink, bool) {
	start, ok := readDroidSessionStart(sessionRef)
	if !ok {
		return agent.SubagentSessionLink{}, false
	}

	// Both IDs are required: the parent session names the metadata directory and
	// the tool-use ID names the task within it. A half-populated link would
	// attribute the work to the wrong place, so treat it as a top-level session.
	if start.CallingSessionID == "" || start.CallingToolUseID == "" {
		return agent.SubagentSessionLink{}, false
	}

	// Both IDs become path segments — of the sibling transcript we stat below and
	// of the checkpoint's metadata directory. Validate before either is joined
	// into a path: the transcript is a file on disk, so its contents are an
	// untrusted input even though Droid is the one that writes it.
	if err := validation.ValidateAgentSessionID(start.CallingSessionID); err != nil {
		return agent.SubagentSessionLink{}, false
	}
	if err := validation.ValidateToolUseID(start.CallingToolUseID); err != nil {
		return agent.SubagentSessionLink{}, false
	}

	subagentType, description := parseDroidSessionTitle(start.SessionTitle)
	return agent.SubagentSessionLink{
		ParentSessionID:      start.CallingSessionID,
		ToolUseID:            start.CallingToolUseID,
		ParentTranscriptPath: droidSiblingTranscriptPath(sessionRef, start.CallingSessionID),
		SubagentType:         subagentType,
		TaskDescription:      description,
	}, true
}

// droidSiblingTranscriptPath locates another session's transcript. Droid keeps
// every session for a working directory as <session-id>.jsonl in one directory,
// so a Worker's parent is always its sibling. Returns "" when absent.
//
// Callers must validate sessionID first; the guard here is belt-and-braces so
// the function cannot be repurposed into statting an arbitrary path.
func droidSiblingTranscriptPath(sessionRef, sessionID string) string {
	if validation.ValidateAgentSessionID(sessionID) != nil {
		return ""
	}
	path := filepath.Join(filepath.Dir(sessionRef), sessionID+".jsonl")
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

// readDroidSessionStart returns the session_start entry from a Droid transcript.
func readDroidSessionStart(path string) (droidSessionStart, bool) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return droidSessionStart{}, false
	}
	defer func() { _ = file.Close() }()

	// ReadBytes rather than bufio.Scanner: session_start embeds the full task
	// prompt in its title, which routinely exceeds Scanner's default line cap.
	reader := bufio.NewReader(file)
	for range sessionStartScanLimit {
		lineBytes, err := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			var start droidSessionStart
			if jsonErr := json.Unmarshal(bytes.TrimSpace(lineBytes), &start); jsonErr == nil &&
				start.Type == "session_start" {
				return start, true
			}
		}
		if err != nil {
			break
		}
	}
	return droidSessionStart{}, false
}

// parseDroidSessionTitle splits a Worker session title into subagent type and
// task description. Droid formats it as "<type>: <description>"; anything else
// is returned as a bare description.
func parseDroidSessionTitle(title string) (subagentType, description string) {
	title = strings.TrimSpace(title)
	if title == "" {
		return "", ""
	}
	prefix, rest, found := strings.Cut(title, ":")
	if !found {
		return "", title
	}
	prefix = strings.TrimSpace(prefix)
	rest = strings.TrimSpace(rest)
	// A colon inside a plain sentence is not a type prefix; require a short,
	// single-token prefix ("worker", "reviewer") to read it as one.
	if prefix == "" || rest == "" || strings.ContainsAny(prefix, " \t") {
		return "", title
	}
	return prefix, rest
}
