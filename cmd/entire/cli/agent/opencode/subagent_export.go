package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// exportSubagent exports a child session and returns the transcript for one
// task call. A child resumed through the task tool's `task_id` backs several
// calls, and its export holds every one of them; the framework reads a task
// record's files and tokens from its whole declared transcript, so each call
// must declare only the messages it produced: from the call's prompt up to the
// next call's prompt, and no later than the call's completion (see
// scopeExportToCall). The end matters whenever the child has already served a
// later call by export time, as in a re-export after the fact (condensation,
// the SessionEnd sweep). The slice is written to
// `.entire/tmp/<child>.<toolUseID>.json` — one file per call, so a later call
// on the same child cannot overwrite an earlier record's transcript before it
// is condensed. Without a start, the call is the last one prompted by until
// (or by now).
func (a *OpenCodeAgent) exportSubagent(ctx context.Context, childID, toolUseID string, since, until time.Time) (string, error) {
	if _, err := a.fetchAndCacheExport(ctx, childID); err != nil {
		return "", err
	}
	if toolUseID == "" {
		return "", errors.New("opencode subagent export needs a tool use ID to scope a call")
	}
	if err := validation.ValidateToolUseID(toolUseID); err != nil {
		return "", fmt.Errorf("invalid tool use ID for subagent export: %w", err)
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}
	root, err := entiredir.OpenAt(repoRoot)
	if err != nil {
		return "", fmt.Errorf("open %s for subagent export: %w", paths.EntireDir, err)
	}
	data, err := entiredir.ReadFile(root, entireTmpName+"/"+childID+".json")
	if err != nil {
		return "", fmt.Errorf("read subagent export: %w", err)
	}
	scoped, kept, err := scopeExportToCall(data, since, until)
	if err != nil {
		return "", err
	}
	if kept == 0 {
		// The plugin's started_at and OpenCode's message times come from the
		// same clock, so no prompt at or after the call start means the call
		// never prompted the child: a joined call whose queued run OpenCode
		// dropped when the job failed or was cancelled. It produced nothing;
		// declaring the full export would hand it every other call's work.
		logging.Warn(logging.WithComponent(ctx, "lifecycle"),
			"opencode: task call never prompted its child; declaring an empty transcript",
			slog.String("subagent_id", childID),
			slog.String("tool_use_id", toolUseID))
	}

	name := entireTmpName + "/" + childID + "." + toolUseID + ".json"
	if err := jsonutil.WriteFileAtomicIn(root, name, scoped, 0o600); err != nil {
		return "", fmt.Errorf("write scoped subagent export: %w", err)
	}
	return filepath.Join(repoRoot, paths.EntireDir, filepath.FromSlash(name)), nil
}

// scopeExportToCall keeps the export's messages for one task call and returns
// the rewritten export with how many it kept. A call's messages begin at its
// prompt, the first prompt created at or after since, and run up to the next
// prompt, which belongs to the next call on the same child: an export taken
// after the child served a later call holds that call too. With since unset
// the slice begins at the last prompt created by until: a stop captures right
// after its call ended, and a re-export is bounded by the call's completion.
// When until is set, messages created after it are dropped too. Messages are carried as raw JSON, so fields the typed export
// structs do not model survive.
func scopeExportToCall(data []byte, since, until time.Time) ([]byte, int, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, 0, fmt.Errorf("parse subagent export: %w", err)
	}
	var messages []json.RawMessage
	if raw, ok := doc["messages"]; ok {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, 0, fmt.Errorf("parse subagent export messages: %w", err)
		}
	}
	untilMs := int64(math.MaxInt64)
	if !until.IsZero() {
		untilMs = until.UnixMilli()
	}
	created := make([]int64, len(messages))
	prompt := make([]bool, len(messages))
	sinceMs := since.UnixMilli()
	if since.IsZero() {
		sinceMs = math.MaxInt64
	}
	for i, m := range messages {
		var head struct {
			Info  MessageInfo `json:"info"`
			Parts []Part      `json:"parts"`
		}
		if err := json.Unmarshal(m, &head); err != nil {
			return nil, 0, fmt.Errorf("parse subagent export message: %w", err)
		}
		created[i] = head.Info.Time.Created
		prompt[i] = isCallPrompt(head.Info, head.Parts)
		if since.IsZero() && prompt[i] && created[i] <= untilMs {
			sinceMs = created[i]
		}
	}
	kept := make([]json.RawMessage, 0, len(messages))
	inCall := false
	for i, m := range messages {
		if prompt[i] {
			if inCall {
				break
			}
			inCall = created[i] >= sinceMs
		}
		if inCall && created[i] <= untilMs {
			kept = append(kept, m)
		}
	}
	rewritten, err := json.Marshal(kept)
	if err != nil {
		return nil, 0, fmt.Errorf("encode scoped messages: %w", err)
	}
	doc["messages"] = rewritten
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, 0, fmt.Errorf("encode scoped subagent export: %w", err)
	}
	return out, len(kept), nil
}

