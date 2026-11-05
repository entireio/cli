package strategy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
)

// CheckpointPushQuestionLogMsg is logged when pre-push asks for the checkpoint
// push policy. E2E fails any test whose logs contain it, as it does for
// CommitLinkingPromptLogMsg: no E2E push has a person behind it.
const CheckpointPushQuestionLogMsg = "pre-push: asking for the checkpoint push policy"

// checkpointPushQuestionBackoff is how long an unanswered question stays away,
// so a terminal nobody watches does not cost every push the prompt timeout.
const checkpointPushQuestionBackoff = 7 * 24 * time.Hour

// checkpointPushQuestionTimeout bounds how long the question waits. It is
// longer than confirmPromptTimeout because this is a three-way policy choice,
// not a yes/no, and running out of time uploads the push being decided about.
// Nothing outside pre-push bounds the hook, so the wait is not cut short.
const checkpointPushQuestionTimeout = 60 * time.Second

const opfDeprecationNotice = "OPF is deprecated and will be removed. To upload only the checkpoints you choose, run `entire configure --checkpoint-push-policy manual`."

// checkpointPushQuestionFormat takes the prompt timeout.
const checkpointPushQuestionFormat = `
Entire saves your AI sessions as checkpoints and uploads them when you push.

  [a] Automatic: checkpoints upload when you push, after redaction
      removes secrets it recognizes.
  [m] Manual: checkpoints stay on this machine until you run
      ` + "`entire checkpoint push`" + `. A push that sends refs/entire/* itself
      with --no-verify isn't covered.
  [n] Not now (this push uploads as usual)

Choice [a/m/n] (%s, default n): `

type checkpointPushQuestionAnswer int

const (
	checkpointPushAnswerNotNow checkpointPushQuestionAnswer = iota
	checkpointPushAnswerAutomatic
	checkpointPushAnswerManual
	checkpointPushAnswerTimedOut
	// checkpointPushAnswerSuperseded is an answer that lost to a policy stored
	// while the question waited; the stored policy decides the push.
	checkpointPushAnswerSuperseded
)

// readCheckpointPushQuestionAnswer reads one answer from r. Anything but an
// automatic or manual choice is "not now", including a closed terminal; only
// running out of time is a timeout.
func readCheckpointPushQuestionAnswer(r io.Reader, timeout time.Duration) checkpointPushQuestionAnswer {
	line, err := readPromptAnswer(r, timeout)
	if errors.Is(err, errPromptTimedOut) {
		return checkpointPushAnswerTimedOut
	}
	if err != nil {
		return checkpointPushAnswerNotNow
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "a", "automatic":
		return checkpointPushAnswerAutomatic
	case "m", "manual":
		return checkpointPushAnswerManual
	default:
		return checkpointPushAnswerNotNow
	}
}

// recordCheckpointPushQuestionAnswer applies answer's back-off to prefs; the
// policy itself is saved in its own file. "Not now" leaves them unchanged, so
// the question returns on the next push.
func recordCheckpointPushQuestionAnswer(prefs *settings.ClonePreferences, answer checkpointPushQuestionAnswer, now time.Time) {
	switch answer {
	case checkpointPushAnswerAutomatic, checkpointPushAnswerManual:
		prefs.CheckpointPushQuestionBackoffUntil = ""
	case checkpointPushAnswerTimedOut:
		prefs.CheckpointPushQuestionBackoffUntil = now.Add(checkpointPushQuestionBackoff).UTC().Format(time.RFC3339)
	case checkpointPushAnswerNotNow, checkpointPushAnswerSuperseded:
	}
}

type firstPushPrompt int

const (
	firstPushPromptNone firstPushPrompt = iota
	firstPushPromptQuestion
	firstPushPromptOPFNotice
)

// firstPushPromptFor decides what a terminal push shows a clone whose
// preferences are prefs, where policyStored says whether it chose a policy.
// OPF can ask its own question on a push (unless its prompt_default answers
// for it), so OPF users get a one-time deprecation line instead of a possible
// second question.
// Commit linking asks in prepare-commit-msg, never during a push, so this is
// the only other question a push can show.
func firstPushPromptFor(policyStored bool, prefs *settings.ClonePreferences, now time.Time, opfEnabled bool) firstPushPrompt {
	if policyStored {
		return firstPushPromptNone
	}
	if opfEnabled {
		if prefs.OPFDeprecationNoticeShown {
			return firstPushPromptNone
		}
		return firstPushPromptOPFNotice
	}
	// A back-off further out than one period came from a clock that was ahead,
	// or a hand edit, and is treated like a malformed one.
	if until, err := time.Parse(time.RFC3339, prefs.CheckpointPushQuestionBackoffUntil); err == nil &&
		now.Before(until) && !until.After(now.Add(checkpointPushQuestionBackoff)) {
		return firstPushPromptNone
	}
	return firstPushPromptQuestion
}

