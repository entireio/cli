package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/binding"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

const (
	bindingReplayEnv        = "ENTIRE_BINDING_REPLAY"
	bindingReplayPrimaryEnv = "ENTIRE_BINDING_PRIMARY"

	// bindingReplayTimeout bounds one replayed hook child. The parent hook is
	// itself running under the agent's Stop deadline (commonly 60s; Codex kills
	// at 3s and the replay is skipped there by the budgeter), so an unbounded
	// wait on a child stuck in a git walk or a lock would carry the whole hook
	// past that deadline. Replays run in parallel, so the wall-clock cost is
	// one timeout, not one per target.
	bindingReplayTimeout = 30 * time.Second
	// bindingReplayStderrCap bounds how much of a failed child's stderr is
	// kept for the log line.
	bindingReplayStderrCap = 4 << 10
)

type bindingTurnCollectorKey struct{}

// bindingTurnCollector is invocation-local. It records only repos whose
// additive state replication succeeded, ordered by their most recent evidence
// in this TurnEnd payload. That makes its last entry the token-primary repo and
// every entry a safe replay target.
//
// Every method is nil-safe: bindingTurnCollectorFromContext returns nil when
// the hook entry point did not install a collector (tests, non-hook callers of
// the lifecycle handlers), and a nil collector behaves as an empty one —
// nothing recorded, no primary, no replay targets. Callers therefore never
// guard for nil; TestSelectBindingTurnPrimary_NilCollectorIsSafe pins this.
type bindingTurnCollector struct {
	mu      sync.Mutex
	repos   map[string]binding.Evidence
	order   []string
	primary string
}

func newBindingTurnCollector() *bindingTurnCollector {
	return &bindingTurnCollector{repos: make(map[string]binding.Evidence)}
}

func withBindingTurnCollector(ctx context.Context, collector *bindingTurnCollector) context.Context {
	return context.WithValue(ctx, bindingTurnCollectorKey{}, collector)
}

func bindingTurnCollectorFromContext(ctx context.Context) *bindingTurnCollector {
	collector, ok := ctx.Value(bindingTurnCollectorKey{}).(*bindingTurnCollector)
	if !ok {
		return nil
	}
	return collector
}

func (c *bindingTurnCollector) recordSuccessfulReplay(ev binding.Evidence) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	commonDir := ev.Repo.CommonDir
	if _, exists := c.repos[commonDir]; exists {
		for i, existing := range c.order {
			if existing == commonDir {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.repos[commonDir] = ev
	c.order = append(c.order, commonDir)
	c.primary = commonDir
}

func (c *bindingTurnCollector) hasReplay(commonDir string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.repos[commonDir]
	return ok
}

func (c *bindingTurnCollector) setPrimary(commonDir string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.primary = commonDir
	c.mu.Unlock()
}

func (c *bindingTurnCollector) replaySnapshot() ([]binding.Evidence, string) {
	if c == nil {
		return nil, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	targets := make([]binding.Evidence, 0, len(c.order))
	for _, commonDir := range c.order {
		targets = append(targets, c.repos[commonDir])
	}
	return targets, c.primary
}

var runBindingReplayHook = executeBindingReplayHook

// replayBindingTurn runs each target's ordinary hook pipeline in that
// worktree. Replays are parallel because Stop hooks commonly have host-side
// deadlines, while the repositories and their session locks are independent.
// Failures are best-effort and never replace the original hook's result.
func replayBindingTurn(ctx context.Context, collector *bindingTurnCollector, agentName, hookName string, payload []byte) {
	if os.Getenv(bindingReplayEnv) == "1" {
		return
	}
	targets, primary := collector.replaySnapshot()
	if len(targets) == 0 {
		return
	}
	logCtx := logging.WithComponent(ctx, "binding")
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			isPrimary := target.Repo.CommonDir == primary
			if err := runBindingReplayHook(ctx, target.Repo.WorktreeRoot, agentName, hookName, payload, isPrimary); err != nil {
				// Warn, not Debug: a failed replay is a turn the target repo
				// silently missed a checkpoint for, and the launching repo's
				// log is the only place it can be seen.
				logging.Warn(logCtx, "per-repo turn-end replay failed; target repo missed this turn's checkpoint",
					slog.String("repo", target.Repo.WorktreeRoot),
					slog.Bool("primary", isPrimary),
					slog.String("error", err.Error()))
			}
		}()
	}
	wg.Wait()
}

func executeBindingReplayHook(ctx context.Context, targetRoot, agentName, hookName string, payload []byte, primary bool) error {
	if testing.Testing() {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve Entire executable: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, bindingReplayTimeout)
	defer cancel()
	cmd := execx.NonInteractive(runCtx, executable, "hooks", agentName, hookName)
	cmd.Dir = targetRoot
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = io.Discard
	stderr := &cappedBuffer{limit: bindingReplayStderrCap}
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	primaryValue := "0"
	if primary {
		primaryValue = "1"
	}
	cmd.Env = append(os.Environ(), bindingReplayEnv+"=1", bindingReplayPrimaryEnv+"="+primaryValue)
	if err := cmd.Run(); err != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("run replayed hook: timed out after %s", bindingReplayTimeout)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("run replayed hook: cancelled: %w", ctx.Err())
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("run replayed hook: %w: %s", err, msg)
		}
		return fmt.Errorf("run replayed hook: %w", err)
	}
	return nil
}

