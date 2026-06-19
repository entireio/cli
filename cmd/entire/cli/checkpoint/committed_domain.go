package checkpoint

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Session is the committed session document written by SessionWriter.
type Session = WriteCommittedOptions

type sessionRefMode int

const (
	sessionRefLatest sessionRefMode = iota
	sessionRefIndex
	sessionRefID
)

// SessionRef identifies a session within today's embedded checkpoint layout.
type SessionRef struct {
	checkpointID   id.CheckpointID
	sessionID      string
	sessionIndex   int
	sessionRefMode sessionRefMode
}

// LatestSessionRef targets the latest session in a checkpoint.
func LatestSessionRef(checkpointID id.CheckpointID) SessionRef {
	return SessionRef{checkpointID: checkpointID, sessionIndex: -1, sessionRefMode: sessionRefLatest}
}

// SessionIndexRef targets a session by its checkpoint-local index.
func SessionIndexRef(checkpointID id.CheckpointID, sessionIndex int) SessionRef {
	return SessionRef{checkpointID: checkpointID, sessionIndex: sessionIndex, sessionRefMode: sessionRefIndex}
}

// SessionIDRef targets a session by its session ID.
func SessionIDRef(checkpointID id.CheckpointID, sessionID string) SessionRef {
	return SessionRef{checkpointID: checkpointID, sessionID: sessionID, sessionIndex: -1, sessionRefMode: sessionRefID}
}

// CheckpointID returns the checkpoint that contains the session.
func (r SessionRef) CheckpointID() id.CheckpointID { return r.checkpointID }

// SessionID returns the target session ID when the ref was created by SessionIDRef.
func (r SessionRef) SessionID() string { return r.sessionID }

// SessionIndex returns the target index when the ref was created by SessionIndexRef.
func (r SessionRef) SessionIndex() (int, bool) {
	if r.sessionRefMode != sessionRefIndex {
		return 0, false
	}
	return r.sessionIndex, true
}

type sessionReadMode int

const (
	sessionReadFull sessionReadMode = iota
	sessionReadMetadataOnly
	sessionReadMetadataAndPrompts
)

type readOptions struct {
	mode sessionReadMode
}

// ReadOption customizes a session read without expanding the reader interface.
type ReadOption func(*readOptions)

// WithSessionMetadataOnly reads session metadata without transcript or prompts.
func WithSessionMetadataOnly() ReadOption {
	return func(opts *readOptions) {
		opts.mode = sessionReadMetadataOnly
	}
}

// WithSessionMetadataAndPrompts reads metadata and prompts without transcript.
func WithSessionMetadataAndPrompts() ReadOption {
	return func(opts *readOptions) {
		opts.mode = sessionReadMetadataAndPrompts
	}
}

type writeOptions struct {
	transcript       redact.RedactedBytes
	transcriptSet    bool
	prompts          []string
	promptsSet       bool
	agent            types.AgentType
	skillEvents      []agent.SkillEvent
	skillEventsSet   bool
	precomputedBlobs *PrecomputedTranscriptBlobs
	summary          *Summary
	summarySet       bool
	attribution      *InitialAttribution
	attributionSet   bool
}

// WriteOption customizes session or checkpoint updates.
type WriteOption func(*writeOptions)

// WithTranscript replaces a session transcript.
func WithTranscript(transcript redact.RedactedBytes, agentType types.AgentType) WriteOption {
	return func(opts *writeOptions) {
		opts.transcript = transcript
		opts.transcriptSet = true
		opts.agent = agentType
	}
}

// WithPrompts replaces a session's prompt content.
func WithPrompts(prompts []string) WriteOption {
	return func(opts *writeOptions) {
		opts.prompts = prompts
		opts.promptsSet = true
	}
}

// WithSkillEvents replaces a session's recorded skill events.
func WithSkillEvents(events []agent.SkillEvent) WriteOption {
	return func(opts *writeOptions) {
		opts.skillEvents = events
		opts.skillEventsSet = true
	}
}