// askCheckpointPushPolicyOnFirstPush asks a person at a terminal, once, which
// checkpoint push policy this clone uses, when the policy is unset and
// checkpoints are waiting to upload. It runs before the policy is read for this
// push, so an answer of manual already holds it. It returns the answer, "not
// now" when nothing was asked, and whether that answer was saved.
func askCheckpointPushPolicyOnFirstPush(ctx context.Context, repo *git.Repository) (checkpointPushQuestionAnswer, bool) {
	if !interactive.CanPromptInteractively() {
		return checkpointPushAnswerNotNow, false
	}
	// An unreadable policy is not asked about; announceCheckpointPushPolicy
	// reports it.
	if _, err := settings.LoadCheckpointPushPolicy(ctx); err != nil {
		return checkpointPushAnswerNotNow, false
	}
	stored, err := settings.CheckpointPushPolicyStored(ctx)
	if err != nil {
		return checkpointPushAnswerNotNow, false
	}
	prefs, err := settings.LoadClonePreferences(ctx)
	if err != nil {
		return checkpointPushAnswerNotNow, false
	}
	now := time.Now()
	prompt := firstPushPromptFor(stored, prefs, now, redact.OPFEnabled())
	if prompt == firstPushPromptNone || waitingCheckpointCount(ctx, repo) == 0 {
		return checkpointPushAnswerNotNow, false
	}

	if prompt == firstPushPromptOPFNotice {
		fmt.Fprintln(stderrWriter, "[entire] "+opfDeprecationNotice)
		if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
			p.OPFDeprecationNoticeShown = true
			return nil
		}); err != nil {
			logging.Warn(ctx, "pre-push: could not record the OPF deprecation notice", slog.String("error", err.Error()))
		}
		return checkpointPushAnswerNotNow, false
	}

	logging.Info(ctx, CheckpointPushQuestionLogMsg)
	answer := askCheckpointPushQuestion()
	if answer == checkpointPushAnswerNotNow {
		return answer, false
	}
	if policy, chosen := checkpointPushQuestionPolicy(answer); chosen {
		saved, err := settings.SaveCheckpointPushPolicyIfUnset(ctx, policy)
		if err != nil {
			logging.Warn(ctx, "pre-push: could not save the checkpoint push policy answer", slog.String("error", err.Error()))
			if answer == checkpointPushAnswerManual {
				fmt.Fprintf(stderrWriter, "[entire] No checkpoints were pushed: could not save the checkpoint push policy: %v\n", err)
			}
			return answer, false
		}
		if !saved {
			// The stored choice wins, so this push follows it, not the answer.
			fmt.Fprintln(stderrWriter, "[entire] A checkpoint push policy was chosen while this question waited; keeping that one.")
			return checkpointPushAnswerSuperseded, false
		}
	}
	if err := settings.ModifyClonePreferences(ctx, func(p *settings.ClonePreferences) error {
		recordCheckpointPushQuestionAnswer(p, answer, now)
		return nil
	}); err != nil {
		logging.Warn(ctx, "pre-push: could not save the checkpoint push question back-off", slog.String("error", err.Error()))
		if answer == checkpointPushAnswerTimedOut {
			return answer, false
		}
	}
	switch answer {
	case checkpointPushAnswerManual:
		// The waiting line that follows names the upload command.
		fmt.Fprintln(stderrWriter, "[entire] Checkpoint push policy: manual.")
	case checkpointPushAnswerAutomatic:
		fmt.Fprintln(stderrWriter, "[entire] Checkpoint push policy: automatic. Change it with: entire configure --checkpoint-push-policy manual")
	case checkpointPushAnswerNotNow, checkpointPushAnswerTimedOut, checkpointPushAnswerSuperseded:
	}
	return answer, true
}

// checkpointPushQuestionPolicy is the policy an answer chooses, if it chooses
// one.
func checkpointPushQuestionPolicy(answer checkpointPushQuestionAnswer) (settings.CheckpointPushPolicy, bool) {
	switch answer {
	case checkpointPushAnswerAutomatic:
		return settings.CheckpointPushPolicyAutomatic, true
	case checkpointPushAnswerManual:
		return settings.CheckpointPushPolicyManual, true
	case checkpointPushAnswerNotNow, checkpointPushAnswerTimedOut, checkpointPushAnswerSuperseded:
	}
	return "", false
}

// askCheckpointPushQuestion shows the question and returns the answer; tests
// replace it because under test the real question reads nothing.
var askCheckpointPushQuestion = askCheckpointPushQuestionTTY

// askCheckpointPushQuestionTTY shows the question on the controlling terminal,
// like askConfirmTTY. Callers check interactive.CanPromptInteractively first.
func askCheckpointPushQuestionTTY() checkpointPushQuestionAnswer {
	if interactive.UnderTest() {
		return checkpointPushAnswerNotNow
	}
	tty, err := interactive.OpenPromptTTY()
	if err != nil {
		return checkpointPushAnswerNotNow
	}
	defer tty.Close()

	fmt.Fprintf(tty, checkpointPushQuestionFormat, checkpointPushQuestionTimeout)
	answer := readCheckpointPushQuestionAnswer(tty, checkpointPushQuestionTimeout)
	if answer == checkpointPushAnswerTimedOut {
		fmt.Fprintf(tty, "\nNo answer within %s, so this push uploads as usual. Entire will ask again in %d days.\n",
			checkpointPushQuestionTimeout, int(checkpointPushQuestionBackoff.Hours()/24))
	}
	return answer
}