// cappedBuffer keeps the first limit bytes written and drops the rest.
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		b.buf.Write(p)
	}
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }

// bindingTurnKeepsTokenUsage decides whether this repo's checkpoint for the
// turn carries the turn's tokens. One turn's tokens exist once; when the turn
// touched several repos exactly one of them — the token-primary repo, the one
// with the latest evidence — puts them on its checkpoint, so a cross-repo
// session is never double-counted in checkpoint token reports.
//
// In a replay child the parent decided: the primary flag on the environment
// is the whole answer. In the launching process, "no primary selected" is the
// ordinary single-repo turn (the collector saw no successful replica) and the
// launching repo keeps its tokens; only a primary that names ANOTHER repo
// moves them there. This affects the checkpoint delta alone — see
// strategy.StepContext.TokensAttributedElsewhere — the session-wide total in
// every repo's state keeps accumulating, since the transcript is shared and
// `entire status` reports it per repo.
func bindingTurnKeepsTokenUsage(ctx context.Context, currentCommonDir string) bool {
	if os.Getenv(bindingReplayEnv) == "1" {
		return os.Getenv(bindingReplayPrimaryEnv) == "1"
	}
	collector := bindingTurnCollectorFromContext(ctx)
	_, primary := collector.replaySnapshot()
	return primary == "" || primary == currentCommonDir
}

func bindingReplayActive() bool {
	return os.Getenv(bindingReplayEnv) == "1"
}

// replicaDirtyTrackedBaseline returns the tracked paths that were already
// dirty in this worktree when the bound session was replicated here (see
// State.DirtyTrackedFilesAtStart); empty when the state is missing or the
// session was launched here.
func replicaDirtyTrackedBaseline(ctx context.Context, sessionID string) []string {
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil || state == nil {
		return nil
	}
	return state.DirtyTrackedFilesAtStart
}

// excludeFiles returns files without any entry of exclude, preserving order.
func excludeFiles(files, exclude []string) []string {
	if len(exclude) == 0 || len(files) == 0 {
		return files
	}
	drop := make(map[string]struct{}, len(exclude))
	for _, f := range exclude {
		drop[f] = struct{}{}
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		if _, dropped := drop[f]; !dropped {
			out = append(out, f)
		}
	}
	return out
}

func filterBindingReplayNewFiles(newFiles, transcriptFiles []string) []string {
	evidenced := make(map[string]struct{}, len(transcriptFiles))
	for _, file := range transcriptFiles {
		evidenced[file] = struct{}{}
	}
	out := make([]string, 0, len(newFiles))
	for _, file := range newFiles {
		if _, ok := evidenced[file]; ok {
			out = append(out, file)
		}
	}
	return out
}

// selectBindingTurnPrimary chooses the repo owning the latest eligible path in
// transcript order. Relative paths belong to the current hook repo; absolute
// paths are resolved so nested and sibling repos retain their own identity.
func selectBindingTurnPrimary(ctx context.Context, collector *bindingTurnCollector, currentRoot string, modifiedFiles []string, currentChanged bool) string {
	current, currentOK := binding.ResolveRepoForPath(ctx, filepath.Join(currentRoot, ".git"))
	for i := len(modifiedFiles) - 1; i >= 0; i-- {
		file := modifiedFiles[i]
		if file == "" {
			continue
		}
		if !filepath.IsAbs(file) {
			if currentChanged && currentOK {
				collector.setPrimary(current.CommonDir)
				return current.CommonDir
			}
			continue
		}
		candidate, ok := binding.ResolveRepoForPath(ctx, file)
		if !ok {
			continue
		}
		if currentChanged && currentOK && candidate.CommonDir == current.CommonDir {
			collector.setPrimary(current.CommonDir)
			return current.CommonDir
		}
		if collector.hasReplay(candidate.CommonDir) {
			collector.setPrimary(candidate.CommonDir)
			return candidate.CommonDir
		}
	}
	_, primary := collector.replaySnapshot()
	if primary == "" && currentChanged && currentOK {
		collector.setPrimary(current.CommonDir)
		return current.CommonDir
	}
	return primary
}
