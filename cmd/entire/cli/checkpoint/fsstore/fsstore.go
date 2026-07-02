// Package fsstore is a reference, test-only persistent checkpoint backend that
// stores checkpoints as JSON files on disk. It exists to exercise the pluggable
// backend seam (registry + mirror fan-out) with a real, non-git implementation
// of the api/checkpoint contract, and to serve as a worked example for new
// backends.
//
// It is deliberately NOT registered by production code: only a test-only helper
// (registerForTesting, in register_test.go) wires it into the checkpoint
// registry, so a production binary can never select it. As a mirror it receives
// best-effort write fan-out; it intentionally ignores
// the git-specific blob-hash fields of the contract and stores transcript bytes
// directly, which keeps the example small and makes the contract's remaining
// git leakage concrete.
//
// It is faithful to the contract's per-session metadata and write-request
// semantics — including prompt and summary redaction via the shared
// checkpoint helpers — but it does not replicate two git-writer behaviors:
// cross-session aggregation (the root summary's TokenUsage reflects the latest
// session, not a sum) and derived stamps the git writer adds (CLI version,
// skill-events version). Neither is needed to validate the pluggable seam.
package fsstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cp "github.com/entireio/cli/api/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// Store is a JSON-file-backed persistent checkpoint store. One file per
// checkpoint (<root>/<checkpoint-id>.json) holds the root summary plus all
// session content.
type Store struct {
	root string
	mu   sync.Mutex
}

// New constructs a filesystem store rooted at dir.
func New(dir string) *Store {
	return &Store{root: dir}
}

type storedSession struct {
	SessionID  string      `json:"session_id"`
	Metadata   cp.Metadata `json:"metadata"`
	Transcript []byte      `json:"transcript,omitempty"`
	Prompts    string      `json:"prompts,omitempty"`
}

type storedCheckpoint struct {
	Summary  cp.CheckpointSummary `json:"summary"`
	Sessions []storedSession      `json:"sessions"`
}

var (
	_ cp.PersistentStore = (*Store)(nil)
	_ cp.Writer          = (*Store)(nil)
)

func (s *Store) path(checkpointID id.CheckpointID) string {
	return filepath.Join(s.root, string(checkpointID)+".json")
}

// load reads the stored checkpoint, returning (nil, nil) when it does not exist.
func (s *Store) load(checkpointID id.CheckpointID) (*storedCheckpoint, error) {
	data, err := os.ReadFile(s.path(checkpointID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil //nolint:nilnil // absent checkpoint, not an error
		}
		return nil, fmt.Errorf("fsstore: read %s: %w", checkpointID, err)
	}
	var sc storedCheckpoint
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("fsstore: parse %s: %w", checkpointID, err)
	}
	return &sc, nil
}

func (s *Store) save(sc *storedCheckpoint) error {
	if err := os.MkdirAll(s.root, 0o750); err != nil {
		return fmt.Errorf("fsstore: create root %s: %w", s.root, err)
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return fmt.Errorf("fsstore: encode %s: %w", sc.Summary.CheckpointID, err)
	}
	// Write atomically via the shared helper (unique temp file, cleaned up on
	// failure) so a reader never observes a partial document. This guards a
	// single writer's in-progress write; cross-process concurrency is out of
	// scope for this test-only backend.
	if err := jsonutil.WriteFileAtomic(s.path(sc.Summary.CheckpointID), data, 0o600); err != nil {
		return fmt.Errorf("fsstore: write %s: %w", sc.Summary.CheckpointID, err)
	}
	return nil
}

// Write dispatches on the request type, mirroring the git store's Write.
func (s *Store) Write(_ context.Context, req cp.WriteRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch r := req.(type) {
	case cp.Session:
		return s.writeSession(cp.WriteOptions(r))
	case cp.SessionTranscript:
		return s.backfillTranscript(cp.UpdateOptions(r))
	case cp.SessionSummary:
		return s.writeSessionSummary(r)
	case cp.CheckpointAttribution:
		return s.writeAttribution(r)
	default:
		return fmt.Errorf("fsstore: unsupported write request %T", req)
	}
}

func (s *Store) writeSession(opts cp.WriteOptions) error {
	sc, err := s.load(opts.CheckpointID)
	if err != nil {
		return err
	}
	if sc == nil {
		sc = &storedCheckpoint{Summary: cp.CheckpointSummary{CheckpointID: opts.CheckpointID}}
	}

	session := storedSession{
		SessionID:  opts.SessionID,
		Metadata:   metadataFromWriteOptions(opts),
		Transcript: opts.Transcript.Bytes(),
		Prompts:    checkpoint.RedactedJoinedPrompts(opts.Prompts),
	}
	sc.Sessions = upsertSession(sc.Sessions, session)

	// Summary-level flags accumulate across sessions and survive recompute.
	sc.Summary.HasReview = sc.Summary.HasReview || opts.HasReview
	sc.Summary.HasInvestigation = sc.Summary.HasInvestigation || opts.HasInvestigation
	if opts.CombinedAttribution != nil {
		// Migration path: an initial write may carry holistic attribution. Normal
		// condensation sets this later via a CheckpointAttribution write instead.
		sc.Summary.CombinedAttribution = opts.CombinedAttribution
	}

	recomputeSummary(sc)
	return s.save(sc)
}

