package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// Sync-block reasons the control plane states today. The wire field is an
// open string (MirrorSyncBlock.reason documents that more may be added), so
// anything else gets the generic rendering rather than a branch of its own.
const (
	syncBlockOrgIPAllowlist        = "org_ip_allowlist"
	syncBlockInstallationSuspended = "installation_suspended"
)

// mirrorSyncBlock is the --json shape of a GitHub installation block, on the
// repo row rather than on a placement: the block is per installation, so every
// placement of the upstream carries the same value and repeating it per row
// would say nothing the first copy did not.
type mirrorSyncBlock struct {
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observedAt"`
}

// lookupMirrorSyncBlock answers whether GitHub currently blocks the Entire
// installation serving one upstream, by reading that upstream's own mirror
// record. The /repos entry already names it: RepoPlacement.ID is the mirror
// ID (see repoCellPlacement), so this is one exact read, GET /mirrors/{id},
// which follows core's cross-cell redirect wherever the placement lives. Any
// one placement answers, since the block is per installation and every
// placement of the upstream carries the same value.
//
// Reading the entry's own ID rather than resolving placements matters for
// who sees the block: /mirrors/placements is pull-gated, while /repos and
// GET /mirrors/{id} are gated on viewing metadata, so a caller who can see
// the repo without being able to clone it (a direct grant, a project member)
// would otherwise have had the block silently withheld.
//
// The entry's own id is read first: core sets it to the canonical placement,
// the caller's home-jurisdiction copy where one exists, so the read stays on
// the home core instead of taking a cross-cell redirect to whichever copy
// sorts first. The placements are the fallback for an entry without one.
//
// blocked is false for "not blocked, as far as the caller can see": no
// placement with an ID, or a mirror carrying no block. A block whose reason
// cleans to nothing is still a block — the server stamped it — and renders
// with the generic wording (see syncBlockProblem).
func lookupMirrorSyncBlock(ctx context.Context, c mirrorStatusGetter, entry coreapi.RepoIndexEntry) (block mirrorSyncBlock, blocked bool, err error) {
	id := canonicalMirrorID(entry)
	if id == "" {
		return mirrorSyncBlock{}, false, nil
	}
	m, err := c.GetMirror(ctx, coreapi.GetMirrorParams{MirrorId: id})
	if err != nil {
		return mirrorSyncBlock{}, false, fmt.Errorf("read mirror: %w", err)
	}
	b, ok := m.SyncBlock.Get()
	if !ok {
		return mirrorSyncBlock{}, false, nil
	}
	return mirrorSyncBlock{Reason: cleanSyncReason(b.Reason), ObservedAt: b.ObservedAt}, true, nil
}

// canonicalMirrorID is the mirror ID to read a GitHub entry's record by: the
// entry's own id (its canonical placement), else the first placement carrying
// one, else "" for an entry with nothing placed.
func canonicalMirrorID(entry coreapi.RepoIndexEntry) string {
	if id := strings.TrimSpace(entry.ID); id != "" {
		return id
	}
	for _, p := range entry.Placements {
		if id := strings.TrimSpace(p.ID); id != "" {
			return id
		}
	}
	return ""
}

// maxSyncReasonRunes bounds a reason on its way to the terminal. The known
// values are short tokens; anything longer is not one the CLI can explain.
const maxSyncReasonRunes = 64

// cleanSyncReason strips control and format characters from a reason before
// it is printed. The value is server-controlled and the known ones are plain
// tokens, but it is echoed into a terminal, so an escape sequence must not
// reach it whatever the server sends.
func cleanSyncReason(raw string) string {
	if runes := []rune(raw); len(runes) > maxSyncReasonRunes {
		raw = string(runes[:maxSyncReasonRunes])
	}
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, raw))
}

// syncStatusUnavailable is the --json marker for a view whose block read
// failed: an absent syncBlock alone would read as "not blocked", which the
// stderr note says it is not, and a script reads neither stderr nor notes.
const syncStatusUnavailable = "unavailable"

