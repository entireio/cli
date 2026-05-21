package agents

import "testing"

func TestAntigravityIdentity(t *testing.T) {
	t.Parallel()
	a := &Antigravity{}
	if got, want := a.Name(), "antigravity"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := a.Binary(), "agy"; got != want {
		t.Errorf("Binary() = %q, want %q", got, want)
	}
	if got, want := a.EntireAgent(), "antigravity"; got != want {
		t.Errorf("EntireAgent() = %q, want %q", got, want)
	}
	if a.TimeoutMultiplier() <= 0 {
		t.Errorf("TimeoutMultiplier() = %v, want > 0", a.TimeoutMultiplier())
	}
}
