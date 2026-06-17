// Package spawn provides the Spawner interface used by both `entire review`
// and `entire investigate` to start an agent process non-interactively.
//
// The interface is intentionally env-contract-agnostic: callers compose
// their own ENTIRE_REVIEW_* or ENTIRE_INVESTIGATE_* env via
// review.AppendReviewEnv or investigate.AppendInvestigateEnv before calling
// BuildCmd. Spawners only own the agent-specific argv shape and stdin
// wiring; they do not append review/investigate env.
package spawn

import (
	"context"
	"os/exec"
)

// Spawner builds *exec.Cmd values for a specific agent in non-interactive,
// review/investigate mode. The returned Cmd MUST NOT be started yet —
// callers may attach pipes, modify env, etc., before invoking Start.
type Spawner interface {
	// Name returns the agent's stable registry name (e.g. "claude-code").
	Name() string

	// BuildCmd constructs the *exec.Cmd to spawn the agent.
	//   - env: the full process environment to set on cmd.Env (the caller has
	//     already appended ENTIRE_REVIEW_* or ENTIRE_INVESTIGATE_* values
	//     and stripped any stale entries before calling).
	//   - prompt: the composed prompt string. The spawner decides whether
	//     this goes via argv or stdin per the agent's CLI shape.
	BuildCmd(ctx context.Context, env []string, prompt string) *exec.Cmd
}
