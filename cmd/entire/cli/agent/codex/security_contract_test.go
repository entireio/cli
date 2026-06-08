package codex

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// TestResolveSessionFile_RejectsAbsolute pins the security contract for Codex's
// ResolveSessionFile: an absolute agentSessionID is rejected with an error
// rather than returned verbatim. Returning it verbatim would let an
// attacker-influenceable session ID (e.g. from checkpoint metadata on the
// shared entire/checkpoints/v1 branch) resolve to an arbitrary path and, via
// the resume/rewind write path, overwrite files outside the session directory.
//
// This is defense in depth at the function itself, not just at the caller. The
// test also asserts the shared validator still rejects absolute IDs, so the
// two layers can't silently drift apart.
func TestResolveSessionFile_RejectsAbsolute(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	const abs = "/etc/evil.jsonl"

	got, err := ag.ResolveSessionFile("/home/u/.codex/sessions", abs)
	if err == nil {
		t.Fatalf("ResolveSessionFile(%q) = %q, nil; want error rejecting the absolute path", abs, got)
	}
	if got != "" {
		t.Fatalf("ResolveSessionFile(%q) returned path %q alongside error; want empty path", abs, got)
	}
	if err := validation.ValidateSessionID(abs); err == nil {
		t.Fatalf("ValidateSessionID(%q) = nil; the validator MUST also reject absolute IDs", abs)
	}
}
