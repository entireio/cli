package investigate

// loop.go implements the round-robin investigation loop driver.
//
// The loop runs a fixed list of agents in a strict round-robin order. For
// each turn it:
//
//  1. Fingerprints the findings file BEFORE the turn.
//  2. Composes a prompt via ComposeInvestigatePrompt.
//  3. Spawns the agent via Spawner.BuildCmd with ENTIRE_INVESTIGATE_* env
//     populated by AppendInvestigateEnv.
//  4. Discards the agent's stdout/stderr — the lifecycle hooks capture the
//     full session transcript on the shadow branch and condense it onto
//     entire/checkpoints/v1 on the next commit.
//  5. Waits for the agent to exit. Re-fingerprints the findings doc.
//  6. Reloads state.json from disk. The agent has written its stance into
//     state.PendingTurn; the loop validates it, appends a TurnStance, and
//     clears PendingTurn.
//  7. Records a TurnStance in the persisted RunState and notifies the
//     ProgressSink (TUI dashboard or headless text writer).
//  8. Decides whether to terminate (quorum, stalled, paused, cancelled) or
//     advance to the next agent.
//
// The loop is single-threaded: each turn waits for the previous to exit
// before starting. This keeps the order of recorded stances deterministic
// and avoids racing two agents on the same shared findings doc.
//
// Privacy: operational metadata only. Prompts, file bodies, agent stdout,
// and commit messages are NEVER logged (CLAUDE.md privacy rule).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/spawn"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// LoopDeps collects the runtime-injectable hooks RunInvestigateLoop needs.
type LoopDeps struct {
	// SpawnerFor maps an agent name → Spawner. Returns nil for an unknown
	// agent name, in which case the loop pauses with an error.
	SpawnerFor func(agentName string) spawn.Spawner

	// States persists/loads RunState across turns. In production this is
	// a *StateStore rooted at <git-common-dir>/entire-investigations.
	States *StateStore

	// Progress receives turn lifecycle events. Production wires either a
	// tuiProgressSink (TTY) or textProgressSink (non-TTY). nil is treated
	// as nullProgressSink (no-op).
	Progress ProgressSink

	// Now returns the current time. Defaults to time.Now if nil.
	Now func() time.Time
}

// LoopInput carries everything RunInvestigateLoop needs that isn't a hook.
type LoopInput struct {
	RunID        string    // 12-hex
	Topic        string    // human-readable subject of the investigation
	Agents       []string  // ordered, length >= 1
	MaxTurns     int       // per-agent turn budget; 0 → defaultMaxTurns (2)
	Quorum       int       // approvals needed; 0 → len(Agents)
	AlwaysPrompt string    // optional, appended verbatim to every prompt
	FindingsDoc  string    // absolute path
	StartingSHA  string    // git HEAD when `entire investigate` was invoked
	Resume       *RunState // when non-nil, resume from this state
}

// LoopOutcome describes how the loop ended.
type LoopOutcome string

const (
	// OutcomeQuorum means the most recent completed round produced enough
	// approve stances to meet Quorum. The loop ends successfully.
	OutcomeQuorum LoopOutcome = "quorum"
	// OutcomeStalled means the per-agent turn budget was exhausted without
	// reaching quorum. The investigation produced findings but no
	// consensus.
	OutcomeStalled LoopOutcome = "stalled"
	// OutcomePaused means two consecutive agent invocations failed (process
	// error, non-zero exit). The loop stops so the user can investigate;
	// state is preserved for `--continue`.
	OutcomePaused LoopOutcome = "paused"
	// OutcomeCancelled means the context was cancelled (Ctrl+C, parent
	// command shutdown). State is preserved for resume.
	OutcomeCancelled LoopOutcome = "cancelled"
)

// LoopResult is the loop's final report.
type LoopResult struct {
	Outcome LoopOutcome
	State   *RunState
	// Err holds the most recent per-turn spawn error, if any. Informational:
	// when Outcome is Quorum/Stalled it is typically nil. When Outcome is
	// Paused this surfaces the underlying agent failure.
	Err error
}