// isCallPrompt reports whether a child message is a task call's prompt: a
// user message that is neither OpenCode's own synthetic text (a background
// result) nor a compaction marker.
func isCallPrompt(info MessageInfo, parts []Part) bool {
	if info.Role != roleUser || OnlySyntheticText(parts) {
		return false
	}
	for _, part := range parts {
		if part.Type == "compaction" {
			return false
		}
	}
	return true
}

// LiveSubagentModifiedFiles exports a running child to a private staging file
// and returns the files its task call has modified so far. Unlike
// exportSubagent it writes neither the shared export cache nor the call's
// declared file: the call may stop while this export runs, and its stop writes
// the complete transcript to those names.
func (a *OpenCodeAgent) LiveSubagentModifiedFiles(ctx context.Context, childID, _ string, since time.Time) ([]string, error) {
	if err := validation.ValidateSessionID(childID); err != nil {
		return nil, fmt.Errorf("invalid session ID for live subagent export: %w", err)
	}
	// Best-effort: an unresolved repository is no evidence, never the CWD.
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree for live subagent export: %w", err)
	}
	root, err := entiredir.OpenAt(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open %s for live subagent export: %w", paths.EntireDir, err)
	}
	var data []byte
	if name, ok := mockExportName(childID); ok {
		data, err = entiredir.ReadFile(root, name)
	} else {
		data, err = exportToPrivateFile(ctx, root, childID)
	}
	if err != nil {
		return nil, err
	}
	scoped, _, err := scopeExportToCall(data, since, time.Time{})
	if err != nil {
		return nil, err
	}
	session, err := ParseExportSession(scoped)
	if err != nil || session == nil {
		return nil, err
	}
	return modifiedFilesFromMessages(completedToolParts(session.Messages), 0), nil
}

// completedToolParts returns msgs with only finished tool parts. A live
// export carries pending, running and failed edits too, and none of those is
// evidence the subagent changed a file.
func completedToolParts(msgs []ExportMessage) []ExportMessage {
	kept := make([]ExportMessage, len(msgs))
	for i, msg := range msgs {
		kept[i] = ExportMessage{Info: msg.Info}
		for _, part := range msg.Parts {
			if part.Type == "tool" && (part.State == nil || part.State.Status != "completed") {
				continue
			}
			kept[i].Parts = append(kept[i].Parts, part)
		}
	}
	return kept
}

// exportToPrivateFile runs `opencode export` for sessionID into a staging file
// of its own and returns the bytes, removing the file.
func exportToPrivateFile(ctx context.Context, root *os.Root, sessionID string) ([]byte, error) {
	if err := osroot.MkdirAllNoSymlink(root, entireTmpName, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	staged, err := stageExportPath(root, entireTmpName, sessionID)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = root.Remove(staged) //nolint:errcheck // best-effort cleanup of a private staging file
	}()
	if err := runOpenCodeExportToFileFn(ctx, root, sessionID, staged); err != nil {
		return nil, err
	}
	data, err := entiredir.ReadFile(root, staged)
	if err != nil {
		return nil, fmt.Errorf("failed to read export file: %w", err)
	}
	return data, nil
}
