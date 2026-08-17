//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/entireio/cli/internal/procsignal"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

type loginURLActionTestResult struct {
	action loginURLAction
	err    error
}

func TestReadLoginURLActionFromTTY_SingleKeyAndRestoresTerminal(t *testing.T) {
	t.Parallel()

	ptmx, tty, observer, before := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()

	resultCh := make(chan loginURLActionTestResult, 1)
	go func() {
		action, err := readLoginURLActionFromTTY(context.Background(), io.Discard, tty)
		resultCh <- loginURLActionTestResult{action: action, err: err}
	}()
	waitForLoginPromptTTYRaw(t, observer, before)

	// Alt+c, the arrow sequence, and the obsolete o action must be ignored,
	// while Enter opens immediately. Bubble Tea owns decoding and raw-mode
	// restoration.
	if _, err := ptmx.WriteString("\x1bc\x1b[Co\r"); err != nil {
		t.Fatalf("write key to pty: %v", err)
	}

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("readLoginURLActionFromTTY() error = %v", result.err)
		}
		if result.action != loginURLOpen {
			t.Errorf("action = %v, want loginURLOpen", result.action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("single-key input blocked waiting for a newline")
	}

	assertLoginPromptTTYRestored(t, observer, before)
}

func waitForLoginPromptTTYRaw(t *testing.T, tty *os.File, before *term.State) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, err := term.GetState(int(tty.Fd()))
		if err != nil {
			t.Fatalf("read terminal state: %v", err)
		}
		if !reflect.DeepEqual(state, before) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("terminal did not enter raw mode")
}

func TestReadLoginURLActionFromTTY_UnavailableInputContinuesAndCloses(t *testing.T) {
	t.Parallel()

	notTTY, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatalf("create non-TTY input: %v", err)
	}

	action, err := readLoginURLActionFromTTY(context.Background(), io.Discard, notTTY)
	if err != nil {
		t.Fatalf("readLoginURLActionFromTTY() error = %v", err)
	}
	if action != loginURLNone {
		t.Errorf("action = %v, want loginURLNone", action)
	}
	if _, err := notTTY.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("input was not closed after fallback: %v", err)
	}
}

// A successful authentication must cancel and join an active Bubble Tea input
// reader before returning. Waiting until the PTY is observably raw prevents the
// test from passing because authentication won a startup race before Bubble Tea
// took ownership of the terminal.
func TestWaitForLoginURLResult_AuthCompletionCancelsTTYAndRestoresTerminal(t *testing.T) {
	t.Parallel()

	ptmx, tty, observer, before := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()

	authComplete := make(chan struct{})
	interactor := loginURLInteractor{
		keysAvailable: func() bool { return true },
		readAction: func(ctx context.Context) (loginURLAction, error) {
			return readLoginURLActionFromTTY(ctx, io.Discard, tty)
		},
		copyURL: noopCopyURL,
		openURL: noopOpenURL,
	}

	resultCh := make(chan loginURLWaitResult[string], 1)
	go func() {
		value, err := waitForLoginURLResult(
			context.Background(),
			io.Discard,
			io.Discard,
			"https://auth.test/authorize",
			"Waiting... ",
			interactor,
			func(context.Context) (string, error) {
				<-authComplete
				return testLoginComplete, nil
			},
		)
		resultCh <- loginURLWaitResult[string]{value: value, err: err}
	}()

	waitForLoginPromptTTYRaw(t, observer, before)
	close(authComplete)

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("waitForLoginURLResult() error = %v", result.err)
		}
		if result.value != testLoginComplete {
			t.Errorf("result = %q, want %q", result.value, testLoginComplete)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authentication completed but the active TTY reader was not joined")
	}

	assertLoginPromptTTYRestored(t, observer, before)
}

// Ctrl-C is delivered as a keypress while Bubble Tea owns the raw TTY, rather
// than as SIGINT. The reader must record the equivalent process signal so the
// top-level command preserves its normal quiet SIGINT/130 exit semantics.
func TestReadLoginURLActionFromTTY_ControlCRecordsInterrupt(t *testing.T) {
	procsignal.Reset()
	t.Cleanup(procsignal.Reset)

	ptmx, tty, observer, before := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()

	resultCh := make(chan loginURLActionTestResult, 1)
	go func() {
		action, err := readLoginURLActionFromTTY(context.Background(), io.Discard, tty)
		resultCh <- loginURLActionTestResult{action: action, err: err}
	}()
	waitForLoginPromptTTYRaw(t, observer, before)

	if _, err := ptmx.Write([]byte{3}); err != nil {
		t.Fatalf("write Ctrl-C to pty: %v", err)
	}

	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("readLoginURLActionFromTTY() error = %v, want context.Canceled", result.err)
		}
		if result.action != loginURLNone {
			t.Errorf("action = %v, want loginURLNone", result.action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ctrl-C did not stop the login URL reader")
	}

	if got := procsignal.Load(); got != os.Interrupt {
		t.Errorf("procsignal.Load() = %v, want SIGINT", got)
	}
	assertLoginPromptTTYRestored(t, observer, before)
}

// TestReadLoginURLActionFromTTY_CancelledLeavesDescriptorOpen pins the one
// exception to this function owning the descriptor it is handed.
//
// When the program is killed by context cancellation, Bubble Tea's
// shutdown(kill=true) skips waitForReadLoop(), so its input reader goroutine can
// outlive Run. Closing the descriptor there races that reader (on Linux
// cancelreader's epoll wait loop reads tty.Fd()) and frees an fd number the live
// reader still holds, so the cancelled path must leave the descriptor alone.
func TestReadLoginURLActionFromTTY_CancelledLeavesDescriptorOpen(t *testing.T) {
	t.Parallel()

	ptmx, tty, observer, _ := openLoginPromptPTY(t)
	defer ptmx.Close()
	defer observer.Close()
	// Ownership stays with us on this path, so this close is the real one.
	defer tty.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	action, err := readLoginURLActionFromTTY(ctx, io.Discard, tty)
	if action != loginURLNone {
		t.Errorf("action = %v, want loginURLNone", action)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}

	// A closed *os.File reports Fd() as -1, so this fails if the descriptor was
	// reclaimed while a killed reader could still be holding it.
	if _, err := term.GetState(int(tty.Fd())); err != nil {
		t.Errorf("descriptor was closed on the cancelled path: %v", err)
	}
}

func openLoginPromptPTY(t *testing.T) (ptmx, tty, observer *os.File, before *term.State) {
	t.Helper()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("cannot open a pty here: %v", err)
	}

	observerFD, err := unix.Dup(int(tty.Fd()))
	if err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		t.Fatalf("duplicate tty descriptor: %v", err)
	}
	observer = os.NewFile(uintptr(observerFD), "login-prompt-tty-observer")

	before, err = term.GetState(int(observer.Fd()))
	if err != nil {
		_ = ptmx.Close()
		_ = tty.Close()
		_ = observer.Close()
		t.Fatalf("read initial terminal state: %v", err)
	}

	return ptmx, tty, observer, before
}

func assertLoginPromptTTYRestored(t *testing.T, tty *os.File, before *term.State) {
	t.Helper()

	after, err := term.GetState(int(tty.Fd()))
	if err != nil {
		t.Fatalf("read restored terminal state: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Error("terminal state was not restored after reading a login action")
	}
}
