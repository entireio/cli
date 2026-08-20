package cli

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/benchutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// BenchmarkEnableCommand benchmarks the non-interactive enable path
// (setupAgentHooksNonInteractive) which is the hot path for `entire enable --agent claude-code`.
//
// Cannot use t.Parallel() because os.Chdir is process-global state.
func BenchmarkEnableCommand(b *testing.B) {
	ag, err := agent.Get(agent.AgentNameClaudeCode)
	if err != nil {
		b.Fatalf("get agent: %v", err)
	}

	b.Run("NewRepo_ClaudeCode", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{})
			//nolint:usetesting // b.Chdir() restores only once at cleanup; we need a fresh dir each iteration
			if err := os.Chdir(repo.Dir); err != nil {
				b.Fatalf("chdir: %v", err)
			}
			paths.ClearWorktreeRootCache()
			strategy.ClearHooksDirCache()
			b.StartTimer()

			w := &bytes.Buffer{}
			if err := setupAgentHooksNonInteractive(context.Background(), w, ag, EnableOptions{}); err != nil {
				b.Fatalf("setupAgentHooksNonInteractive: %v", err)
			}
		}
	})

	b.Run("ReEnable_ClaudeCode", func(b *testing.B) {
		b.StopTimer()
		repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{})
		b.Chdir(repo.Dir)
		paths.ClearWorktreeRootCache()
		strategy.ClearHooksDirCache()

		// First enable to set up everything
		w := &bytes.Buffer{}
		if err := setupAgentHooksNonInteractive(context.Background(), w, ag, EnableOptions{}); err != nil {
			b.Fatalf("initial enable: %v", err)
		}
		b.StartTimer()

		for b.Loop() {
			b.StopTimer()
			paths.ClearWorktreeRootCache()
			strategy.ClearHooksDirCache()
			b.StartTimer()

			w.Reset()
			if err := setupAgentHooksNonInteractive(context.Background(), w, ag, EnableOptions{}); err != nil {
				b.Fatalf("setupAgentHooksNonInteractive: %v", err)
			}
		}
	})
}
