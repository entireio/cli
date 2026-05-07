// Package types defines the per-agent abstraction interfaces for `entire review`.
//
// AgentReviewer is the contract every supported agent (claude-code, codex,
// gemini-cli, future additions) implements in its own package. The orchestrator
// in cmd/entire/cli/review/run.go consumes this interface, never importing
// concrete agent packages — that's how new agents land as additive files
// without touching shared code.
//
// Events flow as a stream: implementations spawn the agent process, parse
// stdout into a sequence of typed Events (Started, AssistantText, ToolCall,
// Tokens, Finished, RunError), and surface them via Process.Events. Per-agent
// quirks (codex's chrome stripping, gemini's stdin requirement, claude's argv
// shape) are entirely encapsulated inside each agent's adapter — shared code
// only sees the cleaned event stream.
//
// Living in a subpackage (not the review root package) avoids import cycles:
// per-agent reviewers and the orchestrator both depend on these types
// without depending on each other.
package types

import "context"

// AgentReviewer drives a single agent's review run.
type AgentReviewer interface {
	// Name returns the agent's registry key (e.g., "claude-code", "codex",
	// "gemini-cli"). Stable identifier; do not change after release without
	// updating settings migration.
	Name() string

	// Start spawns the agent with the given run configuration. The returned
	// Process exposes streaming events via Events() and a Wait() that returns
	// when the process exits.
	//
	// Implementations MUST set the ENTIRE_REVIEW_* env vars on the spawned
	// child process (see cmd/entire/cli/review/env.go) so the agent's
	// lifecycle hooks adopt the session as a review session.
	//
	// Errors from Start indicate failure to construct or launch the process
	// (e.g., binary not on PATH at exec.Cmd.Start time, invalid argv). Once
	// Start returns nil, errors during the run flow through Process.Events
	// (as RunError) and Process.Wait.
	Start(ctx context.Context, run RunConfig) (Process, error)
}

// Process represents a running agent review.
type Process interface {
	// Events returns a channel that emits structured events as the agent
	// produces output. The channel is closed when the process exits — cleanly,
	// with error, or via context cancellation. Consumers should range over it.
	Events() <-chan Event

	// Wait blocks until the process exits and returns:
	//   - nil on clean exit (exit code 0)
	//   - ctx.Err() on cancellation
	//   - an error wrapping *exec.ExitError on non-zero exit
	//   - other error types for I/O or pipe failures
	//
	// Wait must be called exactly once per Process. It is safe to call Wait
	// after the Events channel has closed. Consumers must drain Events until
	// close before calling Wait; otherwise an implementation that forwards
	// parsed events from another goroutine may block while sending.
	Wait() error
}

// RunConfig is the immutable per-run configuration passed to AgentReviewer.Start.
//
// All fields are optional from a marshalling standpoint (zero value is valid),
// but Skills is typically non-empty in practice — it carries the skill
// invocations (e.g., "/pr-review-toolkit:review-pr") the configured agent
// should run.
type RunConfig struct {
	// PromptOverride, when non-empty, is the exact prompt sent to the agent.
	// It preserves settings.ReviewConfig.Prompt's existing verbatim-override
	// contract: configured skills are still recorded as structured metadata,
	// but they are not prepended to the prompt text.
	PromptOverride string

	// Skills are skill invocation strings passed to the agent verbatim.
	Skills []string

	// AlwaysPrompt is the per-agent always-prompt configured in settings.
	// Concatenated with Skills + PerRunPrompt + a scope clause to form the
	// composed agent prompt.
	AlwaysPrompt string

	// PerRunPrompt is optional textarea input from a single invocation.
	PerRunPrompt string

	// ScopeBaseRef is the git ref the review is scoped against (typically the
	// closest ancestor branch). Used to compose the scope clause and as the
	// base for `git diff` operations the agent may perform.
	ScopeBaseRef string

	// CheckpointContext is best-effort context derived from checkpoints in the
	// branch review scope. It is appended to generated prompts so every agent
	// can use checkpoint IDs, file summaries, and transcript lookup commands
	// while reviewing. PromptOverride remains verbatim and does not receive
	// this context.
	CheckpointContext string

	// StartingSHA is HEAD at invocation time, propagated to the lifecycle
	// hook via ENTIRE_REVIEW_STARTING_SHA so checkpoint metadata records
	// the commit that was reviewed.
	StartingSHA string
}

// Event is the sealed sum type emitted by Process.Events. The unexported
// isEvent method prevents external packages from adding event variants;
// adding a new event requires updating this file (and every consumer's
// type switch — that's intentional, since a new event variant is a contract
// change).
type Event interface{ isEvent() }

// Started signals the agent has begun the review. Adapters typically emit
// this once at the top of the event stream.
type Started struct{}

func (Started) isEvent() {}

// AssistantText is narrative output from the agent (the actual review
// response, not tool-call chrome or session metadata).
type AssistantText struct {
	Text string
}

func (AssistantText) isEvent() {}

// ToolCall reports a tool invocation by the agent. Args is an opaque
// adapter-defined string — typically JSON for agents that emit structured
// tool input (e.g., claude-code), or a single-line summary for those that
// don't (e.g., codex). Shared consumers should treat Args as display-only
// and not parse it; structured tool data is intentionally not part of this
// contract.
type ToolCall struct {
	Name string
	Args string
}

func (ToolCall) isEvent() {}

// Tokens reports cumulative token counts (running totals, not deltas).
// Adapters may emit multiple Tokens events during a run; each emission
// supersedes the prior, so consumers should overwrite (not sum) on receipt.
// Adapters that only receive aggregate shutdown totals should emit at most one
// final Tokens event rather than inventing deltas.
type Tokens struct {
	In  int
	Out int
}

func (Tokens) isEvent() {}

// Finished signals the agent has produced its final output and the event
// stream is about to end. Success indicates the agent self-reported a
// clean task completion. Success=false means the agent finished its event
// stream but signaled it did not complete the review task — distinct from
// process exit failure (which surfaces via Process.Wait, not as an event).
//
// Adapters typically emit this once at the bottom of the event stream,
// before the channel closes.
type Finished struct {
	Success bool
}

func (Finished) isEvent() {}

// RunError reports a non-fatal error during the run (e.g., hook failure,
// transient stream parse error). Fatal errors that stop the agent surface
// via Process.Wait, not as events.
type RunError struct {
	Err error
}

func (RunError) isEvent() {}
