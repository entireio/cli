package githelper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRun_StdinCloseExitsCleanly: when git closes the helper's
// stdin without sending a command, Run must return nil (not error).
// This is the normal shutdown path — git ends the session by
// closing the pipe.
func TestRun_StdinCloseExitsCleanly(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	var out bytes.Buffer
	if err := Run(context.Background(), testTransport(server), ModeConnect, strings.NewReader(""), &out); err != nil {
		t.Fatalf("Run on empty stdin: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output on empty stdin, got %q", out.String())
	}
}

// TestRun_PartialCommandLineErrors: when stdin terminates mid-line
// (no trailing newline), the partial token reaches the dispatcher
// and is rejected as an unknown command. This matches upstream
// transport-helper.c, which dies on unknown helper-protocol
// commands rather than silently ignoring them — a typo'd command
// would otherwise look like a no-op.
func TestRun_PartialCommandLineErrors(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	var out bytes.Buffer
	err := Run(context.Background(), testTransport(server), ModeConnect, strings.NewReader("capab"), &out)
	if err == nil {
		t.Fatal("expected error on partial / unknown command")
	}
	if !strings.Contains(err.Error(), "unsupported command") {
		t.Errorf("error = %v, want unsupported-command shape", err)
	}
}

// TestRun_CtxCancelDuringFetchSurfacesError: a context cancellation
// during a hanging git-upload-pack POST must surface as an error
// from Run — not a silent return that would leave git thinking the
// fetch completed cleanly.
func TestRun_CtxCancelDuringFetchSurfacesError(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			_, _ = w.Write([]byte(serviceAnnouncement(serviceUploadPack, //nolint:errcheck // test
				strings.Repeat("a", 40)+" refs/heads/main\x00multi_ack\n")))
		default:
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
	}))
	defer server.Close()
	defer close(hold)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Connect-mode upload-pack: refs read, then wants + flush + done
	// triggers a POST.
	wantPkt := pktLine("want " + strings.Repeat("a", 40) + " multi_ack\n")
	donePkt := pktLine("done\n")
	stdin := strings.NewReader("connect git-upload-pack\n" + wantPkt + "0000" + donePkt)

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, testTransport(server), ModeConnect, stdin, &out)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("expected error on ctx cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx timeout")
	}
}

// blockingReader returns 0,nil forever until unblock is closed, then
// returns 0,io.EOF. Simulates an SSH/HTTP stdin that's open but
// silent — the same shape git presents while the user thinks before
// the next command.
type blockingReader struct {
	unblock chan struct{}
	once    sync.Once
}

func (b *blockingReader) Read(_ []byte) (int, error) {
	<-b.unblock
	return 0, io.EOF
}

// TestRun_ParentContextCancelStopsBlockedRead: when stdin is a slow
// reader and the parent context cancels, Run currently cannot abort
// stdin reads (they're not ctx-aware), but it must not panic and
// must not write spurious output before exiting. This pins current
// behaviour: ctx cancellation stops new HTTP work, but stdin reads
// only finish when git closes its end. Documenting this so a future
// change that introduces ctx-aware stdin reads has a checkpoint.
func TestRun_ParentContextCancelStopsBlockedRead(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	br := &blockingReader{unblock: make(chan struct{})}
	t.Cleanup(func() { br.once.Do(func() { close(br.unblock) }) })

	ctx, cancel := context.WithCancel(context.Background())

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, testTransport(server), ModeConnect, br, &out)
	}()

	// Give Run time to enter the read; then cancel. Run won't return
	// until the read unblocks (stdin isn't ctx-aware), but it must
	// not have written spurious output.
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if out.Len() != 0 {
		t.Errorf("Run wrote output before stdin returned: %q", out.String())
	}

	// Unblock so the test exits.
	br.once.Do(func() { close(br.unblock) })
	select {
	case err := <-done:
		// EOF on stdin → clean return (nil) is the documented shape.
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("Run after unblock: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after stdin EOF")
	}
}
