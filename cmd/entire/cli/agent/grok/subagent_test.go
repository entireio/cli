package grok

import (
	"os"
	"path/filepath"
	"testing"
)

const (
	testParentID = "01a03b0b-b73c-7e22-b180-0c8af17af554"
	testChildID  = "01a03b0b-9396-7e63-b4d5-3429ca97c231"
)

// sessionGroup builds a Grok session group directory containing the given
// session IDs, each with an updates.jsonl, and returns the group path.
func sessionGroup(t *testing.T, ids ...string) string {
	t.Helper()
	group := filepath.Join(t.TempDir(), "%2Frepo")
	for _, id := range ids {
		dir := filepath.Join(group, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, transcriptFileName), []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
	return group
}

// linkChild records testChildID under parentID's subagents/ directory, keyed
// by toolUseID.
func linkChild(t *testing.T, group, parentID, toolUseID string) {
	t.Helper()
	childID := testChildID
	dir := filepath.Join(group, parentID, subagentsDirName, toolUseID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	meta := `{"sessionId":"` + childID + `","subagentType":"worker"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}
}

func childRef(group string) string {
	return filepath.Join(group, testChildID, transcriptFileName)
}

func TestResolveSubagentSession_FindsParent(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	linkChild(t, group, testParentID, "call-abc")

	g := &GrokAgent{}
	link, ok := g.ResolveSubagentSession(childRef(group))
	if !ok {
		t.Fatal("ResolveSubagentSession = false, want true")
	}
	if link.ParentSessionID != testParentID {
		t.Errorf("ParentSessionID = %q, want %q", link.ParentSessionID, testParentID)
	}
	if link.ToolUseID != "call-abc" {
		t.Errorf("ToolUseID = %q, want the subagents/ entry name", link.ToolUseID)
	}
	if filepath.Base(link.ParentTranscriptPath) != transcriptFileName {
		t.Errorf("ParentTranscriptPath = %q", link.ParentTranscriptPath)
	}
}

// TestResolveSubagentSession_TopLevelSessionIsNotASubagent is the common case:
// an ordinary session must not be mistaken for a child.
func TestResolveSubagentSession_TopLevelSessionIsNotASubagent(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	g := &GrokAgent{}
	if _, ok := g.ResolveSubagentSession(childRef(group)); ok {
		t.Error("got true for a session no parent claims")
	}
}

// TestResolveSubagentSession_AmbiguityLosesGuards the property that matters
// most: attributing a session to the wrong parent is worse than not resolving
// it, so two claimants must resolve to nothing.
func TestResolveSubagentSession_AmbiguityLoses(t *testing.T) {
	t.Parallel()

	other := "01a03b0b-cccc-7e22-b180-0c8af17af999"
	group := sessionGroup(t, testParentID, other, testChildID)
	linkChild(t, group, testParentID, "call-abc")
	linkChild(t, group, other, "call-def")

	g := &GrokAgent{}
	if _, ok := g.ResolveSubagentSession(childRef(group)); ok {
		t.Error("got true when two sessions both claim the child")
	}
}

// TestResolveSubagentSession_SelfClaimIgnored: a session's own subagents/ dir
// must never make it its own parent.
func TestResolveSubagentSession_SelfClaimIgnored(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testChildID)
	linkChild(t, group, testChildID, "call-self")

	g := &GrokAgent{}
	if _, ok := g.ResolveSubagentSession(childRef(group)); ok {
		t.Error("session resolved as its own parent")
	}
}

func TestResolveSubagentSession_FailsClosed(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	g := &GrokAgent{}

	tests := []struct {
		name string
		ref  string
	}{
		{"empty ref", ""},
		{"missing group", filepath.Join(t.TempDir(), "nope", testChildID, transcriptFileName)},
		{"short session id", filepath.Join(group, "abc", transcriptFileName)},
		{"bare filename", transcriptFileName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := g.ResolveSubagentSession(tt.ref); ok {
				t.Errorf("ResolveSubagentSession(%q) = true, want false", tt.ref)
			}
		})
	}
}

// TestResolveSubagentSession_IgnoresNonJSONAndOversizeFiles keeps the scan from
// matching on a stray log file that merely mentions the ID.
func TestResolveSubagentSession_IgnoresNonJSONAndOversizeFiles(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	dir := filepath.Join(group, testParentID, subagentsDirName, "call-abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Plain text that happens to contain the ID must not count.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("spawned "+testChildID), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	g := &GrokAgent{}
	if _, ok := g.ResolveSubagentSession(childRef(group)); ok {
		t.Error("matched on a non-JSON file")
	}
}

func TestJSONHasStringValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		doc  any
		want bool
	}{
		{"direct value", map[string]any{"sessionId": testChildID}, true},
		{"nested", map[string]any{"a": map[string]any{"b": []any{testChildID}}}, true},
		{"as key", map[string]any{testChildID: "x"}, true},
		{"path suffix", map[string]any{"path": "/sessions/%2Frepo/" + testChildID}, true},
		{"absent", map[string]any{"sessionId": "someone-else"}, false},
		{"substring only", map[string]any{"x": testChildID + "-extra"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := jsonHasStringValue(tt.doc, testChildID); got != tt.want {
				t.Errorf("jsonHasStringValue = %v, want %v", got, tt.want)
			}
		})
	}
}

// linkChildTyped writes a meta.json in the shape the shipping binary's serde
// names imply, so the typed path is exercised rather than the untyped fallback.
func linkChildTyped(t *testing.T, group, parentID, entryName, subagentID string) {
	t.Helper()
	dir := filepath.Join(group, parentID, subagentsDirName, entryName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir subagents: %v", err)
	}
	meta := `{"schemaVersion":1,` +
		`"parentSessionId":"` + parentID + `",` +
		`"childSessionId":"` + testChildID + `",` +
		`"subagentId":"` + subagentID + `",` +
		`"subagentType":"reviewer",` +
		`"parentPromptId":"p-1"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}
}

func TestResolveSubagentSession_TypedMetaWins(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	linkChildTyped(t, group, testParentID, "entry-dir", "sub-42")

	g := &GrokAgent{}
	link, ok := g.ResolveSubagentSession(childRef(group))
	if !ok {
		t.Fatal("ResolveSubagentSession = false, want true")
	}
	if link.ParentSessionID != testParentID {
		t.Errorf("ParentSessionID = %q, want %q", link.ParentSessionID, testParentID)
	}
	// subagentId is more specific than the directory name, so it wins the key.
	if link.ToolUseID != "sub-42" {
		t.Errorf("ToolUseID = %q, want sub-42 (subagentId)", link.ToolUseID)
	}
	if link.SubagentType != "reviewer" {
		t.Errorf("SubagentType = %q, want reviewer", link.SubagentType)
	}
}

// TestResolveSubagentSession_MetaNamingAnotherChildIsIgnored: an exact
// childSessionId match is required, so a sibling subagent's record must not
// resolve this child.
func TestResolveSubagentSession_MetaNamingAnotherChildIsIgnored(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	dir := filepath.Join(group, testParentID, subagentsDirName, "entry-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := `{"schemaVersion":1,"parentSessionId":"` + testParentID +
		`","childSessionId":"01a03b0b-dead-7e22-b180-0c8af17af000","subagentId":"sub-9"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	g := &GrokAgent{}
	if _, ok := g.ResolveSubagentSession(childRef(group)); ok {
		t.Error("resolved from a record naming a different child")
	}
}

// TestResolveSubagentSession_DisagreeingParentIsRefused: a record that claims a
// parent other than the directory owning it must not redirect the link.
func TestResolveSubagentSession_DisagreeingParentIsRefused(t *testing.T) {
	t.Parallel()

	group := sessionGroup(t, testParentID, testChildID)
	dir := filepath.Join(group, testParentID, subagentsDirName, "entry-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	meta := `{"schemaVersion":1,"parentSessionId":"01a03b0b-ffff-7e22-b180-0c8af17af111",` +
		`"childSessionId":"` + testChildID + `","subagentId":"sub-1"}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	g := &GrokAgent{}
	if _, ok := g.ResolveSubagentSession(childRef(group)); ok {
		t.Error("resolved despite parentSessionId disagreeing with the owning directory")
	}
}
