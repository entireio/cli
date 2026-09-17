package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

// promptDepsRecorder builds an unattributedPromptDeps whose calls are
// recorded, so tests can assert both what was printed and what was (or was
// not) invoked downstream — declaring an alias is a one-way write, so "did we
// call declare" matters as much as the text on screen.
type promptDepsRecorder struct {
	canPrompt     bool
	confirmResult bool
	confirmErr    error
	declareErr    error
	label         string

	confirmCalls    []string
	declareCalls    []struct{ email, repoID string }
	invalidateCalls int
}

func (r *promptDepsRecorder) deps() unattributedPromptDeps {
	return unattributedPromptDeps{
		canPrompt: func() bool { return r.canPrompt },
		confirm: func(_ context.Context, _ io.Writer, title string) (bool, error) {
			r.confirmCalls = append(r.confirmCalls, title)
			return r.confirmResult, r.confirmErr
		},
		profileLabel: func(_ context.Context) string { return r.label },
		declare: func(_ context.Context, email, repoID string) error {
			r.declareCalls = append(r.declareCalls, struct{ email, repoID string }{email, repoID})
			return r.declareErr
		},
		invalidate: func(_ context.Context) { r.invalidateCalls++ },
	}
}

func TestUnattributedSentence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		author unattributedAuthor
		want   string
	}{
		{
			unattributedAuthor{Email: "me@h.local", Count: 1},
			"1 commit in this repo is authored by me@h.local, which isn't linked to any account.",
		},
		{
			unattributedAuthor{Email: "me@h.local", Count: 9},
			"9 commits in this repo are authored by me@h.local, which isn't linked to any account.",
		},
	}
	for _, tc := range cases {
		if got := unattributedSentence(tc.author); got != tc.want {
			t.Errorf("unattributedSentence(%+v) = %q, want %q", tc.author, got, tc.want)
		}
	}
}

func TestUnattributedAuthorsCheck_NoCandidates_Silent(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	rec := &promptDepsRecorder{}
	runUnattributedAuthorsCheck(context.Background(), &out, detectionOutcome{}, rec.deps())
	if out.String() != "" {
		t.Fatalf("expected no output, got:\n%s", out.String())
	}
}

func TestUnattributedAuthorsCheck_LoggedOut_PointsAtLogin(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{Candidates: []string{"me@h.local"}, LoggedIn: false}
	runUnattributedAuthorsCheck(context.Background(), &out, o, (&promptDepsRecorder{}).deps())
	want := "Unattributed authors: NOT CHECKED (not logged in)\n" +
		"  1 author address here looks like yours (me@h.local).\n" +
		"  Fix: run `entire login`, then `entire doctor` again to check it against Entire.\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestUnattributedAuthorsCheck_LoggedOut_Plural(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{Candidates: []string{"a@h.local", "b@h.local"}, LoggedIn: false}
	runUnattributedAuthorsCheck(context.Background(), &out, o, (&promptDepsRecorder{}).deps())
	want := "Unattributed authors: NOT CHECKED (not logged in)\n" +
		"  2 author addresses here look like yours (a@h.local, b@h.local).\n" +
		"  Fix: run `entire login`, then `entire doctor` again to check them against Entire.\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestUnattributedAuthorsCheck_Skipped_OneLine(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{Candidates: []string{"me@h.local"}, LoggedIn: true, Skipped: "could not reach Entire"}
	runUnattributedAuthorsCheck(context.Background(), &out, o, (&promptDepsRecorder{}).deps())
	want := "Unattributed authors: SKIPPED (could not reach Entire)\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestUnattributedAuthorsCheck_NothingBroken_OK(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{Candidates: []string{"me@h.local"}, LoggedIn: true}
	runUnattributedAuthorsCheck(context.Background(), &out, o, (&promptDepsRecorder{}).deps())
	want := "✓ Unattributed authors: OK\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestUnattributedAuthorsCheck_NonInteractive_ReportsNoPrompt(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"me@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 1}},
	}
	rec := &promptDepsRecorder{canPrompt: false}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	want := "Unattributed authors: UNLINKED\n" +
		"  " + unattributedSentence(o.Authors[0]) + "\n" +
		"  Fix: run `entire doctor` in a terminal to link them.\n"
	if out.String() != want {
		t.Errorf("got:\n%q\nwant:\n%q", out.String(), want)
	}
	if len(rec.declareCalls) != 0 {
		t.Errorf("declare should not be called non-interactively, got %v", rec.declareCalls)
	}
	if len(rec.confirmCalls) != 0 {
		t.Errorf("confirm should not be called non-interactively, got %v", rec.confirmCalls)
	}
}