// pauseAfterConsecutiveFailures is the number of back-to-back per-turn
// agent failures that trigger OutcomePaused. Two: one failure could be
// transient, two strongly suggests a configuration problem the user must
// fix before continuing.
const pauseAfterConsecutiveFailures = 2

// defaultMaxTurns is the per-agent turn budget when LoopInput.MaxTurns is 0.
const defaultMaxTurns = 2

// stanceApprove and friends pin the stance vocabulary so callers can compare
// without typo risk. The PendingTurn validator normalises to one of these
// or "unknown".
const (
	stanceApprove        = "approve"
	stanceRequestChanges = "request-changes"
	stanceReject         = "reject"
	stanceUnknown        = "unknown"
)

// RunInvestigateLoop runs the round-robin investigation loop until it
// reaches quorum, stalls, gets paused, or the context is cancelled.
//
// On every turn the function persists state via deps.States so a crash mid-
// turn leaves a recoverable RunState on disk. The returned LoopResult is
// always populated, even on context cancellation.
//
// The function returns (result, error) where error is non-nil only for
// programmer errors (invalid input, missing dependencies). Per-turn agent
// failures are reflected in result.Outcome and result.Err, not the return
// error.
func RunInvestigateLoop(ctx context.Context, in LoopInput, deps LoopDeps) (LoopResult, error) {
	if err := validateLoopInput(in); err != nil {
		return LoopResult{}, err
	}
	if deps.States == nil {
		return LoopResult{}, errors.New("LoopDeps.States is required")
	}
	if deps.SpawnerFor == nil {
		return LoopResult{}, errors.New("LoopDeps.SpawnerFor is required")
	}
	if deps.Progress == nil {
		deps.Progress = nullProgressSink{}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}

	maxTurnsPerAgent := in.MaxTurns
	if maxTurnsPerAgent == 0 {
		maxTurnsPerAgent = defaultMaxTurns
	}
	quorum := in.Quorum
	if quorum == 0 {
		quorum = len(in.Agents)
	}
	maxOverall := maxTurnsPerAgent * len(in.Agents)

	state := initLoopState(in, now, maxTurnsPerAgent, quorum)

	// Persist the initial state once so external observers (status command,
	// `--continue`) see the run as soon as the loop starts.
	if err := deps.States.Save(ctx, state); err != nil {
		return LoopResult{State: state}, fmt.Errorf("save initial run state: %w", err)
	}

	cfg := turnConfig{
		input:       in,
		deps:        deps,
		now:         now,
		quorum:      quorum,
		maxPerAgent: maxTurnsPerAgent,
		stateDoc:    deps.States.runStatePath(in.RunID),
	}
	consecutiveFails := 0
	var lastErr error

	for state.Turn < maxOverall {
		if ctx.Err() != nil {
			// Cancellation is a normal terminal outcome surfaced through
			// LoopResult.Outcome. Contract is "always-returns-result,
			// error only for programmer bugs".
			deps.Progress.RunFinished(OutcomeCancelled)
			//nolint:nilerr // ctx cancellation is reported via Outcome, not the error return
			return LoopResult{Outcome: OutcomeCancelled, State: state, Err: lastErr}, nil
		}

		outcome := runOneTurn(ctx, cfg, state)

		// ctx cancellation classifies the run regardless of the turn's
		// recorded result. cmd.Run() returns context.Canceled wrapped as
		// an *exec.ExitError when SIGINT kills the child mid-run, which
		// runOneTurn would otherwise report as a normal turn failure —
		// and two such cancels in a row would mis-trigger OutcomePaused.
		if ctx.Err() != nil {
			if saveErr := deps.States.Save(context.Background(), state); saveErr != nil {
				logging.Warn(ctx, "investigate: save state on cancel failed",
					sErr(saveErr), sRun(in.RunID))
			}
			deps.Progress.RunFinished(OutcomeCancelled)

			return LoopResult{Outcome: OutcomeCancelled, State: state, Err: lastErr}, nil
		}

		if outcome.failed {
			lastErr = outcome.err
			consecutiveFails++
			if consecutiveFails >= pauseAfterConsecutiveFailures {
				// Persist before returning paused so --continue resumes
				// from a snapshot that includes the failing turn (PR#11).
				if saveErr := deps.States.Save(ctx, state); saveErr != nil {
					logging.Warn(ctx, "investigate: save state on pause failed",
						sErr(saveErr), sRun(in.RunID))
				}
				deps.Progress.RunFinished(OutcomePaused)
				return LoopResult{Outcome: OutcomePaused, State: state, Err: lastErr}, nil
			}
		} else {
			consecutiveFails = 0
		}
		advanceAgent(state)
		if saveErr := deps.States.Save(ctx, state); saveErr != nil {
			// On-disk state is now one turn stale; if the process crashes
			// or is killed before the next Save, `--continue` will resume
			// from an older snapshot. Log at Warn so the staleness is
			// visible without aborting the run.
			logging.Warn(ctx, "investigate: save state after turn failed",
				sErr(saveErr), sRun(in.RunID))
		}
		if state.NextAgentIdx == 0 {
			if approveCountInRound(state.Stances, outcome.round) >= quorum {
				deps.Progress.RunFinished(OutcomeQuorum)
				return LoopResult{Outcome: OutcomeQuorum, State: state, Err: nil}, nil
			}
		}
	}

	deps.Progress.RunFinished(OutcomeStalled)
	return LoopResult{Outcome: OutcomeStalled, State: state, Err: lastErr}, nil
}

