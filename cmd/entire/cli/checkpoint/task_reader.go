package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/validation"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Layout of the subagent task records at the checkpoint root, shared by the
// writer (writeTaskRecordEntry) and the reader below:
//
//	tasks/<tool_use_id>/task.json             TaskRecord
//	tasks/<tool_use_id>/agent-<agent_id>.jsonl  subagent transcript (optional)
const (
	taskRecordsDirName   = "tasks"
	taskRecordFileName   = "task.json"
	taskTranscriptStem   = "agent-"
	taskTranscriptSuffix = ".jsonl"
)

// taskTranscriptFileName names a task record's transcript blob.
func taskTranscriptFileName(agentID string) string {
	return taskTranscriptStem + agentID + taskTranscriptSuffix
}

// ListTasks implements TaskReader for the git-branch store.
func (s *GitStore) ListTasks(ctx context.Context, checkpointID id.CheckpointID) ([]TaskEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	checkpointTree, err := s.taskCheckpointTree(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	return listTasksFromCheckpointTree(checkpointTree)
}

// taskCheckpointTree resolves the checkpoint tree for the task reads. Only a
// genuinely absent checkpoint or metadata ref becomes ErrCheckpointNotFound;
// a ref that resolves to an unreadable commit or tree is a storage failure
// and is surfaced as one, as gitRefsStore.checkpointTree does.
func (s *GitStore) taskCheckpointTree(ctx context.Context, checkpointID id.CheckpointID) (*FetchingTree, error) {
	checkpointTree, err := s.getCheckpointFetchingTree(ctx, checkpointID)
	if err == nil {
		return checkpointTree, nil
	}
	if errors.Is(err, ErrCheckpointNotFound) || errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, ErrCheckpointNotFound
	}
	return nil, fmt.Errorf("read checkpoint %s: %w", checkpointID, err)
}

// ReadTaskTranscript implements TaskReader for the git-branch store.
func (s *GitStore) ReadTaskTranscript(ctx context.Context, checkpointID id.CheckpointID, toolUseID string) ([]byte, error) {
	if err := validateTaskSelector(toolUseID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err //nolint:wrapcheck // Propagating context cancellation
	}
	checkpointTree, err := s.taskCheckpointTree(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	return readTaskTranscriptFromCheckpointTree(checkpointTree, toolUseID)
}

// ListTasks implements TaskReader for the git-refs store.
func (s *gitRefsStore) ListTasks(ctx context.Context, checkpointID id.CheckpointID) ([]TaskEntry, error) {
	checkpointTree, err := s.checkpointTree(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	return listTasksFromCheckpointTree(checkpointTree)
}

// ReadTaskTranscript implements TaskReader for the git-refs store.
func (s *gitRefsStore) ReadTaskTranscript(ctx context.Context, checkpointID id.CheckpointID, toolUseID string) ([]byte, error) {
	if err := validateTaskSelector(toolUseID); err != nil {
		return nil, err
	}
	checkpointTree, err := s.checkpointTree(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	return readTaskTranscriptFromCheckpointTree(checkpointTree, toolUseID)
}

// validateTaskSelector rejects an empty or path-unsafe tool_use_id before it is
// used as a tree path. ValidateToolUseID alone accepts "" (the field is
// optional on payloads), which here would address the tasks/ directory itself.
func validateTaskSelector(toolUseID string) error {
	if toolUseID == "" {
		return errors.New("tool use ID is required")
	}
	if err := validation.ValidateToolUseID(toolUseID); err != nil {
		return fmt.Errorf("invalid task selector: %w", err)
	}
	return nil
}

// tasksTree returns the checkpoint's tasks/ subtree, or (nil, nil) when the
// checkpoint has none — every checkpoint written before task records existed,
// and every session without subagent work.
//
// Absence is decided from the tree entries, not from the subtree read's error:
// go-git reports a listed subtree whose object is missing as
// ErrDirectoryNotFound too, and that is a broken record set, not an empty one.
func tasksTree(checkpointTree *FetchingTree) (*FetchingTree, error) {
	if !hasRawEntry(checkpointTree, taskRecordsDirName) {
		return nil, nil //nolint:nilnil // no task records is not an error
	}
	tree, err := checkpointTree.Tree(taskRecordsDirName)
	if err != nil {
		return nil, fmt.Errorf("read %s/: %w", taskRecordsDirName, err)
	}
	return tree, nil
}

// listTasksFromCheckpointTree reads every tasks/<tool_use_id>/ record from a
// checkpoint tree. Shared by both git backends, which differ only in how they
// navigate to the checkpoint tree. task.json is pushed checkpoint data that
// anyone with push access can author, so each record's identifiers are
// validated before they name a tree path.
func listTasksFromCheckpointTree(checkpointTree *FetchingTree) ([]TaskEntry, error) {
	tasks, err := tasksTree(checkpointTree)
	if err != nil {
		return nil, err
	}
	if tasks == nil {
		return []TaskEntry{}, nil
	}

	entries := make([]TaskEntry, 0, len(tasks.RawEntries()))
	for _, raw := range tasks.RawEntries() {
		if raw.Mode != filemode.Dir {
			continue
		}
		entry := TaskEntry{ToolUseID: raw.Name}
		if err := validateTaskSelector(raw.Name); err != nil {
			entry.Err = err
			entries = append(entries, entry)
			continue
		}
		taskDir, err := tasks.Tree(raw.Name)
		if err != nil {
			entry.Err = err
			entries = append(entries, entry)
			continue
		}
		record, err := readTaskRecord(taskDir)
		if err != nil {
			entry.Err = err
			entries = append(entries, entry)
			continue
		}
		entry.Record = *record
		entry.TranscriptStored = hasRawEntry(taskDir, taskTranscriptFileName(record.AgentID))
		entries = append(entries, entry)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].Record.StartedAt, entries[j].Record.StartedAt
		if !a.Equal(b) {
			return a.Before(b)
		}
		return entries[i].ToolUseID < entries[j].ToolUseID
	})
	return entries, nil
}

// readTaskTranscriptFromCheckpointTree returns the stored transcript of the
// task record named toolUseID (already validated by the caller).
func readTaskTranscriptFromCheckpointTree(checkpointTree *FetchingTree, toolUseID string) ([]byte, error) {
	tasks, err := tasksTree(checkpointTree)
	if err != nil {
		return nil, err
	}
	if tasks == nil || !hasRawEntry(tasks, toolUseID) {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, toolUseID)
	}
	// The entry is listed, so a failure here is an unreadable record, not an
	// absent one; ErrTaskNotFound would stop the routing store from trying a
	// fallback backend that can read it.
	taskDir, err := tasks.Tree(toolUseID)
	if err != nil {
		return nil, fmt.Errorf("task %s: read tree: %w", toolUseID, err)
	}
	record, err := readTaskRecord(taskDir)
	if err != nil {
		return nil, fmt.Errorf("task %s: %w", toolUseID, err)
	}
	name := taskTranscriptFileName(record.AgentID)
	if !hasRawEntry(taskDir, name) {
		reason := record.TranscriptUnavailableReason
		if reason == "" {
			reason = "no transcript stored"
		}
		return nil, fmt.Errorf("task %s: %w: %s", toolUseID, ErrNoTranscript, reason)
	}
	file, err := taskDir.File(name)
	if err != nil {
		return nil, fmt.Errorf("task %s: read %s: %w", toolUseID, name, err)
	}
	content, err := readTaskBlob(file, taskBlobReadLimit)
	if err != nil {
		return nil, fmt.Errorf("task %s: read %s: %w", toolUseID, name, err)
	}
	return content, nil
}

