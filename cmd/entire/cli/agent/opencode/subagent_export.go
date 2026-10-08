package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
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
// is condensed. With neither bound known, the full export is returned.
func (a *OpenCodeAgent) exportSubagent(ctx context.Context, childID, toolUseID string, since, until time.Time) (string, error) {
	full, err := a.fetchAndCacheExport(ctx, childID)
	if err != nil || (since.IsZero() && until.IsZero()) {
		return full, err
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
// the slice begins at the first prompt. When until is set, messages created after it are
// dropped too. Messages are carried as raw JSON, so fields the typed export
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
	sinceMs := since.UnixMilli()
	if since.IsZero() {
		sinceMs = math.MinInt64
	}
	untilMs := int64(math.MaxInt64)
	if !until.IsZero() {
		untilMs = until.UnixMilli()
	}
	kept := make([]json.RawMessage, 0, len(messages))
	inCall := false
	for _, m := range messages {
		var head struct {
			Info  MessageInfo `json:"info"`
			Parts []Part      `json:"parts"`
		}
		if err := json.Unmarshal(m, &head); err != nil {
			return nil, 0, fmt.Errorf("parse subagent export message: %w", err)
		}
		created := head.Info.Time.Created
		if isCallPrompt(head.Info, head.Parts) {
			if inCall {
				break
			}
			inCall = created >= sinceMs
		}
		if inCall && created <= untilMs {
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
