// Package onboarding models the setup ladder shared by `entire enable` and
// `entire status`: an ordered list of rungs (hooks, auth, mirror, import),
// each recomputing done/missing from ground truth on every check. Keeping the
// model in one place guarantees enable's prompts, its closing summary, and
// status's checklist can never disagree about what is connected.
package onboarding

import "context"

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
	// StateBlocked means a prerequisite rung is missing (e.g. mirror needs login).
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
	// Hint is the exact command to run later when the rung is skipped/missing,
	// e.g. "entire auth login".
	Hint string
}

// Rung is one step of the setup ladder, defined as data rather than an
// interface so ladders stay trivially composable and testable.
type Rung struct {
	Key   string
	Title string
	// Check recomputes the rung's state from ground truth. It must not prompt.
	Check func(ctx context.Context) Check
	// Offer runs the rung's interactive setup step. Nil means the rung has no
	// inline offer (e.g. import until the enable-time offer lands).
	Offer func(ctx context.Context) error
}

// Ladder is an ordered set of rungs.
type Ladder []Rung

// Result pairs a rung with its check outcome.
type Result struct {
	Rung  Rung
	Check Check
}

// Checks runs every rung's Check in order.
func (l Ladder) Checks(ctx context.Context) []Result {
	results := make([]Result, 0, len(l))
	for _, r := range l {
		results = append(results, Result{Rung: r, Check: r.Check(ctx)})
	}
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
