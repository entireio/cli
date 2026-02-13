package cli

import (
	"errors"
	"fmt"
	"testing"
)

func TestSilentError(t *testing.T) {
	t.Parallel()

	t.Run("wraps error message", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("something went wrong")
		silent := NewSilentError(inner)

		if silent.Error() != "something went wrong" {
			t.Errorf("expected %q, got %q", "something went wrong", silent.Error())
		}
	})

	t.Run("unwraps to inner error", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("inner error")
		silent := NewSilentError(inner)

		if !errors.Is(silent, inner) {
			t.Error("expected Unwrap to return inner error")
		}
	})

	t.Run("detectable with errors.As", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("test")
		silent := NewSilentError(inner)

		var target *SilentError
		if !errors.As(silent, &target) {
			t.Error("expected errors.As to find SilentError")
		}
	})
}

func TestExitCodeError(t *testing.T) {
	t.Parallel()

	t.Run("wraps error message", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("command failed")
		exitErr := NewExitCodeError(inner, 2)

		if exitErr.Error() != "command failed" {
			t.Errorf("expected %q, got %q", "command failed", exitErr.Error())
		}
	})

	t.Run("stores exit code", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("test")
		exitErr := NewExitCodeError(inner, 42)

		if exitErr.ExitCode != 42 {
			t.Errorf("expected exit code 42, got %d", exitErr.ExitCode)
		}
	})

	t.Run("unwraps to inner error", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("inner error")
		exitErr := NewExitCodeError(inner, 1)

		if !errors.Is(exitErr, inner) {
			t.Error("expected Unwrap to return inner error")
		}
	})

	t.Run("detectable with errors.As", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("test")
		exitErr := NewExitCodeError(inner, 3)

		var target *ExitCodeError
		if !errors.As(exitErr, &target) {
			t.Error("expected errors.As to find ExitCodeError")
		}

		if target.ExitCode != 3 {
			t.Errorf("expected exit code 3, got %d", target.ExitCode)
		}
	})

	t.Run("detectable when wrapped by SilentError", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("already printed")
		exitErr := NewExitCodeError(inner, 5)
		silent := NewSilentError(exitErr)

		var target *ExitCodeError
		if !errors.As(silent, &target) {
			t.Error("expected errors.As to find ExitCodeError through SilentError")
		}

		if target.ExitCode != 5 {
			t.Errorf("expected exit code 5, got %d", target.ExitCode)
		}
	})

	t.Run("detectable when wrapping SilentError", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("already printed")
		silent := NewSilentError(inner)
		exitErr := NewExitCodeError(silent, 7)

		var silentTarget *SilentError
		if !errors.As(exitErr, &silentTarget) {
			t.Error("expected errors.As to find SilentError through ExitCodeError")
		}

		var exitTarget *ExitCodeError
		if !errors.As(exitErr, &exitTarget) {
			t.Error("expected errors.As to find ExitCodeError")
		}

		if exitTarget.ExitCode != 7 {
			t.Errorf("expected exit code 7, got %d", exitTarget.ExitCode)
		}
	})

	t.Run("works with fmt.Errorf wrapping", func(t *testing.T) {
		t.Parallel()

		inner := errors.New("root cause")
		exitErr := NewExitCodeError(inner, 128)
		wrapped := fmt.Errorf("command failed: %w", exitErr)

		var target *ExitCodeError
		if !errors.As(wrapped, &target) {
			t.Error("expected errors.As to find ExitCodeError through fmt.Errorf wrapping")
		}

		if target.ExitCode != 128 {
			t.Errorf("expected exit code 128, got %d", target.ExitCode)
		}
	})
}
