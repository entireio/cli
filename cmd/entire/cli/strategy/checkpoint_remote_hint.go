package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// warnIgnoredCheckpointRemote tells the user, during their own push, that the
// checkpoint store they configured is not the one their checkpoints are going
// to, and gives the single command that fixes it.
//
// The rejection is not new: checkpointRemoteIsInherited has always refused a
// committed checkpoint_remote whose owner does not match every remote
// identifying this repo, and has always said so in a Warn log. But a log is
// read by someone who already suspects a problem, and the visible symptom here
// — checkpoints arriving in the code repository — is a working setup, just not
// the one the user asked for. Pre-push stderr is where the condition reaches
// them without being looked for, and where an agent pushing on their behalf
// sees it too.
//
// It blocks nothing, deliberately. Checkpoints keep flowing to the elected
// remote, because refusing to push them turns a misconfiguration into lost
// work, and a ref delivered to the code repo is not stranded there: claiming
// the store re-delivers everything already pushed
// (resyncCheckpointRefsOnDestinationChange on git-refs; on git-branch the whole
// v1 branch travels by construction).
//
// Every push while the condition holds, not once ever — a one-shot notice is
// seen by whoever set the repo up and not by whoever hits the problem.
func warnIgnoredCheckpointRemote(ctx context.Context, ps pushSettings) {
	// Two jobs, and neither is redundant with the ownership verdict below.
	//
	// checkpointRemoteConfigured is the free half: an empty checkpointURL means
	// "none configured" as often as "configured and refused", and only the
	// second is worth a settings load and two git subprocesses.
	//
	// hasCheckpointURL is the authoritative half. PushURL is what decided where
	// this push sends checkpoints, and InheritedCheckpointRemote re-runs the
	// ownership vote over an identity set that PushURL's own derivation can
	// diverge from (the entire:// mirror path, and the divergence
	// computeCheckpointSyncInfo already documents). Asking the second question
	// about a store the first one ADOPTED would warn about a destination that
	// is working, on every push.
	if ps.hasCheckpointURL() || !ps.checkpointRemoteConfigured {
		return
	}
	s, err := settings.Load(ctx)
	if err != nil {
		return
	}
	repo, reason, inherited := remote.InheritedCheckpointRemote(ctx, s, ps.remote)
	if !inherited {
		// Configured, not adopted, and ownership is not why. PushURL fell back
		// for some other reason (an unparseable remote URL, an unreachable
		// derivation) and logged its own cause; naming ownership here would
		// send the user after the wrong problem, and the claim command would
		// not fix it.
		return
	}

	fmt.Fprintf(stderrWriter,
		"[entire] Checkpoints are going to %q, not to the configured checkpoint_remote %s: %s.\n",
		ps.remote, repo, reason)
	if claim := remote.ClaimCheckpointRemoteCommand(s.GetCheckpointRemote()); claim != "" {
		fmt.Fprintf(stderrWriter,
			"[entire] If %s is yours, run: %s — checkpoints already pushed follow it on the next push.\n",
			repo, claim)
	} else {
		fmt.Fprintf(stderrWriter,
			"[entire] If %s is yours, declare it in .entire/settings.local.json — checkpoints already pushed follow it on the next push.\n",
			repo)
	}
	logging.Info(ctx, "ignored checkpoint_remote surfaced at pre-push",
		slog.String("checkpoint_repo", repo),
		slog.String("reason", reason),
		slog.String("push_remote", ps.remote))
}