// turnConfig packages the immutable knobs runOneTurn needs. Splitting these
// out from RunInvestigateLoop's call frame keeps the per-turn helper
// signature tight without re-deriving values on every iteration.
type turnConfig struct {
	input       LoopInput
	deps        LoopDeps
	now         func() time.Time
	quorum      int
	maxPerAgent int
	stateDoc    string // absolute path to state.json (passed to the agent)
}

// turnOutcome reports the post-turn state runOneTurn produces. The loop
// uses these flags to drive the consecutive-failure counter and the round
// boundary check; every other side effect (state mutation, persistence,
// logging) happens inside runOneTurn.
type turnOutcome struct {
	round  int
	failed bool
	err    error
}

// runOneTurn executes a single agent turn and records the resulting
// TurnStance on state.Stances. It mutates state.Turn (incremented) and
// state.Stances; the caller is responsible for advanceAgent and the post-
// turn Save.
func runOneTurn(ctx context.Context, cfg turnConfig, state *RunState) turnOutcome {
	in := cfg.input
	deps := cfg.deps
	agentName := state.Agents[state.NextAgentIdx]
	state.Turn++
	round := ((state.Turn - 1) / len(state.Agents)) + 1

	preFindings := fileFingerprint(ctx, in.FindingsDoc)

	deps.Progress.TurnStarted(agentName, state.Turn, round, cfg.maxPerAgent)

	spawner := deps.SpawnerFor(agentName)
	if spawner == nil {
		err := fmt.Errorf("no spawner for agent %q", agentName)
		recordFailureStance(state, round, agentName, err, cfg.now)
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, 0, true, err, "")
		return turnOutcome{round: round, failed: true, err: err}
	}

	prompt := ComposeInvestigatePrompt(ComposeInput{
		Topic:        in.Topic,
		AgentName:    agentName,
		Round:        round,
		MaxTurns:     cfg.maxPerAgent,
		Turn:         state.Turn,
		AlwaysPrompt: in.AlwaysPrompt,
		Files:        Files{Findings: in.FindingsDoc, State: cfg.stateDoc},
	})
	env := AppendInvestigateEnv(os.Environ(), AppendOptions{
		AgentName:   agentName,
		RunID:       in.RunID,
		Topic:       in.Topic,
		FindingsDoc: in.FindingsDoc,
		StateDoc:    cfg.stateDoc,
		StartingSHA: in.StartingSHA,
	})
	cmd := spawner.BuildCmd(ctx, env, prompt)

	// Agent stdout/stderr are captured by the lifecycle hooks into the
	// session transcript (full.jsonl) and condensed onto
	// entire/checkpoints/v1 on commit. Discard the raw streams here.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	logging.Info(ctx, "investigate: turn start",
		sRun(in.RunID), sAgent(agentName), sTurn(state.Turn), sRound(round))

	turnStart := cfg.now()
	runErr := cmd.Run()
	turnDuration := cfg.now().Sub(turnStart)

	postFindings := fileFingerprint(ctx, in.FindingsDoc)

	if runErr != nil {
		turn := TurnStance{
			Round:       round,
			Turn:        state.Turn,
			Agent:       agentName,
			Stance:      stanceUnknown,
			PlanChanged: preFindings != postFindings,
			Note:        classifyRunErr(runErr),
		}
		state.Stances = append(state.Stances, turn)
		state.PendingTurn = nil
		updateRoundCounter(state)
		state.UpdatedAt = cfg.now()
		logging.Warn(ctx, "investigate: turn failed",
			sRun(in.RunID), sAgent(agentName),
			sTurn(state.Turn), sRound(round),
			slog.String("err", runErr.Error()))
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, turnDuration, true, runErr, "")
		return turnOutcome{round: round, failed: true, err: runErr}
	}

	// Reload state from disk: the agent (running with cfg.stateDoc on the
	// filesystem) may have written PendingTurn. Merge that into the
	// in-memory state, then clear it on disk after recording the stance.
	stance, note, hasPending := readPendingTurn(ctx, deps.States, in.RunID, state)
	turn := TurnStance{
		Round:       round,
		Turn:        state.Turn,
		Agent:       agentName,
		Stance:      stance,
		PlanChanged: preFindings != postFindings,
		Note:        note,
	}
	state.Stances = append(state.Stances, turn)
	state.PendingTurn = nil
	updateRoundCounter(state)
	state.UpdatedAt = cfg.now()
	logging.Info(ctx, "investigate: turn end",
		sRun(in.RunID), sAgent(agentName),
		sTurn(state.Turn), sRound(round),
		slog.String("stance", stance),
		slog.Bool("plan_changed", turn.PlanChanged))

	// Treat a missing pending_turn as a soft failure: the agent ran cleanly
	// but produced no structured stance, so it should not count toward
	// quorum and consecutive misses must trip pause-on-failure. The
	// TurnStance is still recorded for diagnostics, but the loop sees this
	// as a failure for budget-control purposes.
	if !hasPending {
		logging.Warn(ctx, "investigate: turn missing pending_turn",
			sRun(in.RunID), sAgent(agentName),
			sTurn(state.Turn), sRound(round))
		missingPending := errors.New("agent did not write pending_turn to state.json")
		deps.Progress.TurnFinished(agentName, state.Turn, stanceUnknown, turnDuration, true, missingPending, "")
		return turnOutcome{round: round, failed: true, err: missingPending}
	}

	deps.Progress.TurnFinished(agentName, state.Turn, stance, turnDuration, false, nil, note)
	return turnOutcome{round: round, failed: false}
}

