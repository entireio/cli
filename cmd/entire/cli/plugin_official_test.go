package cli

import "testing"

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
