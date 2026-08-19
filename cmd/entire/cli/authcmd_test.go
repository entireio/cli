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
		result := renderDataAPIAuthError(context.Background(), &errW, wrappedDeadline)

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
		result := renderDataAPIAuthError(ctx, &errW, wrappedCanceled)

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
	result := renderDataAPIAuthError(context.Background(), &errW, err)

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
