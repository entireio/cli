package cli

import (
	"strings"
	"testing"
)

// The renderer prints these verbatim on both commands, so a reason that says
// "checkpoint" is a false statement on `session tokens`. Only the TTL reason is
// legitimately checkpoint-specific: live state always knows the split.
func TestUnpricedReasons_AreScopeNeutral(t *testing.T) {
	t.Parallel()

	// Any noun that presumes one command's scope is a false statement on the
	// other. "checkpoint" was the original slip; "sessions" (plural) is the one
	// the obvious reword introduces, since session tokens has exactly one.
	for name, reason := range map[string]string{
		"no model":     unpricedNoModel,
		"mixed models": unpricedMixedModels,
		"no cost":      unpricedNoCost,
	} {
		for _, presumed := range []string{"checkpoint", "sessions"} {
			if strings.Contains(reason, presumed) {
				t.Errorf("%s reason is printed by both commands and must not say %q: %q", name, presumed, reason)
			}
		}
	}

	if !strings.Contains(unpricedUnknownTTL, "checkpoint") {
		t.Error("the TTL reason is checkpoint-only by construction (live state always knows the split) and should keep saying so")
	}
}