// readPendingTurn loads the on-disk state.json (which the agent may have
// just rewritten) and returns the validated stance + note pair plus a
// "has pending" flag. The in-memory state is NOT mutated here — the caller
// owns the canonical state and clears PendingTurn after recording.
//
// Validation rules:
//   - missing file or unreadable file → ("unknown", "<diagnostic>", false)
//   - missing pending_turn field      → ("unknown", "missing pending_turn", false)
//   - stance not in the vocabulary    → ("unknown", "invalid stance: <value>", true)
//   - valid pending_turn              → (stance, note, true)
func readPendingTurn(ctx context.Context, store *StateStore, runID string, _ *RunState) (stance, note string, hasPending bool) {
	loaded, err := store.Load(ctx, runID)
	if err != nil {
		return stanceUnknown, "state read error: " + err.Error(), false
	}
	if loaded == nil || loaded.PendingTurn == nil {
		return stanceUnknown, "missing pending_turn", false
	}
	raw := strings.ToLower(strings.TrimSpace(loaded.PendingTurn.Stance))
	switch raw {
	case stanceApprove:
		return stanceApprove, strings.TrimSpace(loaded.PendingTurn.Note), true
	case stanceRequestChanges, "requestchanges", "request_changes":
		return stanceRequestChanges, strings.TrimSpace(loaded.PendingTurn.Note), true
	case stanceReject:
		return stanceReject, strings.TrimSpace(loaded.PendingTurn.Note), true
	default:
		// The agent wrote *something* — record it as an invalid-stance
		// pending_turn so the loop's "no pending" branch doesn't fire,
		// but mark the stance unknown so quorum can't count it.
		return stanceUnknown, "invalid stance: " + loaded.PendingTurn.Stance, true
	}
}

