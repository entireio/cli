//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/benchutil"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// BenchmarkHookSessionStart measures the end-to-end latency of the
// "entire hooks claude-code session-start" subprocess.
//
// Each sub-benchmark isolates a single scaling dimension that appears
// in the session-start hot path (see hook_registry.go → lifecycle.go):
//
//   - Sessions:      store.List() called 2x, ReadDir + ReadFile + unmarshal + repo.Reference() per file
//   - SessionsXRefs: sessions × refs interaction (repo.Reference scans packed-refs per session)
//   - PackedRefs:    go-git PlainOpen + repo.Reference cost with many packed refs
//   - GitObjects:    go-git PlainOpen cost with large .git/objects packfile
//   - Subprocess:    isolates git rev-parse and binary spawn overhead
//
// Run all:
//
//	go test -tags=integration -bench=BenchmarkHookSessionStart -benchtime=3x -run='^$' -timeout=10m ./cmd/entire/cli/integration_test/...
//
// Run one dimension:
//
//	go test -tags=integration -bench=BenchmarkHookSessionStart/Subprocess -benchtime=5x -run='^$' ./cmd/entire/cli/integration_test/...
func BenchmarkHookSessionStart(b *testing.B) {
	b.Run("Sessions", benchSessions)
	b.Run("SessionsXRefs", benchSessionsXRefs)
	b.Run("PackedRefs", benchPackedRefs)
	b.Run("GitObjects", benchGitObjects)
	b.Run("Subprocess", benchSubprocessOverhead)
}

// benchSessions scales session state files in .git/entire-sessions/.
// listAllSessionStates() is called twice: once in FindMostRecentSession (logging init),
// once in CountOtherActiveSessionsWithCheckpoints. Each call does
// ReadDir + (ReadFile + JSON unmarshal + repo.Reference) per file.
func benchSessions(b *testing.B) {
	for _, n := range []int{0, 1, 5, 10, 25, 50, 100, 200} {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{
				FileCount:     10,
				FeatureBranch: "feature/bench",
			})
			for range n {
				repo.CreateSessionState(b, benchutil.SessionOpts{
					StepCount:    3,
					FilesTouched: []string{"src/file_000.go", "src/file_001.go"},
				})
			}
			runSessionStartHook(b, repo)
		})
	}
}

// benchSessionsXRefs tests the interaction between session count and ref count.
// For each session, listAllSessionStates calls repo.Reference() which scans packed-refs.
// Cost should be O(sessions × packed-refs-size).
func benchSessionsXRefs(b *testing.B) {
	type scenario struct {
		sessions int
		refs     int
	}
	scenarios := []scenario{
		{5, 10},
		{5, 500},
		{50, 10},
		{50, 500},
		{100, 500},
	}
	for _, sc := range scenarios {
		b.Run(fmt.Sprintf("s%d_r%d", sc.sessions, sc.refs), func(b *testing.B) {
			repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{
				FileCount:     10,
				FeatureBranch: "feature/bench",
			})
			for range sc.sessions {
				repo.CreateSessionState(b, benchutil.SessionOpts{
					StepCount:    3,
					FilesTouched: []string{"src/file_000.go"},
				})
			}
			repo.SeedBranches(b, "feature/team-", sc.refs)
			repo.PackRefs(b)
			runSessionStartHook(b, repo)
		})
	}
}

// benchPackedRefs scales the number of packed git refs (branches).
// go-git PlainOpen reads packed-refs, and every repo.Reference() call
// scans it. Session count held constant at 5.
func benchPackedRefs(b *testing.B) {
	for _, n := range []int{0, 50, 200, 500, 1000, 2000} {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{
				FileCount:     10,
				FeatureBranch: "feature/bench",
			})
			for range 5 {
				repo.CreateSessionState(b, benchutil.SessionOpts{
					StepCount:    3,
					FilesTouched: []string{"src/file_000.go"},
				})
			}
			if n > 0 {
				repo.SeedBranches(b, "feature/team-", n)
				repo.PackRefs(b)
			}
			runSessionStartHook(b, repo)
		})
	}
}