func TestUnattributedAuthorsCheck_Declined_Skipped(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"me@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 1}},
	}
	rec := &promptDepsRecorder{canPrompt: true, confirmResult: false, label: "you@entire.io"}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	if len(rec.confirmCalls) != 1 {
		t.Fatalf("expected exactly one confirm call, got %v", rec.confirmCalls)
	}
	if len(rec.declareCalls) != 0 {
		t.Errorf("declare should not be called after a decline, got %v", rec.declareCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidate should not be called after a decline, got %d", rec.invalidateCalls)
	}
}

func TestUnattributedAuthorsCheck_ConfirmError_LoggedNotFatal(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"me@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 1}},
	}
	rec := &promptDepsRecorder{canPrompt: true, confirmErr: errors.New("prompt exploded"), label: "you@entire.io"}

	// Must not panic and must not fail the process; runUnattributedAuthorsCheck
	// has no error return at all.
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	if len(rec.declareCalls) != 0 {
		t.Errorf("declare should not be called when the prompt itself errored, got %v", rec.declareCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidate should not be called when the prompt itself errored, got %d", rec.invalidateCalls)
	}
}

func TestUnattributedAuthorsCheck_Confirmed_DeclaresAndInvalidates(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"me@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 9}},
	}
	rec := &promptDepsRecorder{canPrompt: true, confirmResult: true, label: "you@entire.io"}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	if len(rec.declareCalls) != 1 {
		t.Fatalf("expected exactly one declare call, got %v", rec.declareCalls)
	}
	if rec.declareCalls[0].email != "me@h.local" || rec.declareCalls[0].repoID != "01REPO" {
		t.Errorf("declare called with %+v, want {me@h.local 01REPO}", rec.declareCalls[0])
	}
	if rec.invalidateCalls != 1 {
		t.Errorf("expected exactly one invalidate call, got %d", rec.invalidateCalls)
	}
	if !strings.Contains(out.String(), "  ✓ Fixed: linked 9 commits by me@h.local to you@entire.io\n") {
		t.Errorf("output missing success line:\n%s", out.String())
	}
}

func TestUnattributedAuthorsCheck_TwoAddresses_PromptsEach(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"a@h.local", "b@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors: []unattributedAuthor{
			{Email: "a@h.local", Count: 1},
			{Email: "b@h.local", Count: 2},
		},
	}
	rec := &promptDepsRecorder{canPrompt: true, confirmResult: true, label: "you@entire.io"}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	if len(rec.confirmCalls) != 2 {
		t.Fatalf("expected two confirm calls, got %v", rec.confirmCalls)
	}
	if len(rec.declareCalls) != 2 {
		t.Fatalf("expected two declare calls, got %v", rec.declareCalls)
	}
	if rec.declareCalls[0].email != "a@h.local" || rec.declareCalls[1].email != "b@h.local" {
		t.Errorf("declare calls in wrong order/content: %+v", rec.declareCalls)
	}
	if rec.invalidateCalls != 2 {
		t.Errorf("expected two invalidate calls (both authors succeeded), got %d", rec.invalidateCalls)
	}
}

