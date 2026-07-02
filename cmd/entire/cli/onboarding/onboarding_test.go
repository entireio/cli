package onboarding

import (
	"context"
	"testing"
)

func staticRung(key string, state State) Rung {
	return Rung{
		Key:   key,
		Title: key,
		Check: func(context.Context) Check { return Check{State: state} },
	}
}

func TestLadderChecks_PreservesOrderAndPairsResults(t *testing.T) {
	t.Parallel()
	l := Ladder{
		staticRung("hooks", StateDone),
		staticRung("auth", StateMissing),
		staticRung("mirror", StateBlocked),
	}

	results := l.Checks(context.Background())

	if len(results) != 3 {
		t.Fatalf("Checks() returned %d results, want 3", len(results))
	}
	wantKeys := []string{"hooks", "auth", "mirror"}
	wantStates := []State{StateDone, StateMissing, StateBlocked}
	for i, r := range results {
		if r.Rung.Key != wantKeys[i] {
			t.Errorf("results[%d].Rung.Key = %q, want %q", i, r.Rung.Key, wantKeys[i])
		}
		if r.Check.State != wantStates[i] {
			t.Errorf("results[%d].Check.State = %v, want %v", i, r.Check.State, wantStates[i])
		}
	}
}

func TestSummary_CountsDoneOverApplicable(t *testing.T) {
	t.Parallel()
	l := Ladder{
		staticRung("hooks", StateDone),
		staticRung("auth", StateDone),
		staticRung("mirror", StateMissing),
		staticRung("import", StateBlocked),
	}

	done, total := Summary(l.Checks(context.Background()))

	if done != 2 || total != 4 {
		t.Errorf("Summary() = (%d, %d), want (2, 4)", done, total)
	}
}

func TestSummary_ExcludesNotApplicable(t *testing.T) {
	t.Parallel()
	l := Ladder{
		staticRung("hooks", StateDone),
		staticRung("mirror", StateNotApplicable), // e.g. non-GitHub origin
		staticRung("import", StateMissing),
	}

	done, total := Summary(l.Checks(context.Background()))

	if done != 1 || total != 2 {
		t.Errorf("Summary() = (%d, %d), want (1, 2)", done, total)
	}
}

func TestComplete_TrueWhenAllApplicableDone(t *testing.T) {
	t.Parallel()
	results := Ladder{
		staticRung("hooks", StateDone),
		staticRung("mirror", StateNotApplicable),
	}.Checks(context.Background())

	if !Complete(results) {
		t.Error("Complete() = false, want true when every applicable rung is done")
	}
}

func TestComplete_FalseWhenAnyMissingBlockedOrUnknown(t *testing.T) {
	t.Parallel()
	for _, state := range []State{StateMissing, StateBlocked, StateUnknown} {
		results := Ladder{
			staticRung("hooks", StateDone),
			staticRung("other", state),
		}.Checks(context.Background())

		if Complete(results) {
			t.Errorf("Complete() = true with a %v rung, want false", state)
		}
	}
}

func TestMissing_ReturnsOnlyActionableRungs(t *testing.T) {
	t.Parallel()
	results := Ladder{
		staticRung("hooks", StateDone),
		staticRung("auth", StateMissing),
		staticRung("mirror", StateBlocked),
		staticRung("na", StateNotApplicable),
		staticRung("unknown", StateUnknown),
	}.Checks(context.Background())

	missing := Missing(results)

	// Blocked rungs become actionable once their prerequisite is met, so they
	// count as remaining work. Unknown (e.g. offline probe) does not: we can't
	// claim work is needed when we couldn't check.
	wantKeys := []string{"auth", "mirror"}
	if len(missing) != len(wantKeys) {
		t.Fatalf("Missing() returned %d results, want %d", len(missing), len(wantKeys))
	}
	for i, r := range missing {
		if r.Rung.Key != wantKeys[i] {
			t.Errorf("missing[%d].Rung.Key = %q, want %q", i, r.Rung.Key, wantKeys[i])
		}
	}
}

func TestStateString_JSONStableNames(t *testing.T) {
	t.Parallel()
	cases := map[State]string{
		StateDone:          "done",
		StateMissing:       "missing",
		StateBlocked:       "blocked",
		StateUnknown:       "unknown",
		StateNotApplicable: "not_applicable",
	}
	for state, want := range cases {
		if got := state.String(); got != want {
			t.Errorf("State(%d).String() = %q, want %q", state, got, want)
		}
	}
}
