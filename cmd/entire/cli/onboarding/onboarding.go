// Package onboarding models the setup ladder shared by `entire enable` and
// `entire status`: an ordered list of rungs (hooks, auth, mirror, import),
// each recomputing done/missing from ground truth — or a short-TTL cache of
// it, for network probes — on every check. Keeping the
// model in one place guarantees enable's prompts, its closing summary, and
// status's checklist can never disagree about what is connected.
package onboarding

import (
	"context"
	"sync"
)

// Rung keys — stable identifiers shared by the ladder, enable's offer map,
// the status renderer, and the `status --json` setup object.
const (
	KeyHooks  = "hooks"
	KeyAuth   = "auth"
	KeyMirror = "mirror"
	KeyImport = "import"
)

// State classifies a rung's ground-truth check result.
type State int

const (
	// StateUnknown means the check could not run (e.g. offline mirror probe).
	StateUnknown State = iota
	// StateDone means the rung is satisfied.
	StateDone
	// StateMissing means the rung is actionable now.
	StateMissing
	// StateBlocked means the rung cannot be fixed by an offer right now:
	// a prerequisite rung is missing (e.g. mirror needs login), or recovery
	// is out of the user's hands (e.g. an admin-suspended mirror).
	StateBlocked
	// StateNotApplicable means the rung does not apply here (e.g. non-GitHub origin).
	StateNotApplicable
)

// String returns the stable machine-readable name used in `status --json`.
func (s State) String() string {
	switch s {
	case StateDone:
		return "done"
	case StateMissing:
		return "missing"
	case StateBlocked:
		return "blocked"
	case StateNotApplicable:
		return "not_applicable"
	case StateUnknown:
		return "unknown"
	}
	return "unknown"
}

// Check is the outcome of one rung's status probe.
type Check struct {
	State State
	// Detail is a short human fragment for checklist rows, e.g. a login handle
	// or "12 sessions found, not imported".
	Detail string
	// Hint is the exact command to run later when the rung is skipped/missing
	// (e.g. "entire auth login"), or the retry/diagnose command when the
	// check couldn't run (rendered as "couldn't check — retry with ...").
	Hint string
}

// Rung is one step of the setup ladder, defined as data rather than an
// interface so ladders stay trivially composable and testable.
type Rung struct {
	Key   string
	Title string
	// Check recomputes the rung's state from ground truth. It must not prompt.
	// Offers (the interactive setup steps) are wired separately in the enable
	// runner's offer map, keyed by Rung.Key.
	Check func(ctx context.Context) Check
}

// Ladder is an ordered set of rungs.
type Ladder []Rung

// Result pairs a rung with its check outcome.
type Result struct {
	Rung  Rung
	Check Check
}

// Checks runs every rung's Check concurrently and returns results in ladder
// order. Concurrency keeps hot paths (`entire status`) at the latency of the
// slowest single probe instead of the sum; rung checks are independent,
// read-only ground-truth reads.
func (l Ladder) Checks(ctx context.Context) []Result {
	results := make([]Result, len(l))
	var wg sync.WaitGroup
	for i, r := range l {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = Result{Rung: r, Check: r.Check(ctx)}
		}()
	}
	wg.Wait()
	return results
}

// Summary returns how many applicable rungs are done and how many apply at
// all. NotApplicable rungs are excluded from both counts.
func Summary(results []Result) (done, total int) {
	for _, r := range results {
		if r.Check.State == StateNotApplicable {
			continue
		}
		total++
		if r.Check.State == StateDone {
			done++
		}
	}
	return done, total
}

// Complete reports whether every applicable rung is done.
func Complete(results []Result) bool {
	done, total := Summary(results)
	return done == total
}

// Missing returns the rungs that still need action, in ladder order. Blocked
// rungs are included (they become actionable once prerequisites are met);
// Unknown rungs are not, since an unreachable check proves nothing.
func Missing(results []Result) []Result {
	var missing []Result
	for _, r := range results {
		if r.Check.State == StateMissing || r.Check.State == StateBlocked {
			missing = append(missing, r)
		}
	}
	return missing
}