// noteSyncStatusUnavailable decides what a failed block lookup means for the
// view it was made for. An interruption (the context cancelled or expired) is
// returned as the failure it is, so the command exits the way an interrupted
// command does instead of rendering a table over it. Any other failure is a
// note on stderr and the view goes on: the table it accompanies is still
// right about what it shows, so the command does not fail — but a reader
// checking why a mirror is stale must not take the absence of a block for
// its presence being ruled out.
//
// noted reports that the failure was downgraded to a note, so the caller can
// mark its --json output the same way.
func noteSyncStatusUnavailable(ctx context.Context, errW io.Writer, err error) (noted bool, fatal error) {
	if err == nil {
		return false, nil
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	fmt.Fprintf(errW, "Note: GitHub sync status unavailable: %v\n", renderCoreError(err))
	return true, nil
}

// syncBlockProblem says what is wrong, in terms the person who can fix it
// recognises. Only an unknown reason shows Entire's enum, since there is
// nothing else to say about it — and an empty one shows nothing at all,
// rather than an empty pair of brackets.
func syncBlockProblem(owner, reason string) string {
	switch reason {
	case syncBlockOrgIPAllowlist:
		return "The GitHub IP allow list of the " + owner + " organization blocks Entire, so its mirrors cannot sync from GitHub."
	case syncBlockInstallationSuspended:
		return "The Entire GitHub App installation for " + owner + " is suspended, so its mirrors cannot sync from GitHub."
	case "":
		return "GitHub blocks the Entire app installation for " + owner + ", so its mirrors cannot sync from GitHub."
	default:
		return "GitHub blocks the Entire app installation for " + owner + " (" + reason + "), so its mirrors cannot sync from GitHub."
	}
}

// syncBlockFix is the action, addressed to whoever administers the owner on
// GitHub: only they can take it, and a collaborator reading this is told whom
// to ask. An IP allow list is an organization feature; a suspension can also
// hit a personal account's installation, so that wording names no org.
func syncBlockFix(owner, reason string) string {
	switch reason {
	case syncBlockOrgIPAllowlist:
		return "Ask an admin of the " + owner + " organization to enable access for installed GitHub Apps in its IP allow list settings on GitHub."
	case syncBlockInstallationSuspended:
		return "Ask an admin of " + owner + " on GitHub to unsuspend the Entire app in its GitHub Apps settings."
	default:
		return "Ask an admin of " + owner + " on GitHub to restore the Entire app's access."
	}
}

// renderSyncBlock prints the block as a section of the `repo view` record:
// when it was last seen, what is wrong, what to do, and the command that
// confirms the fix — resync probes GitHub before it queues anything, so it is
// the check as well as the catch-up.
func renderSyncBlock(w io.Writer, st statusStyles, ref string, block *mirrorSyncBlock) {
	owner := mirrorRefOwner(ref)
	fmt.Fprintln(w, st.render(st.yellow, "Sync:"))
	label := "Blocked"
	if !block.ObservedAt.IsZero() {
		// A server clock ahead of this one must not render "observed in 2m":
		// the block was seen, and the nearest true thing is "just now".
		label += " (observed " + formatRelativeDuration(max(time.Since(block.ObservedAt), 0)) + ")"
	}
	fmt.Fprintf(w, "  %s %s\n", st.render(st.red, label+":"), syncBlockProblem(owner, block.Reason))
	fmt.Fprintf(w, "  %s Then run `entire repo mirror resync %s` to confirm the fix and catch the mirror up.\n", syncBlockFix(owner, block.Reason), ref)
}

// syncBlockDetailLocation is where the server attaches the block reason on
// the 409 a resync answers while GitHub still blocks the installation.
const syncBlockDetailLocation = "body.owner"

func newRepoMirrorResyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resync <repo>",
		Short: "Queue a fresh sync of a GitHub mirror on every cluster",
		Long: "Queue a fresh sync of a GitHub mirror on every cluster it is placed on, " +
			"so a mirror that has fallen behind its upstream catches up without " +
			"waiting for the next GitHub event.\n\n" +
			"This is also the check after fixing a block. When GitHub refused the " +
			"Entire app's access while syncing (an org IP allow list, or a " +
			"suspension hit mid-sync — `entire repo view` shows which), the " +
			"server probes GitHub first: if the block has been lifted " +
			"it clears the block and queues the sync; if not, nothing is queued and " +
			"the command reports what still needs fixing.\n\n" +
			"Requires write access to the GitHub repository. Serves /gh/ references " +
			"only: an Entire-native repository has no upstream to sync from.\n\n" +
			"--json prints the server's response (mirrorId, status) plus the repo.",
		Example: "  entire repo mirror resync /gh/octocat/hello-world\n" +
			"  entire repo mirror resync /gh/octocat/hello-world --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := parseMirrorRepoRef(args[0], mirrorCloneForge)
			if err != nil {
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				out, err := c.ResyncMirror(ctx, &coreapi.ResyncMirrorInputBody{
					Provider: coreapi.ResyncMirrorInputBodyProviderGithub,
					Owner:    ref.owner,
					Repo:     ref.repo,
				})
				if err != nil {
					return resyncError(ref, err)
				}
				if jsonRequested(cmd) {
					// The wire object plus the repo it was asked about, so a
					// field the server adds later reaches the reader unchanged.
					obj, err := mergeSynthesizedField(out, "repo", ref.qualified)
					if err != nil {
						return err
					}
					return printJSON(cmd.OutOrStdout(), obj)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Resync %s for %s on every cluster it is mirrored on.\n", out.Status, ref.qualified())
				return nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// resyncError turns the server's refusal into the message a reader can act
// on. A 409 is the one coded case: GitHub still blocks the installation, the
// server has re-stamped the block, and the reason rides in a problem detail
// at body.owner — rendered with the same words `repo view` uses, so the two
// never describe one block two ways. Everything else keeps the server's own
// message (renderCoreError, via runCore).
func resyncError(ref mirrorRepoRef, err error) error {
	if coreapi.APIErrorStatus(err) != http.StatusConflict {
		return err
	}
	detail, ok := coreapi.APIErrorDetail(err, syncBlockDetailLocation)
	if !ok {
		return err
	}
	var reason string
	if jerr := json.Unmarshal(detail.Value, &reason); jerr != nil {
		return err
	}
	if reason = cleanSyncReason(reason); reason == "" {
		return err
	}
	return errors.New(syncBlockProblem(ref.owner, reason) + " Nothing was queued.\n" +
		syncBlockFix(ref.owner, reason) + " Then run this command again.")
}