func (s *Store) backfillTranscript(opts cp.UpdateOptions) error {
	sc, err := s.load(opts.CheckpointID)
	if err != nil {
		return err
	}
	if sc == nil {
		return fmt.Errorf("fsstore: cannot backfill transcript for unknown checkpoint %s", opts.CheckpointID)
	}
	idx := sessionIndexByID(sc.Sessions, opts.SessionID)
	if idx < 0 {
		return fmt.Errorf("fsstore: cannot backfill transcript for unknown session %q in %s", opts.SessionID, opts.CheckpointID)
	}
	// Replace semantics, but do not clobber sibling fields (matches the git
	// store's stop-time transcript backfill).
	sc.Sessions[idx].Transcript = opts.Transcript.Bytes()
	sc.Sessions[idx].Prompts = checkpoint.RedactedJoinedPrompts(opts.Prompts)
	if len(opts.SkillEvents) > 0 {
		sc.Sessions[idx].Metadata.SkillEvents = opts.SkillEvents
	}
	return s.save(sc)
}

func (s *Store) writeSessionSummary(r cp.SessionSummary) error {
	sc, err := s.load(r.CheckpointID)
	if err != nil {
		return err
	}
	if sc == nil || len(sc.Sessions) == 0 {
		return fmt.Errorf("fsstore: cannot set summary for unknown checkpoint %s", r.CheckpointID)
	}
	sc.Sessions[len(sc.Sessions)-1].Metadata.Summary = checkpoint.RedactSummary(r.Summary)
	return s.save(sc)
}

func (s *Store) writeAttribution(r cp.CheckpointAttribution) error {
	sc, err := s.load(r.CheckpointID)
	if err != nil {
		return err
	}
	if sc == nil {
		return fmt.Errorf("fsstore: cannot set attribution for unknown checkpoint %s", r.CheckpointID)
	}
	sc.Summary.CombinedAttribution = r.Attribution
	return s.save(sc)
}

// Read returns the checkpoint summary, or (nil, nil) when absent so the
// contract helper normalizes it to ErrCheckpointNotFound.
func (s *Store) Read(_ context.Context, checkpointID id.CheckpointID) (*cp.CheckpointSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, err := s.load(checkpointID)
	if err != nil || sc == nil {
		return nil, err
	}
	summary := sc.Summary
	return &summary, nil
}

func (s *Store) List(_ context.Context) ([]cp.CheckpointInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fsstore: list %s: %w", s.root, err)
	}

	var infos []cp.CheckpointInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		checkpointID := id.CheckpointID(strings.TrimSuffix(e.Name(), ".json"))
		sc, err := s.load(checkpointID)
		if err != nil {
			return nil, err
		}
		if sc == nil {
			continue
		}
		infos = append(infos, infoFromStored(sc))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.After(infos[j].CreatedAt) })
	return infos, nil
}

