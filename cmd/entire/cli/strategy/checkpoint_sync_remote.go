package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"slices"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// CheckpointSyncRemoteSource identifies which precedence rule elected the
// checkpoint sync remote.
type CheckpointSyncRemoteSource string

const (
	// SyncRemoteSourceConfig: strategy_options.checkpoint_push_remote.
	SyncRemoteSourceConfig CheckpointSyncRemoteSource = "config"
	// SyncRemoteSourceObserved: elected by evidence — a past push that agreed
	// with the branch's declared push destination (see
	// commitCapturedSyncRemote). Named for what was observed, not for the
	// latch that recorded it, which is why the internal names still say "capture".
	SyncRemoteSourceObserved CheckpointSyncRemoteSource = "observed"
	// SyncRemoteSourceDefault: "origin" exists.
	SyncRemoteSourceDefault CheckpointSyncRemoteSource = "default"
	// SyncRemoteSourceOverride: named by the caller for this operation only
	// (`entire trust --remote <name>`), never persisted and never elected.
	SyncRemoteSourceOverride CheckpointSyncRemoteSource = "override"
	// SyncRemoteSourceSole: exactly one remote configured.
	SyncRemoteSourceSole CheckpointSyncRemoteSource = "sole"
	// SyncRemoteSourceFirst: first remote in .git/config order.
	SyncRemoteSourceFirst CheckpointSyncRemoteSource = "first"
)

// CheckpointSyncRemote is the single git remote elected to carry checkpoint
// data. Name is empty when no remotes are configured.
type CheckpointSyncRemote struct {
	Name   string
	Source CheckpointSyncRemoteSource
}

// ResolveCheckpointSyncRemote elects the one configured git remote that
// checkpoint data syncs to. Pure local lookup — no network. Precedence:
// checkpoint_push_remote setting (fail-closed if the named remote does not
// exist), then the captured election (evidence-elected by a past push that
// agreed with the branch's declared push destination; fail-soft if that
// remote is gone), then "origin", then the sole remote, then the first remote
// in .git/config order. It knows nothing about the checkpoint_remote URL
// feature; callers exempt that case themselves.
//
// Deliberately NOT keyed on the branch's tracking config alone
// (branch.<name>.pushRemote / remote.pushDefault / branch.<name>.remote).
// Election is compared against the remote of the push actually being made, so
// electing the tracking remote from config at rest silently drops checkpoint
// sync on every push to any OTHER remote — `git push <other> HEAD`, a
// `git clone -o base` whose checkpoints go to a separately added origin, any
// repo with remote.pushDefault set.
// TestAlternates_RelativeObjectAlternate_CheckpointSync is the regression:
// it clones with `-o base` and pushes checkpoints to `origin`, and a tracking
// tier makes the pre-push hook a silent no-op. The captured tier is the safe
// form of the same intent: tracking config nominates a remote, but only an
// actual push that delivers checkpoints to it elects it (see
// commitCapturedSyncRemote).
//
// The fork setup that motivated the tracking tier — clone the base repo, add
// your fork, push there, with origin unpushable — is served automatically by
// capture, or explicitly by checkpoint_push_remote. Either way the result is
// readable from the same clone: the read paths (resume, explain, discovery)
// consult the elected remote first and fall back to origin as a read-only
// legacy tier (see CheckpointReadRemotes). Tracking config on its own still
// elects nothing, for the silent-no-op reason above.
func ResolveCheckpointSyncRemote(ctx context.Context) (CheckpointSyncRemote, error) {
	// Fail closed on an unreadable settings file: election must never
	// override a checkpoint_push_remote the file may contain but we could
	// not read, or checkpoints would silently re-route away from the remote
	// the user configured for isolation.
	s, err := settings.Load(ctx)
	if err != nil {
		return CheckpointSyncRemote{}, fmt.Errorf("cannot read settings to resolve the checkpoint sync remote: %w", err)
	}
	if name := s.GetCheckpointPushRemote(); name != "" {
		if !isConfiguredRemote(ctx, name) {
			return CheckpointSyncRemote{}, fmt.Errorf(
				"checkpoint_push_remote %q is not a configured git remote; checkpoint sync disabled until fixed", name)
		}
		return CheckpointSyncRemote{Name: name, Source: SyncRemoteSourceConfig}, nil
	}

	// Captured tier: fail-soft, unlike the explicit setting above — capture
	// is automatic state, so a captured remote that was since renamed or
	// removed falls through to the default tiers instead of disabling sync.
	for _, name := range loadCapturedSyncRemotes(ctx) {
		if isConfiguredRemote(ctx, name) {
			return CheckpointSyncRemote{Name: name, Source: SyncRemoteSourceObserved}, nil
		}
		logging.Debug(ctx, "captured checkpoint sync remote is not configured; falling through",
			slog.String("remote", name))
	}

	remotes := configuredRemotesInConfigOrder(ctx)
	switch {
	case len(remotes) == 0:
		return CheckpointSyncRemote{}, nil
	case slices.Contains(remotes, "origin"):
		return CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, nil
	case len(remotes) == 1:
		return CheckpointSyncRemote{Name: remotes[0], Source: SyncRemoteSourceSole}, nil
	default:
		return CheckpointSyncRemote{Name: remotes[0], Source: SyncRemoteSourceFirst}, nil
	}
}

