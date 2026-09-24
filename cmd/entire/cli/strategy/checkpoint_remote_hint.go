package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// warnIgnoredCheckpointRemote tells the user, in their own push output, that
// the checkpoint store they configured is not where checkpoints are going, and
// names the command that fixes it.
//
// checkpointRemoteIsInherited has always refused an inherited store and always
// logged it, but a log is read by someone who already suspects a problem, and
// the symptom here — checkpoints arriving in the code repository — looks like a
// working setup. It blocks nothing: refusing the push would turn a
// misconfiguration into lost work, and claiming the store re-delivers what
// already went to the wrong one.
//
// Fires on every push while the condition holds. A one-shot notice is seen by
// whoever set the repo up, not by whoever hits the problem.
func warnIgnoredCheckpointRemote(ctx context.Context, ps pushSettings) {
	// hasCheckpointURL is the authoritative half: PushURL decided where this
	// push sends checkpoints, while InheritedCheckpointRemote re-runs the vote
	// over an identity set that can diverge from it, so asking the second about a
	// store the first ADOPTED would warn about a working destination on every
	// push. checkpointRemoteConfigured is the free half — an empty checkpointURL
	// means "none configured" as often as "configured and refused".
	if ps.hasCheckpointURL() || !ps.checkpointRemoteConfigured {
		return
	}
	s, err := settings.Load(ctx)
	if err != nil {
		return
	}
	verdict, reason := remote.InheritedCheckpointRemoteVerdict(ctx, s, ps.remote)
	if !verdict.Refused() {
		// Configured, not adopted, and ownership is not why. PushURL fell back
		// for some other reason (an unparseable remote URL, an unreachable
		// derivation) and logged its own cause; naming ownership here would
		// send the user after the wrong problem, and the claim command would
		// not fix it.
		return
	}

	repo := s.GetCheckpointRemote().Repo
	explanation, claim := remote.IgnoredCheckpointRemoteGuidance(ctx, s.GetCheckpointRemote(), verdict, reason)
	fmt.Fprintf(stderrWriter,
		"[entire] Checkpoints are going to %q, not to the configured checkpoint_remote %s: %s.\n",
		ps.remote, repo, explanation)
	if claim != "" {
		fmt.Fprintf(stderrWriter,
			"[entire] If %s is yours, confirm it for this clone: %s — checkpoints already pushed follow it on the next push.\n",
			repo, claim)
	}

	logging.Info(ctx, "ignored checkpoint_remote surfaced at pre-push",
		slog.String("checkpoint_repo", repo),
		slog.String("reason", reason),
		slog.String("push_remote", ps.remote))
}
