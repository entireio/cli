package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/settings/usersettings"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// GlobalEnablePushConsentLogMsg is logged when pre-push asks whether to push
// checkpoints from a repository on only through global enable. E2E fails any
// test whose logs contain it, as it does for CheckpointPushQuestionLogMsg: no
// E2E push has a person behind it.
const GlobalEnablePushConsentLogMsg = "pre-push: asking to push checkpoints from a global-enable repository"

// ErrCheckpointPushHeldForConsent is returned by the explicit checkpoint
// pushes when the repository is on only through global enable and the
// developer has not agreed to push its checkpoints: they stay queued.
var ErrCheckpointPushHeldForConsent = errors.New(
	"this repository is on only through global enable and you have not agreed to push its checkpoints; " +
		"run `git push` in a terminal and answer yes, and they stay queued until then")

// consentYes is the long form of the consent question's yes.
const consentYes = "yes"

// globalEnablePushConsentTimeout bounds the question, like the first-push
// policy question: nothing else bounds the hook, and running out of time holds.
const globalEnablePushConsentTimeout = 60 * time.Second

// GlobalEnablePushConsent says whether checkpoints from this repository may
// be pushed, as far as global enable is concerned.
type GlobalEnablePushConsent int

const (
	// GlobalEnableConsentNotNeeded: the developer enabled the repository
	// themselves, or global enable is off.
	GlobalEnableConsentNotNeeded GlobalEnablePushConsent = iota
	// GlobalEnableConsentAgreed: the repository's `repos` entry says
	// `push_checkpoints: true`.
	GlobalEnableConsentAgreed
	// GlobalEnableConsentOwnerListed: every URL the checkpoints go to belongs
	// to an owner on the user's global_enable_owners list.
	GlobalEnableConsentOwnerListed
	// GlobalEnableConsentHeld: on only through global enable and not agreed
	// to, so checkpoints stay local.
	GlobalEnableConsentHeld
)

// globalEnablePushConsent decides ps's consent. Only a repository on only
// through global enable needs any: one the developer enabled was agreed to
// when they did.
func globalEnablePushConsent(ctx context.Context, ps pushSettings) GlobalEnablePushConsent {
	switch {
	case !ps.enabledOnlyByGlobal:
		return GlobalEnableConsentNotNeeded
	case ps.pushConsent:
		return GlobalEnableConsentAgreed
	case len(ps.owners) > 0 && destinationsOwnerListed(checkpointDestinationURLs(ctx, ps), ps.owners):
		return GlobalEnableConsentOwnerListed
	default:
		return GlobalEnableConsentHeld
	}
}

// destinationsOwnerListed reports whether every URL belongs to a listed
// owner. Fails closed: no URLs, or any URL that names no forge owner, is not
// listed.
func destinationsOwnerListed(urls, owners []string) bool {
	if len(urls) == 0 {
		return false
	}
	for _, url := range urls {
		owner := usersettings.NormalizeOwner(url)
		if owner == "" || !slices.Contains(owners, owner) {
			return false
		}
	}
	return true
}

// checkpointDestinationURLs is every URL ps sends checkpoints to: the
// checkpoint_remote URL when one is configured, a URL the push names
// directly, or every push URL of the remote. Nil when the remote's URLs
// cannot be read, which no owner matches.
func checkpointDestinationURLs(ctx context.Context, ps pushSettings) []string {
	if ps.hasCheckpointURL() {
		return []string{ps.checkpointURL}
	}
	if remote.IsURL(ps.remote) {
		return []string{ps.remote}
	}
	dir, env := gitHookSafeDirEnv(ctx)
	urls, err := gitremote.GetPushURLsInDir(ctx, dir, env, ps.remote)
	if err != nil {
		return nil
	}
	return urls
}

// destinationOwner is the owner to suggest adding to the list: the one owner
// every destination shares, or "" when there is none.
func destinationOwner(ctx context.Context, ps pushSettings) string {
	var shared string
	for i, url := range checkpointDestinationURLs(ctx, ps) {
		owner := usersettings.NormalizeOwner(url)
		if owner == "" || (i > 0 && owner != shared) {
			return ""
		}
		shared = owner
	}
	return shared
}

// heldForGlobalEnableConsent is the explicit pushes' form of the gate: they
// never ask, and report a hold as ErrCheckpointPushHeldForConsent.
func heldForGlobalEnableConsent(ctx context.Context, ps pushSettings) error {
	if globalEnablePushConsent(ctx, ps) == GlobalEnableConsentHeld {
		return ErrCheckpointPushHeldForConsent
	}
	return nil
}