// WithPrecomputedTranscriptBlobs reuses already-written transcript blobs.
func WithPrecomputedTranscriptBlobs(blobs *PrecomputedTranscriptBlobs) WriteOption {
	return func(opts *writeOptions) {
		opts.precomputedBlobs = blobs
	}
}

// WithSummary updates a session summary.
func WithSummary(summary *Summary) WriteOption {
	return func(opts *writeOptions) {
		opts.summary = summary
		opts.summarySet = true
	}
}

// WithAttribution updates checkpoint-level attribution.
func WithAttribution(attribution *InitialAttribution) WriteOption {
	return func(opts *writeOptions) {
		opts.attribution = attribution
		opts.attributionSet = true
	}
}

// SessionReader reads committed session documents.
type SessionReader interface {
	ReadSession(ctx context.Context, ref SessionRef, opts ...ReadOption) (*SessionContent, error)
}

// SessionWriter writes and backfills committed session documents.
type SessionWriter interface {
	WriteSession(ctx context.Context, ref SessionRef, session Session) error
	UpdateSession(ctx context.Context, ref SessionRef, opts ...WriteOption) error
}

// SessionStore reads and writes committed sessions.
type SessionStore interface {
	SessionReader
	SessionWriter
}

// Reader reads committed checkpoint documents.
type Reader interface {
	ListCheckpoints(ctx context.Context) ([]CommittedInfo, error)
	ReadCheckpoint(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
}

// Writer backfills committed checkpoint documents.
type Writer interface {
	UpdateCheckpoint(ctx context.Context, checkpointID id.CheckpointID, opts ...WriteOption) error
}

// MetadataStore reads and writes committed checkpoint documents.
type MetadataStore interface {
	Reader
	Writer
}

func (s *GitStore) ListCheckpoints(ctx context.Context) ([]CommittedInfo, error) {
	return s.ListCommitted(ctx)
}

func (s *GitStore) ReadCheckpoint(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	summary, err := s.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, fmt.Errorf("read committed checkpoint: %w", err)
	}
	if summary == nil {
		return nil, ErrCheckpointNotFound
	}
	return summary, nil
}

func (s *GitStore) ReadSession(ctx context.Context, ref SessionRef, opts ...ReadOption) (*SessionContent, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}

	readOpts := applyReadOptions(opts)
	sessionIndex, err := s.resolveSessionIndex(ctx, ref)
	if err != nil {
		return nil, err
	}

	switch readOpts.mode {
	case sessionReadFull:
		return s.ReadSessionContent(ctx, ref.checkpointID, sessionIndex)
	case sessionReadMetadataOnly:
		metadata, readErr := s.ReadSessionMetadata(ctx, ref.checkpointID, sessionIndex)
		if readErr != nil {
			return nil, readErr
		}
		if metadata == nil {
			return nil, ErrCheckpointNotFound
		}
		return &SessionContent{Metadata: *metadata}, nil
	case sessionReadMetadataAndPrompts:
		return s.ReadSessionMetadataAndPrompts(ctx, ref.checkpointID, sessionIndex)
	default:
		return nil, errors.New("unknown session read mode")
	}
}

func (s *GitStore) WriteSession(ctx context.Context, ref SessionRef, session Session) error {
	if err := ref.validateForWrite(); err != nil {
		return err
	}

	opts := session
	switch {
	case opts.CheckpointID.IsEmpty():
		opts.CheckpointID = ref.checkpointID
	case opts.CheckpointID != ref.checkpointID:
		return fmt.Errorf("session checkpoint ID %s does not match ref %s", opts.CheckpointID, ref.checkpointID)
	}
	switch {
	case opts.SessionID == "":
		opts.SessionID = ref.sessionID
	case opts.SessionID != ref.sessionID:
		return fmt.Errorf("session ID %q does not match ref %q", opts.SessionID, ref.sessionID)
	}
	return s.WriteCommitted(ctx, opts)
}

