package strategy

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// A prompt nobody answers must not hold the commit hook: the read gives up
// and reports the timeout, so askConfirmTTY falls back to its default.
func TestReadPromptAnswer_GivesUpWhenUnanswered(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })

	start := time.Now()
	_, err := readPromptAnswer(r, 50*time.Millisecond)
	if !errors.Is(err, errPromptTimedOut) {
		t.Fatalf("err = %v, want errPromptTimedOut", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("gave up after %s, want about the timeout", elapsed)
	}
}

func TestReadPromptAnswer_ReturnsAnswer(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	go func() {
		if _, err := io.WriteString(w, "a\n"); err != nil {
			t.Errorf("write answer: %v", err)
		}
	}()

	line, err := readPromptAnswer(r, time.Minute)
	if err != nil {
		t.Fatalf("readPromptAnswer: %v", err)
	}
	if line != "a\n" {
		t.Fatalf("line = %q, want %q", line, "a\n")
	}
}

func answerCheckpointPushQuestion(t *testing.T, input string) checkpointPushQuestionAnswer {
	t.Helper()
	r, w := io.Pipe()
	go func() {
		_, _ = io.WriteString(w, input) //nolint:errcheck // a failed write shows up as the wrong answer
		_ = w.Close()
	}()
	return readCheckpointPushQuestionAnswer(r, time.Minute)
}

func TestReadCheckpointPushQuestionAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input string
		want  checkpointPushQuestionAnswer
	}{
		{"a\n", checkpointPushAnswerAutomatic},
		{" A \n", checkpointPushAnswerAutomatic},
		{"automatic\n", checkpointPushAnswerAutomatic},
		{"m\n", checkpointPushAnswerManual},
		{"M\r\n", checkpointPushAnswerManual},
		{"manual\n", checkpointPushAnswerManual},
		{"n\n", checkpointPushAnswerNotNow},
		{"\n", checkpointPushAnswerNotNow},
		{"yes\n", checkpointPushAnswerNotNow},
		// A closed terminal is not a timeout: it must not start the back-off.
		{"", checkpointPushAnswerNotNow},
	} {
		if got := answerCheckpointPushQuestion(t, tc.input); got != tc.want {
			t.Errorf("answer %q = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestReadCheckpointPushQuestionAnswer_TimesOut(t *testing.T) {
	t.Parallel()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	if got := readCheckpointPushQuestionAnswer(r, 50*time.Millisecond); got != checkpointPushAnswerTimedOut {
		t.Fatalf("unanswered question = %v, want timed out", got)
	}
}

func TestRecordCheckpointPushQuestionAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	staleBackoff := now.Add(-time.Hour).Format(time.RFC3339)

	for _, tc := range []struct {
		answer      checkpointPushQuestionAnswer
		wantBackoff string
	}{
		{checkpointPushAnswerAutomatic, ""},
		{checkpointPushAnswerManual, ""},
		{checkpointPushAnswerNotNow, staleBackoff},
		{checkpointPushAnswerTimedOut, now.Add(7 * 24 * time.Hour).Format(time.RFC3339)},
	} {
		prefs := &settings.ClonePreferences{CheckpointPushQuestionBackoffUntil: staleBackoff}
		recordCheckpointPushQuestionAnswer(prefs, tc.answer, now)
		if prefs.CheckpointPushQuestionBackoffUntil != tc.wantBackoff {
			t.Errorf("%v: back-off = %q, want %q", tc.answer, prefs.CheckpointPushQuestionBackoffUntil, tc.wantBackoff)
		}
	}
}

func TestFirstPushPromptFor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		stored bool
		prefs  settings.ClonePreferences
		opf    bool
		want   firstPushPrompt
	}{
		{name: "policy unset", want: firstPushPromptQuestion},
		{name: "policy chosen", stored: true, want: firstPushPromptNone},
		{name: "backing off", prefs: settings.ClonePreferences{CheckpointPushQuestionBackoffUntil: now.Add(time.Hour).Format(time.RFC3339)}, want: firstPushPromptNone},
		{name: "back-off over", prefs: settings.ClonePreferences{CheckpointPushQuestionBackoffUntil: now.Add(-time.Second).Format(time.RFC3339)}, want: firstPushPromptQuestion},
		// A bad value may only bring the question back, never hide it for good.
		{name: "malformed back-off", prefs: settings.ClonePreferences{CheckpointPushQuestionBackoffUntil: "next tuesday"}, want: firstPushPromptQuestion},
		// A back-off further out than one back-off period was written by a
		// clock that was ahead, or by hand, and must not hide the question.
		{name: "back-off beyond one period", prefs: settings.ClonePreferences{CheckpointPushQuestionBackoffUntil: now.Add(5 * 365 * 24 * time.Hour).Format(time.RFC3339)}, want: firstPushPromptQuestion},
		{name: "OPF user", opf: true, want: firstPushPromptOPFNotice},
		{name: "OPF user while backing off", opf: true, prefs: settings.ClonePreferences{CheckpointPushQuestionBackoffUntil: now.Add(time.Hour).Format(time.RFC3339)}, want: firstPushPromptOPFNotice},
		{name: "OPF notice already shown", opf: true, prefs: settings.ClonePreferences{OPFDeprecationNoticeShown: true}, want: firstPushPromptNone},
		{name: "OPF user with a policy", opf: true, stored: true, want: firstPushPromptNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := firstPushPromptFor(tc.stored, &tc.prefs, now, tc.opf); got != tc.want {
				t.Fatalf("firstPushPromptFor = %v, want %v", got, tc.want)
			}
		})
	}
}

// The back-off a timeout records must keep the question away for the whole
// 7 days and let it back after.
func TestCheckpointPushQuestionBackoffAfterTimeout(t *testing.T) {
	t.Parallel()
	answeredAt := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	prefs := &settings.ClonePreferences{}
	recordCheckpointPushQuestionAnswer(prefs, checkpointPushAnswerTimedOut, answeredAt)

	if got := firstPushPromptFor(false, prefs, answeredAt.Add(7*24*time.Hour-time.Minute), false); got != firstPushPromptNone {
		t.Fatalf("within 7 days: %v, want no question", got)
	}
	if got := firstPushPromptFor(false, prefs, answeredAt.Add(7*24*time.Hour+time.Minute), false); got != firstPushPromptQuestion {
		t.Fatalf("after 7 days: %v, want the question", got)
	}
}
