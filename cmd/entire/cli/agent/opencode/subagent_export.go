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
// must declare only the messages it produced: those created from the call's
// start through its completion. The end matters for a re-export after the
// fact (condensation, the SessionEnd sweep), when the child may already have
// served a later call. The slice is written to
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
		// The child always gets the call's prompt as a message, so an empty
		// slice means the timestamps disagree with the call start. Declaring
		// the full export over-attributes; declaring nothing loses the work.
		logging.Warn(logging.WithComponent(ctx, "lifecycle"),
			"opencode: no child messages after the task call started; declaring the full export",
			slog.String("subagent_id", childID),
			slog.String("tool_use_id", toolUseID))
		return full, nil
	}

	name := entireTmpName + "/" + childID + "." + toolUseID + ".json"
	if err := jsonutil.WriteFileAtomicIn(root, name, scoped, 0o600); err != nil {
		return "", fmt.Errorf("write scoped subagent export: %w", err)
	}
	return filepath.Join(repoRoot, paths.EntireDir, filepath.FromSlash(name)), nil
}

// scopeExportToCall keeps the export's messages created at or after since and,
// when until is set, at or before it, and returns the rewritten export with
// how many it kept. Messages are carried as raw JSON, so fields the typed
// export structs do not model survive.
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
	untilMs := int64(math.MaxInt64)
	if !until.IsZero() {
		untilMs = until.UnixMilli()
	}
	kept := make([]json.RawMessage, 0, len(messages))
	for _, m := range messages {
		var head struct {
			Info struct {
				Time Time `json:"time"`
			} `json:"info"`
		}
		if err := json.Unmarshal(m, &head); err != nil {
			return nil, 0, fmt.Errorf("parse subagent export message: %w", err)
		}
		if created := head.Info.Time.Created; created >= sinceMs && created <= untilMs {
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