// checkpointSyncAllowedForRemote reports whether a push to pushRemote may
// carry checkpoint data. False for every remote except the elected
// checkpoint sync remote — including raw-URL pushes (git passes the URL as
// the hook arg) and the fail-closed misconfigured case. Callers exempt the
// dedicated checkpoint_remote URL mode before calling.
// pendingCapture, when non-empty, is the remote this push is about to elect but
// has not persisted yet — the gate must let the electing push carry the
// checkpoints that justify the election.
func checkpointSyncAllowedForRemote(ctx context.Context, pushRemote, pendingCapture string) bool {
	if pendingCapture != "" && pendingCapture == pushRemote {
		return true
	}
	syncRemote, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		// Neutral wording: err covers both a misconfigured checkpoint_push_remote
		// and an unreadable settings file, and the wrapped error already carries
		// the specifics.
		logging.Warn(ctx, "checkpoint sync skipped: cannot resolve checkpoint sync remote",
			slog.String("error", err.Error()))
		return false
	}
	if syncRemote.Name == "" || syncRemote.Name != pushRemote {
		logging.Debug(ctx, "checkpoint sync skipped: push remote is not the checkpoint sync remote",
			slog.String("push_remote", pushRemote),
			slog.String("checkpoint_sync_remote", syncRemote.Name))
		return false
	}
	return true
}

