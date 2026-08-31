package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

const (
	checkpointKindAgentReview      = "agent_review"
	checkpointKindAgentInvestigate = "agent_investigate"
	checkpointKindImported         = "imported"
)

type preparedBatchSessions struct {
	request  BatchSessions
	sessions []preparedBatchSession
	tasks    map[string]plumbing.Hash
}

type preparedBatchSession struct {
	opts      WriteOptions
	treeHash  plumbing.Hash
	filePaths SessionFilePaths
	metadata  *Metadata
}

type persistentRefWriter interface {
	batchRefName(checkpointID id.CheckpointID) (plumbing.ReferenceName, error)
	batchRepo() *git.Repository
	prepareBatchRef(ctx context.Context) error
	afterBatchPublish(ctx context.Context, refName plumbing.ReferenceName)
}

type batchRefWriter interface {
	persistentRefWriter
	prepareBatchSessions(ctx context.Context, req BatchSessions) (*preparedBatchSessions, error)
	buildPreparedBatchCommit(ctx context.Context, prepared *preparedBatchSessions, parentHash plumbing.Hash) (plumbing.Hash, error)
}

type attributionRefWriter interface {
	persistentRefWriter
	buildPreparedAttributionCommit(ctx context.Context, checkpointID id.CheckpointID, attribution *Attribution, parentHash plumbing.Hash, authorName, authorEmail string) (plumbing.Hash, error)
}

type finalBatchSession struct {
	metadata         *Metadata
	hasReview        bool
	hasInvestigation bool
	imported         bool
}

// batchBuildStats is returned by the builder so tests can measure checkpoint
// content visitation without timing noise from Git object compression or sort.
type batchBuildStats struct {
	RootEntriesVisited        int
	SessionEntriesVisited     int
	ExistingMetadataRead      int
	TaskEntriesVisited        int
	FinalSessionContributions int
}

func (s batchBuildStats) checkpointContentVisits() int {
	return s.RootEntriesVisited + s.SessionEntriesVisited + s.ExistingMetadataRead +
		s.TaskEntriesVisited + s.FinalSessionContributions
}

