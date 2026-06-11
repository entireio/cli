package copilotcli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// TestResolveSessionFile_DirComponent_GuardedByValidator pins the security
// contract for Copilot's ResolveSessionFile: it uses agentSessionID as a
// directory component (<dir>/<id>/events.jsonl). A bare ".." therefore escapes
// the session directory even though it contains no path separator, so the
// shared validator must reject it. Callers sourcing the ID from untrusted data
// must validate first.
//
// This test fails if the validator stops rejecting ".." (regressing the
// resume/rewind guard) or if the layout changes such that the ID is no longer a
// directory component without a matching guard update.
func TestResolveSessionFile_DirComponent_GuardedByValidator(t *testing.T) {
	t.Parallel()

	ag := &CopilotCLIAgent{}
	sessionDir := "/home/user/.copilot/session-state"

	// A ".." id used as a directory component escapes sessionDir.
	escaped := ag.ResolveSessionFile(sessionDir, "..")
	rel, err := filepath.Rel(sessionDir, escaped)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q) error: %v", sessionDir, escaped, err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("expected %q to escape %q, but it did not (rel=%q)", escaped, sessionDir, rel)
	}

	// The shared validator is the guard that prevents that id from reaching here.
	if err := validation.ValidateSessionID(".."); err == nil {
		t.Fatal(`ValidateSessionID("..") = nil; the validator MUST reject ".." to guard this directory-component footgun`)
	}
}
