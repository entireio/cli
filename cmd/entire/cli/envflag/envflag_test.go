package envflag

import "testing"

func TestTruthy(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"FALSE": false,
		"False": false,
		"1":     true,
		"true":  true,
		"yes":   true,
		"00":    true,
	} {
		if got := Truthy(value); got != want {
			t.Errorf("Truthy(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestEnabled(t *testing.T) {
	t.Setenv("ENVFLAG_TEST", "0")
	if Enabled("ENVFLAG_TEST") {
		t.Error(`Enabled with "0" = true, want false`)
	}
	t.Setenv("ENVFLAG_TEST", "1")
	if !Enabled("ENVFLAG_TEST") {
		t.Error(`Enabled with "1" = false, want true`)
	}
}
