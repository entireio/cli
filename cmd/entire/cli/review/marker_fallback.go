// Package review — see env.go for package-level rationale.
//
// marker_fallback.go provides the PendingReviewMarker type and its
// write/read/clear helpers, plus RunMarkerFallback which handles review for
// non-launchable agents (cursor, opencode, factoryai-droid, copilot-cli) —
// agents that don't (yet) implement AgentReviewer.
//
// For launchable agents (claude-code, codex, gemini-cli) the new
// architecture uses env-var handshake (env.go) + AgentReviewer.Start, and
// the lifecycle hook reads ENTIRE_REVIEW_* env vars off the spawned
// process — there is no marker-file adoption code path.
//
// For non-launchable agents the marker is purely a record of what the user
// was asked to do: RunMarkerFallback writes it before printing manual-start
// guidance, and `entire attach --review <session-id>` (and its discovery
// shortcut `entire review attach`) reads the marker to tag a manual
// session after the fact. ReadPendingReviewMarker / ClearPendingReviewMarker
// are exported for that attach flow; nothing else reads the marker.
package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

const pendingReviewMarkerFilename = "review-pending.json"

// PendingReviewMarker is written by `entire review` before instructing the
// user to open a non-launchable agent. The marker records which agent and
// skills should run so that `entire review attach` can tag the resulting
// session after the fact.
//
// WorktreePath scopes the marker to the worktree `entire review` was invoked
// from: multiple worktrees in one repo share .git/entire-sessions/, so without
// this field any session in any worktree could race to claim the marker. A
// blank WorktreePath (pre-fix markers) falls back to the legacy unscoped
// behavior — any session can adopt.
type PendingReviewMarker struct {
	AgentName string   `json:"agent_name"`
	Skills    []string `json:"skills"`
	// Prompt is the composed review prompt the agent will receive.
	// Stored on the marker (rather than recomputed on adoption) so session
	// metadata records exactly what the agent was asked to do.
	Prompt       string    `json:"prompt,omitempty"`
	StartingSHA  string    `json:"starting_sha"`
	StartedAt    time.Time `json:"started_at"`
	WorktreePath string    `json:"worktree_path,omitempty"`
}

func pendingMarkerPath(ctx context.Context) (string, error) {
	commonDir, err := session.GetGitCommonDir(ctx)
	if err != nil {
		return "", fmt.Errorf("locate git common dir: %w", err)
	}
	return filepath.Join(commonDir, session.SessionStateDirName, pendingReviewMarkerFilename), nil
}

// WritePendingReviewMarker persists the marker. Overwrites any existing marker.
func WritePendingReviewMarker(ctx context.Context, m PendingReviewMarker) error {
	path, err := pendingMarkerPath(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marker: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
}

// ReadPendingReviewMarker returns the marker if one exists.
// ok=false with err=nil indicates "no pending review."
func ReadPendingReviewMarker(ctx context.Context) (PendingReviewMarker, bool, error) {
	path, err := pendingMarkerPath(ctx)
	if err != nil {
		return PendingReviewMarker{}, false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path derived from git dir
	if errors.Is(err, os.ErrNotExist) {
		return PendingReviewMarker{}, false, nil
	}
	if err != nil {
		return PendingReviewMarker{}, false, fmt.Errorf("read marker: %w", err)
	}
	var m PendingReviewMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return PendingReviewMarker{}, false, fmt.Errorf("parse marker: %w", err)
	}
	return m, true, nil
}

// ClearPendingReviewMarker removes the marker. Missing file is not an error.
func ClearPendingReviewMarker(ctx context.Context) error {
	path, err := pendingMarkerPath(ctx)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove marker: %w", err)
	}
	return nil
}

// RunMarkerFallback handles review for non-launchable agents (cursor,
// opencode, factoryai-droid, copilot-cli) by writing the pending-review
// marker file and printing manual-start guidance. The user is told to open
// the agent themselves and run the configured skills.
//
// The marker is NOT auto-adopted by anything — the lifecycle hook reads
// ENTIRE_REVIEW_* env vars on the spawned process, not the marker file.
// For non-launchable agents the user starts the agent manually, so no env
// inheritance happens. The marker exists purely so that `entire attach
// --review <session-id>` (and its `entire review attach` shortcut) has a
// record of what the user was asked to review when tagging the session
// after the fact.
//
// agentName must be the agent's registry key (e.g. "cursor").
// cfg carries skills and the starting SHA.
// worktreePath scopes the marker so sessions in other worktrees don't claim it.
// out is the destination for user-facing guidance.
func RunMarkerFallback(ctx context.Context, agentName string, cfg reviewtypes.RunConfig, worktreePath string, out io.Writer) error {
	prompt := ComposeReviewPrompt(cfg)
	if err := WritePendingReviewMarker(ctx, PendingReviewMarker{
		AgentName:    agentName,
		Skills:       cfg.Skills,
		Prompt:       prompt,
		StartingSHA:  cfg.StartingSHA,
		StartedAt:    time.Now().UTC(),
		WorktreePath: worktreePath,
	}); err != nil {
		return fmt.Errorf("write pending marker: %w", err)
	}

	fmt.Fprintf(out, "%s does not support subprocess launch yet. Marker written.\n", agentName)
	if len(cfg.Skills) > 0 {
		fmt.Fprintf(out, "Start %s manually and run these skills:\n", agentName)
		for i, skill := range cfg.Skills {
			fmt.Fprintf(out, "  %d. %s\n", i+1, skill)
		}
		fmt.Fprintln(out)
	}
	if prompt != "" {
		fmt.Fprintf(out, "Use this prompt:\n\n%s\n", prompt)
	}
	return nil
}