// hintGatedCheckpointSync tells the user, on a gated pre-push, that
// checkpoints are waiting for the elected sync remote and how to re-route
// them — otherwise the gate is a silent no-op and checkpoints strand locally
// until the elected remote happens to be pushed.
//
// Stays quiet unless every condition holds: the push target is a configured
// remote (checkpoint_push_remote takes a remote name, so a raw-URL push has
// no actionable suggestion), the election succeeded AND was automatic (an
// explicit checkpoint_push_remote is a decision already made, and the
// fail-closed misconfigured case logs a warning through the gate itself), and
// checkpoints are actually waiting. Fully local — no network.
//
// The hint names .entire/settings.local.json: a remote name is a per-clone
// fact, and committing it to the tracked settings.json would fail-close
// checkpoint sync for every teammate whose clone lacks that remote name.
func hintGatedCheckpointSync(ctx context.Context, pushRemote string) {
	if !isConfiguredRemote(ctx, pushRemote) {
		return
	}
	syncRemote, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil || syncRemote.Name == "" || syncRemote.Source == SyncRemoteSourceConfig {
		return
	}
	// Only for a remote this branch actually pushes to. The advice is "point
	// checkpoint_push_remote at the remote you just pushed", which is right for a
	// habitual destination and wrong — actively harmful — for a one-off: a
	// deploy-target push (`git push heroku`) or a single `git push upstream` to
	// open a PR would be told to publish session transcripts there, which is the
	// leak the single-remote gate exists to prevent.
	//
	// Since capture takes a declared destination on the first push that agrees
	// with it AND delivers checkpoints there, this gate leaves the hint speaking
	// for exactly what capture cannot fix: a declared remote capture will not
	// elect because a capture is already in force (phase-1
	// first-capture-sticks), where naming the setting really is the only way to
	// re-route. A push that capture is about to elect never reaches the hint —
	// the gate admits it on the pending capture — so a failed delivery does not
	// produce advice to hand-configure what the next successful push will do by
	// itself.
	if declaredPushDestination(ctx) != pushRemote {
		logging.Debug(ctx, "gated checkpoint sync hint suppressed: push target is not this branch's declared destination",
			slog.String("push_remote", pushRemote),
			slog.String("checkpoint_sync_remote", syncRemote.Name))
		return
	}
	count, err := CountUnpushedCheckpoints(ctx, syncRemote.Name)
	if err != nil {
		// Stay quiet toward the user (best-effort hint in their push), but
		// leave a trace: a persistently failing count would otherwise
		// re-create the silent stall this hint exists to surface.
		logging.Debug(ctx, "gated checkpoint sync hint suppressed: cannot count unpushed checkpoints",
			slog.String("checkpoint_sync_remote", syncRemote.Name),
			slog.String("error", err.Error()))
		return
	}
	if count == 0 {
		return
	}
	fmt.Fprintf(stderrWriter,
		"[entire] %d checkpoint(s) are waiting to sync to %q, this repo's checkpoint sync remote; this push to %q does not carry them.\n",
		count, syncRemote.Name, pushRemote)
	fmt.Fprintf(stderrWriter,
		"[entire] To sync checkpoints to %q instead, set strategy_options.checkpoint_push_remote to %q in .entire/settings.local.json.\n",
		pushRemote, pushRemote)
	logging.Info(ctx, "gated checkpoint sync hint shown",
		slog.Int("unpushed_checkpoints", count),
		slog.String("checkpoint_sync_remote", syncRemote.Name),
		slog.String("push_remote", pushRemote))
}

// configuredRemotesInConfigOrder lists remote names in .git/config section
// order (approximates "first remote added"; `git remote` output is
// alphabetical and unsuitable). Remotes configured with only pushurl are
// deliberately invisible (spec Unit 1). Errors yield an empty list.
func configuredRemotesInConfigOrder(ctx context.Context) []string {
	return cachedRemotesInConfigOrder(ctx, readRemotesInConfigOrder)
}

// readRemotesInConfigOrder lists remote names, distinguishing "this repo has no
// remotes" from "the read failed". Both used to collapse to nil, which was
// harmless while every caller re-ran the command — but the per-invocation cache
// would memoize a failure's nil as a legitimately empty list and then skip
// checkpoint sync for the rest of the process. `git config --get-regexp` exits 1
// for no match, so that exit code alone is the empty answer; anything else (a
// fork failure under load, a cancelled context, a locked config) is an error the
// cache must not keep.
func readRemotesInConfigOrder(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "config", "--local", "--get-regexp", `^remote\..*\.url$`)
	if worktreeRoot, ok := settings.WorktreeRoot(ctx); ok {
		cmd.Dir = worktreeRoot
	}
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil // no remote.*.url keys: a real, cacheable answer
		}
		return nil, fmt.Errorf("list configured remotes: %w", err)
	}
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// line: "remote.<name>.url <url>"; <name> may contain dots, so trim
		// the fixed prefix and the ".url <value>" suffix instead of splitting.
		key, _, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url")
		if name == "" || name == key || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}
