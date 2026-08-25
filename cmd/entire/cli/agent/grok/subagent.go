package grok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

var _ agent.SubagentSessionResolver = (*GrokAgent)(nil)

// subagentsDirName is the per-session directory holding one entry per subagent
// the session spawned. Grok's own docs describe it only as "per-subagent
// metadata (meta.json); the child sessions live in the normal sessions tree".
const subagentsDirName = "subagents"

// ResolveSubagentSession reports whether sessionRef belongs to a session that
// another session spawned, and if so identifies the parent.
//
// Why this interface at all: Grok runs subagents as *independent child
// sessions*, not as a blocking tool call inside the parent's turn. Observed in
// a real run — one `spawn_subagent` produced two distinct session IDs, each
// firing its own UserPromptSubmit / Stop / SessionEnd, and only the parent
// fired SessionStart (Grok does not fire it for a child). Without this
// resolver the child is recorded as a second top-level session and the file it
// wrote is counted twice, once under each session.
//
// How the link is found: the parent's session directory holds a
// `subagents/` entry per child. There is no documented schema for it, so
// rather than assume field names this scans sibling sessions in the same
// working-directory group and looks for the child's session ID appearing
// anywhere inside their `subagents/` metadata.
//
// It is deliberately unforgiving. A match must be unique — zero matches, two
// matches, an unreadable tree, or an unexpected layout all return false, which
// puts the caller back on the existing top-level-session path. Being wrong
// here would attribute one session's work to an unrelated session, which is
// worse than the double-count it is fixing, so ambiguity always loses.
//
// The layout is doc-derived and has NOT been verified against a real
// subagents/ directory; the quota ran out before one could be captured. If it
// turns out Grok keys that metadata by something other than the child session
// ID, this returns false everywhere and behaviour is unchanged.
func (g *GrokAgent) ResolveSubagentSession(sessionRef string) (agent.SubagentSessionLink, bool) {
	if sessionRef == "" {
		return agent.SubagentSessionLink{}, false
	}

	childDir := filepath.Dir(sessionRef)
	childID := filepath.Base(childDir)
	groupDir := filepath.Dir(childDir)
	if childID == "" || childID == "." || groupDir == "" || groupDir == "." {
		return agent.SubagentSessionLink{}, false
	}
	// A session ID short enough to collide with arbitrary text would make the
	// substring scan below unsafe.
	if len(childID) < 16 {
		return agent.SubagentSessionLink{}, false
	}

	entries, err := os.ReadDir(groupDir)
	if err != nil {
		return agent.SubagentSessionLink{}, false
	}

	var (
		found agent.SubagentSessionLink
		hits  int
	)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == childID {
			continue
		}
		toolUseID, ok := subagentEntryFor(filepath.Join(groupDir, e.Name(), subagentsDirName), childID)
		if !ok {
			continue
		}
		hits++
		if hits > 1 {
			// Ambiguous: refuse rather than pick.
			return agent.SubagentSessionLink{}, false
		}
		parentDir := filepath.Join(groupDir, e.Name())
		found = agent.SubagentSessionLink{
			ParentSessionID:      e.Name(),
			ToolUseID:            toolUseID,
			ParentTranscriptPath: filepath.Join(parentDir, transcriptFileName),
		}
	}
	if hits != 1 {
		return agent.SubagentSessionLink{}, false
	}
	return found, true
}

// subagentEntryFor reports whether subagentsDir records childID, returning the
// tool-use ID to key the task checkpoint by.
//
// The entry directory's own name is preferred as the tool-use ID because that
// is what Grok keys the subagent by; the child session ID is the fallback so
// the returned link always has a stable, unique key.
func subagentEntryFor(subagentsDir, childID string) (string, bool) {
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			if metaMentions(filepath.Join(subagentsDir, e.Name()), childID) {
				return e.Name(), true
			}
			continue
		}
		if fileMentions(filepath.Join(subagentsDir, e.Name()), childID) {
			return childID, true
		}
	}
	return "", false
}

// metaMentions reports whether any regular file directly inside dir names
// childID.
func metaMentions(dir, childID string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fileMentions(filepath.Join(dir, e.Name()), childID) {
			return true
		}
	}
	return false
}

// maxSubagentMetaSize bounds how much of a metadata file is read. These are
// small records; anything larger is not the file we are looking for, and the
// scan must not pull an arbitrarily large blob into memory.
const maxSubagentMetaSize = 1 << 20 // 1 MiB

// fileMentions reports whether path is small, parses as JSON, and contains
// childID as a string value.
//
// Requiring valid JSON and a *value* match — rather than a plain substring
// search — is what keeps this from matching an unrelated log line or a path
// that merely embeds the ID.
func fileMentions(path, childID string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxSubagentMetaSize {
		return false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path built from Grok's own session tree
	if err != nil {
		return false
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	return jsonHasStringValue(doc, childID)
}

// jsonHasStringValue walks a decoded JSON document looking for target as a
// string value or as an object key.
func jsonHasStringValue(node any, target string) bool {
	switch v := node.(type) {
	case string:
		return v == target || strings.HasSuffix(v, "/"+target)
	case []any:
		for _, item := range v {
			if jsonHasStringValue(item, target) {
				return true
			}
		}
	case map[string]any:
		for key, item := range v {
			if key == target {
				return true
			}
			if jsonHasStringValue(item, target) {
				return true
			}
		}
	}
	return false
}