// validateLoopInput rejects programmer errors before the loop starts.
// These are bugs in the caller, not user errors, so they short-circuit
// with a plain error rather than entering the OutcomePaused/Stalled paths.
func validateLoopInput(in LoopInput) error {
	if err := validateRunID(in.RunID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}
	if len(in.Agents) == 0 {
		return errors.New("at least one agent is required")
	}
	if in.FindingsDoc == "" {
		return errors.New("FindingsDoc is required")
	}
	return nil
}

// initLoopState builds the starting RunState. When in.Resume is non-nil it
// takes its turn/round/idx + accumulated stances; otherwise it initialises
// a fresh state with Turn=0, NextAgentIdx=0.
func initLoopState(in LoopInput, now func() time.Time, maxTurns, quorum int) *RunState {
	if in.Resume != nil {
		st := *in.Resume
		// Always use the LoopInput's RunID/Agents/etc. — Resume is a
		// snapshot, but the caller is the source of truth for run config.
		st.RunID = in.RunID
		st.Topic = in.Topic
		st.Agents = append([]string(nil), in.Agents...)
		st.MaxTurns = maxTurns
		st.Quorum = quorum
		st.FindingsDoc = in.FindingsDoc
		st.StartingSHA = in.StartingSHA
		// Discard any pending_turn carried over from the resumed snapshot;
		// the next agent will write a fresh one.
		st.PendingTurn = nil
		if st.StartedAt.IsZero() {
			st.StartedAt = now()
		}
		st.UpdatedAt = now()
		return &st
	}
	t := now()
	return &RunState{
		RunID:           in.RunID,
		Topic:           in.Topic,
		Agents:          append([]string(nil), in.Agents...),
		MaxTurns:        maxTurns,
		Quorum:          quorum,
		CompletedRounds: 0,
		Turn:            0,
		NextAgentIdx:    0,
		FindingsDoc:     in.FindingsDoc,
		StartingSHA:     in.StartingSHA,
		StartedAt:       t,
		UpdatedAt:       t,
	}
}

// advanceAgent rolls NextAgentIdx forward modulo the agent count.
func advanceAgent(state *RunState) {
	state.NextAgentIdx = (state.NextAgentIdx + 1) % len(state.Agents)
}

// updateRoundCounter recomputes state.CompletedRounds from the current Turn.
// With N agents:
//   - Turn 1..N → round 1 in progress, completed rounds = 0
//   - Turn N+1..2N → round 2 in progress, completed rounds = 1
//
// The per-stance Round (TurnStance.Round) is 1-indexed and tracks the
// round each individual turn belongs to — the two fields are not
// interchangeable.
func updateRoundCounter(state *RunState) {
	state.CompletedRounds = state.Turn / len(state.Agents)
}

