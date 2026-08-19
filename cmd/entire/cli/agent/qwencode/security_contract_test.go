package qwencode

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// TestResolveSessionFile_FilenameComponent_GuardedByValidator pins the security
// contract: agentSessionID becomes a filename component (<dir>/<id>.jsonl), so a
// traversal sequence escapes the session directory and the shared validator must
// reject it before the ID reaches here.
//
// Qwen supplies transcript_path on stdin, so the common path does not build a
// path from the ID at all. This guards the restore/rewind path, which does.
func TestResolveSessionFile_FilenameComponent_GuardedByValidator(t *testing.T) {
	t.Parallel()

	a := &QwenCodeAgent{}
	sessionDir := "/home/user/.qwen/projects/repo/chats"

	escaped := a.ResolveSessionFile(sessionDir, filepath.Join("..", "..", "evil"))
	rel, err := filepath.Rel(sessionDir, escaped)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("expected %q to escape %q (rel=%q)", escaped, sessionDir, rel)
	}

	for _, bad := range []string{"..", "../evil", "a/b", `..\evil`} {
		if err := validation.ValidateSessionID(bad); err == nil {
			t.Errorf("ValidateSessionID(%q) = nil; it MUST reject this to guard the path join", bad)
		}
	}
}