func (s *GitStore) UpdateSession(ctx context.Context, ref SessionRef, opts ...WriteOption) error {
	if err := ref.validate(); err != nil {
		return err
	}

	writeOpts := applyWriteOptions(opts)
	if !writeOpts.hasSessionUpdate() {
		return errors.New("no session update options")
	}

	if writeOpts.hasTranscriptUpdate() {
		sessionID, err := s.resolveSessionID(ctx, ref)
		if err != nil {
			return err
		}
		if err := s.UpdateCommitted(ctx, UpdateCommittedOptions{
			CheckpointID:     ref.checkpointID,
			SessionID:        sessionID,
			Transcript:       writeOpts.transcript,
			Prompts:          writeOpts.prompts,
			Agent:            writeOpts.agent,
			SkillEvents:      writeOpts.skillEvents,
			PrecomputedBlobs: writeOpts.precomputedBlobs,
		}); err != nil {
			return err
		}
	}

	if writeOpts.summarySet {
		return s.updateSessionSummary(ctx, ref, writeOpts.summary)
	}
	return nil
}

func (s *GitStore) UpdateCheckpoint(ctx context.Context, checkpointID id.CheckpointID, opts ...WriteOption) error {
	writeOpts := applyWriteOptions(opts)
	if !writeOpts.attributionSet {
		return errors.New("no checkpoint update options")
	}
	return s.UpdateCheckpointSummary(ctx, checkpointID, writeOpts.attribution)
}

func applyReadOptions(opts []ReadOption) readOptions {
	readOpts := readOptions{mode: sessionReadFull}
	for _, opt := range opts {
		opt(&readOpts)
	}
	return readOpts
}

func applyWriteOptions(opts []WriteOption) writeOptions {
	var writeOpts writeOptions
	for _, opt := range opts {
		opt(&writeOpts)
	}
	return writeOpts
}

func (opts writeOptions) hasTranscriptUpdate() bool {
	return opts.transcriptSet || opts.promptsSet || opts.skillEventsSet || opts.precomputedBlobs != nil
}

func (opts writeOptions) hasSessionUpdate() bool {
	return opts.hasTranscriptUpdate() || opts.summarySet
}

func (r SessionRef) validate() error {
	if r.checkpointID.IsEmpty() {
		return errors.New("session ref checkpoint ID is required")
	}
	if r.sessionRefMode == sessionRefID && r.sessionID == "" {
		return errors.New("session ref session ID is required")
	}
	if r.sessionRefMode == sessionRefIndex && r.sessionIndex < 0 {
		return fmt.Errorf("session ref index must be non-negative: %d", r.sessionIndex)
	}
	return nil
}

func (r SessionRef) validateForWrite() error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.sessionRefMode != sessionRefID {
		return errors.New("write session requires a session ID ref")
	}
	return nil
}

func (s *GitStore) resolveSessionID(ctx context.Context, ref SessionRef) (string, error) {
	if ref.sessionID != "" {
		return ref.sessionID, nil
	}
	content, err := s.ReadSession(ctx, ref, WithSessionMetadataOnly())
	if err != nil {
		return "", err
	}
	return content.Metadata.SessionID, nil
}

func (s *GitStore) resolveSessionIndex(ctx context.Context, ref SessionRef) (int, error) {
	if ref.sessionRefMode == sessionRefIndex {
		return ref.sessionIndex, nil
	}

	summary, err := s.ReadCheckpoint(ctx, ref.checkpointID)
	if err != nil {
		return 0, err
	}
	return s.resolveSessionIndexFromSummary(ctx, ref, summary)
}

