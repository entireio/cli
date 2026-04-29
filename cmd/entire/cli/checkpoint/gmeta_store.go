package checkpoint

import (
	"context"
	"crypto/sha1" //nolint:gosec // SHA-1 used per gmeta spec for fanout/set keys, not for security
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// GmetaRefName is the local ref for gmeta exchange format metadata.
// Per gmeta spec, local metadata lives on refs/meta/local/main.
const GmetaRefName = "refs/meta/local/main"

// GmetaRemoteRefName is the ref name used on the remote server.
// Per gmeta spec, the remote stores metadata at refs/meta/main (no local/ prefix).
// Push refspec: refs/meta/local/main:refs/meta/main
const GmetaRemoteRefName = "refs/meta/main"

// GmetaStore provides checkpoint storage in gmeta exchange format.
// It writes metadata to refs/meta/local/main using the gmeta tree layout
// convention (change-id targets with string/__value, list/__list, set/__set).
//
// GmetaStore is non-authoritative — it's a third write alongside v1 and v2,
// proving interop with the gmeta Rust CLI.
type GmetaStore struct {
	repo *git.Repository
	gs   *GitStore // shared entry-building helpers
}

// NewGmetaStore creates a new gmeta checkpoint store backed by the given git repository.
func NewGmetaStore(repo *git.Repository) *GmetaStore {
	return &GmetaStore{
		repo: repo,
		gs:   &GitStore{repo: repo},
	}
}

// WriteCommitted writes or appends a session to a checkpoint in gmeta format.
// If the checkpoint already exists, the new session is added alongside existing ones.
// Handles both session and task checkpoints (including incremental).
func (s *GmetaStore) WriteCommitted(ctx context.Context, opts WriteCommittedOptions) error {
	if err := validateWriteOpts(opts); err != nil {
		return err
	}

	refName := plumbing.ReferenceName(GmetaRefName)
	parentHash, rootTreeHash, err := s.getRefStateOrEmpty(refName)
	if err != nil {
		return err
	}

	targetPath := gmetaTargetPath(opts.CheckpointID)
	basePath := targetPath + "/"

	// Read existing entries at this target path
	entries, err := s.flattenTargetEntries(rootTreeHash, targetPath)
	if err != nil {
		return err
	}

	// Write checkpoint-level fields
	s.writeCheckpointFields(opts, basePath, entries)

	// Write session data
	sessionID := opts.SessionID
	sessionPath := basePath + "session/" + sessionID + "/"

	if opts.IsTask {
		if err := s.writeTaskEntries(ctx, opts, sessionPath, entries); err != nil {
			return err
		}
	} else {
		if err := s.writeSessionEntries(ctx, opts, sessionPath, entries); err != nil {
			return err
		}
	}

	// Add session ID to the ordered list (if not already present)
	s.addSessionIDToList(basePath, sessionID, entries)

	// Build tree and commit
	return s.commitEntries(ctx, refName, parentHash, rootTreeHash, targetPath, basePath, entries,
		"", opts.AuthorName, opts.AuthorEmail)
}

// UpdateCommitted replaces transcript and prompts for an existing session.
// Used at stop time to finalize with complete session data.
func (s *GmetaStore) UpdateCommitted(ctx context.Context, opts UpdateCommittedOptions) error {
	if opts.CheckpointID.IsEmpty() {
		return errors.New("invalid update options: checkpoint ID is required")
	}

	refName := plumbing.ReferenceName(GmetaRefName)
	parentHash, rootTreeHash, err := s.getRefState(refName)
	if err != nil {
		return ErrCheckpointNotFound
	}

	targetPath := gmetaTargetPath(opts.CheckpointID)
	basePath := targetPath + "/"
	sessionPath := basePath + "session/" + opts.SessionID + "/"

	entries, err := s.flattenTargetEntries(rootTreeHash, targetPath)
	if err != nil {
		return err
	}

	// Check that the checkpoint exists
	if len(entries) == 0 {
		return ErrCheckpointNotFound
	}

	// Replace transcript
	if len(opts.Transcript.Bytes()) > 0 {
		// Clear existing transcript list entries
		listPrefix := sessionPath + "transcript/__list/"
		for key := range entries {
			if strings.HasPrefix(key, listPrefix) {
				delete(entries, key)
			}
		}
		if err := s.writeTranscriptList(ctx, opts.Transcript.Bytes(), opts.Agent, sessionPath, entries); err != nil {
			return err
		}
	}

	// Replace prompt
	if len(opts.Prompts) > 0 {
		promptContent := redact.String(JoinPrompts(opts.Prompts))
		blobHash, err := CreateBlobFromContent(s.repo, []byte(promptContent))
		if err != nil {
			return fmt.Errorf("failed to create prompt blob: %w", err)
		}
		entries[sessionPath+"prompt/__value"] = object.TreeEntry{
			Name: sessionPath + "prompt/__value",
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	return s.commitEntries(ctx, refName, parentHash, rootTreeHash, targetPath, basePath, entries,
		"", authorName, authorEmail)
}

// gmetaTargetPath returns the gmeta tree base path for a checkpoint ID.
// Per gmeta spec: change-id/<sha1(checkpoint-id)[:2]>/<checkpoint-id>/
func gmetaTargetPath(cpID id.CheckpointID) string {
	fanout := gmetaFanout(string(cpID))
	return "change-id/" + fanout + "/" + string(cpID)
}

// gmetaFanout returns the first 2 hex chars of SHA-1(value).
// Per gmeta spec, change-id targets use SHA-1 hash of the value for fanout.
func gmetaFanout(value string) string {
	h := sha1.Sum([]byte(value)) //nolint:gosec // gmeta spec requires SHA-1 for fanout
	return fmt.Sprintf("%02x", h[0])
}

// gmetaListEntryID generates a list entry ID: <timestamp-ms>-<content-hash-prefix>.
// Per gmeta spec, list entries use this format for deterministic ordering.
func gmetaListEntryID(content []byte, offsetMs int) string {
	ts := time.Now().UnixMilli() + int64(offsetMs)
	h := sha1.Sum(content) //nolint:gosec // gmeta spec uses SHA-1 for content hash prefix
	hashPrefix := hex.EncodeToString(h[:])
	return fmt.Sprintf("%d-%s", ts, hashPrefix[:5])
}

// gmetaSetEntryName returns the set entry filename: sha1(value).
func gmetaSetEntryName(value string) string {
	// The reference implementation uses the Git blob object ID for set members,
	// not a raw content hash. That keeps the tree entry name equal to the blob ID.
	h := sha1.Sum(append([]byte(fmt.Sprintf("blob %d\x00", len(value))), value...)) //nolint:gosec // gmeta spec/reference impl uses SHA-1 object IDs
	return hex.EncodeToString(h[:])
}

// writeCheckpointFields writes checkpoint-level gmeta entries (strategy, cli-version, branch, etc.).
func (s *GmetaStore) writeCheckpointFields(opts WriteCommittedOptions, basePath string, entries map[string]object.TreeEntry) {
	entirePrefix := basePath + "entire/"

	// String values
	stringFields := map[string]string{
		"strategy":          opts.Strategy,
		"cli-version":       versioninfo.Version,
		"branch":            opts.Branch,
		"checkpoints-count": strconv.Itoa(opts.CheckpointsCount),
	}
	for key, value := range stringFields {
		if value == "" {
			continue
		}
		s.writeStringValue(entirePrefix+key+"/__value", value, entries)
	}

	// Set: files-touched
	if len(opts.FilesTouched) > 0 {
		setPrefix := entirePrefix + "files-touched/__set/"
		for _, file := range opts.FilesTouched {
			entryName := gmetaSetEntryName(file)
			path := setPrefix + entryName
			s.writeStringValue(path, file, entries)
		}
	}
}

// writeSessionEntries writes session-level gmeta entries (agent info, prompt, transcript).
func (s *GmetaStore) writeSessionEntries(ctx context.Context, opts WriteCommittedOptions, sessionPath string, entries map[string]object.TreeEntry) error {
	// Agent info
	if opts.Agent != "" {
		s.writeStringValue(sessionPath+"agent/name/__value", string(opts.Agent), entries)
	}
	if opts.Model != "" {
		s.writeStringValue(sessionPath+"agent/model/__value", opts.Model, entries)
	}

	// Token usage — individual keys for gmeta queryability
	if opts.TokenUsage != nil {
		writeTokenUsageEntries(s, sessionPath+"entire/token-usage/", opts.TokenUsage, entries)
	}

	// Attribution — individual keys for gmeta queryability
	if opts.InitialAttribution != nil {
		if err := writeInitialAttributionEntries(s, sessionPath+"entire/attribution/", opts.InitialAttribution, entries); err != nil {
			return err
		}
	}

	// Prompt
	if len(opts.Prompts) > 0 {
		promptContent := redact.String(JoinPrompts(opts.Prompts))
		blobHash, err := CreateBlobFromContent(s.repo, []byte(promptContent))
		if err != nil {
			return fmt.Errorf("failed to create prompt blob: %w", err)
		}
		entries[sessionPath+"prompt/__value"] = object.TreeEntry{
			Name: sessionPath + "prompt/__value",
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	// Transcript
	transcriptBytes := opts.Transcript.Bytes()
	if len(transcriptBytes) == 0 && opts.TranscriptPath != "" {
		var readErr error
		transcriptBytes, readErr = os.ReadFile(opts.TranscriptPath)
		if readErr != nil {
			transcriptBytes = nil
		}
	}
	if len(transcriptBytes) > 0 {
		if err := s.writeTranscriptList(ctx, transcriptBytes, opts.Agent, sessionPath, entries); err != nil {
			return err
		}
	}

	return nil
}

// writeTokenUsageEntries writes token usage as individual gmeta string values.
// Layout: <prefix>input/__value, <prefix>output/__value, etc.
func writeTokenUsageEntries(s *GmetaStore, prefix string, usage *agent.TokenUsage, entries map[string]object.TreeEntry) {
	if usage.InputTokens > 0 {
		s.writeStringValue(prefix+"input/__value", strconv.Itoa(usage.InputTokens), entries)
	}
	if usage.OutputTokens > 0 {
		s.writeStringValue(prefix+"output/__value", strconv.Itoa(usage.OutputTokens), entries)
	}
	if usage.CacheReadTokens > 0 {
		s.writeStringValue(prefix+"cache-read/__value", strconv.Itoa(usage.CacheReadTokens), entries)
	}
	if usage.CacheCreationTokens > 0 {
		s.writeStringValue(prefix+"cache-creation/__value", strconv.Itoa(usage.CacheCreationTokens), entries)
	}
	if usage.APICallCount > 0 {
		s.writeStringValue(prefix+"api-calls/__value", strconv.Itoa(usage.APICallCount), entries)
	}
	// Total for convenient querying
	total := usage.InputTokens + usage.OutputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if total > 0 {
		s.writeStringValue(prefix+"total/__value", strconv.Itoa(total), entries)
	}
}

// readTokenUsageFromTree reads token usage from gmeta string values in a tree.
// It prefers the namespaced session/entire/token-usage layout and falls back to
// the earlier session/token-usage layout for backward compatibility.
func readTokenUsageFromTree(repo *git.Repository, sessionTree *object.Tree) *agent.TokenUsage {
	usageTree, ok := readTreeFirstAvailable(sessionTree, "entire/token-usage", "token-usage")
	if !ok {
		return nil
	}
	usage := &agent.TokenUsage{
		InputTokens:         readGmetaIntValue(repo, usageTree, "input"),
		OutputTokens:        readGmetaIntValue(repo, usageTree, "output"),
		CacheReadTokens:     readGmetaIntValue(repo, usageTree, "cache-read"),
		CacheCreationTokens: readGmetaIntValue(repo, usageTree, "cache-creation"),
		APICallCount:        readGmetaIntValue(repo, usageTree, "api-calls"),
	}

	// Return nil if all zeros (no usage data)
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 {
		return nil
	}
	return usage
}

func writeInitialAttributionEntries(s *GmetaStore, prefix string, attr *InitialAttribution, entries map[string]object.TreeEntry) error {
	if attr == nil {
		return nil
	}

	if !attr.CalculatedAt.IsZero() {
		s.writeStringValue(prefix+"calculated-at/__value", attr.CalculatedAt.UTC().Format(time.RFC3339Nano), entries)
	}
	s.writeStringValue(prefix+"agent-lines/__value", strconv.Itoa(attr.AgentLines), entries)
	s.writeStringValue(prefix+"agent-removed/__value", strconv.Itoa(attr.AgentRemoved), entries)
	s.writeStringValue(prefix+"human-added/__value", strconv.Itoa(attr.HumanAdded), entries)
	s.writeStringValue(prefix+"human-modified/__value", strconv.Itoa(attr.HumanModified), entries)
	s.writeStringValue(prefix+"human-removed/__value", strconv.Itoa(attr.HumanRemoved), entries)
	s.writeStringValue(prefix+"total-committed/__value", strconv.Itoa(attr.TotalCommitted), entries)
	s.writeStringValue(prefix+"total-lines-changed/__value", strconv.Itoa(attr.TotalLinesChanged), entries)
	s.writeStringValue(prefix+"agent-percentage/__value", strconv.FormatFloat(attr.AgentPercentage, 'f', -1, 64), entries)
	s.writeStringValue(prefix+"metric-version/__value", strconv.Itoa(attr.MetricVersion), entries)
	return nil
}

func readInitialAttributionFromTree(repo *git.Repository, sessionTree *object.Tree) *InitialAttribution {
	return readInitialAttributionFromSubtree(repo, sessionTree, "entire/attribution")
}

func readInitialAttributionFromSubtree(repo *git.Repository, tree *object.Tree, path string) *InitialAttribution {
	attrTree, err := tree.Tree(path)
	if err != nil {
		return nil
	}

	attr := &InitialAttribution{
		AgentLines:        readGmetaIntValue(repo, attrTree, "agent-lines"),
		AgentRemoved:      readGmetaIntValue(repo, attrTree, "agent-removed"),
		HumanAdded:        readGmetaIntValue(repo, attrTree, "human-added"),
		HumanModified:     readGmetaIntValue(repo, attrTree, "human-modified"),
		HumanRemoved:      readGmetaIntValue(repo, attrTree, "human-removed"),
		TotalCommitted:    readGmetaIntValue(repo, attrTree, "total-committed"),
		TotalLinesChanged: readGmetaIntValue(repo, attrTree, "total-lines-changed"),
		MetricVersion:     readGmetaIntValue(repo, attrTree, "metric-version"),
	}

	if ts := readGmetaStringValue(repo, attrTree, "calculated-at"); ts != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, ts); parseErr == nil {
			attr.CalculatedAt = parsed
		}
	}
	if pct := readGmetaStringValue(repo, attrTree, "agent-percentage"); pct != "" {
		if parsed, parseErr := strconv.ParseFloat(pct, 64); parseErr == nil {
			attr.AgentPercentage = parsed
		}
	}

	if attr.CalculatedAt.IsZero() &&
		attr.AgentLines == 0 &&
		attr.AgentRemoved == 0 &&
		attr.HumanAdded == 0 &&
		attr.HumanModified == 0 &&
		attr.HumanRemoved == 0 &&
		attr.TotalCommitted == 0 &&
		attr.TotalLinesChanged == 0 &&
		attr.AgentPercentage == 0 &&
		attr.MetricVersion == 0 {
		return nil
	}
	return attr
}

// readGmetaIntValue reads a string value from a tree and parses it as an int.
// Returns 0 if the value doesn't exist or can't be parsed.
func readGmetaIntValue(repo *git.Repository, tree *object.Tree, key string) int {
	v := readGmetaStringValue(repo, tree, key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}

// writeTaskEntries writes task checkpoint entries under session/<id>/task/<tool-use-id>/.
func (s *GmetaStore) writeTaskEntries(ctx context.Context, opts WriteCommittedOptions, sessionPath string, entries map[string]object.TreeEntry) error {
	if err := validation.ValidateToolUseID(opts.ToolUseID); err != nil {
		return fmt.Errorf("invalid tool use ID: %w", err)
	}

	taskPath := sessionPath + "task/" + opts.ToolUseID + "/"

	if opts.IsIncremental {
		// Incremental task checkpoint: append the full checkpoint envelope to incremental/__list/
		redactedData, err := redact.JSONLBytes(opts.IncrementalData)
		if err != nil {
			redactedData = redact.AlreadyRedacted(redact.Bytes(opts.IncrementalData))
		}

		checkpoint := incrementalCheckpointData{
			Type:      opts.IncrementalType,
			ToolUseID: opts.ToolUseID,
			Timestamp: time.Now().UTC(),
			Data:      json.RawMessage(redactedData.Bytes()),
		}
		checkpointData, err := json.Marshal(checkpoint)
		if err != nil {
			return fmt.Errorf("failed to marshal incremental checkpoint: %w", err)
		}

		entryID := gmetaListEntryID(checkpointData, 0)
		entryPath := taskPath + "incremental/__list/" + entryID
		blobHash, err := CreateBlobFromContent(s.repo, checkpointData)
		if err != nil {
			return fmt.Errorf("failed to create incremental blob: %w", err)
		}
		entries[entryPath] = object.TreeEntry{
			Name: entryPath,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
		return nil
	}

	// Final task checkpoint
	if opts.AgentID != "" {
		s.writeStringValue(taskPath+"agent-id/__value", opts.AgentID, entries)
	}
	if opts.CheckpointUUID != "" {
		s.writeStringValue(taskPath+"checkpoint-uuid/__value", opts.CheckpointUUID, entries)
	}

	// Subagent transcript
	if opts.SubagentTranscriptPath != "" {
		transcriptData, err := os.ReadFile(opts.SubagentTranscriptPath)
		if err != nil {
			logging.Warn(ctx, "gmeta: failed to read subagent transcript",
				slog.String("path", opts.SubagentTranscriptPath),
				slog.String("error", err.Error()),
			)
		} else if len(transcriptData) > 0 {
			redacted, redactErr := redact.JSONLBytes(transcriptData)
			var redactedBytes []byte
			if redactErr != nil {
				redactedBytes = redact.Bytes(transcriptData)
			} else {
				redactedBytes = redacted.Bytes()
			}

			chunks, chunkErr := agent.ChunkTranscript(ctx, redactedBytes, opts.Agent)
			if chunkErr != nil {
				return fmt.Errorf("failed to chunk subagent transcript: %w", chunkErr)
			}
			for i, chunk := range chunks {
				entryID := gmetaListEntryID(chunk, i)
				entryPath := taskPath + "transcript/__list/" + entryID
				blobHash, err := CreateBlobFromContent(s.repo, chunk)
				if err != nil {
					return fmt.Errorf("failed to create transcript chunk blob: %w", err)
				}
				entries[entryPath] = object.TreeEntry{
					Name: entryPath,
					Mode: filemode.Regular,
					Hash: blobHash,
				}
			}
		}
	}

	return nil
}

// writeTranscriptList writes redacted, chunked transcript as gmeta list entries.
func (s *GmetaStore) writeTranscriptList(ctx context.Context, transcript []byte, agentType types.AgentType, sessionPath string, entries map[string]object.TreeEntry) error {
	redacted, err := redact.JSONLBytes(transcript)
	if err != nil {
		return fmt.Errorf("failed to redact transcript: %w", err)
	}

	chunks, err := agent.ChunkTranscript(ctx, redacted.Bytes(), agentType)
	if err != nil {
		return fmt.Errorf("failed to chunk transcript: %w", err)
	}

	listPrefix := sessionPath + "transcript/__list/"
	for i, chunk := range chunks {
		entryID := gmetaListEntryID(chunk, i)
		entryPath := listPrefix + entryID
		blobHash, err := CreateBlobFromContent(s.repo, chunk)
		if err != nil {
			return fmt.Errorf("failed to create transcript chunk blob: %w", err)
		}
		entries[entryPath] = object.TreeEntry{
			Name: entryPath,
			Mode: filemode.Regular,
			Hash: blobHash,
		}
	}

	return nil
}

// addSessionIDToList adds a session ID to the session/ids/__list/ if not already present.
func (s *GmetaStore) addSessionIDToList(basePath, sessionID string, entries map[string]object.TreeEntry) {
	listPrefix := basePath + "session/ids/__list/"

	// Check if session ID is already in the list by scanning existing entries
	for key, entry := range entries {
		if strings.HasPrefix(key, listPrefix) {
			// Read blob to check if it matches
			blob, err := s.repo.BlobObject(entry.Hash)
			if err == nil {
				reader, err := blob.Reader()
				if err == nil {
					content := make([]byte, blob.Size)
					if _, readErr := reader.Read(content); readErr == nil {
						if string(content) == sessionID {
							_ = reader.Close()
							return // Already present
						}
					}
					_ = reader.Close()
				}
			}
		}
	}

	// Add new entry
	entryID := gmetaListEntryID([]byte(sessionID), 0)
	entryPath := listPrefix + entryID
	blobHash, err := CreateBlobFromContent(s.repo, []byte(sessionID))
	if err != nil {
		return // Best-effort
	}
	entries[entryPath] = object.TreeEntry{
		Name: entryPath,
		Mode: filemode.Regular,
		Hash: blobHash,
	}
}

// writeStringValue is a helper that creates a blob and adds a tree entry.
func (s *GmetaStore) writeStringValue(path, value string, entries map[string]object.TreeEntry) {
	blobHash, err := CreateBlobFromContent(s.repo, []byte(value))
	if err != nil {
		return // Best-effort; caller logs warning
	}
	entries[path] = object.TreeEntry{
		Name: path,
		Mode: filemode.Regular,
		Hash: blobHash,
	}
}

// getRefState returns the parent commit hash and root tree hash for a ref.
func (s *GmetaStore) getRefState(refName plumbing.ReferenceName) (parentHash, treeHash plumbing.Hash, err error) {
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("ref %s not found: %w", refName, err)
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("failed to get commit: %w", err)
	}

	return ref.Hash(), commit.TreeHash, nil
}

// getRefStateOrEmpty returns the current ref state, or zero hashes when the ref does not exist yet.
func (s *GmetaStore) getRefStateOrEmpty(refName plumbing.ReferenceName) (parentHash, treeHash plumbing.Hash, err error) {
	parentHash, treeHash, err = s.getRefState(refName)
	if err == nil {
		return parentHash, treeHash, nil
	}

	if errors.Is(err, plumbing.ErrReferenceNotFound) || strings.Contains(err.Error(), "reference not found") {
		return plumbing.ZeroHash, plumbing.ZeroHash, nil
	}
	return plumbing.ZeroHash, plumbing.ZeroHash, err
}

// flattenTargetEntries reads entries under a gmeta target path from the root tree.
func (s *GmetaStore) flattenTargetEntries(rootTreeHash plumbing.Hash, targetPath string) (map[string]object.TreeEntry, error) {
	entries := make(map[string]object.TreeEntry)
	if rootTreeHash == plumbing.ZeroHash {
		return entries, nil
	}

	rootTree, err := s.repo.TreeObject(rootTreeHash)
	if err != nil {
		return entries, nil //nolint:nilerr // Tree doesn't exist yet
	}

	subtree, err := rootTree.Tree(targetPath)
	if err != nil {
		return entries, nil //nolint:nilerr // Target doesn't exist yet
	}

	if err := FlattenTree(s.repo, subtree, targetPath, entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// commitEntries builds a tree from entries, splices it into the root, and commits.
// targetPath is like "change-id/a3/a3b2c4d5e6f7" (no trailing slash).
// basePath is targetPath + "/" (with trailing slash).
func (s *GmetaStore) commitEntries(ctx context.Context, refName plumbing.ReferenceName, parentHash, rootTreeHash plumbing.Hash, targetPath, basePath string, entries map[string]object.TreeEntry, message, authorName, authorEmail string) error {
	oldEntries, err := s.flattenTargetEntries(rootTreeHash, targetPath)
	if err != nil {
		return err
	}

	// Convert entries to relative paths (strip basePath prefix)
	relEntries := make(map[string]object.TreeEntry, len(entries))
	for path, entry := range entries {
		relPath := strings.TrimPrefix(path, basePath)
		if relPath == path {
			continue
		}
		relEntries[relPath] = entry
	}

	// Build the target subtree from relative entries
	targetTreeHash, err := BuildTreeFromEntries(ctx, s.repo, relEntries)
	if err != nil {
		return fmt.Errorf("failed to build gmeta subtree: %w", err)
	}

	// Splice into root tree using tree surgery.
	// targetPath = "change-id/<fanout>/<checkpoint-id>"
	// We splice at ["change-id", "<fanout>"] with the checkpoint-id entry.
	segments := strings.Split(targetPath, "/")
	if len(segments) < 3 {
		return fmt.Errorf("invalid gmeta target path: %s", targetPath)
	}
	// Path segments for UpdateSubtree: all but the last segment
	parentSegments := segments[:len(segments)-1]
	leafName := segments[len(segments)-1]

	newRootHash, err := UpdateSubtree(s.repo, rootTreeHash, parentSegments, []object.TreeEntry{
		{Name: leafName, Mode: filemode.Dir, Hash: targetTreeHash},
	}, UpdateSubtreeOptions{MergeMode: MergeKeepExisting})
	if err != nil {
		return fmt.Errorf("failed to splice gmeta subtree: %w", err)
	}

	changes := gmetaCollectChanges(targetPath, basePath, oldEntries, entries)
	if message == "" {
		message = gmetaCommitMessage(changes)
	}

	// Commit
	if authorName == "" || authorEmail == "" {
		authorName, authorEmail = GetGitAuthorFromRepo(s.repo)
	}
	commitHash, err := CreateCommit(ctx, s.repo, newRootHash, parentHash, message, authorName, authorEmail)
	if err != nil {
		return fmt.Errorf("failed to create gmeta commit: %w", err)
	}

	ref := plumbing.NewHashReference(refName, commitHash)
	if err := s.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("failed to update gmeta ref: %w", err)
	}
	return nil
}

// --- Read methods ---

// ReadCommitted reads a checkpoint summary from the gmeta tree.
// Returns nil, nil if the checkpoint doesn't exist.
func (s *GmetaStore) ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	if checkpointID.IsEmpty() {
		return nil, nil //nolint:nilnil // Empty ID means no checkpoint
	}

	refName := plumbing.ReferenceName(GmetaRefName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Ref doesn't exist means no checkpoint
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Invalid ref
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Invalid tree
	}

	targetPath := gmetaTargetPath(checkpointID)
	targetTree, err := tree.Tree(targetPath)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // Checkpoint doesn't exist
	}

	summary := &CheckpointSummary{
		CheckpointID: checkpointID,
	}

	// Read checkpoint-level fields from entire/
	if entireTree, treeErr := targetTree.Tree("entire"); treeErr == nil {
		summary.Strategy = readGmetaStringValue(s.repo, entireTree, "strategy")
		summary.CLIVersion = readGmetaStringValue(s.repo, entireTree, "cli-version")
		summary.Branch = readGmetaStringValue(s.repo, entireTree, "branch")
		if countStr := readGmetaStringValue(s.repo, entireTree, "checkpoints-count"); countStr != "" {
			if n, parseErr := strconv.Atoi(countStr); parseErr == nil {
				summary.CheckpointsCount = n
			}
		}
		summary.FilesTouched = readGmetaSetValues(s.repo, entireTree, "files-touched")
		summary.CombinedAttribution = readInitialAttributionFromSubtree(s.repo, entireTree, "combined-attribution")
	}

	// Read session IDs from session/ids/__list/
	sessionIDs := s.readSessionIDList(targetTree)

	// Build sessions array and aggregate token usage
	summary.Sessions = make([]SessionFilePaths, len(sessionIDs))
	var aggregatedUsage *agent.TokenUsage
	for i, sid := range sessionIDs {
		summary.Sessions[i] = SessionFilePaths{
			Metadata: "/" + targetPath + "/session/" + sid + "/",
		}
		// Read per-session token usage for aggregation
		if sessionTree, sessionErr := targetTree.Tree("session/" + sid); sessionErr == nil {
			if usage := readTokenUsageFromTree(s.repo, sessionTree); usage != nil {
				if aggregatedUsage == nil {
					aggregatedUsage = &agent.TokenUsage{}
				}
				aggregatedUsage.InputTokens += usage.InputTokens
				aggregatedUsage.OutputTokens += usage.OutputTokens
				aggregatedUsage.CacheReadTokens += usage.CacheReadTokens
				aggregatedUsage.CacheCreationTokens += usage.CacheCreationTokens
				aggregatedUsage.APICallCount += usage.APICallCount
			}
		}
	}
	summary.TokenUsage = aggregatedUsage

	return summary, nil
}

// ReadSessionContent reads a session's transcript, prompt, and metadata from gmeta.
// sessionID is the session identifier (not an index).
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
// Returns ErrNoTranscript if the session exists but has no transcript.
func (s *GmetaStore) ReadSessionContent(_ context.Context, checkpointID id.CheckpointID, sessionID string) (*SessionContent, error) {
	if checkpointID.IsEmpty() {
		return nil, ErrCheckpointNotFound
	}

	refName := plumbing.ReferenceName(GmetaRefName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	rootTree, err := commit.Tree()
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	targetPath := gmetaTargetPath(checkpointID)
	targetTree, err := rootTree.Tree(targetPath)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	sessionPath := "session/" + sessionID
	sessionTree, err := targetTree.Tree(sessionPath)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}

	result := &SessionContent{}

	// Read agent info
	result.Metadata.CheckpointID = checkpointID
	result.Metadata.SessionID = sessionID
	if agentTree, treeErr := sessionTree.Tree("agent"); treeErr == nil {
		agentName := readGmetaStringValue(s.repo, agentTree, "name")
		result.Metadata.Agent = types.AgentType(agentName)
		result.Metadata.Model = readGmetaStringValue(s.repo, agentTree, "model")
	}

	// Read token usage
	result.Metadata.TokenUsage = readTokenUsageFromTree(s.repo, sessionTree)
	result.Metadata.InitialAttribution = readInitialAttributionFromTree(s.repo, sessionTree)

	// Read checkpoint-level fields into metadata
	if entireTree, treeErr := targetTree.Tree("entire"); treeErr == nil {
		result.Metadata.Strategy = readGmetaStringValue(s.repo, entireTree, "strategy")
		result.Metadata.Branch = readGmetaStringValue(s.repo, entireTree, "branch")
		result.Metadata.FilesTouched = readGmetaSetValues(s.repo, entireTree, "files-touched")
		if countStr := readGmetaStringValue(s.repo, entireTree, "checkpoints-count"); countStr != "" {
			if n, parseErr := strconv.Atoi(countStr); parseErr == nil {
				result.Metadata.CheckpointsCount = n
			}
		}
	}

	// Read prompt
	if promptValue := readGmetaStringValue(s.repo, sessionTree, "prompt"); promptValue != "" {
		result.Prompts = promptValue
	}

	// Read transcript from transcript/__list/
	result.Transcript = readGmetaListConcat(s.repo, sessionTree, "transcript")

	if len(result.Transcript) == 0 {
		return nil, ErrNoTranscript
	}

	return result, nil
}

// GetSessionLog retrieves the session transcript and session ID for a checkpoint.
// Reads the latest (last) session from the gmeta tree.
// Returns ErrCheckpointNotFound if the checkpoint doesn't exist.
// Returns ErrNoTranscript if the checkpoint exists but has no transcript.
func (s *GmetaStore) GetSessionLog(ctx context.Context, cpID id.CheckpointID) ([]byte, string, error) {
	summary, err := s.ReadCommitted(ctx, cpID)
	if err != nil {
		return nil, "", err
	}
	if summary == nil {
		return nil, "", ErrCheckpointNotFound
	}

	sessionIDs, err := s.listSessionIDs(cpID)
	if err != nil {
		return nil, "", err
	}
	if len(sessionIDs) == 0 {
		return nil, "", ErrCheckpointNotFound
	}

	latestSessionID := sessionIDs[len(sessionIDs)-1]
	content, err := s.ReadSessionContent(ctx, cpID, latestSessionID)
	if err != nil {
		return nil, "", err
	}
	return content.Transcript, latestSessionID, nil
}

func (s *GmetaStore) listSessionIDs(cpID id.CheckpointID) ([]string, error) {
	refName := plumbing.ReferenceName(GmetaRefName)
	ref, err := s.repo.Reference(refName, true)
	if err != nil {
		return nil, ErrCheckpointNotFound
	}
	commit, err := s.repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, ErrCheckpointNotFound
	}
	rootTree, err := commit.Tree()
	if err != nil {
		return nil, ErrCheckpointNotFound
	}
	targetTree, err := rootTree.Tree(gmetaTargetPath(cpID))
	if err != nil {
		return nil, ErrCheckpointNotFound
	}
	return s.readSessionIDList(targetTree), nil
}

// readSessionIDList reads ordered session IDs from session/ids/__list/.
func (s *GmetaStore) readSessionIDList(targetTree *object.Tree) []string {
	sessionTree, err := targetTree.Tree("session")
	if err != nil {
		return nil
	}
	return readGmetaListValuesWithOrder(s.repo, sessionTree, "ids")
}

// readGmetaStringValue reads a string value from <key>/__value in a tree.
func readGmetaStringValue(_ *git.Repository, tree *object.Tree, key string) string {
	valuePath := key + "/__value"
	file, err := tree.File(valuePath)
	if err != nil {
		return ""
	}
	content, err := file.Contents()
	if err != nil {
		return ""
	}
	return content
}

func readTreeFirstAvailable(tree *object.Tree, paths ...string) (*object.Tree, bool) {
	for _, path := range paths {
		if subtree, err := tree.Tree(path); err == nil {
			return subtree, true
		}
	}
	return nil, false
}

// UpdateCheckpointSummary updates checkpoint-level gmeta fields derived from the
// full set of sessions already written to the checkpoint.
func (s *GmetaStore) UpdateCheckpointSummary(ctx context.Context, checkpointID id.CheckpointID, combinedAttribution *InitialAttribution) error {
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // Propagating context cancellation
	}
	if checkpointID.IsEmpty() {
		return ErrCheckpointNotFound
	}

	refName := plumbing.ReferenceName(GmetaRefName)
	parentHash, rootTreeHash, err := s.getRefState(refName)
	if err != nil {
		return ErrCheckpointNotFound
	}

	targetPath := gmetaTargetPath(checkpointID)
	basePath := targetPath + "/"
	entries, err := s.flattenTargetEntries(rootTreeHash, targetPath)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return ErrCheckpointNotFound
	}

	attrPrefix := basePath + "entire/combined-attribution/"
	for key := range entries {
		if strings.HasPrefix(key, attrPrefix) {
			delete(entries, key)
		}
	}
	if err := writeInitialAttributionEntries(s, attrPrefix, combinedAttribution, entries); err != nil {
		return err
	}

	authorName, authorEmail := GetGitAuthorFromRepo(s.repo)
	return s.commitEntries(ctx, refName, parentHash, rootTreeHash, targetPath, basePath, entries,
		"", authorName, authorEmail)
}

