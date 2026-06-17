// Package agentlaunch is the shared "launch a normal coding agent session
// with a composed prompt" helper, used by `entire review --fix` and
// `entire investigate fix`. Both commands feed accepted findings back into
// a follow-up coding agent without spawning a review/investigate session
// themselves.
//
// The package is a leaf so review and investigate (which depend on it)
// avoid an import cycle. The env-var names it strips live in
// cmd/entire/cli/provenance (also a leaf).
package agentlaunch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	agenttypes "github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/provenance"
)

// LaunchFixAgent starts a normal coding agent session with the given
// prompt. ENTIRE_REVIEW_* and ENTIRE_INVESTIGATE_* env entries are stripped
// from the child process so the fix session is not tagged as a review or
// investigate.
//
// agentName must be a launchable agent registry name. Returns nil on clean
// exit, or a wrapped error on cancellation / non-zero exit. Output / input
// are connected to the calling process's stdio so the user can interact
// with the fix session in their terminal.
func LaunchFixAgent(ctx context.Context, agentName string, prompt string) error {
	ag, err := agent.Get(agenttypes.AgentName(agentName))
	if err != nil {
		return fmt.Errorf("resolve fix agent %s: %w", agentName, err)
	}
	launcher, ok := agent.LauncherFor(ag.Name())
	if !ok {
		return fmt.Errorf("agent %s cannot be launched for fix sessions", agentName)
	}
	cmd, err := launcher.LaunchCmd(ctx, prompt)
	if err != nil {
		return fmt.Errorf("build fix command: %w", err)
	}
	cmd.Env = withoutReviewOrInvestigateEnv(cmd.Env)
	if len(cmd.Env) == 0 {
		cmd.Env = withoutReviewOrInvestigateEnv(os.Environ())
	}
	if err := cmd.Run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("fix agent cancelled: %w", err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("fix agent exited with status %d: %w", exitErr.ExitCode(), err)
		}
		return fmt.Errorf("run fix agent: %w", err)
	}
	return nil
}

// withoutReviewOrInvestigateEnv returns a copy of base with all
// ENTIRE_REVIEW_* and ENTIRE_INVESTIGATE_* entries removed. The returned
// slice is fresh — base is never mutated.
func withoutReviewOrInvestigateEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if provenance.IsEntry(kv) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
