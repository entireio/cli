package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Output struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
}

type Option func(*runConfig)
type runConfig struct {
	Model          string
	PermissionMode string
	PromptTimeout  time.Duration
}

func WithPromptTimeout(d time.Duration) Option {
	return func(c *runConfig) { c.PromptTimeout = d }
}

// PromptTimeoutEnv is the environment variable that overrides every agent's
// per-prompt timeout. It is documented in e2e/README.md and CLAUDE.md.
const PromptTimeoutEnv = "E2E_TIMEOUT"

// promptTimeout resolves the per-prompt deadline for a single RunPrompt call.
//
// Precedence is agentDefault < E2E_TIMEOUT < WithPromptTimeout, so the
// environment widens a runner's own default and an individual test still has
// the last word. Every runner resolves through here; before this existed each
// one open-coded the chain, and the copies drifted — only opencode read
// E2E_TIMEOUT, and five runners accepted WithPromptTimeout and then ignored it.
//
// A zero result means "impose no per-prompt bound", which is how the runners
// that never had one stay that way: for them the scenario context from
// ForEachAgent remains the only deadline unless someone asks for a tighter
// one. Callers must therefore branch on the result rather than passing it
// straight to context.WithTimeout, where zero means "already expired".
//
// A malformed E2E_TIMEOUT is an error, not a fallback to the default. Silently
// ignoring it is how you widen a budget, watch the run fail at the old ceiling
// anyway, and conclude the agent is slow.
func promptTimeout(agentDefault time.Duration, cfg *runConfig) (time.Duration, error) {
	timeout := agentDefault
	if v := strings.TrimSpace(os.Getenv(PromptTimeoutEnv)); v != "" {
		parsed, err := time.ParseDuration(v)
		if err != nil {
			return 0, fmt.Errorf("%s=%q is not a valid duration (e.g. 90s, 4m): %w", PromptTimeoutEnv, v, err)
		}
		if parsed <= 0 {
			return 0, fmt.Errorf("%s=%q must be positive", PromptTimeoutEnv, v)
		}
		timeout = parsed
	}
	if cfg != nil && cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}
	return timeout, nil
}

// boundPrompt applies the resolved per-prompt timeout to ctx, for the runners
// that want nothing from it but a deadline. The returned cancel is always
// non-nil, so callers defer it unconditionally.
//
// This exists so the "zero means leave ctx alone" rule from promptTimeout is
// implemented once. It was hand-copied into five runners at first, and a rule
// restated in five places is the shape of the drift this whole change is
// undoing.
//
// Runners that need the duration itself — cursor computes an absolute deadline
// from it, and codex, copilot-cli and gemini derive a separate promptCtx — call
// promptTimeout directly.
func boundPrompt(ctx context.Context, agentDefault time.Duration, cfg *runConfig) (context.Context, context.CancelFunc, error) {
	timeout, err := promptTimeout(agentDefault, cfg)
	if err != nil {
		return ctx, func() {}, err
	}
	if timeout <= 0 {
		return ctx, func() {}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, nil
}

type Agent interface {
	Name() string
	// Binary returns the CLI binary name (e.g. "claude", "gemini").
	Binary() string
	EntireAgent() string
	PromptPattern() string
	// TimeoutMultiplier returns a factor applied to per-test timeouts.
	// Slower agents (e.g. Gemini) return values > 1.
	TimeoutMultiplier() float64
	RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error)
	StartSession(ctx context.Context, dir string) (Session, error)
	// Bootstrap performs one-time CI setup (auth config, warmup, etc.).
	// Called before any tests run. Implementations should be idempotent.
	Bootstrap() error
	// IsTransientError returns true if the error from RunPrompt looks like
	// a transient API failure (e.g. 500, rate limit, network error) that
	// is worth retrying.
	IsTransientError(out Output, err error) bool
}

type Session interface {
	Send(input string) error
	WaitFor(pattern string, timeout time.Duration) (string, error)
	Capture() string
	Close() error
}

// ExternalAgent is an optional interface for agents discovered via the
// external agent protocol (entire-agent-* binaries). SetupRepo uses this
// to pre-configure external_agents in settings before running `entire enable`.
type ExternalAgent interface {
	IsExternalAgent() bool
}

// RepoSeeder is implemented by an agent that needs files planted in a fresh
// test repo before it first runs there. SetupRepo calls SeedRepo once the repo
// exists and `entire enable` has written the agent's own config into it.
//
// Seeding is best-effort by contract: an agent that cannot seed should return
// nil and leave the repo alone, so the tests degrade to whatever the agent does
// for itself rather than failing on setup.
type RepoSeeder interface {
	SeedRepo(dir string) error
}

var registry []Agent
var gates = map[string]chan struct{}{}

func Register(a Agent) {
	registry = append(registry, a)
}

// RegisterGate sets a concurrency limit for an agent's tests.
// Tests call AcquireSlot/ReleaseSlot to respect this limit.
// The limit can be overridden via E2E_CONCURRENT_TEST_LIMIT.
func RegisterGate(name string, defaultMax int) {
	max := defaultMax
	if v, err := strconv.Atoi(os.Getenv("E2E_CONCURRENT_TEST_LIMIT")); err == nil && v > 0 {
		max = v
	}
	gates[name] = make(chan struct{}, max)
}

// AcquireSlot blocks until a test slot is available for the agent or the
// context is cancelled. Returns a non-nil error if the context expires
// before a slot opens.
func AcquireSlot(ctx context.Context, a Agent) error {
	g, ok := gates[a.Name()]
	if !ok {
		return nil
	}
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseSlot frees a test slot for the agent.
func ReleaseSlot(a Agent) {
	if g, ok := gates[a.Name()]; ok {
		<-g
	}
}

func All() []Agent {
	return registry
}

// runCapture runs cmd and returns its exit code (-1 when the process
// didn't produce one) and the run error. When promptCtx hit its deadline
// the error wraps context.DeadlineExceeded so IsTransientError can detect
// it — cmd.Run reports "signal: killed" in that case, not the context error.
func runCapture(cmd *exec.Cmd, promptCtx context.Context) (int, error) {
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	exitCode := -1
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	}
	if errors.Is(promptCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("%w: %w", err, context.DeadlineExceeded)
	}
	return exitCode, err
}

// filterEnv returns env with entries matching any of the given variable names
// removed. Used to strip test-only overrides (e.g. ENTIRE_TEST_TTY) from agent
// processes so they exercise real detection paths.
func filterEnv(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		skip := false
		for _, name := range names {
			if strings.HasPrefix(e, name+"=") {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, e)
		}
	}
	return out
}
