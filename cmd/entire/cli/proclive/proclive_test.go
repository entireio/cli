package proclive

import (
	"os"
	"testing"
)

func TestLiveness_String(t *testing.T) {
	t.Parallel()
	cases := map[Liveness]string{
		LivenessUnknown: "unknown",
		LivenessAlive:   "alive",
		LivenessDead:    "dead",
		Liveness(99):    "unknown",
	}
	for l, want := range cases {
		if got := l.String(); got != want {
			t.Errorf("Liveness(%d).String() = %q, want %q", int(l), got, want)
		}
	}
}

func TestCheck_EmptyIdentityIsUnknown(t *testing.T) {
	t.Parallel()
	if got := Check(Identity{}); got != LivenessUnknown {
		t.Errorf("Check(empty) = %v, want unknown", got)
	}
	if got := Check(Identity{PID: 0, Start: "x"}); got != LivenessUnknown {
		t.Errorf("Check(pid=0) = %v, want unknown", got)
	}
}

func TestCheck_HostMismatchIsUnknown(t *testing.T) {
	t.Parallel()
	// A recorded host that cannot match the current machine must yield Unknown
	// regardless of platform, before any process introspection happens.
	id := Identity{PID: os.Getpid(), Start: "anything", Host: "not-this-host-\x00-ever"}
	if got := Check(id); got != LivenessUnknown {
		t.Errorf("Check(host mismatch) = %v, want unknown", got)
	}
}

func TestIsTransient(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"entire", "sh", "bash", "ZSH", " dash ", "Fish", "go"} {
		if !isTransient(name) {
			t.Errorf("isTransient(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"node", "bun", "claude", "cursor", "python3", ""} {
		if isTransient(name) {
			t.Errorf("isTransient(%q) = true, want false", name)
		}
	}
}