func TestAliasErrKind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want aliasErr
	}{
		{"nil", nil, aliasErrNone},
		{"typed 404", &coreapi.ErrorModelStatusCode{StatusCode: http.StatusNotFound}, aliasErrNotFound},
		{"typed 409", &coreapi.ErrorModelStatusCode{StatusCode: http.StatusConflict}, aliasErrConflict},
		{"typed 400", &coreapi.ErrorModelStatusCode{StatusCode: http.StatusBadRequest}, aliasErrOther},
		{
			// The real text ogen's decoder produces for an unregistered route:
			// decodeDeclareAliasResponse falls through to the "default (code
			// %d)" wrap around validate.InvalidContentTypeError, and
			// sendDeclareAlias wraps that again with "decode response" (see
			// internal/coreapi/oas_response_decoders_gen.go).
			"unregistered route decode failure",
			errors.New("decode response: default (code 404): unexpected Content-Type: text/plain"),
			aliasErrNotAvailable,
		},
		{"plain network error", errors.New("dial tcp"), aliasErrOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := aliasErrKind(tc.err); got != tc.want {
				t.Errorf("aliasErrKind(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestUnattributedAuthorsCheck_DeclareNotAvailable(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"a@h.local", "b@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors: []unattributedAuthor{
			{Email: "a@h.local", Count: 1},
			{Email: "b@h.local", Count: 2},
		},
	}
	rec := &promptDepsRecorder{
		canPrompt:     true,
		confirmResult: true,
		label:         "you@entire.io",
		declareErr:    errors.New("decode response: default (code 404): unexpected Content-Type: text/plain"),
	}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	if !strings.Contains(out.String(), "  Linking isn't available on this Entire yet.\n") {
		t.Errorf("output missing not-available line:\n%s", out.String())
	}
	if len(rec.confirmCalls) != 1 {
		t.Errorf("expected the second author never to be prompted once linking is unavailable, got %v", rec.confirmCalls)
	}
	if len(rec.declareCalls) != 1 {
		t.Errorf("expected only one declare attempt, got %v", rec.declareCalls)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidate should not be called when linking is unavailable, got %d", rec.invalidateCalls)
	}
}

func TestUnattributedAuthorsCheck_DeclareOwnedByOther(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"me@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 1}},
	}
	rec := &promptDepsRecorder{
		canPrompt:     true,
		confirmResult: true,
		label:         "you@entire.io",
		declareErr:    &coreapi.ErrorModelStatusCode{StatusCode: http.StatusConflict},
	}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	if !strings.Contains(out.String(), "  me@h.local is already linked to another account.\n") {
		t.Errorf("output missing conflict line:\n%s", out.String())
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidate should not be called on a conflict, got %d", rec.invalidateCalls)
	}
}

func TestUnattributedAuthorsCheck_DeclareOtherError_UsesAPIError(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"me@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors:    []unattributedAuthor{{Email: "me@h.local", Count: 1}},
	}
	statusErr := &coreapi.ErrorModelStatusCode{
		StatusCode: http.StatusBadRequest,
		Response:   coreapi.ErrorModel{Detail: coreapi.NewOptString("repo not onboarded")},
	}
	rec := &promptDepsRecorder{
		canPrompt:     true,
		confirmResult: true,
		label:         "you@entire.io",
		declareErr:    statusErr,
	}
	runUnattributedAuthorsCheck(context.Background(), &out, o, rec.deps())

	want := "  Could not link me@h.local: " + coreapi.APIError(statusErr) + "\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output missing API-error line:\ngot:  %q\nwant: %q", out.String(), want)
	}
	if rec.invalidateCalls != 0 {
		t.Errorf("invalidate should not be called on a non-conflict declare error, got %d", rec.invalidateCalls)
	}
}

// A conflict on one author must not stop the rest: only that address failed
// to link, the others are independent declarations.
func TestUnattributedAuthorsCheck_ConflictContinuesToNext(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	o := detectionOutcome{
		Candidates: []string{"a@h.local", "b@h.local"},
		LoggedIn:   true,
		RepoID:     "01REPO",
		Authors: []unattributedAuthor{
			{Email: "a@h.local", Count: 1},
			{Email: "b@h.local", Count: 2},
		},
	}
	var confirmCalls []string
	var declareCalls []string
	var invalidateCalls int
	declareCallCount := 0
	deps := unattributedPromptDeps{
		canPrompt: func() bool { return true },
		confirm: func(_ context.Context, _ io.Writer, title string) (bool, error) {
			confirmCalls = append(confirmCalls, title)
			return true, nil
		},
		profileLabel: func(context.Context) string { return "you@entire.io" },
		declare: func(_ context.Context, email, _ string) error {
			declareCalls = append(declareCalls, email)
			declareCallCount++
			if declareCallCount == 1 {
				return &coreapi.ErrorModelStatusCode{StatusCode: http.StatusConflict}
			}
			return nil
		},
		invalidate: func(context.Context) { invalidateCalls++ },
	}
	runUnattributedAuthorsCheck(context.Background(), &out, o, deps)

	if len(confirmCalls) != 2 {
		t.Fatalf("expected two confirm calls, got %v", confirmCalls)
	}
	if len(declareCalls) != 2 {
		t.Fatalf("expected two declare calls, got %v", declareCalls)
	}
	if invalidateCalls != 1 {
		t.Errorf("expected exactly one invalidate call (only the second author linked), got %d", invalidateCalls)
	}
	if !strings.Contains(out.String(), "  a@h.local is already linked to another account.\n") {
		t.Errorf("output missing conflict line for the first author:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "  ✓ Fixed: linked 2 commits by b@h.local to you@entire.io\n") {
		t.Errorf("output missing success line for the second author:\n%s", out.String())
	}
}
