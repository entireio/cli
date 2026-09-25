package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
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
// Fires on every push that delivered checkpoints while the condition holds. A
// one-shot notice is seen by whoever set the repo up, not by whoever hits the
// problem. Callers invoke it only after delivery, so a push carrying no
// checkpoints says nothing: nothing went anywhere to warn about.
//
// Only an UNPROVABLE verdict warns here. A disproved one is a remote owned by
// someone else — the ordinary fork contributor, for whom checkpoints landing in
// their fork is the correct outcome — and printing on every one of their
// pushes would be noise they can only silence by claiming a store that is not
// theirs. `entire status` and `entire enable` still report that case with the
// same remedy, for the rare owner whose remote genuinely disagrees.
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
	cr := s.GetCheckpointRemote()
	if cr == nil {
		return
	}
	verdict, reason := remote.InheritedCheckpointRemoteVerdict(ctx, s, ps.remote)
	if verdict != remote.OwnershipUnprovable {
		// Ours: configured, not adopted, and ownership is not why. PushURL fell
		// back for some other reason (an unparseable remote URL, an unreachable
		// derivation) and logged its own cause; naming ownership here would
		// send the user after the wrong problem, and the claim command would
		// not fix it. Disproved: see the doc comment.
		return
	}
	repo := cr.Repo

	// repo comes from the committed settings file; strip escape sequences
	// before it reaches the terminal.
	shown := tuiutil.SanitizeDisplayText(repo)
	fmt.Fprintf(stderrWriter,
		"[entire] Checkpoints are going to %q, not to the configured checkpoint_remote %s: %s.\n",
		ps.remote, shown, reason)
	// Promise only new checkpoints: on git-refs, refs already pushed to the
	// fallback have left the push queue and stay there. Only the git-branch
	// backend would move them (it pushes the whole v1 branch), and it is legacy
	// and will lose support soon, so the wording follows git-refs.
	if claim := remote.ClaimCheckpointRemoteCommand(s.GetCheckpointRemote()); claim != "" {
		fmt.Fprintf(stderrWriter,
			"[entire] If %s is yours, run: %s — new checkpoints go there from your next push.\n",
			shown, claim)
	} else {
		fmt.Fprintf(stderrWriter,
			"[entire] If %s is yours, declare it in .entire/settings.local.json — new checkpoints go there from your next push.\n",
			shown)
	}
	logging.Info(ctx, "ignored checkpoint_remote surfaced at pre-push",
		slog.String("checkpoint_repo", repo),
		slog.String("reason", reason),
		slog.String("push_remote", ps.remote))
}
