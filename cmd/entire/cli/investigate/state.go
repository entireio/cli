package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/provenance"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// InvestigationsDirName is the directory name (under git common dir) where
// investigation runs persist their per-run artifacts (findings.md +
// state.json).
const InvestigationsDirName = "entire-investigations"

// stateFileName is the on-disk name for the per-run state file inside the
// run directory.
const stateFileName = "state.json"

// runIDPattern is the validation regex for investigation run IDs: exactly
// 12 lowercase hex characters. Shares the checkpoint-id format via
// id.Pattern.
var runIDPattern = regexp.MustCompile("^" + id.Pattern + "$")

// RunState is the persisted state of an investigation run, sufficient to
// resume after a crash, Ctrl+C, or `--continue`.
//
// Round semantics: CompletedRounds counts how many full passes through
// every agent have finished — it is 0 mid-round-1, increments to 1 once
// every agent has had its first turn, and so on. By contrast,
// TurnStance.Round records the 1-indexed round each individual turn
// belongs to. The two fields look similar but represent different things;
// readers must pick the one that matches the question they're asking.
type RunState struct {
	RunID           string       `json:"run_id"`
	Topic           string       `json:"topic"`
	Agents          []string     `json:"agents"`
	MaxTurns        int          `json:"max_turns"`
	Quorum          int          `json:"quorum"`
	CompletedRounds int          `json:"completed_rounds"`
	Turn            int          `json:"turn"`           // overall turn index across rounds
	NextAgentIdx    int          `json:"next_agent_idx"` // index into Agents for the NEXT turn
	Stances         []TurnStance `json:"stances,omitempty"`
	FindingsDoc     string       `json:"findings_doc"` // absolute path
	StartingSHA     string       `json:"starting_sha"`
	StartedAt       time.Time    `json:"started_at"`
	UpdatedAt       time.Time    `json:"updated_at"`

	// PendingTurn is the agent-writable section. After each agent turn the
	// agent sets this to its stance + a short note. The loop reads it
	// after the agent process exits, validates it, appends a TurnStance to
	// Stances[], clears PendingTurn, advances cursors, persists.
	PendingTurn *PendingTurn `json:"pending_turn,omitempty"`
}

// PendingTurn is the agent-written stance for the most recent turn. The
// agent populates this before exiting; the loop reads it, appends to
// Stances[], and clears the field. The `agent` and `turn` fields are
// unambiguous from context (the loop knows which turn it just ran), so the
// agent does not include them.
type PendingTurn struct {
	Stance string `json:"stance"`         // "approve" | "request-changes" | "reject"
	Note   string `json:"note,omitempty"` // short explanation; optional
}

// TurnStance is one agent's recorded stance for a turn.
//
// Round here is the 1-indexed round the turn belongs to (turn 1 of round
// 1, turn N+1 starts round 2, etc.) — distinct from
// RunState.CompletedRounds, which counts finished rounds.
type TurnStance struct {
	Round       int    `json:"round"`
	Turn        int    `json:"turn"` // overall turn number
	Agent       string `json:"agent"`
	Stance      string `json:"stance"` // "approve" | "request-changes" | "reject" | "unknown"
	PlanChanged bool   `json:"plan_changed"`
	Note        string `json:"note,omitempty"`
}

// StateStore is the runs-state directory wrapper. The root contains one
// sub-directory per run (named after the run ID), holding findings.md and
// state.json.
type StateStore struct {
	dir string
}

// NewStateStore creates a StateStore rooted at
// <git-common-dir>/entire-investigations. Resolves the common dir via
// session.GetGitCommonDir, so this requires a git repository context.
func NewStateStore(ctx context.Context) (*StateStore, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return nil, fmt.Errorf("get git common dir: %w", err)
	}
	return &StateStore{
		dir: filepath.Join(commonDir, InvestigationsDirName),
	}, nil
}

// NewStateStoreWithDir creates a StateStore rooted at dir. Useful for tests
// that don't want to depend on a real git repository.
func NewStateStoreWithDir(dir string) *StateStore {
	return &StateStore{dir: dir}
}