// readGmetaSetValues reads all values from <key>/__set/ in a tree.
func readGmetaSetValues(repo *git.Repository, tree *object.Tree, key string) []string {
	setPath := key + "/__set"
	setTree, err := tree.Tree(setPath)
	if err != nil {
		return nil
	}

	var values []string
	for _, entry := range setTree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		blob, blobErr := repo.BlobObject(entry.Hash)
		if blobErr != nil {
			continue
		}
		reader, readerErr := blob.Reader()
		if readerErr != nil {
			continue
		}
		content := make([]byte, blob.Size)
		if _, readErr := reader.Read(content); readErr == nil {
			values = append(values, string(content))
		}
		_ = reader.Close()
	}
	return values
}

// readGmetaListValuesWithOrder reads string values from <key>/__list/ in insertion order.
// It reconstructs append order from the monotonic timestamp prefix and breaks ties by
// preserving the tree's existing entry order.
func readGmetaListValuesWithOrder(repo *git.Repository, tree *object.Tree, key string) []string {
	values, entryNames := readGmetaListValuesWithEntryNames(repo, tree, key)
	if len(values) < 2 || len(values) != len(entryNames) {
		return values
	}

	type listValue struct {
		value     string
		entryName string
		treeIndex int
		timestamp int64
	}

	ordered := make([]listValue, 0, len(values))
	for i, value := range values {
		entryName := entryNames[i]
		ts := extractGmetaListTimestamp(entryName)
		ordered = append(ordered, listValue{
			value:     value,
			entryName: entryName,
			treeIndex: i,
			timestamp: ts,
		})
	}

	slices.SortStableFunc(ordered, func(a, b listValue) int {
		switch {
		case a.timestamp < b.timestamp:
			return -1
		case a.timestamp > b.timestamp:
			return 1
		case a.treeIndex < b.treeIndex:
			return -1
		case a.treeIndex > b.treeIndex:
			return 1
		default:
			return strings.Compare(a.entryName, b.entryName)
		}
	})

	result := make([]string, 0, len(ordered))
	for _, item := range ordered {
		result = append(result, item.value)
	}
	return result
}