// benchGitObjects scales the .git/objects packfile size.
// go-git PlainOpen parses pack indexes; large packs may slow it down.
// This also affects repo.Head() and repo.Reference() indirectly.
func benchGitObjects(b *testing.B) {
	for _, n := range []int{0, 1000, 5000, 10000} {
		b.Run(fmt.Sprintf("%d", n), func(b *testing.B) {
			repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{
				FileCount:     10,
				FeatureBranch: "feature/bench",
			})
			for range 5 {
				repo.CreateSessionState(b, benchutil.SessionOpts{
					StepCount:    3,
					FilesTouched: []string{"src/file_000.go"},
				})
			}
			if n > 0 {
				repo.SeedGitObjects(b, n)
			}
			runSessionStartHook(b, repo)
		})
	}
}

// benchSubprocessOverhead isolates the cost of subprocess spawns that happen
// during session-start. The hook calls git rev-parse multiple times (some cached,
// some not) plus spawns the entire binary itself. This benchmark measures each
// component so we can see what fraction of the total is subprocess overhead.
func benchSubprocessOverhead(b *testing.B) {
	repo := benchutil.NewBenchRepo(b, benchutil.RepoOpts{
		FileCount:     10,
		FeatureBranch: "feature/bench",
	})

	// 1. Bare git rev-parse round-trip
	b.Run("GitRevParse_1x", func(b *testing.B) {
		b.ResetTimer()
		for range b.N {
			start := time.Now()
			cmd := exec.Command("git", "rev-parse", "--show-toplevel")
			cmd.Dir = repo.Dir
			cmd.Env = testutil.GitIsolatedEnv()
			if output, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("git rev-parse failed: %v\n%s", err, output)
			}
			b.ReportMetric(float64(time.Since(start).Milliseconds()), "ms/op")
		}
	})

	// 2. Seven sequential git rev-parse calls (pre-optimization baseline)
	b.Run("GitRevParse_7x", func(b *testing.B) {
		b.ResetTimer()
		for range b.N {
			start := time.Now()
			for range 7 {
				cmd := exec.Command("git", "rev-parse", "--show-toplevel")
				cmd.Dir = repo.Dir
				cmd.Env = testutil.GitIsolatedEnv()
				if output, err := cmd.CombinedOutput(); err != nil {
					b.Fatalf("git rev-parse failed: %v\n%s", err, output)
				}
			}
			b.ReportMetric(float64(time.Since(start).Milliseconds()), "ms/op")
		}
	})

	// 3. Bare `entire` binary spawn (version command — minimal work, no git)
	b.Run("EntireBinary_version", func(b *testing.B) {
		binary := getTestBinary()
		b.ResetTimer()
		for range b.N {
			start := time.Now()
			cmd := exec.Command(binary, "version")
			cmd.Dir = repo.Dir
			if output, err := cmd.CombinedOutput(); err != nil {
				b.Fatalf("entire version failed: %v\n%s", err, output)
			}
			b.ReportMetric(float64(time.Since(start).Milliseconds()), "ms/op")
		}
	})

	// 4. Full session-start hook (for direct comparison)
	b.Run("FullHook", func(b *testing.B) {
		for range 5 {
			repo.CreateSessionState(b, benchutil.SessionOpts{
				StepCount:    3,
				FilesTouched: []string{"src/file_000.go"},
			})
		}
		runSessionStartHook(b, repo)
	})
}

// runSessionStartHook is the shared benchmark loop that invokes the session-start
// hook as a subprocess and reports latency in ms/op.
func runSessionStartHook(b *testing.B, repo *benchutil.BenchRepo) {
	b.Helper()

	stdinPayload, err := json.Marshal(map[string]string{
		"session_id":      "bench-session",
		"transcript_path": "",
	})
	if err != nil {
		b.Fatalf("marshal stdin: %v", err)
	}

	binary := getTestBinary()
	claudeProjectDir := b.TempDir()

	b.ResetTimer()
	for range b.N {
		start := time.Now()

		cmd := exec.Command(binary, "hooks", "claude-code", "session-start")
		cmd.Dir = repo.Dir
		cmd.Stdin = bytes.NewReader(stdinPayload)
		cmd.Env = append(testutil.GitIsolatedEnv(),
			"ENTIRE_TEST_CLAUDE_PROJECT_DIR="+claudeProjectDir,
		)

		output, err := cmd.CombinedOutput()
		if err != nil {
			b.Fatalf("session-start hook failed: %v\nOutput: %s", err, output)
		}

		b.ReportMetric(float64(time.Since(start).Milliseconds()), "ms/op")
	}
}