// globalEnablePushAllowed is pre-push's consent gate for a repository on only
// through global enable. Checkpoints were captured there without the
// developer enabling it, so they are pushed only once the developer agrees.
// With checkpoints waiting and a person at a terminal (not an agent), a git
// push asks once; yes is recorded for every clone and worktree and this push
// goes ahead. Otherwise the checkpoints stay queued and one line says so. It
// never fails the user's push. It records on ps that it asked, so the
// first-push policy question does not follow it on the same push.
func globalEnablePushAllowed(ctx context.Context, ps *pushSettings, fromGitHook bool) bool {
	switch globalEnablePushConsent(ctx, *ps) {
	case GlobalEnableConsentOwnerListed:
		ps.ownerListed = true
		return true
	case GlobalEnableConsentNotNeeded, GlobalEnableConsentAgreed:
		return true
	case GlobalEnableConsentHeld:
	}
	waiting, err := CountUnpushedCheckpoints(ctx, ps.remote)
	if err != nil || waiting == 0 {
		// Nothing to push, or nothing countable: hold without a word rather
		// than ask about checkpoints that may not exist.
		return false
	}
	if fromGitHook && canAskGlobalEnablePushConsent() {
		ps.consentAsked = true
		logging.Info(ctx, GlobalEnablePushConsentLogMsg)
		if askGlobalEnablePushConsent(globalEnablePushDestination(*ps)) && recordGlobalEnablePushConsent(ctx, ps) {
			return true
		}
	}
	remedy := "Run `git push` in a terminal and answer yes to push them"
	if owner := destinationOwner(ctx, *ps); owner != "" {
		remedy += ", or push every repository of " + owner + " with `entire enable --global --owner " + owner + "`"
	}
	fmt.Fprintf(stderrWriter,
		"[entire] Entire is holding session checkpoints for %s: this repository is on only through global enable. %s.\n",
		globalEnablePushDestination(*ps), tuiutil.SanitizeDisplayText(remedy))
	return false
}

// canAskGlobalEnablePushConsent reports a person at a terminal. An agent's
// shell can pass the terminal probe, and its yes is not the developer's.
func canAskGlobalEnablePushConsent() bool {
	return interactive.CanPromptInteractively() && review.DetectAgentSession() == ""
}

// recordGlobalEnablePushConsent saves a yes and re-reads the settings, so the
// push goes ahead only if the saved answer is the one Load sees.
func recordGlobalEnablePushConsent(ctx context.Context, ps *pushSettings) bool {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		fmt.Fprintf(stderrWriter, "[entire] Could not save your answer: %v\n", err)
		return false
	}
	if _, err := settings.SetRepoPushConsent(ctx, root, true); err != nil {
		logging.Warn(ctx, "pre-push: could not save global-enable push consent", slog.String("error", err.Error()))
		fmt.Fprintf(stderrWriter, "[entire] Could not save your answer: %v\n", err)
		return false
	}
	s, err := settings.Load(ctx)
	if err != nil || !s.RepoPushConsent() {
		fmt.Fprintln(stderrWriter, "[entire] Your answer was saved but does not apply to this repository yet.")
		return false
	}
	ps.pushConsent = true
	return true
}

// askGlobalEnablePushConsent asks the question and reports a yes; tests
// replace it because under test the real question reads nothing.
var askGlobalEnablePushConsent = askGlobalEnablePushConsentTTY

// askGlobalEnablePushConsentTTY asks on the controlling terminal, like
// askCheckpointPushQuestionTTY. Anything but yes, including no answer in
// time, holds.
func askGlobalEnablePushConsentTTY(destination string) bool {
	if interactive.UnderTest() {
		return false
	}
	tty, err := interactive.OpenPromptTTY()
	if err != nil {
		return false
	}
	defer tty.Close()

	fmt.Fprintf(tty, "\nEntire recorded sessions in this repository because global enable is on, and has not pushed them.\n"+
		"Push its session checkpoints to %s, now and from every clone of this repository? [y/N] (%s): ",
		destination, globalEnablePushConsentTimeout)
	line, err := readPromptAnswer(tty, globalEnablePushConsentTimeout)
	if errors.Is(err, errPromptTimedOut) {
		fmt.Fprintln(tty, "\nNo answer; the checkpoints stay on this machine.")
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == consentYes
}

// GlobalEnablePushReport is what `entire status` says about checkpoint pushes
// from a repository on only through global enable.
type GlobalEnablePushReport struct {
	Consent GlobalEnablePushConsent
	// Waiting counts the checkpoints a held push keeps local.
	Waiting int
	// Owner is the owner a held repository could be pushed under with
	// `entire enable --global --owner`, or "".
	Owner string
}

// GlobalEnablePushStatus decides consent for the checkpoint sync remote the
// way pre-push would, from local state only: unlike resolvePushSettings it
// never fetches a missing metadata branch.
func GlobalEnablePushStatus(ctx context.Context) (GlobalEnablePushReport, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return GlobalEnablePushReport{}, fmt.Errorf("load settings: %w", err)
	}
	if !s.EnabledOnlyByGlobal() {
		return GlobalEnablePushReport{Consent: GlobalEnableConsentNotNeeded}, nil
	}
	syncRemote, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return GlobalEnablePushReport{}, err
	}
	ps := pushSettings{
		remote:              syncRemote.Name,
		enabledOnlyByGlobal: true,
		pushConsent:         s.RepoPushConsent(),
		owners:              s.GlobalEnableOwners(),
	}
	if s.GetCheckpointRemote() != nil {
		if url, enabled, err := remote.PushURL(ctx, syncRemote.Name); err == nil && enabled {
			ps.checkpointURL = url
		}
	}
	report := GlobalEnablePushReport{Consent: globalEnablePushConsent(ctx, ps)}
	switch report.Consent {
	case GlobalEnableConsentHeld:
		report.Waiting, _ = CountUnpushedCheckpoints(ctx, syncRemote.Name) //nolint:errcheck // a count status cannot take is reported as none waiting
		report.Owner = destinationOwner(ctx, ps)
	case GlobalEnableConsentOwnerListed:
		report.Owner = destinationOwner(ctx, ps)
	case GlobalEnableConsentNotNeeded, GlobalEnableConsentAgreed:
	}
	return report, nil
}
