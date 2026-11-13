package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// errLoginPermanent marks a login failure that another attempt cannot fix.
var errLoginPermanent = errors.New("login failure is not retryable")

// permanentError carries its cause's message unchanged and matches
// errLoginPermanent, so marking an error does not alter what is printed.
type permanentError struct{ err error }

func (e permanentError) Error() string        { return e.err.Error() }
func (e permanentError) Unwrap() error        { return e.err }
func (e permanentError) Is(target error) bool { return target == errLoginPermanent }

// permanent marks err as one another login attempt cannot fix.
func permanent(err error) error { return permanentError{err: err} }

// retryPolicy configures retryLogin.
type retryPolicy struct {
	attempts       int
	attemptTimeout time.Duration
	delay          time.Duration // wait before every attempt after the first
	runDir         string        // receives login-attempt-<n>.txt
	log            io.Writer
}

// loginAttemptFunc runs one login. On failure it may return a redacted
// description of the page it gave up on.
type loginAttemptFunc func(ctx context.Context) (diagnosis string, err error)

// retryLogin runs login until it succeeds, fails permanently, or uses up
// p.attempts. Before every retry it waits p.delay and then calls
// beforeRetry. Every failure is logged, and one that comes with a diagnosis
// also leaves it in p.runDir/login-attempt-<n>.txt.
func retryLogin(ctx context.Context, p retryPolicy, login loginAttemptFunc, beforeRetry func(context.Context)) error {
	var errs []error
	for attempt := 1; attempt <= p.attempts; attempt++ {
		if attempt > 1 {
			timer := time.NewTimer(p.delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(append(errs, ctx.Err())...)
			case <-timer.C:
			}
			beforeRetry(ctx)
		}

		attemptCtx, cancel := context.WithTimeout(ctx, p.attemptTimeout)
		diagnosis, err := login(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		err = fmt.Errorf("attempt %d/%d: %w", attempt, p.attempts, err)
		errs = append(errs, err)
		retryable := !errors.Is(err, errLoginPermanent)
		suffix := ""
		if !retryable {
			suffix = " (not retried)"
		}
		fmt.Fprintf(p.log, "control-plane e2e: login %v%s\n", err, suffix)
		if diagnosis != "" {
			fmt.Fprintf(p.log, "control-plane e2e: page at failure:\n%s\n", diagnosis)
			path := filepath.Join(p.runDir, fmt.Sprintf("login-attempt-%d.txt", attempt))
			if writeErr := os.WriteFile(path, []byte(err.Error()+"\n\n"+diagnosis), 0o644); writeErr != nil {
				fmt.Fprintf(p.log, "control-plane e2e: write %s: %v\n", path, writeErr)
			}
		}
		if !retryable {
			break
		}
	}
	return errors.Join(errs...)
}

// truncateText cuts s to at most limit bytes on a rune boundary, marking the
// cut. Truncate only text that is already redacted: a cut can split a secret
// into a fragment the redactor no longer recognizes.
func truncateText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n[truncated]"
}