func (s *Store) ReadSessionContent(_ context.Context, checkpointID id.CheckpointID, sessionIndex int) (*cp.SessionContent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.sessionAt(checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	return &cp.SessionContent{
		Metadata:   session.Metadata,
		Transcript: session.Transcript,
		Prompts:    session.Prompts,
	}, nil
}

func (s *Store) ReadSessionMetadata(_ context.Context, checkpointID id.CheckpointID, sessionIndex int) (*cp.Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.sessionAt(checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	meta := session.Metadata
	return &meta, nil
}

func (s *Store) ReadSessionPrompts(_ context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.sessionAt(checkpointID, sessionIndex)
	if err != nil {
		return "", err
	}
	return session.Prompts, nil
}

func (s *Store) ReadSessionMetadataAndPrompts(_ context.Context, checkpointID id.CheckpointID, sessionIndex int) (*cp.Metadata, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, err := s.sessionAt(checkpointID, sessionIndex)
	if err != nil {
		return nil, "", err
	}
	meta := session.Metadata
	return &meta, session.Prompts, nil
}

func (s *Store) sessionAt(checkpointID id.CheckpointID, sessionIndex int) (*storedSession, error) {
	sc, err := s.load(checkpointID)
	if err != nil {
		return nil, err
	}
	if sc == nil {
		return nil, cp.ErrCheckpointNotFound
	}
	if sessionIndex < 0 || sessionIndex >= len(sc.Sessions) {
		return nil, fmt.Errorf("fsstore: session index %d out of range for %s (%d sessions)", sessionIndex, checkpointID, len(sc.Sessions))
	}
	return &sc.Sessions[sessionIndex], nil
}

func metadataFromWriteOptions(opts cp.WriteOptions) cp.Metadata {
	createdAt := opts.CreatedAt
	if createdAt.IsZero() {
		// Contract: a zero CreatedAt means "use the current time".
		createdAt = time.Now()
	}
	return cp.Metadata{
		CheckpointID:                opts.CheckpointID,
		SessionID:                   opts.SessionID,
		Strategy:                    opts.Strategy,
		CreatedAt:                   createdAt,
		Branch:                      opts.Branch,
		CheckpointsCount:            opts.CheckpointsCount,
		SaveStepCount:               opts.SaveStepCount,
		FilesTouched:                opts.FilesTouched,
		Agent:                       opts.Agent,
		Model:                       opts.Model,
		TurnID:                      opts.TurnID,
		IsTask:                      opts.IsTask,
		ToolUseID:                   opts.ToolUseID,
		TranscriptIdentifierAtStart: opts.TranscriptIdentifierAtStart,
		CheckpointTranscriptStart:   opts.CheckpointTranscriptStart,
		TranscriptLinesAtStart:      opts.CheckpointTranscriptStart, // git writes both for back-compat
		TokenUsage:                  opts.TokenUsage,
		SkillEvents:                 opts.SkillEvents,
		PromptAttributions:          opts.PromptAttributionsJSON,
		SessionMetrics:              opts.SessionMetrics,
		Summary:                     checkpoint.RedactSummary(opts.Summary),
		Attribution:                 opts.Attribution,
		Kind:                        opts.Kind,
		ReviewSkills:                opts.ReviewSkills,
		ReviewPrompt:                opts.ReviewPrompt,
		InvestigateRunID:            opts.InvestigateRunID,
		InvestigateTopic:            opts.InvestigateTopic,
	}
}

func upsertSession(sessions []storedSession, session storedSession) []storedSession {
	if idx := sessionIndexByID(sessions, session.SessionID); idx >= 0 {
		sessions[idx] = session
		return sessions
	}
	return append(sessions, session)
}

func sessionIndexByID(sessions []storedSession, sessionID string) int {
	for i := range sessions {
		if sessions[i].SessionID == sessionID {
			return i
		}
	}
	return -1
}

// recomputeSummary rebuilds the aggregated root summary from the sessions, so a
// reader sees one Sessions entry per stored session and aggregate counts.
func recomputeSummary(sc *storedCheckpoint) {
	summary := &sc.Summary
	summary.Sessions = make([]cp.SessionFilePaths, len(sc.Sessions))
	summary.CheckpointsCount = 0
	files := map[string]struct{}{}
	var orderedFiles []string

	for i := range sc.Sessions {
		session := &sc.Sessions[i]
		summary.Sessions[i] = cp.SessionFilePaths{
			Metadata:   fmt.Sprintf("%d/metadata.json", i+1),
			Transcript: fmt.Sprintf("%d/full.jsonl", i+1),
			Prompt:     fmt.Sprintf("%d/prompt.txt", i+1),
		}
		summary.CheckpointsCount += session.Metadata.CheckpointsCount
		if session.Metadata.Strategy != "" {
			summary.Strategy = session.Metadata.Strategy
		}
		if session.Metadata.Branch != "" {
			summary.Branch = session.Metadata.Branch
		}
		if session.Metadata.TokenUsage != nil {
			summary.TokenUsage = session.Metadata.TokenUsage
		}
		for _, f := range session.Metadata.FilesTouched {
			if _, seen := files[f]; !seen {
				files[f] = struct{}{}
				orderedFiles = append(orderedFiles, f)
			}
		}
	}
	summary.FilesTouched = orderedFiles
}

func infoFromStored(sc *storedCheckpoint) cp.CheckpointInfo {
	info := cp.CheckpointInfo{
		CheckpointID:     sc.Summary.CheckpointID,
		CheckpointsCount: sc.Summary.CheckpointsCount,
		FilesTouched:     sc.Summary.FilesTouched,
		SessionCount:     len(sc.Sessions),
	}
	if n := len(sc.Sessions); n > 0 {
		last := sc.Sessions[n-1]
		info.SessionID = last.SessionID
		info.CreatedAt = last.Metadata.CreatedAt
		info.Agent = last.Metadata.Agent
		info.IsTask = last.Metadata.IsTask
		info.ToolUseID = last.Metadata.ToolUseID
	}
	info.SessionIDs = make([]string, 0, len(sc.Sessions))
	for i := range sc.Sessions {
		info.SessionIDs = append(info.SessionIDs, sc.Sessions[i].SessionID)
	}
	return info
}