func readGmetaListValuesWithEntryNames(repo *git.Repository, tree *object.Tree, key string) ([]string, []string) {
	listPath := key + "/__list"
	listTree, err := tree.Tree(listPath)
	if err != nil {
		return nil, nil
	}

	var values []string
	var entryNames []string
	for _, entry := range listTree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		blob, blobErr := repo.BlobObject(entry.Hash)
		if blobErr != nil {
			continue
		}
		reader, readerErr := blob.Reader()
		if readerErr != nil {
			continue
		}
		content := make([]byte, blob.Size)
		if _, readErr := reader.Read(content); readErr == nil {
			values = append(values, string(content))
			entryNames = append(entryNames, entry.Name)
		}
		_ = reader.Close()
	}
	return values, entryNames
}

func extractGmetaListTimestamp(entryName string) int64 {
	tsPart, _, ok := strings.Cut(entryName, "-")
	if !ok {
		return 0
	}
	ts, err := strconv.ParseInt(tsPart, 10, 64)
	if err != nil {
		return 0
	}
	return ts
}

type gmetaChange struct {
	op     byte
	target string
	key    string
}

func gmetaCollectChanges(targetPath, basePath string, oldEntries, newEntries map[string]object.TreeEntry) []gmetaChange {
	oldState := gmetaLogicalKeyStates(basePath, oldEntries)
	newState := gmetaLogicalKeyStates(basePath, newEntries)

	allKeys := make(map[string]struct{}, len(oldState)+len(newState))
	for key := range oldState {
		allKeys[key] = struct{}{}
	}
	for key := range newState {
		allKeys[key] = struct{}{}
	}

	keys := make([]string, 0, len(allKeys))
	for key := range allKeys {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	targetLabel := gmetaTargetLabel(targetPath)
	changes := make([]gmetaChange, 0, len(keys))
	for _, key := range keys {
		oldVal, oldOK := oldState[key]
		newVal, newOK := newState[key]
		switch {
		case !oldOK && newOK:
			changes = append(changes, gmetaChange{op: 'A', target: targetLabel, key: key})
		case oldOK && !newOK:
			changes = append(changes, gmetaChange{op: 'D', target: targetLabel, key: key})
		case oldOK && newOK && oldVal.sig != newVal.sig:
			changes = append(changes, gmetaChange{op: 'M', target: targetLabel, key: key})
		}
	}

	return changes
}

func gmetaTargetLabel(targetPath string) string {
	parts := strings.Split(targetPath, "/")
	if len(parts) >= 3 {
		return parts[0] + ":" + parts[2]
	}
	return strings.ReplaceAll(targetPath, "/", ":")
}

func gmetaLogicalKeyStates(basePath string, entries map[string]object.TreeEntry) map[string]struct {
	exists bool
	sig    string
} {
	grouped := make(map[string][]string)
	for path, entry := range entries {
		relPath := strings.TrimPrefix(path, basePath)
		if relPath == path {
			continue
		}
		key := gmetaLogicalKey(relPath)
		if key == "" {
			continue
		}
		grouped[key] = append(grouped[key], relPath+"="+entry.Hash.String())
	}

	states := make(map[string]struct {
		exists bool
		sig    string
	}, len(grouped))
	for key, values := range grouped {
		slices.Sort(values)
		states[key] = struct {
			exists bool
			sig    string
		}{
			exists: true,
			sig:    strings.Join(values, "\n"),
		}
	}

	return states
}

func gmetaLogicalKey(relPath string) string {
	segments := strings.Split(relPath, "/")
	var keySegments []string
	for _, segment := range segments {
		if strings.HasPrefix(segment, "__") {
			break
		}
		keySegments = append(keySegments, segment)
	}
	if len(keySegments) == 0 {
		return ""
	}
	return strings.Join(keySegments, ":")
}

func gmetaCommitMessage(changes []gmetaChange) string {
	changeCount := len(changes)
	if changeCount > 1000 {
		return fmt.Sprintf(
			"gmeta: serialize (%d changes)\n\nchanges-omitted: true\ncount: %d",
			changeCount,
			changeCount,
		)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "gmeta: serialize (%d changes)\n", changeCount)
	for _, change := range changes {
		fmt.Fprintf(&b, "\n%c\t%s\t%s", change.op, change.target, change.key)
	}
	return b.String()
}

// readGmetaListConcat reads all blobs from <key>/__list/ and concatenates them.
// Used for transcript chunks that should be reassembled into a single byte slice.
func readGmetaListConcat(repo *git.Repository, tree *object.Tree, key string) []byte {
	listPath := key + "/__list"
	listTree, err := tree.Tree(listPath)
	if err != nil {
		return nil
	}

	var result []byte
	for _, entry := range listTree.Entries {
		if !entry.Mode.IsFile() {
			continue
		}
		blob, blobErr := repo.BlobObject(entry.Hash)
		if blobErr != nil {
			continue
		}
		reader, readerErr := blob.Reader()
		if readerErr != nil {
			continue
		}
		content := make([]byte, blob.Size)
		if _, readErr := reader.Read(content); readErr == nil {
			result = append(result, content...)
		}
		_ = reader.Close()
	}
	return result
}
