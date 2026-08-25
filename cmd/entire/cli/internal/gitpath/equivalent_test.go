package gitpath

import "testing"

func TestEquivalent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "exact", a: ".entire/settings.local.json", b: ".entire/settings.local.json", want: true},
		{name: "case folded", a: ".Entire/Settings.Local.json", b: ".entire/settings.local.json", want: true},
		{name: "Win32 trailing characters", a: ".entire/settings.local.json. ", b: ".entire/settings.local.json", want: true},
		{name: "different path", a: ".entire/settings.json", b: ".entire/settings.local.json", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Equivalent(tt.a, tt.b); got != tt.want {
				t.Fatalf("Equivalent(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