// CanonicalizeBatchSessions validates a batch before any checkpoint object is
// created, then returns an independent, SessionID-sorted request. Duplicate
// session IDs are rejected because two payloads for one ID have no canonical
// winner.
func CanonicalizeBatchSessions(req BatchSessions) (BatchSessions, error) {
	if req.CheckpointID.IsEmpty() {
		return BatchSessions{}, errors.New("invalid batch checkpoint options: checkpoint ID is required")
	}
	if err := id.Validate(req.CheckpointID.String()); err != nil {
		return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: %w", err)
	}
	if len(req.Sessions) == 0 {
		return BatchSessions{}, errors.New("invalid batch checkpoint options: at least one session is required")
	}

	canonical := req
	canonical.Sessions = append([]ReservedSession(nil), req.Sessions...)
	sort.Slice(canonical.Sessions, func(i, j int) bool {
		return WriteOptions(canonical.Sessions[i]).SessionID < WriteOptions(canonical.Sessions[j]).SessionID
	})
	seenSessions := make(map[string]struct{}, len(canonical.Sessions))
	taskOwners := make(map[string]string)
	for i := range canonical.Sessions {
		opts := WriteOptions(canonical.Sessions[i])
		if opts.CheckpointID.IsEmpty() {
			return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: session %q has no checkpoint ID", opts.SessionID)
		}
		if opts.CheckpointID != req.CheckpointID {
			return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: session %q checkpoint ID %s does not match batch checkpoint ID %s", opts.SessionID, opts.CheckpointID, req.CheckpointID)
		}
		if err := validation.ValidateSessionID(opts.SessionID); err != nil {
			return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: %w", err)
		}
		if _, exists := seenSessions[opts.SessionID]; exists {
			return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: duplicate session ID %q", opts.SessionID)
		}
		seenSessions[opts.SessionID] = struct{}{}

		seenTasks := make(map[string]struct{}, len(opts.Tasks))
		for _, task := range opts.Tasks {
			if task.ToolUseID == "" {
				return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: session %q has a task with an empty tool-use ID", opts.SessionID)
			}
			if err := validation.ValidateToolUseID(task.ToolUseID); err != nil {
				return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: %w", err)
			}
			if err := validation.ValidateAgentID(task.AgentID); err != nil {
				return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: %w", err)
			}
			if _, exists := seenTasks[task.ToolUseID]; exists {
				return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: session %q repeats task tool-use ID %q", opts.SessionID, task.ToolUseID)
			}
			seenTasks[task.ToolUseID] = struct{}{}
			if owner, exists := taskOwners[task.ToolUseID]; exists {
				return BatchSessions{}, fmt.Errorf("invalid batch checkpoint options: task tool-use ID %q appears in sessions %q and %q", task.ToolUseID, owner, opts.SessionID)
			}
			taskOwners[task.ToolUseID] = opts.SessionID
		}
	}

	// Git commit timestamps have one-second precision. Use that same precision
	// for zero per-session CreatedAt values so the promised shared instant
	// survives commit encoding and a later read.
	if canonical.CommitTime.IsZero() {
		canonical.CommitTime = time.Now().UTC().Truncate(time.Second)
	} else {
		canonical.CommitTime = canonical.CommitTime.UTC().Truncate(time.Second)
	}
	for i := range canonical.Sessions {
		opts := WriteOptions(canonical.Sessions[i])
		if opts.CreatedAt.IsZero() {
			opts.CreatedAt = canonical.CommitTime
		} else {
			opts.CreatedAt = opts.CreatedAt.UTC()
		}
		canonical.Sessions[i] = ReservedSession(opts)
	}
	return canonical, nil
}

func (s *treeWriter) prepareBatchSessions(ctx context.Context, req BatchSessions) (*preparedBatchSessions, error) {
	canonical, err := CanonicalizeBatchSessions(req)
	if err != nil {
		return nil, err
	}
	if canonical.AuthorName == "" || canonical.AuthorEmail == "" {
		name, email := GetGitAuthorFromRepo(s.repo)
		if canonical.AuthorName == "" {
			canonical.AuthorName = name
		}
		if canonical.AuthorEmail == "" {
			canonical.AuthorEmail = email
		}
	}

	// go-git's filesystem object storer mutates an internal object cache while
	// writing and reading objects, so concurrent builders for one repository
	// must not enter it together. Ref publication remains independently guarded
	// by native Git compare-and-swap.
	objectLock := repositoryObjectLock(s.repo)
	objectLock.Lock()
	defer objectLock.Unlock()

	prepared := &preparedBatchSessions{
		request:  canonical,
		sessions: make([]preparedBatchSession, 0, len(canonical.Sessions)),
		tasks:    make(map[string]plumbing.Hash),
	}
	for _, reserved := range canonical.Sessions {
		opts := WriteOptions(reserved)
		entries := make(map[string]object.TreeEntry)
		filePaths, err := s.writeSessionToSubdirectory(ctx, opts, "", entries)
		if err != nil {
			return nil, fmt.Errorf("prepare session %q: %w", opts.SessionID, err)
		}
		if opts.MetadataDir != "" {
			if err := s.copyMetadataDir(ctx, opts.MetadataDir, "", entries); err != nil {
				return nil, fmt.Errorf("prepare session %q metadata directory: %w", opts.SessionID, err)
			}
		}
		metadataEntry, ok := entries[paths.MetadataFileName]
		if !ok {
			return nil, fmt.Errorf("prepare session %q: metadata entry was not created", opts.SessionID)
		}
		metadata, err := s.readMetadataFromBlob(metadataEntry.Hash)
		if err != nil {
			return nil, fmt.Errorf("prepare session %q metadata: %w", opts.SessionID, err)
		}
		treeHash, err := BuildTreeFromEntries(ctx, s.repo, entries)
		if err != nil {
			return nil, fmt.Errorf("prepare session %q tree: %w", opts.SessionID, err)
		}
		prepared.sessions = append(prepared.sessions, preparedBatchSession{
			opts: opts, treeHash: treeHash, filePaths: filePaths, metadata: metadata,
		})

		for _, task := range opts.Tasks {
			taskHash, err := s.prepareTaskSubtree(ctx, task)
			if err != nil {
				return nil, fmt.Errorf("prepare task %q for session %q: %w", task.ToolUseID, opts.SessionID, err)
			}
			prepared.tasks[task.ToolUseID] = taskHash
		}
	}
	return prepared, nil
}