// taskBlobReadLimit bounds every task-record blob read. It reuses the cap the
// writer enforces: prepareSubagentTranscript refuses a subagent transcript
// over agent.MaxChunkSize, so any honestly written task blob fits, and a
// larger one is pushed data this reader must not materialize.
const taskBlobReadLimit = int64(agent.MaxChunkSize)

// readTaskBlob reads file's content, refusing a blob larger than limit. The
// declared size is checked first so an oversized blob is never read; the
// LimitReader backs that up against a size header that understates.
func readTaskBlob(file *object.File, limit int64) ([]byte, error) {
	if file.Size > limit {
		return nil, fmt.Errorf("blob of %d bytes exceeds the %d-byte limit", file.Size, limit)
	}
	reader, err := file.Reader()
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	defer reader.Close()
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read blob: %w", err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("blob exceeds the %d-byte limit", limit)
	}
	return content, nil
}

// readTaskRecord parses and validates one task directory's task.json.
func readTaskRecord(taskDir *FetchingTree) (*TaskRecord, error) {
	file, err := taskDir.File(taskRecordFileName)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskRecordFileName, err)
	}
	content, err := readTaskBlob(file, taskBlobReadLimit)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", taskRecordFileName, err)
	}
	var record TaskRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return nil, fmt.Errorf("parse %s: %w", taskRecordFileName, err)
	}
	if err := validation.ValidateAgentID(record.AgentID); err != nil {
		return nil, fmt.Errorf("%s: %w", taskRecordFileName, err)
	}
	return &record, nil
}

// hasRawEntry reports whether tree directly contains name, without reading any
// blob (so a missing transcript blob in a partial clone is not fetched just to
// learn that it exists).
func hasRawEntry(tree *FetchingTree, name string) bool {
	for _, e := range tree.RawEntries() {
		if e.Name == name {
			return true
		}
	}
	return false
}