// approveCountInRound returns how many stances in the given round are
// "approve". Scans the slice rather than looking only at the tail so
// resumed runs (whose Stances slice may include earlier rounds) compute
// the right count.
func approveCountInRound(stances []TurnStance, round int) int {
	n := 0
	for _, s := range stances {
		if s.Round == round && s.Stance == stanceApprove {
			n++
		}
	}
	return n
}

// recordFailureStance appends a TurnStance with Stance="unknown" and a Note
// describing the failure. Used when the agent could not be spawned (no
// spawner, log-file open error). PlanChanged is false because nothing ran.
func recordFailureStance(state *RunState, round int, agent string, err error, now func() time.Time) {
	state.Stances = append(state.Stances, TurnStance{
		Round:  round,
		Turn:   state.Turn,
		Agent:  agent,
		Stance: stanceUnknown,
		Note:   "spawn error: " + err.Error(),
	})
	state.PendingTurn = nil
	updateRoundCounter(state)
	state.UpdatedAt = now()
}

// fileFingerprint returns "<size>:<sha256>" for the file at path, or the
// empty string when the file is missing or unreadable. Used to drive
// PlanChanged across a turn.
//
// Includes a content SHA rather than mtime: filesystems with second-
// granularity mtime (FAT, some network mounts) would let a same-length
// sub-second edit escape detection if we keyed on size+mtime alone, and
// the typical findings doc is small enough that hashing once per turn is
// cheap. Stat-only fallback keeps the loop moving when the file does not
// yet exist (turn 1 of a new run); a missing file is detected downstream
// by comparing the empty fingerprint before vs. after the turn.
func fileFingerprint(ctx context.Context, path string) string {
	info, err := os.Stat(path)
	if err != nil {
		logging.Debug(ctx, "investigate: stat findings doc failed",
			slog.String("path", path), sErr(err))
		return ""
	}
	f, err := os.Open(path) //nolint:gosec // path is the findings doc the caller already validated
	if err != nil {
		// Fall back to size+mtime when content cannot be read; better than
		// returning empty (which would mis-report PlanChanged for a missing
		// vs unreadable file).
		logging.Debug(ctx, "investigate: open findings doc for hashing failed",
			slog.String("path", path), sErr(err))
		return fmt.Sprintf("%d:m%d", info.Size(), info.ModTime().UnixNano())
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		logging.Debug(ctx, "investigate: hash findings doc failed",
			slog.String("path", path), sErr(err))
		return fmt.Sprintf("%d:m%d", info.Size(), info.ModTime().UnixNano())
	}
	return fmt.Sprintf("%d:%s", info.Size(), hex.EncodeToString(h.Sum(nil)))
}

// classifyRunErr formats the per-turn agent error so the recorded Note
// distinguishes a process that never started (exec lookup failure, e.g.
// binary missing) from one that started and exited non-zero. Both kinds
// arrive here as cmd.Run() errors, but the operator needs to know which
// to fix: PATH/install for spawn errors, agent behavior for exit errors.
func classifyRunErr(err error) string {
	if err == nil {
		return ""
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return "spawn error: " + err.Error()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Sprintf("exit error: status %d", exitErr.ExitCode())
	}
	return "agent error: " + err.Error()
}

// --- small slog helpers ---------------------------------------------------
//
// Wrappers below encode the key NAME used for common loop attributes
// (sRun → "run_id", sAgent → "agent", etc.) so call sites read as
// data without re-typing the key string. For attributes whose key is
// only used once, call slog.String / slog.Bool directly.

func sRun(runID string) any { return slog.String("run_id", runID) }

func sAgent(agent string) any { return slog.String("agent", agent) }

func sTurn(turn int) any { return slog.Int("turn", turn) }

func sRound(round int) any { return slog.Int("round", round) }

func sErr(err error) any {
	if err == nil {
		return slog.String("err", "")
	}
	return slog.String("err", err.Error())
}
