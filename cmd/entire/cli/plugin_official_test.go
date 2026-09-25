package cli

import (
	"slices"
	"testing"
)

func TestIsOfficialPlugin(t *testing.T) {
	// Snapshot the allowlist so the test is independent of shipped plugins.
	// Cannot t.Parallel — mutates package-level state.
	saved := officialPlugins
	t.Cleanup(func() { officialPlugins = saved })

	officialPlugins = []string{"pgr", "stack"}

	cases := []struct {
		name string
		want bool
	}{
		{"pgr", true},
		{"stack", true},
		{"PGR", false},  // case-sensitive
		{"pgr2", false}, // exact match only
		{"", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := IsOfficialPlugin(tc.name); got != tc.want {
			t.Errorf("IsOfficialPlugin(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestOfficialPlugins_ShippedListIsWellFormed checks the real allowlist, which
// TestIsOfficialPlugin deliberately snapshots away.
//
// A malformed entry fails silently: IsOfficialPlugin simply never matches, so
// the plugin runs untracked and the only symptom is telemetry that never
// arrives. Nothing else validates these strings, because every other entry
// point takes a name from the dispatcher rather than from here.
func TestOfficialPlugins_ShippedListIsWellFormed(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(officialPlugins))
	for i, name := range officialPlugins {
		// The same rules the dispatcher applies, so an entry here can actually
		// correspond to a plugin it will resolve — including the reserved
		// `agent-` prefix, which belongs to the external agent protocol.
		if err := validatePluginName(name); err != nil {
			t.Errorf("officialPlugins[%d]: %v", i, err)
		}
		if seen[name] {
			t.Errorf("officialPlugins[%d]: %q listed more than once", i, name)
		}
		seen[name] = true
	}

	if !slices.IsSorted(officialPlugins) {
		t.Errorf("officialPlugins is not sorted: %v", officialPlugins)
	}
}
