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
// How the link is found: the parent's session directory holds a `subagents/`
// entry per child. This scans sibling sessions in the same working-directory
// group for one whose subagents/ metadata claims this child — preferring a
// typed read of meta.json (see subagentMeta) and falling back to an untyped
// "does this JSON name the child anywhere" scan.
//
// It is deliberately unforgiving. A match must be unique, and the owning
// directory must agree with any parentSessionId the file states — zero
// matches, two matches, a disagreeing record, an unreadable tree, or an
// unexpected layout all return false, which puts the caller back on the
// existing top-level-session path. Being wrong here would attribute one
// session's work to an unrelated session, which is worse than the
// double-count it is fixing, so ambiguity always loses.
//
// Status: the *behaviour* (subagents are independent child sessions) is
// confirmed from a real run. The meta.json *field names* are taken from serde
// strings in the shipping binary, not from documentation, and have not been
// checked against a real subagents/ directory — the quota ran out first. If
// the real records differ, the typed path misses, the untyped scan is the
// safety net, and a total mismatch simply returns false everywhere, leaving
// behaviour as it is today.
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
		meta, toolUseID, ok := subagentEntryFor(filepath.Join(groupDir, e.Name(), subagentsDirName), childID)
		if !ok {
			continue
		}
		hits++
		if hits > 1 {
			// Ambiguous: refuse rather than pick.
			return agent.SubagentSessionLink{}, false
		}
		// The owning directory is the parent by construction; meta's own
		// parentSessionId is only trusted when it agrees, so a stray or copied
		// meta.json cannot redirect the link somewhere else.
		parentID := e.Name()
		if meta.ParentSessionID != "" && meta.ParentSessionID != parentID {
			return agent.SubagentSessionLink{}, false
		}
		found = agent.SubagentSessionLink{
			ParentSessionID:      parentID,
			ToolUseID:            toolUseID,
			SubagentType:         meta.SubagentType,
			ParentTranscriptPath: filepath.Join(groupDir, parentID, transcriptFileName),
		}
	}
	if hits != 1 {
		return agent.SubagentSessionLink{}, false
	}
	return found, true
}

// subagentMeta is the part of Grok's subagents/<id>/meta.json this needs.
//
// The field names are not documented, but they are present as serde names in
// the shipping binary (crates/codegen/xai-grok-shell/src/agent/subagent/mod.rs
// writes "meta.json" alongside schemaVersion / parentSessionId /
// childSessionId / subagentId / subagentType / parentPromptId). They are still
// unverified against a real file, so every reader below tolerates their
// absence and falls back to the untyped scan.
type subagentMeta struct {
	SchemaVersion   int    `json:"schemaVersion"`
	ParentSessionID string `json:"parentSessionId"`
	ChildSessionID  string `json:"childSessionId"`
	SubagentID      string `json:"subagentId"`
	SubagentType    string `json:"subagentType"`
	ParentPromptID  string `json:"parentPromptId"`
}

// subagentEntryFor reports whether subagentsDir records childID, returning the
// tool-use ID to key the task checkpoint by and the subagent type when known.
//
// It prefers a typed read of meta.json — matching childSessionId exactly and
// taking subagentId as the key — and falls back to the untyped "does this JSON
// mention the child ID anywhere" scan when the schema does not match, so an
// unexpected layout degrades instead of breaking.
func subagentEntryFor(subagentsDir, childID string) (subagentMeta, string, bool) {
	entries, err := os.ReadDir(subagentsDir)
	if err != nil {
		return subagentMeta{}, "", false
	}
	for _, e := range entries {
		path := filepath.Join(subagentsDir, e.Name())
		if !e.IsDir() {
			if meta, ok := readSubagentMeta(path, childID); ok {
				return meta, keyFor(meta, e.Name(), childID), true
			}
			if fileMentions(path, childID) {
				return subagentMeta{}, childID, true
			}
			continue
		}
		inner, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, f := range inner {
			if f.IsDir() {
				continue
			}
			fp := filepath.Join(path, f.Name())
			if meta, ok := readSubagentMeta(fp, childID); ok {
				return meta, keyFor(meta, e.Name(), childID), true
			}
			if fileMentions(fp, childID) {
				return subagentMeta{}, e.Name(), true
			}
		}
	}
	return subagentMeta{}, "", false
}

// keyFor picks the most specific stable identifier available for the task
// checkpoint key: the recorded subagent ID, else the entry directory name,
// else the child session ID.
func keyFor(meta subagentMeta, entryName, childID string) string {
	if meta.SubagentID != "" {
		return meta.SubagentID
	}
	if entryName != "" {
		return entryName
	}
	return childID
}

// readSubagentMeta parses path as subagent metadata and reports whether it
// names childID as its child. An exact childSessionId match is required, so a
// file describing a different subagent never matches.
func readSubagentMeta(path, childID string) (subagentMeta, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > maxSubagentMetaSize {
		return subagentMeta{}, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // path built from Grok's own session tree
	if err != nil {
		return subagentMeta{}, false
	}
	var meta subagentMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return subagentMeta{}, false
	}
	if meta.ChildSessionID != childID {
		return subagentMeta{}, false
	}
	return meta, true
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
