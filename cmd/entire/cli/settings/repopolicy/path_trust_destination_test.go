package repopolicy

import (
	"path/filepath"
	"testing"
)

// A path-keyed identity must still name its destination.
//
// TrustIdentity's contract is "new destination, new consent" — a re-election to
// another remote changes the key and re-asks. The path fallback broke that for
// the one shape that reaches it: a filesystem remote. Two different local
// mirrors reduce to the same worktree path, so consent recorded while pushing
// to one silently covered the other, and flipping the remote in .git/config
// redirected transcripts with no prompt.
//
// The fallback itself is deliberate and stays (a local mirror must remain
// trustable and must not defeat trust_all). What is added is a discriminator:
// the destination the path consent was recorded for.
func TestEgressDecision_PathTrustDoesNotCoverADifferentMirror(t *testing.T) {
	root, _ := newPolicyRepo(t)
	mirrorA := filepath.Join(t.TempDir(), "alpha.git")
	mirrorB := filepath.Join(t.TempDir(), "beta.git")
	runPolicyGit(t, root, "remote", "add", "origin", mirrorA)

	// Consent recorded for mirror A, keyed by path plus destination.
	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_paths":["`+filepath.ToSlash(root)+
		`"]},"path_trust_destinations":{"`+filepath.ToSlash(root)+`":["`+filepath.ToSlash(mirrorA)+`"]}}`)
	if got := policyAt(t, root); !got.Trust.Allowed {
		t.Fatalf("trust = %+v, want allowed for the mirror the user consented to", got.Trust)
	}

	// Same worktree, different destination: must re-ask.
	runPolicyGit(t, root, "remote", "set-url", "origin", mirrorB)
	if got := policyAt(t, root); got.Trust.Allowed {
		t.Errorf("trust = %+v, want held: the remote now points at a mirror the user never consented to", got.Trust)
	}
}

// Entries written before the discriminator existed carry no destination and
// must keep working exactly as they did — closing the gap must not silently
// revoke consent anyone already gave.
func TestEgressDecision_LegacyPathEntryStillTrusted(t *testing.T) {
	root, _ := newPolicyRepo(t)
	runPolicyGit(t, root, "remote", "add", "origin", filepath.Join(t.TempDir(), "origin.git"))

	setPolicyGlobal(t, `{"global":{"enabled":true,"trusted_paths":["`+filepath.ToSlash(root)+`"]}}`)
	if got := policyAt(t, root); !got.Trust.Allowed || got.Trust.Source != TrustSourceRepo {
		t.Fatalf("trust = %+v, want a legacy path entry to keep working", got.Trust)
	}
}
