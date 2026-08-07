package shellhook

import (
	"fmt"
	"testing"
	"time"
)

func TestState_RoundTrip(t *testing.T) {
	isolate(t)

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := &State{}
	state.MarkWarned("/repo/.git", now)
	state.MarkDismissed("/other/.git", now)

	if err := SaveState(state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if !got.Repos["/repo/.git"].LastWarnedAt.Equal(now) {
		t.Errorf("LastWarnedAt = %v, want %v", got.Repos["/repo/.git"].LastWarnedAt, now)
	}
	if !got.IsDismissed("/other/.git") {
		t.Error("IsDismissed(/other/.git) = false, want true")
	}
	if got.DismissedCount() != 1 {
		t.Errorf("DismissedCount() = %d, want 1", got.DismissedCount())
	}
}

func TestLoadState_MissingFileIsEmpty(t *testing.T) {
	isolate(t)

	state, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if len(state.Repos) != 0 {
		t.Errorf("Repos = %v, want empty", state.Repos)
	}
}

func TestShouldWarn_Throttle(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	throttle := 24 * time.Hour
	const key = "/repo/.git"

	state := &State{}
	if !state.ShouldWarn(key, now, throttle) {
		t.Fatal("ShouldWarn on a fresh repo = false, want true")
	}

	state.MarkWarned(key, now)
	if state.ShouldWarn(key, now.Add(23*time.Hour), throttle) {
		t.Error("ShouldWarn inside the throttle window = true, want false")
	}
	if !state.ShouldWarn(key, now.Add(24*time.Hour), throttle) {
		t.Error("ShouldWarn at the throttle boundary = false, want true")
	}
	// A future timestamp (clock skew, restored backup) must not silence
	// the repository forever.
	if !state.ShouldWarn(key, now.Add(-time.Hour), throttle) {
		t.Error("ShouldWarn with LastWarnedAt in the future = false, want true")
	}
}

func TestShouldWarn_DismissedNeverWarns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := &State{}
	state.MarkDismissed("/repo/.git", now)

	if state.ShouldWarn("/repo/.git", now.Add(10*365*24*time.Hour), time.Hour) {
		t.Error("ShouldWarn on a dismissed repo = true, want false")
	}
}

func TestState_PruneKeepsMostRecent(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := &State{}
	total := MaxStateEntries + 25
	for i := range total {
		state.MarkWarned(fmt.Sprintf("/repo-%03d/.git", i), base.Add(time.Duration(i)*time.Minute))
	}

	state.prune(MaxStateEntries)

	if len(state.Repos) != MaxStateEntries {
		t.Fatalf("len(Repos) = %d, want %d", len(state.Repos), MaxStateEntries)
	}
	if _, ok := state.Repos["/repo-000/.git"]; ok {
		t.Error("oldest entry survived pruning")
	}
	newest := fmt.Sprintf("/repo-%03d/.git", total-1)
	if _, ok := state.Repos[newest]; !ok {
		t.Errorf("newest entry %q was pruned", newest)
	}
}

func TestSaveState_PrunesOnWrite(t *testing.T) {
	isolate(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	state := &State{}
	for i := range MaxStateEntries + 10 {
		state.MarkWarned(fmt.Sprintf("/repo-%03d/.git", i), base.Add(time.Duration(i)*time.Minute))
	}
	if err := SaveState(state); err != nil {
		t.Fatalf("SaveState() error = %v", err)
	}

	got, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState() error = %v", err)
	}
	if len(got.Repos) != MaxStateEntries {
		t.Errorf("len(Repos) = %d, want %d", len(got.Repos), MaxStateEntries)
	}
}

func TestLoadState_CorruptFileStartsOver(t *testing.T) {
	isolate(t)

	if err := writeFileAtomic(StatePath(), []byte("{ broken")); err != nil {
		t.Fatalf("writeFileAtomic() error = %v", err)
	}

	state, err := LoadState()
	if err != nil {
		t.Fatalf("LoadState() error = %v, want nil (corrupt cache must not be fatal)", err)
	}
	if len(state.Repos) != 0 {
		t.Errorf("Repos = %v, want empty", state.Repos)
	}
}
