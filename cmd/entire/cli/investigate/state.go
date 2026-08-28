package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
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

// FindingsFileName is the on-disk name for the per-run findings document
// inside the run directory.
const FindingsFileName = "findings.md"

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
	// dir is the absolute store directory. It is kept because RunDir hands
	// absolute paths to callers (the findings doc travels through manifests and
	// into agent prompts); all I/O here is a name inside root.
	dir     string
	parent  string
	dirName string
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
		dir:     filepath.Join(commonDir, InvestigationsDirName),
		parent:  commonDir,
		dirName: InvestigationsDirName,
	}, nil
}

// NewStateStoreWithDir creates a StateStore rooted at dir. Useful for tests
// that don't want to depend on a real git repository.
func NewStateStoreWithDir(dir string) *StateStore {
	return &StateStore{
		dir:     dir,
		parent:  filepath.Dir(dir),
		dirName: filepath.Base(dir),
	}
}

// root returns the shared *os.Root over the directory this store's run
// directories sit inside — the git common dir in production, a temp stand-in
// under test. Run IDs are validated before reaching a path, but resolving them
// as names inside the root is what keeps that a defence rather than the only
// defence: RunDir feeds os.RemoveAll.
func (s *StateStore) root() (*os.Root, error) {
	return gitdir.OpenAt(s.parent) //nolint:wrapcheck // gitdir names the directory and returns a missing one unwrapped for os.IsNotExist
}

// name renders a path under the store as a name relative to root.
func (s *StateStore) name(parts ...string) string {
	return s.dirName + "/" + path.Join(parts...)
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

	root, err := s.root()
	if err != nil {
		return fmt.Errorf("open investigation store: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, s.name(st.RunID), 0o750); err != nil {
		return fmt.Errorf("create investigation run directory: %w", err)
	}

	data, err := jsonutil.MarshalIndentWithNewline(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run state: %w", err)
	}

	if err := jsonutil.WriteFileAtomicIn(root, s.runStateName(st.RunID), data, 0o600); err != nil {
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

	root, err := s.root()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // no store => no such run
		}
		return nil, fmt.Errorf("open investigation store: %w", err)
	}
	data, err := osroot.ReadFile(root, s.runStateName(runID))
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

// FindingsPath returns the absolute path of runID's findings document.
// Absolute for the same reason RunStatePath is: the investigating agent writes
// it, and the manifest records where it was.
func (s *StateStore) FindingsPath(runID string) string {
	return filepath.Join(s.RunDir(runID), FindingsFileName)
}

// ReadFindings reads runID's findings document through the store's root.
// A missing document is (nil, false, nil).
func (s *StateStore) ReadFindings(runID string) ([]byte, bool, error) {
	if err := validateRunID(runID); err != nil {
		return nil, false, fmt.Errorf("invalid run ID: %w", err)
	}
	root, err := s.root()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open investigation store: %w", err)
	}
	data, err := osroot.ReadFile(root, s.name(runID, FindingsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read findings: %w", err)
	}
	return data, true, nil
}

// WriteFindings writes runID's findings document through the store's root,
// creating the per-run directory. MkdirAllNoSymlink rather than MkdirAll: a
// symlinked component under entire-investigations is a directory Entire did not
// create and cannot vouch for, and this is the write that establishes the run.
func (s *StateStore) WriteFindings(runID string, body []byte) error {
	if err := validateRunID(runID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}
	root, err := s.root()
	if err != nil {
		return fmt.Errorf("open investigation store: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, s.name(runID), 0o750); err != nil {
		return fmt.Errorf("create investigation run directory: %w", err)
	}
	if err := osroot.WriteFile(root, s.name(runID, FindingsFileName), body, 0o600); err != nil {
		return fmt.Errorf("write findings doc: %w", err)
	}
	return nil
}

// ReadRunFindings reads runID's findings document through store.
//
// It exists for the two renderers that hold a LocalManifest and used to read
// m.FindingsDoc directly. That field is an absolute path decoded from a JSON
// file on disk, so following it means a manifest gets to choose which file is
// read and printed. The run id is the part of the manifest that is validated,
// and resolving it as a name inside the store keeps the read in the tree the
// store owns.
//
// Everything is soft: a nil store, an invalid id, a missing run, a missing
// document, and an unreadable one all yield "". The callers print a shorter
// block, and an empty document is indistinguishable from an absent one because
// neither has anything to render.
func ReadRunFindings(store *StateStore, runID string) string {
	if store == nil || !IsValidRunID(runID) {
		return ""
	}
	data, found, err := store.ReadFindings(runID)
	if err != nil || !found {
		return ""
	}
	return string(data)
}

// RemoveRun deletes runID's per-run directory and everything in it.
//
// This is the operation RunDir's doc comment warns about — the one an
// unvalidated id would turn into a path-traversal sink. It resolves runID as a
// NAME inside the store's root rather than joining it into an absolute path, so
// the containment is the kernel's rather than a precondition each caller has to
// keep honouring. validateRunID stays as the layer above it.
//
// A store or run directory that is not there is not an error: clean converges
// when run against a partially removed state.
func (s *StateStore) RemoveRun(runID string) error {
	if err := validateRunID(runID); err != nil {
		return fmt.Errorf("invalid run ID: %w", err)
	}
	root, err := s.root()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open investigation store: %w", err)
	}
	if err := root.RemoveAll(s.name(runID)); err != nil {
		return fmt.Errorf("remove run directory: %w", err)
	}
	return nil
}

// RunStatePath returns the absolute path of runID's state file. Absolute on
// purpose: it is handed to the investigating agent, which is a separate process.
// Entire's own reads and writes of the same file go through the root.
func (s *StateStore) RunStatePath(runID string) string {
	return filepath.Join(s.RunDir(runID), stateFileName)
}

// runStateName returns runID's state file as a name relative to root.
func (s *StateStore) runStateName(runID string) string {
	return s.name(runID, stateFileName)
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