// Root returns the absolute path the store is rooted at. Useful for callers
// that need to derive sibling paths (e.g. findings.md alongside state.json).
func (s *StateStore) Root() string {
	return s.dir
}

// RunDir returns the absolute path of the per-run directory for runID,
// where findings.md and state.json both live. The directory may or may
// not exist on disk; callers that need it materialised should MkdirAll
// before writing.
//
// Precondition: runID MUST be a validated 12-hex id. RunDir joins it into a
// path that callers feed to os.RemoveAll (via clean), so an unvalidated id
// would be a path-traversal sink. Every path that reaches here enforces this:
// Save/Load validate before calling; manifest List/ResolveByRunID drop
// manifests whose RunID fails validateRunID before any RunID reaches clean.
func (s *StateStore) RunDir(runID string) string {
	return filepath.Join(s.dir, runID)
}

// Save writes the run state atomically (temp file + rename).
func (s *StateStore) Save(ctx context.Context, st *RunState) error {
	_ = ctx // Reserved for future use

	if err := validateRunID(st.RunID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}

	runDir := s.RunDir(st.RunID)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return fmt.Errorf("create investigation run directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run state: %w", err)
	}

	finalPath := s.runStatePath(st.RunID)
	if err := jsonutil.WriteFileAtomic(finalPath, data, 0o600); err != nil {
		return fmt.Errorf("write run state: %w", err)
	}
	return nil
}

// Load reads the run state for runID. Returns (nil, nil) when the file does
// not exist (treat as "no such run").
func (s *StateStore) Load(ctx context.Context, runID string) (*RunState, error) {
	_ = ctx // Reserved for future use

	if err := validateRunID(runID); err != nil {
		return nil, fmt.Errorf("invalid run ID: %w", err)
	}

	data, err := os.ReadFile(s.runStatePath(runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // nil,nil indicates run not found
		}
		return nil, fmt.Errorf("read run state: %w", err)
	}

	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("unmarshal run state: %w", err)
	}
	return &st, nil
}

// List returns all persisted run states. Returns nil (and no error) when the
// state directory does not exist.
func (s *StateStore) List(ctx context.Context) ([]*RunState, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read investigations directory: %w", err)
	}

	var states []*RunState
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		if err := validateRunID(runID); err != nil {
			// Skip directories that don't match the run-ID format — they
			// are not ours (e.g. the manifests/ sibling).
			continue
		}
		st, loadErr := s.Load(ctx, runID)
		if loadErr != nil {
			// state.json exists but won't parse — surface so the user can
			// inspect or `entire investigate clean <runID>`. Listing keeps
			// going so one bad run doesn't hide the rest.
			logging.Warn(ctx, "investigate: list skipped unreadable run state",
				slog.String("run_id", runID),
				slog.String("err", loadErr.Error()))
			continue
		}
		if st == nil {
			continue
		}
		states = append(states, st)
	}
	return states, nil
}

// Clear removes the persisted state for runID. Missing files are treated as a
// successful clear (no-op).
func (s *StateStore) Clear(ctx context.Context, runID string) error {
	_ = ctx // Reserved for future use

	if err := validateRunID(runID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}

	if err := os.Remove(s.runStatePath(runID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove run state file: %w", err)
	}
	return nil
}

// runStatePath returns the on-disk path for runID's state file.
func (s *StateStore) runStatePath(runID string) string {
	return filepath.Join(s.RunDir(runID), stateFileName)
}

// validateRunID enforces that runID is exactly 12 lowercase hex characters.
// Anything else is rejected to prevent path traversal and to keep the format
// stable for sharded directory layouts elsewhere in the codebase.
func validateRunID(runID string) error {
	if runID == "" {
		return errors.New("run ID cannot be empty")
	}
	if !runIDPattern.MatchString(runID) {
		return fmt.Errorf("invalid run ID %q: must be 12 lowercase hex characters", runID)
	}
	return nil
}

// IsValidRunID reports whether runID matches the 12-lowercase-hex format.
// Delegates to provenance.IsValidRunID — the canonical validator lives
// alongside the env-var contract it's most often paired with.
func IsValidRunID(runID string) bool {
	return provenance.IsValidRunID(runID)
}
