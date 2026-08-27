package goose

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// TestResolveSessionFile_FilenameComponent_GuardedByValidator pins the security
// contract for Goose's ResolveSessionFile: agentSessionID becomes a filename
// component (<dir>/<id>.json). A traversal sequence therefore escapes the
// session directory, so the shared validator must reject it before the ID gets
// that far.
//
// This test fails if the validator stops rejecting traversal (regressing the
// guard) or if the layout changes such that the ID is no longer a path component
// without a matching guard update.
func TestResolveSessionFile_FilenameComponent_GuardedByValidator(t *testing.T) {
	t.Parallel()

	ag := &GooseAgent{}
	sessionDir := "/home/user/.cache/entire-goose"

	escaped := ag.ResolveSessionFile(sessionDir, filepath.Join("..", "..", "evil"))
	rel, err := filepath.Rel(sessionDir, escaped)
	if err != nil {
		t.Fatalf("filepath.Rel(%q, %q) error: %v", sessionDir, escaped, err)
	}
	if !strings.HasPrefix(rel, "..") {
		t.Fatalf("expected %q to escape %q, but it did not (rel=%q)", escaped, sessionDir, rel)
	}

	// The shared validator is the guard that stops such an ID reaching here.
	for _, bad := range []string{"..", "../evil", "a/b", `..\evil`} {
		if err := validation.ValidateSessionID(bad); err == nil {
			t.Errorf("ValidateSessionID(%q) = nil; the validator MUST reject it to guard this path-component footgun", bad)
		}
	}
}

// The two entry points that turn a hook-supplied session ID into a filesystem
// path must both validate. These are the paths untrusted input actually travels.
func TestSessionPathHelpers_ValidateBeforeJoining(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ag := &GooseAgent{}

	if _, err := sessionTranscriptPath(ctx, "../../etc/passwd"); err == nil {
		t.Error("sessionTranscriptPath accepted a traversal session ID")
	}
	if err := ag.fetchAndCacheExport(ctx, "../../etc/passwd"); err == nil {
		t.Error("fetchAndCacheExport accepted a traversal session ID")
	}
}
