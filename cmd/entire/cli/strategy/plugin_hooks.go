package strategy

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/plugins"
)

// firePluginPostCommit dispatches the post_commit observer hook after a commit
// is processed by the strategy. Best-effort: a no-op when no plugin is enabled
// and never propagates plugin failures into the git hook.
func firePluginPostCommit(ctx context.Context, commitSHA, checkpointID string, hasCheckpoint bool) {
	payload := map[string]any{
		"commit":         commitSHA,
		"has_checkpoint": hasCheckpoint,
	}
	if checkpointID != "" {
		payload["checkpoint_id"] = checkpointID
	}
	plugins.FireHook(ctx, plugins.HookPostCommit, payload)
}

// firePluginPrePush dispatches the pre_push hook (observer side effects for all
// enabled plugins, plus a veto for plugins granted the pre_push capability). It
// returns a non-nil error when a capable plugin vetoes the push; the caller
// propagates that error so the git pre-push hook exits non-zero and the push is
// aborted. Runs before the built-in OPF rewrite and checkpoint-ref push so a
// veto short-circuits that work.
func firePluginPrePush(ctx context.Context, remote, pushTarget string) error {
	payload := map[string]any{"remote": remote}
	if pushTarget != "" {
		payload["push_target"] = pushTarget
	}
	//nolint:wrapcheck // the veto error is already user-facing ("push vetoed by plugin ..."); the pre-push hook adds its own "pre-push:" prefix, so wrapping here would only duplicate context
	return plugins.FirePrePush(ctx, payload)
}

// appendPluginCommitTrailers appends trailer lines contributed by
// prepare_commit_msg plugins (those granted the commit_msg capability) to the
// commit message file. It runs after the strategy's own trailer handling so the
// built-in Entire-Checkpoint trailer is never displaced. Best-effort: any read
// or write failure is logged and skipped so it never blocks the commit.
func appendPluginCommitTrailers(ctx context.Context, commitMsgFile, source string) {
	trailers := plugins.FireCommitMsg(ctx, map[string]any{"source": source})
	if len(trailers) == 0 {
		return
	}
	content, err := os.ReadFile(commitMsgFile) //nolint:gosec // commitMsgFile is provided by the git prepare-commit-msg hook
	if err != nil {
		logging.Warn(logging.WithComponent(ctx, "plugins"), "prepare_commit_msg: read commit message failed",
			slog.String("error", err.Error()))
		return
	}
	updated := insertCommitTrailers(string(content), trailers)
	//nolint:gosec // commitMsgFile is the path passed by the git prepare-commit-msg hook, not user-tainted input
	if err := os.WriteFile(commitMsgFile, []byte(updated), 0o600); err != nil {
		logging.Warn(logging.WithComponent(ctx, "plugins"), "prepare_commit_msg: write commit message failed",
			slog.String("error", err.Error()))
	}
}

// insertCommitTrailers inserts trailer lines before the trailing git comment
// block (lines starting with '#'), or at the end when there is no comment
// block. Each trailer may itself contain newlines; blank/whitespace-only
// trailers are dropped.
func insertCommitTrailers(msg string, trailers []string) string {
	var lines []string
	for _, t := range trailers {
		for _, sub := range strings.Split(strings.TrimRight(t, "\n"), "\n") {
			if strings.TrimSpace(sub) == "" {
				continue
			}
			lines = append(lines, sub)
		}
	}
	if len(lines) == 0 {
		return msg
	}

	existing := strings.Split(msg, "\n")
	insertAt := len(existing)
	for i, ln := range existing {
		if strings.HasPrefix(strings.TrimSpace(ln), "#") {
			insertAt = i
			break
		}
	}

	out := make([]string, 0, len(existing)+len(lines))
	out = append(out, existing[:insertAt]...)
	out = append(out, lines...)
	out = append(out, existing[insertAt:]...)
	return strings.Join(out, "\n")
}