func (s *treeWriter) prepareTaskSubtree(ctx context.Context, task TaskPayload) (plumbing.Hash, error) {
	entries := make(map[string]object.TreeEntry)
	if err := s.writeTaskRecordEntry(task, "", entries); err != nil {
		return plumbing.ZeroHash, err
	}
	rootHash, err := BuildTreeFromEntries(ctx, s.repo, entries)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	root, err := s.repo.TreeObject(rootHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read prepared task root: %w", err)
	}
	taskTree, err := root.Tree(checkpointSubtreePath("tasks", task.ToolUseID))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read prepared task subtree: %w", err)
	}
	return taskTree.Hash, nil
}

func singleSessionBatch(opts WriteOptions) BatchSessions {
	return BatchSessions{
		CheckpointID:  opts.CheckpointID,
		Sessions:      []ReservedSession{ReservedSession(opts)},
		AuthorName:    opts.AuthorName,
		AuthorEmail:   opts.AuthorEmail,
		CommitSubject: opts.CommitSubject,
	}
}

func writeSingleSessionViaBatch(ctx context.Context, backend persistentWriteBackend, opts WriteOptions) error {
	if err := backend.writeBatchSessions(ctx, singleSessionBatch(opts)); err != nil {
		return err
	}
	if opts.CombinedAttribution != nil {
		return backend.backfillAttribution(ctx, opts.CheckpointID, opts.CombinedAttribution)
	}
	return nil
}

func (s *GitStore) writeBatchSessions(ctx context.Context, req BatchSessions) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	prepared, err := s.prepareBatchSessions(ctx, req)
	if err != nil {
		return err
	}
	return writePreparedBatch(ctx, s, prepared)
}

func (s *GitStore) batchRefName(id.CheckpointID) (plumbing.ReferenceName, error) {
	return s.refs.Primary, nil
}

func (s *gitRefsStore) writeBatchSessions(ctx context.Context, req BatchSessions) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	prepared, err := s.prepareBatchSessions(ctx, req)
	if err != nil {
		return err
	}
	return writePreparedBatch(ctx, s, prepared)
}

func (s *gitRefsStore) batchRefName(checkpointID id.CheckpointID) (plumbing.ReferenceName, error) {
	return RefName(checkpointID)
}

func (s *GitStore) batchRepo() *git.Repository {
	return s.repo
}

func (s *gitRefsStore) batchRepo() *git.Repository {
	return s.repo
}

func (s *GitStore) prepareBatchRef(ctx context.Context) error {
	if err := s.ensureSessionsBranch(ctx); err != nil {
		return fmt.Errorf("failed to ensure sessions branch: %w", err)
	}
	return nil
}

func (*GitStore) afterBatchPublish(context.Context, plumbing.ReferenceName) {}

func (*gitRefsStore) prepareBatchRef(context.Context) error {
	return nil
}

func (s *gitRefsStore) afterBatchPublish(ctx context.Context, refName plumbing.ReferenceName) {
	s.enqueueForPush(ctx, refName)
}

