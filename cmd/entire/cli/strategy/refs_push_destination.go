package strategy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// refsPushDestination is the single place checkpoint refs are pushed to.
type refsPushDestination struct {
	// target is passed to git push and to the recovery fetch: a remote name, or a URL.
	target string
	// checkpointRemote records that target came from a configured
	// checkpoint_remote. It cannot be recovered from target's shape — a
	// resolved push URL is URL-shaped too — and it decides both how the
	// destination is named and whether the "a checkpoint remote is configured"
	// hint applies.
	checkpointRemote bool
	// ignoredPushURLs counts the push URLs of a multi-URL remote that will NOT
	// receive checkpoint refs. Zero in every single-destination topology.
	ignoredPushURLs int
}

// resolveRefsPushDestination picks the single destination for checkpoint-ref
// pushes.
//
// Checkpoint refs need ONE deterministic destination, because the push-discovery
// queue records only a ref (`{"ref": …}`) with no per-destination state: a ref is
// removed from the queue once "the push" succeeds, so "the push" has to mean one
// place. Relying on git's fan-out across a remote's several push URLs breaks that
// in both directions — a single failing URL fails the whole invocation and no ref
// unqueues even though some URLs took it, and an unreachable FIRST URL makes git
// die() before it reaches any later URL at all.
//
// So when a remote carries more than one push URL we target its first push URL
// directly (the one git itself would push to first) and ignore the rest. The
// recovery fetch in fetchAndRebaseRefCommon uses the same target, so — unlike the
// fan-out path, which reconciled the remote's FETCH url while pushing to its
// pushurls — the URL we reconcile is finally the URL we push to.
//
// Consequences, deliberately accepted:
//   - Checkpoint refs live in exactly one repository. Cloning that repository
//     resolves them (its url becomes the clone's fetch URL); cloning a different
//     mirror of the same code does not. Mirroring checkpoints to several
//     repositories is what checkpoint_remote is for.
//   - A first push URL that REJECTS a ref (non-fast-forward) no longer lets later
//     URLs receive it. git would have carried on to them; we stop. That is the
//     price of a deterministic destination, and it is the case the queue can
//     actually reason about.
//
// The git-branch backend deliberately keeps git's fan-out: its v1 branch is a
// single shared ref with no queue to keep coherent, and mirroring it to every
// push URL is behavior users configure their remotes for.
//
// A single push URL keeps the remote NAME as the target rather than resolving it
// to a URL, so the overwhelmingly common topology behaves exactly as before —
// remote-tracking refs still update, output still says "origin", and no
// URL-keyed promisor config appears.
//
// Call this only once there is something to push: it spawns `git remote get-url`
// and its result is unused on an empty queue.
func resolveRefsPushDestination(ctx context.Context, ps pushSettings) refsPushDestination {
	target := ps.pushTarget()

	// A configured checkpoint_remote, or a push straight to a URL (git hands the
	// hook a bare URL verbatim), is already a single explicit destination.
	if ps.hasCheckpointURL() || remote.IsURL(target) {
		return refsPushDestination{target: target, checkpointRemote: ps.hasCheckpointURL()}
	}

	urls, err := remote.GetPushURLs(ctx, target)
	if err != nil {
		// Not a configured remote, or git could not report its URLs. Keep the
		// target as given; the push itself will report any real problem.
		logging.Debug(ctx, "git-refs push: could not enumerate push URLs; using target as given",
			slog.String("target", target),
			slog.String("error", err.Error()),
		)
	}
	if len(urls) < 2 {
		return refsPushDestination{target: target}
	}
	return refsPushDestination{target: urls[0], ignoredPushURLs: len(urls) - 1}
}

// display names the destination for progress and warning output.
//
// Deliberately not displayPushTarget: that maps ANY URL to the literal words
// "checkpoint remote", which was only ever true because a URL target implied a
// configured checkpoint_remote. A push URL we resolved ourselves is URL-shaped
// but is not a checkpoint remote, so it is named by its (redacted) URL.
func (d refsPushDestination) display() string {
	switch {
	case d.checkpointRemote:
		return "checkpoint remote"
	case d.ignoredPushURLs > 0:
		return fmt.Sprintf("%s (first of %d push URLs)", remote.RedactURLOrPath(d.target), d.ignoredPushURLs+1)
	default:
		return remote.RedactURLOrPath(d.target)
	}
}

// warnIgnoredPushURLs tells the user that checkpoint refs are going to one URL of
// a multi-URL remote — otherwise the choice is invisible and looks like the other
// mirrors silently lost their checkpoints. Call it only when there are refs to
// push, so a no-op push stays quiet.
func (d refsPushDestination) warnIgnoredPushURLs(ctx context.Context) {
	if d.ignoredPushURLs == 0 {
		return
	}
	fmt.Fprintf(stderrWriter, "[entire] Checkpoints go to one repository: %s. %d other push URL(s) of this remote will not receive them.\n",
		d.display(), d.ignoredPushURLs)
	fmt.Fprintln(stderrWriter, "[entire] To store checkpoints in a specific repository instead, set checkpoint_remote in .entire/settings.json.")
	logging.Info(ctx, "git-refs push: multi-URL remote, pushing checkpoint refs to the first push URL only",
		slog.String("target", remote.RedactURLOrPath(d.target)),
		slog.Int("ignored_push_urls", d.ignoredPushURLs),
	)
}
