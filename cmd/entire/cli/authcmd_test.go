package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestRenderDataAPIAuthError_DistinguishesCallerContextFromWrappedDeadline
// guards against a regression where any error chain merely satisfying
// errors.Is(err, context.DeadlineExceeded) was treated as the caller's own
// context firing. resolveRepoCellTarget runs under its own internal
// context.WithTimeout, so its timeout error still satisfies that errors.Is
// check even when the caller's context is perfectly live — silencing on that
// basis alone would print nothing for a slow-but-reachable control plane
// (worse than the bug the fail-loud change was meant to fix).
func TestRenderDataAPIAuthError_DistinguishesCallerContextFromWrappedDeadline(t *testing.T) {
	t.Parallel()

	wrappedDeadline := fmt.Errorf("resolve the Entire cell for acme/widget: %w", context.DeadlineExceeded)

	t.Run("live caller context with wrapped DeadlineExceeded is printed, not swallowed", func(t *testing.T) {
		t.Parallel()

		var errW bytes.Buffer
		result := renderDataAPIAuthError(context.Background(), &errW, "acme/widget", wrappedDeadline)

		var silent *SilentError
		if errors.As(result, &silent) {
			t.Fatalf("expected a non-silent error for a live caller context, got silent: %v", result)
		}
		if !errors.Is(result, context.DeadlineExceeded) {
			t.Fatalf("expected returned error to still wrap context.DeadlineExceeded, got %v", result)
		}
		// Only the not-logged-in and not-onboarded branches print; a plain
		// resolution failure is returned for main.go to render.
		if errW.Len() != 0 {
			t.Fatalf("expected nothing written to stderr, got %q", errW.String())
		}
	})

	t.Run("actually cancelled caller context stays silent", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// Pair the cancelled context with a Canceled-wrapping error: that is
		// what the real path produces, since the in-flight call that observed
		// the cancellation is what builds this error.
		wrappedCanceled := fmt.Errorf("resolve the Entire cell for acme/widget: %w", context.Canceled)

		var errW bytes.Buffer
		result := renderDataAPIAuthError(ctx, &errW, "acme/widget", wrappedCanceled)

		var silent *SilentError
		if !errors.As(result, &silent) {
			t.Fatalf("expected a SilentError when the caller's own context is cancelled, got %v", result)
		}
		if errW.Len() != 0 {
			t.Fatalf("expected a silent error to print nothing, got %q", errW.String())
		}
	})
}

// The not-onboarded chain is a stack of internal resolution steps, so the render
// boundary replaces it with one actionable line instead of printing it.
func TestRenderDataAPIAuthError_NotOnboardedPrintsOneActionableLine(t *testing.T) {
	t.Parallel()

	// The shape resolveRepoCellPlacement actually returns: the sentinel wrapped
	// by the caller's context, which is what errors.Is has to see through.
	err := fmt.Errorf("resolve processing placement for acme/widget: %w", errRepoNotOnboarded)

	var errW bytes.Buffer
	result := renderDataAPIAuthError(context.Background(), &errW, "acme/widget", err)

	var silent *SilentError
	if !errors.As(result, &silent) {
		t.Fatalf("expected a SilentError so main.go does not also print the raw chain, got %v", result)
	}
	out := errW.String()
	if !strings.Contains(out, "not onboarded to Entire") {
		t.Errorf("stderr = %q, want it to say the repo is not onboarded", out)
	}
	if !strings.Contains(out, "entire repo mirror create") {
		t.Errorf("stderr = %q, want it to name the command that onboards the repo", out)
	}
	// The whole point is that the internal resolution chain does not reach the
	// user; printing it alongside the clean line would defeat the change.
	if strings.Contains(out, "resolve processing placement") {
		t.Errorf("stderr = %q, want the internal resolution chain replaced, not appended", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("stderr = %q, want exactly one line", out)
	}
}

// The rendered line must name the repo the failed call was SCOPED to, not the
// current clone: `--repo gh/other/thing` reaches this path from an onboarded
// repo, and because the branch returns a SilentError the raw chain that did
// name it never prints, so this line is the only place it can appear.
func TestRenderRepoNotOnboarded_NamesTheScopedRepo(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("resolve processing placement for other/thing: %w", errRepoNotOnboarded)

	var errW bytes.Buffer
	if result := renderRepoNotOnboarded(&errW, "other/thing", err); result == nil {
		t.Fatal("expected the sentinel to be rendered")
	}
	out := errW.String()
	if !strings.Contains(out, "other/thing") {
		t.Errorf("stderr = %q, want it to name the repo the call was scoped to", out)
	}
	if strings.Contains(out, "This repository") {
		t.Errorf("stderr = %q, want the named repo instead of a deictic that points at the wrong one", out)
	}
	if strings.Count(out, "other/thing") != 1 {
		t.Errorf("stderr = %q, want the repo named exactly once", out)
	}
}

// errRepoNotOnboarded covers three shapes and only one is really "not
// onboarded" — zero rows also means the caller cannot see the repo, and a row
// with no processing primary can be mid-onboarding. The line must not assert a
// missing mirror as the only explanation.
func TestRenderRepoNotOnboarded_DoesNotOverclaim(t *testing.T) {
	t.Parallel()

	var errW bytes.Buffer
	if rendered := renderRepoNotOnboarded(&errW, "acme/widget", fmt.Errorf("wrapped: %w", errRepoNotOnboarded)); rendered == nil {
		t.Fatal("expected the sentinel to be rendered")
	}

	if out := errW.String(); !strings.Contains(out, "not visible to your login") {
		t.Errorf("stderr = %q, want it to allow for the access/visibility shape too", out)
	}
}

// Only the sentinel is rendered; every other error passes through so callers
// can chain this as a guard ahead of their own wrapping.
func TestRenderRepoNotOnboarded_IgnoresOtherErrors(t *testing.T) {
	t.Parallel()

	var errW bytes.Buffer
	if result := renderRepoNotOnboarded(&errW, "acme/widget", errors.New("boom")); result != nil {
		t.Fatalf("expected nil for a non-sentinel error, got %v", result)
	}
	if errW.Len() != 0 {
		t.Errorf("stderr = %q, want nothing written for a non-sentinel error", errW.String())
	}
}

// Fallback wording for the callers with no repo to name (the generic data-API
// gate, and experts' ULID form).
func TestRenderRepoNotOnboarded_FallsBackWithoutARepo(t *testing.T) {
	t.Parallel()

	var errW bytes.Buffer
	if rendered := renderRepoNotOnboarded(&errW, "  ", fmt.Errorf("wrapped: %w", errRepoNotOnboarded)); rendered == nil {
		t.Fatal("expected the sentinel to be rendered")
	}

	if out := errW.String(); !strings.Contains(out, "This repository") {
		t.Errorf("stderr = %q, want the generic subject when no repo is known", out)
	}
}