func (s *GitStore) buildPreparedBatchCommit(ctx context.Context, prepared *preparedBatchSessions, parentHash plumbing.Hash) (plumbing.Hash, error) {
	rootTreeHash, err := s.rootTreeHashAt(parentHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	existing, err := s.subtreeObjAt(rootTreeHash, prepared.request.CheckpointID.Path())
	if err != nil {
		return plumbing.ZeroHash, err
	}
	checkpointSubtree, _, err := s.applyPreparedBatch(ctx, prepared, existing, prepared.request.CheckpointID.Path()+"/")
	if err != nil {
		return plumbing.ZeroHash, err
	}
	newTreeHash, err := s.spliceCheckpointSubtree(rootTreeHash, prepared.request.CheckpointID, checkpointSubtree)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	newTreeHash, err = s.maybeMergeVercelConfig(ctx, newTreeHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return createCommitAt(ctx, s.repo, newTreeHash, parentHash, buildBatchCommitMessage(prepared.request),
		prepared.request.AuthorName, prepared.request.AuthorEmail, prepared.request.CommitTime)
}

func (s *gitRefsStore) buildPreparedBatchCommit(ctx context.Context, prepared *preparedBatchSessions, parentHash plumbing.Hash) (plumbing.Hash, error) {
	existing, err := s.checkpointTreeAt(parentHash, prepared.request.CheckpointID)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	checkpointSubtree, _, err := s.applyPreparedBatch(ctx, prepared, existing, "")
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return createCommitAt(ctx, s.repo, checkpointSubtree, parentHash, buildBatchCommitMessage(prepared.request),
		prepared.request.AuthorName, prepared.request.AuthorEmail, prepared.request.CommitTime)
}

func (s *GitStore) buildPreparedAttributionCommit(ctx context.Context, checkpointID id.CheckpointID, attribution *Attribution, parentHash plumbing.Hash, authorName, authorEmail string) (plumbing.Hash, error) {
	if parentHash.IsZero() {
		return plumbing.ZeroHash, ErrCheckpointNotFound
	}
	rootTreeHash, err := s.rootTreeHashAt(parentHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	existing, err := s.subtreeObjAt(rootTreeHash, checkpointID.Path())
	if err != nil {
		return plumbing.ZeroHash, err
	}
	checkpointSubtree, err := s.applyAttributionBackfill(ctx, existing, checkpointID.Path()+"/", attribution)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	newTreeHash, err := s.spliceCheckpointSubtree(rootTreeHash, checkpointID, checkpointSubtree)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return CreateCommit(ctx, s.repo, newTreeHash, parentHash, fmt.Sprintf("Update checkpoint summary for %s", checkpointID), authorName, authorEmail)
}

func (s *gitRefsStore) buildPreparedAttributionCommit(ctx context.Context, checkpointID id.CheckpointID, attribution *Attribution, parentHash plumbing.Hash, authorName, authorEmail string) (plumbing.Hash, error) {
	existing, err := s.checkpointTreeAt(parentHash, checkpointID)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	checkpointSubtree, err := s.applyAttributionBackfill(ctx, existing, "", attribution)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return CreateCommit(ctx, s.repo, checkpointSubtree, parentHash, fmt.Sprintf("Update checkpoint summary for %s", checkpointID), authorName, authorEmail)
}

func writePreparedBatch(ctx context.Context, writer batchRefWriter, prepared *preparedBatchSessions) error {
	refName, err := writer.batchRefName(prepared.request.CheckpointID)
	if err != nil {
		return err
	}
	if err := writer.prepareBatchRef(ctx); err != nil {
		return err
	}
	_, err = RunRefTransaction(ctx, writer.batchRepo(), refName, func(parentHash plumbing.Hash) (plumbing.Hash, bool, error) {
		commitHash, err := writer.buildPreparedBatchCommit(ctx, prepared, parentHash)
		return commitHash, true, err
	})
	if err != nil {
		return err
	}
	writer.afterBatchPublish(ctx, refName)
	return nil
}

func (s *treeWriter) applyPreparedBatch(
	ctx context.Context,
	prepared *preparedBatchSessions,
	existing *object.Tree,
	basePath string,
) (plumbing.Hash, batchBuildStats, error) {
	var stats batchBuildStats
	if err := ctx.Err(); err != nil {
		return plumbing.ZeroHash, stats, err //nolint:wrapcheck // Propagating context cancellation
	}

	rootEntries := make(map[string]object.TreeEntry)
	if existing != nil {
		for _, entry := range existing.Entries {
			stats.RootEntriesVisited++
			rootEntries[entry.Name] = entry
		}
	}

	var existingSummary *CheckpointSummary
	if entry, ok := rootEntries[paths.MetadataFileName]; ok {
		var err error
		existingSummary, err = s.readSummaryFromBlob(entry.Hash)
		if err != nil {
			return plumbing.ZeroHash, stats, fmt.Errorf("read existing checkpoint summary: %w", err)
		}
		if existingSummary.CheckpointID != prepared.request.CheckpointID {
			return plumbing.ZeroHash, stats, fmt.Errorf("existing checkpoint summary ID %s does not match batch checkpoint ID %s", existingSummary.CheckpointID, prepared.request.CheckpointID)
		}
	} else if existing != nil && len(existing.Entries) != 0 {
		return plumbing.ZeroHash, stats, errors.New("existing checkpoint tree has no root metadata")
	}

	var sessionPaths []SessionFilePaths
	finalByID := make(map[string]finalBatchSession)
	indexBySession := make(map[string]int)
	if existingSummary != nil {
		sessionPaths = append([]SessionFilePaths(nil), existingSummary.Sessions...)
		for index := range existingSummary.Sessions {
			entry, ok := rootEntries[strconv.Itoa(index)]
			if !ok || entry.Mode != filemode.Dir {
				return plumbing.ZeroHash, stats, fmt.Errorf("existing session %d subtree is missing", index)
			}
			metadata, err := s.readSessionMetadataFromSubtree(entry.Hash, &stats)
			if err != nil {
				return plumbing.ZeroHash, stats, fmt.Errorf("read existing session %d metadata: %w", index, err)
			}
			if _, duplicate := finalByID[metadata.SessionID]; duplicate {
				return plumbing.ZeroHash, stats, fmt.Errorf("existing checkpoint repeats session ID %q", metadata.SessionID)
			}
			finalByID[metadata.SessionID] = finalSessionFromMetadata(metadata)
			indexBySession[metadata.SessionID] = index
		}
	}

	nextSessionIndex := len(sessionPaths)
	for _, incoming := range prepared.sessions {
		index, exists := indexBySession[incoming.opts.SessionID]
		if !exists {
			index = nextSessionIndex
			nextSessionIndex++
			indexBySession[incoming.opts.SessionID] = index
			sessionPaths = append(sessionPaths, SessionFilePaths{})
		}
		rootEntries[strconv.Itoa(index)] = object.TreeEntry{
			Name: strconv.Itoa(index), Mode: filemode.Dir, Hash: incoming.treeHash,
		}
		sessionPaths[index] = qualifySessionFilePaths(basePath, index, incoming.filePaths)
		finalByID[incoming.opts.SessionID] = finalBatchSession{
			metadata:  incoming.metadata,
			hasReview: incoming.opts.HasReview || incoming.metadata.Kind == checkpointKindAgentReview,
			hasInvestigation: incoming.opts.HasInvestigation ||
				incoming.metadata.Kind == checkpointKindAgentInvestigate,
			imported: incoming.opts.Kind == checkpointKindImported,
		}
	}

	if len(prepared.tasks) > 0 {
		tasksHash, err := s.mergePreparedTasks(rootEntries, prepared.tasks, &stats)
		if err != nil {
			return plumbing.ZeroHash, stats, err
		}
		rootEntries["tasks"] = object.TreeEntry{Name: "tasks", Mode: filemode.Dir, Hash: tasksHash}
	}

	summary := reduceBatchSummary(prepared.request.CheckpointID, existingSummary, sessionPaths, finalByID, &stats)
	metadataJSON, err := jsonutil.MarshalIndentWithNewline(summary, "", "  ")
	if err != nil {
		return plumbing.ZeroHash, stats, fmt.Errorf("marshal checkpoint summary: %w", err)
	}
	metadataHash, err := CreateBlobFromContent(s.repo, metadataJSON)
	if err != nil {
		return plumbing.ZeroHash, stats, fmt.Errorf("create checkpoint summary blob: %w", err)
	}
	rootEntries[paths.MetadataFileName] = object.TreeEntry{
		Name: paths.MetadataFileName, Mode: filemode.Regular, Hash: metadataHash,
	}

	rootHash, err := storeTreeEntryMap(s.repo, rootEntries)
	if err != nil {
		return plumbing.ZeroHash, stats, fmt.Errorf("store checkpoint root tree: %w", err)
	}
	return rootHash, stats, nil
}

func (s *treeWriter) readSessionMetadataFromSubtree(hash plumbing.Hash, stats *batchBuildStats) (*Metadata, error) {
	tree, err := s.repo.TreeObject(hash)
	if err != nil {
		return nil, fmt.Errorf("read session subtree: %w", err)
	}
	for _, entry := range tree.Entries {
		stats.SessionEntriesVisited++
		if entry.Name == paths.MetadataFileName && entry.Mode != filemode.Dir {
			stats.ExistingMetadataRead++
			return s.readMetadataFromBlob(entry.Hash)
		}
	}
	return nil, errors.New("metadata.json not found")
}

func (s *treeWriter) mergePreparedTasks(
	rootEntries map[string]object.TreeEntry,
	prepared map[string]plumbing.Hash,
	stats *batchBuildStats,
) (plumbing.Hash, error) {
	taskEntries := make(map[string]object.TreeEntry)
	if existing, ok := rootEntries["tasks"]; ok {
		if existing.Mode != filemode.Dir {
			return plumbing.ZeroHash, errors.New("existing tasks entry is not a directory")
		}
		tree, err := s.repo.TreeObject(existing.Hash)
		if err != nil {
			return plumbing.ZeroHash, fmt.Errorf("read existing tasks tree: %w", err)
		}
		for _, entry := range tree.Entries {
			stats.TaskEntriesVisited++
			taskEntries[entry.Name] = entry
		}
	}
	for toolUseID, hash := range prepared {
		taskEntries[toolUseID] = object.TreeEntry{Name: toolUseID, Mode: filemode.Dir, Hash: hash}
	}
	return storeTreeEntryMap(s.repo, taskEntries)
}

func reduceBatchSummary(
	checkpointID id.CheckpointID,
	existing *CheckpointSummary,
	sessionPaths []SessionFilePaths,
	finalByID map[string]finalBatchSession,
	stats *batchBuildStats,
) *CheckpointSummary {
	orderedIDs := make([]string, 0, len(finalByID))
	for sessionID := range finalByID {
		orderedIDs = append(orderedIDs, sessionID)
	}
	sort.Strings(orderedIDs)

	summary := &CheckpointSummary{
		CheckpointID: checkpointID,
		CLIVersion:   versioninfo.Version,
		Sessions:     sessionPaths,
	}
	if existing != nil {
		summary.CommitSHA = existing.CommitSHA
		summary.CombinedAttribution = existing.CombinedAttribution
		summary.HasReview = existing.HasReview
		summary.HasInvestigation = existing.HasInvestigation
		summary.Imported = existing.Imported
	}
	files := make(map[string]struct{})
	for _, sessionID := range orderedIDs {
		stats.FinalSessionContributions++
		final := finalByID[sessionID]
		metadata := final.metadata
		summary.Strategy = metadata.Strategy
		summary.Branch = metadata.Branch
		if metadata.CommitSHA != "" {
			summary.CommitSHA = metadata.CommitSHA
		}
		summary.CheckpointsCount += metadata.CheckpointsCount
		summary.TokenUsage = types.AddTokenUsage(summary.TokenUsage, metadata.TokenUsage)
		summary.HasReview = summary.HasReview || final.hasReview
		summary.HasInvestigation = summary.HasInvestigation || final.hasInvestigation
		summary.Imported = summary.Imported || final.imported
		for _, file := range metadata.FilesTouched {
			files[filepath.ToSlash(file)] = struct{}{}
		}
	}
	if len(files) > 0 {
		summary.FilesTouched = make([]string, 0, len(files))
		for file := range files {
			summary.FilesTouched = append(summary.FilesTouched, file)
		}
		sort.Strings(summary.FilesTouched)
	}
	return summary
}

func finalSessionFromMetadata(metadata *Metadata) finalBatchSession {
	return finalBatchSession{
		metadata:         metadata,
		hasReview:        metadata.Kind == checkpointKindAgentReview,
		hasInvestigation: metadata.Kind == checkpointKindAgentInvestigate,
		imported:         metadata.Kind == checkpointKindImported,
	}
}

func qualifySessionFilePaths(basePath string, index int, relative SessionFilePaths) SessionFilePaths {
	qualify := func(value string) string {
		if value == "" {
			return ""
		}
		return "/" + checkpointSubtreePath(basePath, strconv.Itoa(index), strings.TrimPrefix(value, "/"))
	}
	return SessionFilePaths{
		Metadata:          qualify(relative.Metadata),
		Transcript:        qualify(relative.Transcript),
		CompactTranscript: qualify(relative.CompactTranscript),
		ContentHash:       qualify(relative.ContentHash),
		Prompt:            qualify(relative.Prompt),
		AssetsManifest:    qualify(relative.AssetsManifest),
	}
}

func storeTreeEntryMap(repo *git.Repository, entries map[string]object.TreeEntry) (plumbing.Hash, error) {
	ordered := make([]object.TreeEntry, 0, len(entries))
	for name, entry := range entries {
		entry.Name = name
		ordered = append(ordered, entry)
	}
	sortTreeEntries(ordered)
	return storeTree(repo, ordered)
}

func buildBatchCommitMessage(req BatchSessions) string {
	var message strings.Builder
	fmt.Fprintf(&message, "Checkpoint: %s\n\n", req.CheckpointID)
	if req.CommitSubject != "" {
		message.WriteString(req.CommitSubject)
		message.WriteString("\n\n")
	}
	for _, session := range req.Sessions {
		fmt.Fprintf(&message, "%s: %s\n", trailers.SessionTrailerKey, WriteOptions(session).SessionID)
	}
	if value, common := commonBatchValue(req.Sessions, func(opts WriteOptions) string { return opts.Strategy }); common {
		fmt.Fprintf(&message, "%s: %s\n", trailers.StrategyTrailerKey, value)
	}
	if value, common := commonBatchValue(req.Sessions, func(opts WriteOptions) string { return string(opts.Agent) }); common && value != "" {
		fmt.Fprintf(&message, "%s: %s\n", trailers.AgentTrailerKey, value)
	}
	if value, common := commonBatchValue(req.Sessions, func(opts WriteOptions) string { return opts.EphemeralBranch }); common && value != "" {
		fmt.Fprintf(&message, "%s: %s\n", trailers.EphemeralBranchTrailerKey, value)
	}
	return message.String()
}

func commonBatchValue(sessions []ReservedSession, value func(WriteOptions) string) (string, bool) {
	first := value(WriteOptions(sessions[0]))
	for _, session := range sessions[1:] {
		if value(WriteOptions(session)) != first {
			return "", false
		}
	}
	return first, true
}
