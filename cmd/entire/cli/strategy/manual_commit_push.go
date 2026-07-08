package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"
)

// errOPFAbortedByUser is returned when the user chose Abort (or pressed
// Ctrl-C) at the OPF prompt. PrePush returns it verbatim; the hook
// command propagates the non-zero exit code so git push aborts.
var errOPFAbortedByUser = errors.New("OPF prompt aborted by user; push cancelled")

var opfPrePushProgressWriter io.Writer = os.Stderr

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes each ref in refs.Push alongside the user's push.
//
// If a checkpoint_remote is configured in settings, checkpoint branches/refs
// are pushed to the derived URL instead of the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	// Load settings once for remote resolution and push_sessions check.
	// Spanned because checkpoint-remote resolution can perform a one-time
	// network fetch of the metadata branch (fetchMetadataBranchIfMissing),
	// which is otherwise invisible in the pre-push trace.
	resolveCtx, resolveSpan := perf.Start(ctx, "resolve_push_settings")
	ps := resolvePushSettings(resolveCtx, remote)
	resolveSpan.End()

	if ps.pushDisabled {
		return nil
	}

	refs := checkpoint.ResolveCommittedRefs(ctx)

	printPushSummary(ctx, remote, ps.pushTarget())

	// OPF pre-push rewrite: if OPF is configured, resolve the user's
	// decision (env > settings > prompt > non-TTY auto-run), then
	// re-redact unpushed v1 commits with the 8-layer pipeline before
	// pushing. Skipped entirely when OPF is off, so the common-case
	// fast path is unchanged.
	if redact.OPFEnabled() {
		cfg, _ := settings.Load(ctx) //nolint:errcheck // Load already failed at hook init; fall back to nil
		var opfCfg *settings.OPFSettings
		if cfg != nil && cfg.Redaction != nil {
			opfCfg = cfg.Redaction.OpenAIPrivacyFilter
		}
		decision, decisionErr := resolveOPFDecisionForPrePush(ctx, opfCfg, opfPrePushProgressWriter)
		if decisionErr != nil {
			logging.Warn(ctx, "OPF pre-push decision failed; aborting push",
				slog.String("error", decisionErr.Error()),
			)
			return decisionErr
		}
		switch decision {
		case OPFAbort:
			return errOPFAbortedByUser
		case OPFSkip:
			// User opted out for this push (or settings/env say
			// "never"). Push 7-layer content as-is.
			logging.Info(ctx, "OPF skipped for this push (user choice or settings)")
		case OPFRun:
			_, opfSpan := perf.Start(ctx, "opf_pre_push_rewrite")
			repo, repoErr := OpenRepository(ctx)
			if repoErr != nil {
				opfSpan.RecordError(repoErr)
				opfSpan.End()
				logging.Warn(ctx, "OPF pre-push: failed to open repo; aborting push",
					slog.String("error", repoErr.Error()),
				)
				return repoErr
			}
			defer repo.Close()
			if _, rewriteErr := RewriteUnpushedV1WithOPF(ctx, repo, ps.pushTarget()); rewriteErr != nil {
				opfSpan.RecordError(rewriteErr)
				opfSpan.End()
				logging.Warn(ctx, "OPF pre-push rewrite failed; aborting push",
					slog.String("error", rewriteErr.Error()),
				)
				return rewriteErr
			}
			opfSpan.End()
		}
	}

	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	pushCtx, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoint_refs")
	for _, ref := range refs.Push {
		if err := pushRefIfNeeded(pushCtx, ps.pushTarget(), ref); err != nil {
			pushCheckpointsSpan.RecordError(err)
			pushCheckpointsSpan.End()
			return err
		}
	}
	pushCheckpointsSpan.End()

	// Post-push cleanup: only when all configured checkpoint refs were pushed
	// successfully, so we know condensed checkpoint data reached the remote.
	// Failures here are non-fatal — shadow branches just accumulate until
	// `entire clean` or the next successful push.
	if deleted, cleanupErr := CleanupPushedShadowBranches(ctx); cleanupErr != nil {
		logging.Warn(ctx, "post-push shadow branch cleanup failed",
			slog.String("error", cleanupErr.Error()),
		)
	} else if deleted > 0 {
		logging.Info(ctx, "cleaned up vestigial shadow branches",
			slog.Int("count", deleted),
		)
	}

	return nil
}

// printPushSummary writes a session-level summary of pending checkpoint commits.
// Silent on any failure — never blocks the push.
func printPushSummary(ctx context.Context, remoteName, target string) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return
	}

	branchName := paths.MetadataBranchName
	logFormat := "%H|%s|%(trailers:key=" + trailers.SessionTrailerKey + ",valueonly,separator=%x20)|%aI"

	var logOutput string
	isNewBranch := false

	if !checkpointremote.IsURL(target) {
		rangeSpec := "refs/remotes/" + remoteName + "/" + branchName + "..refs/heads/" + branchName
		firstOut, firstErr := runPushSummaryGitLog(ctx, repoRoot, logFormat, rangeSpec)
		if firstErr == nil && strings.TrimSpace(firstOut) != "" {
			logOutput = firstOut
		} else {
			fallbackOut, fallbackErr := runPushSummaryGitLog(ctx, repoRoot, logFormat, "refs/heads/"+branchName)
			if fallbackErr == nil && strings.TrimSpace(fallbackOut) != "" {
				logOutput = fallbackOut
				isNewBranch = true
			}
		}
	} else {
		fallbackOut, fallbackErr := runPushSummaryGitLog(ctx, repoRoot, logFormat, "refs/heads/"+branchName)
		if fallbackErr == nil {
			logOutput = fallbackOut
			isNewBranch = strings.TrimSpace(logOutput) != ""
		}
	}
	if strings.TrimSpace(logOutput) == "" {
		return
	}

	summaries := parsePushSummaryFromLog(logOutput)
	if len(summaries) == 0 {
		return
	}

	totalCommits := len(strings.Split(strings.TrimSpace(logOutput), "\n"))
	lines := formatSessionTree(summaries, formatSessionTreeOpts{
		TotalCommits: totalCommits,
		IsNewBranch:  isNewBranch,
	})
	w := pushProgressOutput()
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
}

func runPushSummaryGitLog(ctx context.Context, repoRoot, format, rangeSpec string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--format="+format, rangeSpec)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git log: %w", err)
	}
	return string(out), nil
}
