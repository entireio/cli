package strategy

import (
	"context"
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
	// SyncRemoteSourceDefault: "origin" exists.
	SyncRemoteSourceDefault CheckpointSyncRemoteSource = "default"
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
// exist), then "origin", then the sole remote, then the first remote in
// .git/config order. It knows nothing about the checkpoint_remote URL
// feature; callers exempt that case themselves.
//
// Deliberately NOT keyed on the branch's tracking config
// (branch.<name>.pushRemote / remote.pushDefault / branch.<name>.remote).
// Election is compared against the remote of the push actually being made, so
// electing the tracking remote silently drops checkpoint sync on every push to
// any OTHER remote — `git push <other> HEAD`, a `git clone -o base` whose
// checkpoints go to a separately added origin, any repo with remote.pushDefault
// set. TestAlternates_RelativeObjectAlternate_CheckpointSync is the regression:
// it clones with `-o base` and pushes checkpoints to `origin`, and a tracking
// tier makes the pre-push hook a silent no-op.
//
// The fork setup that motivated the tracking tier — clone the base repo, add
// your fork, push there, with origin unpushable — is served by setting
// checkpoint_push_remote explicitly. That is also the only form of it that
// works end to end: read paths (resume, explain) resolve checkpoints through
// origin's remote-tracking refs, so a silently elected non-origin remote
// produces checkpoints that cannot be read back from the same clone.
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
func checkpointSyncAllowedForRemote(ctx context.Context, pushRemote string) bool {
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

// configuredRemotesInConfigOrder lists remote names in .git/config section
// order (approximates "first remote added"; `git remote` output is
// alphabetical and unsuitable). Remotes configured with only pushurl are
// deliberately invisible (spec Unit 1). Errors yield an empty list.
func configuredRemotesInConfigOrder(ctx context.Context) []string {
	out, err := exec.CommandContext(ctx, "git", "config", "--local", "--get-regexp", `^remote\..*\.url$`).Output()
	if err != nil {
		return nil
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
	return names
}