func (s *GitStore) resolveSessionIndexFromSummary(ctx context.Context, ref SessionRef, summary *CheckpointSummary) (int, error) {
	if summary == nil || len(summary.Sessions) == 0 {
		return 0, ErrCheckpointNotFound
	}

	switch ref.sessionRefMode {
	case sessionRefLatest:
		return len(summary.Sessions) - 1, nil
	case sessionRefIndex:
		if ref.sessionIndex >= len(summary.Sessions) {
			return 0, fmt.Errorf("session index %d out of range for checkpoint %s: %w", ref.sessionIndex, ref.checkpointID, ErrCheckpointNotFound)
		}
		return ref.sessionIndex, nil
	case sessionRefID:
		for index := range summary.Sessions {
			metadata, err := s.ReadSessionMetadata(ctx, ref.checkpointID, index)
			if err != nil {
				return 0, err
			}
			if metadata != nil && metadata.SessionID == ref.sessionID {
				return index, nil
			}
		}
		return 0, fmt.Errorf("session %q not found in checkpoint %s: %w", ref.sessionID, ref.checkpointID, ErrCheckpointNotFound)
	default:
		return 0, errors.New("unknown session ref mode")
	}
}

func (s *GitStore) updateSessionSummary(ctx context.Context, ref SessionRef, summary *Summary) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	if err := s.ensureSessionsBranch(ctx); err != nil {
		return fmt.Errorf("failed to ensure sessions branch: %w", err)
	}

	parentHash, rootTreeHash, err := s.getSessionsBranchRef()
	if err != nil {
		return err
	}

	basePath := ref.checkpointID.Path() + "/"
	entries, err := s.flattenCheckpointEntries(rootTreeHash, ref.checkpointID.Path())
	if err != nil {
		return err
	}

	rootMetadataPath := basePath + paths.MetadataFileName
	rootEntry, exists := entries[rootMetadataPath]
	if !exists {
		return ErrCheckpointNotFound
	}
	checkpointSummary, err := s.readSummaryFromBlob(rootEntry.Hash)
	if err != nil {
		return fmt.Errorf("failed to read checkpoint summary: %w", err)
	}
	sessionIndex, err := s.resolveSessionIndexFromSummary(ctx, ref, checkpointSummary)
	if err != nil {
		return err
	}

	sessionMetadataPath := fmt.Sprintf("%s%d/%s", basePath, sessionIndex, paths.MetadataFileName)
	sessionEntry, exists := entries[sessionMetadataPath]
	if !exists {
		return fmt.Errorf("session metadata not found at %s", sessionMetadataPath)
	}
	existingMetadata, err := s.readMetadataFromBlob(sessionEntry.Hash)
	if err != nil {
		return fmt.Errorf("failed to read session metadata: %w", err)
	}

	existingMetadata.Summary = redactSummary(summary)
	metadataHash, err := createCommittedMetadataBlob(s, existingMetadata)
	if err != nil {
		return err
	}
	entries[sessionMetadataPath] = object.TreeEntry{
		Name: sessionMetadataPath,
		Mode: filemode.Regular,
		Hash: metadataHash,
	}

	newTreeHash, err := s.spliceCheckpointSubtree(ctx, rootTreeHash, ref.checkpointID, basePath, entries)
	if err != nil {
		return err
	}
	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	commitMsg := fmt.Sprintf("Update summary for checkpoint %s (session: %s)", ref.checkpointID, existingMetadata.SessionID)
	newCommitHash, err := s.createCommit(ctx, newTreeHash, parentHash, commitMsg, authorName, authorEmail)
	if err != nil {
		return err
	}
	return s.setPrimaryRef(newCommitHash)
}

func createCommittedMetadataBlob(store *GitStore, metadata *CommittedMetadata) (plumbing.Hash, error) {
	metadataJSON, err := jsonutil.MarshalIndentWithNewline(metadata, "", "  ")
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to marshal metadata: %w", err)
	}
	metadataHash, err := CreateBlobFromContent(store.repo, metadataJSON)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to create metadata blob: %w", err)
	}
	return metadataHash, nil
}
