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

func TestAncestryDepth_MatchesWhenCurrentBootIDIsUnavailable(t *testing.T) {
	t.Parallel()

	ancestry := Ancestry{
		host:  "host-a",
		chain: []Identity{{PID: 42, Start: "100"}},
	}
	recorded := Identity{PID: 42, Start: "100", Boot: "boot-a", Host: "host-a"}

	if depth := ancestry.Depth(recorded); depth != 0 {
		t.Fatalf("Depth() = %d, want 0 when only the current boot ID is unavailable", depth)
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

func TestWalkAncestry_Completeness(t *testing.T) {
	t.Parallel()

	// parents maps a PID to its parent; a missing PID fails the stat.
	statFrom := func(parents map[int]int) statFunc {
		return func(pid int) (int, string, string, error) {
			ppid, ok := parents[pid]
			if !ok {
				return 0, "", "", errProcessGone
			}
			return ppid, "p", "start", nil
		}
	}

	for _, tc := range []struct {
		name     string
		parents  map[int]int
		limit    int
		wantLen  int
		complete bool
	}{
		{"reaches init", map[int]int{30: 20, 20: 1}, 64, 2, true},
		{"reaches init exactly at the limit", map[int]int{30: 20, 20: 1}, 2, 2, true},
		{"cut off by the limit", map[int]int{30: 20, 20: 10, 10: 1}, 2, 2, false},
		{"stat failure mid-walk", map[int]int{30: 20}, 64, 1, false},
		{"parent outside the PID namespace", map[int]int{30: 20, 20: 0}, 64, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chain, complete := walkAncestry(statFrom(tc.parents), 30, "", "h", tc.limit)
			if len(chain) != tc.wantLen || complete != tc.complete {
				t.Fatalf("walkAncestry = %d entries, complete=%v; want %d, %v", len(chain), complete, tc.wantLen, tc.complete)
			}
		})
	}
}

func TestAncestryExcludes(t *testing.T) {
	t.Parallel()

	ancestor := Identity{PID: 42, Start: "100", Host: "host-a"}
	complete := Ancestry{host: "host-a", boot: "boot-a", chain: []Identity{ancestor}, complete: true}
	truncated := complete
	truncated.complete = false

	for _, tc := range []struct {
		name     string
		ancestry Ancestry
		id       Identity
		want     bool
	}{
		{"absent from a complete chain", complete, Identity{PID: 7, Start: "1", Host: "host-a"}, true},
		{"an ancestor", complete, ancestor, false},
		{"recycled ancestor PID", complete, Identity{PID: 42, Start: "999", Host: "host-a"}, true},
		{"absent from a truncated chain", truncated, Identity{PID: 7, Start: "1", Host: "host-a"}, false},
		{"no start fingerprint", complete, Identity{PID: 7, Host: "host-a"}, false},
		{"no host", complete, Identity{PID: 7, Start: "1"}, false},
		{"another host", complete, Identity{PID: 7, Start: "1", Host: "host-b"}, false},
		{"another boot", complete, Identity{PID: 7, Start: "1", Host: "host-a", Boot: "boot-b"}, false},
		{"zero identity", complete, Identity{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.ancestry.Excludes(tc.id); got != tc.want {
				t.Fatalf("Excludes(%+v) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}
