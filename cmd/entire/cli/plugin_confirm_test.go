package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPluginDependencyConfirmationUsesWriter(t *testing.T) { //nolint:paralleltest // isolates terminal opener and accessibility
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv("ENTIRE_TEST_TTY", "1")
	original := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("y\n"))}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = original })
	var stderr bytes.Buffer
	ok, err := confirmPluginAction(t.Context(), &stderr, "Install them now?", false)
	if err != nil || !ok {
		t.Fatalf("answer=%v err=%v", ok, err)
	}
	if !strings.Contains(stderr.String(), "Install them now? [y/N]") {
		t.Fatalf("missing prompt on stderr: %q", stderr.String())
	}
}

func TestPluginAccessibleConfirmationCancellation(t *testing.T) { //nolint:paralleltest // isolates terminal opener and accessibility
	for _, fallback := range []bool{false, true} {
		name := "pollable input"
		if fallback {
			name = "fallback input"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("ACCESSIBLE", "1")
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			tracked := &pluginPromptCloseTracker{ReadCloser: input}
			original := openPluginPromptTerminal
			openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
				if fallback {
					return pluginPromptTerminal{in: tracked}, nil
				}
				return pluginPromptTerminal{in: input}, nil
			}
			t.Cleanup(func() { openPluginPromptTerminal = original })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready := make(chan struct{}, 1)
			result := make(chan error, 1)
			go func() {
				_, promptErr := runPluginConfirm(ctx, pluginPromptNotifyWriter{ready}, "Install?", true)
				result <- promptErr
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("prompt did not start")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v, want cancellation", err)
				}
			case <-time.After(5 * time.Second):
				_ = writer.Close()
				<-result
				t.Fatal("accessible prompt did not stop on cancellation")
			}
			if fallback && tracked.closes != 1 {
				t.Fatalf("input closed %d times, want exactly once", tracked.closes)
			}
		})
	}
}

type pluginPromptNotifyWriter struct{ ready chan<- struct{} }

func (w pluginPromptNotifyWriter) Write(p []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}

// Hiding the file descriptor forces cancelreader's non-pollable fallback:
// Cancel returns false, so closing the input must unblock the prompt.
type pluginPromptCloseTracker struct {
	io.ReadCloser

	closes int
}

func (r *pluginPromptCloseTracker) Close() error {
	r.closes++
	return r.ReadCloser.Close()
}

// The answer comes from the terminal, so the question has to appear there.
// A writer that is not a terminal — `entire graph 2>log`, or a wrapper
// capturing stderr — rendered the prompt into the redirect: nothing reached
// the terminal, which sat in raw mode waiting for a keypress, and with Yes as
// the default an idle Enter authorized a download-and-exec nobody was shown.
func TestPluginConfirmationRendersOnTerminalWhenWriterIsRedirected(t *testing.T) { //nolint:paralleltest // isolates terminal opener and accessibility
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv("ENTIRE_TEST_TTY", "1")
	var terminal bytes.Buffer
	original := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("y\n")), out: &terminal}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = original })

	var redirected bytes.Buffer // stands in for a redirected stderr
	ok, err := runPluginConfirm(t.Context(), &redirected, "Install the entire-graph plugin?", true)
	if err != nil || !ok {
		t.Fatalf("answer=%v err=%v", ok, err)
	}
	if !strings.Contains(terminal.String(), "Install the entire-graph plugin?") {
		t.Errorf("prompt did not reach the terminal: %q", terminal.String())
	}
	if redirected.Len() != 0 {
		t.Errorf("prompt leaked into the redirected writer: %q", redirected.String())
	}
}

// The other half of the same rule: a writer that IS a terminal is what the
// prompt renders to, so the caller keeps deciding where its own output goes.
// Covered for real terminals by TestPluginConfirmationRedirectedInput; here
// the check is that a non-terminal writer with no terminal handle to fall
// back to is still used rather than dropped.
func TestPluginConfirmationUsesSuppliedWriterWithoutATerminalHandle(t *testing.T) { //nolint:paralleltest // isolates terminal opener and accessibility
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv("ENTIRE_TEST_TTY", "1")
	original := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("y\n"))}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = original })

	var supplied bytes.Buffer
	if _, err := runPluginConfirm(t.Context(), &supplied, "Install?", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(supplied.String(), "Install?") {
		t.Errorf("prompt did not reach the supplied writer: %q", supplied.String())
	}
}
