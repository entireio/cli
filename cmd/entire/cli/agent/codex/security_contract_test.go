package codex

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// TestResolveSessionFile_AbsoluteVerbatim_GuardedByValidator pins the security
// contract for Codex's ResolveSessionFile: an absolute agentSessionID is
// returned verbatim (a deliberate feature for agent-recorded transcript paths),
// which makes it a path-traversal footgun if fed untrusted input. Callers that
// source the ID from untrusted data (checkpoint metadata, hook input) must
// reject it first via validation.ValidateSessionID.
//
// This test fails if either the verbatim behavior changes silently OR the shared
// validator stops rejecting absolute IDs — i.e. it guards the resume/rewind fix
// from regressing out from under this agent.
func TestResolveSessionFile_AbsoluteVerbatim_GuardedByValidator(t *testing.T) {
	t.Parallel()

	ag := &CodexAgent{}
	const abs = "/etc/evil.jsonl"

	if got := ag.ResolveSessionFile("/home/u/.codex/sessions", abs); got != abs {
		t.Fatalf("ResolveSessionFile returned %q, want verbatim %q (behavior change — re-check the validator guard)", got, abs)
	}
	if err := validation.ValidateSessionID(abs); err == nil {
		t.Fatalf("ValidateSessionID(%q) = nil; the validator MUST reject absolute IDs to guard this footgun", abs)
	}
}
