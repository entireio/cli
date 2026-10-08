package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// repoDeleteFake is the core `repo delete` talks to: the native-mirror
// listing the mirror count reads, the DELETE, and the repo read the wait polls.
type repoDeleteFake struct {
	mirrors         []coreapi.NativeMirrorPlacement
	mirrorsStatus   int // non-zero: the listing answers this problem instead
	deleteStatus    int
	deleteDetail    string // problem detail for a 4xx deleteStatus
	readsBeforeGone int    // 200s the repo read serves before it answers 404
	rejectReads     bool   // a repo read is a test failure (--no-wait)

	mu      sync.Mutex
	deletes []url.Values
	reads   int
	listed  int
}

func (f *repoDeleteFake) handler(t *testing.T) http.Handler {
	t.Helper()
	repoPath := "/api/v1/repos/" + testDeleteULID
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/resolve":
			writeJSONResponse(t, w, http.StatusOK, nativeResolution(testDeletePath, testDeleteULID))
		case r.Method == http.MethodGet && r.URL.Path == repoPath+"/native-mirrors":
			f.listed++
			if f.mirrorsStatus != 0 {
				writeCoreProblem(t, w, f.mirrorsStatus, "listing unavailable")
				return
			}
			writeJSONResponse(t, w, http.StatusOK, &coreapi.ListNativeMirrorsOutputBody{NativeMirrors: f.mirrors})
		case r.Method == http.MethodDelete && r.URL.Path == repoPath:
			f.deletes = append(f.deletes, r.URL.Query())
			if f.deleteStatus >= 400 {
				writeCoreProblem(t, w, f.deleteStatus, f.deleteDetail)
				return
			}
			w.WriteHeader(f.deleteStatus)
		case r.Method == http.MethodGet && r.URL.Path == repoPath:
			if f.rejectReads {
				t.Errorf("unexpected repo read under --no-wait")
			}
			f.reads++
			if f.reads > f.readsBeforeGone {
				writeNotFoundProblem(t, w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":%q,"owningProjectId":"01HZX7QABCDEFGHJKMNPQRSTVX","name":"web","state":"deleting"}`, testDeleteULID)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
}

// testDeletePath is the <project>/<repo> full name the fake resolves to
// testDeleteULID.
const testDeletePath = "acme/web"

func twoMirrors() []coreapi.NativeMirrorPlacement {
	eu := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusReady)
	au := nativeMirrorAt(coreapi.NativeMirrorPlacementStatusSuspended)
	au.ClusterSlug = "aws-ap-southeast-2"
	return []coreapi.NativeMirrorPlacement{eu, au}
}

// TestRepoDelete_Cascade pins the COR-1361 contract on the client side: the
// cascade parameter goes on the wire only under --cascade, a 204 reports a
// finished delete, a 202 is waited out until the repo read answers 404, and
// --no-wait stops after the 202.
//
// Not parallel: swaps the package-level activeCoreClient seam and the poll
// interval.
func TestRepoDelete_Cascade(t *testing.T) {
	prev := mirrorPollInterval
	mirrorPollInterval = time.Millisecond
	t.Cleanup(func() { mirrorPollInterval = prev })

	runWithStderr := func(t *testing.T, fake *repoDeleteFake, args ...string) (string, string, error) {
		t.Helper()
		srv := httptest.NewServer(fake.handler(t))
		t.Cleanup(srv.Close)
		return runCoreCmd(t, newRepoDeleteCmd, srv.URL, append([]string{testDeleteULID, "--force"}, args...)...)
	}
	run := func(t *testing.T, fake *repoDeleteFake, args ...string) (string, error) {
		t.Helper()
		out, _, err := runWithStderr(t, fake, args...)
		return out, err
	}

	t.Run("without --cascade sends no cascade param", func(t *testing.T) {
		fake := &repoDeleteFake{deleteStatus: http.StatusNoContent}
		out, err := run(t, fake)
		require.NoError(t, err)
		require.Len(t, fake.deletes, 1)
		require.False(t, fake.deletes[0].Has("cascade"))
		require.Zero(t, fake.listed, "no mirror count without --cascade")
		require.Contains(t, out, "✓ Deleted repo "+testDeleteULID)
	})

	t.Run("--cascade sends cascade=true and a 204 is a finished delete", func(t *testing.T) {
		fake := &repoDeleteFake{mirrors: twoMirrors(), deleteStatus: http.StatusNoContent}
		out, err := run(t, fake, "--cascade")
		require.NoError(t, err)
		require.Len(t, fake.deletes, 1)
		require.Equal(t, "true", fake.deletes[0].Get("cascade"))
		require.Contains(t, out, "✓ Deleted repo "+testDeleteULID)
		require.NotContains(t, out, "Deleting")
		require.Zero(t, fake.reads, "a 204 needs no wait")
	})

	t.Run("--cascade waits out a 202 until the repo read answers 404", func(t *testing.T) {
		fake := &repoDeleteFake{mirrors: twoMirrors(), deleteStatus: http.StatusAccepted, readsBeforeGone: 2}
		out, err := run(t, fake, "--cascade")
		require.NoError(t, err)
		require.Contains(t, out, "Deleting repo "+testDeleteULID+" and its 2 mirrors…")
		require.Contains(t, out, "✓ Deleted repo "+testDeleteULID)
		require.Equal(t, 3, fake.reads, "two deleting reads, then the 404")
	})

	t.Run("--no-wait returns after the 202", func(t *testing.T) {
		fake := &repoDeleteFake{mirrors: twoMirrors()[:1], deleteStatus: http.StatusAccepted, rejectReads: true}
		out, err := run(t, fake, "--cascade", "--no-wait")
		require.NoError(t, err)
		require.Contains(t, out, "Deleting repo "+testDeleteULID+" and its mirror in the background.")
		require.NotContains(t, out, "✓ Deleted")
	})

	t.Run("mirrors already being removed are not counted", func(t *testing.T) {
		mirrors := twoMirrors()
		mirrors[1].DesiredState = coreapi.NativeMirrorPlacementDesiredStateDeleted
		fake := &repoDeleteFake{mirrors: mirrors, deleteStatus: http.StatusAccepted}
		out, err := run(t, fake, "--cascade")
		require.NoError(t, err)
		require.Contains(t, out, "Deleting repo "+testDeleteULID+" and its mirror…")
	})

	t.Run("a failed mirror count degrades the wording only", func(t *testing.T) {
		fake := &repoDeleteFake{mirrorsStatus: http.StatusNotFound, deleteStatus: http.StatusAccepted}
		out, err := run(t, fake, "--cascade")
		require.NoError(t, err)
		require.Contains(t, out, "Deleting repo "+testDeleteULID+" and any mirrors…")
		require.Contains(t, out, "✓ Deleted repo "+testDeleteULID)
	})

	t.Run("a timeout says the server is still deleting", func(t *testing.T) {
		fake := &repoDeleteFake{mirrors: twoMirrors(), deleteStatus: http.StatusAccepted, readsBeforeGone: 1 << 30}
		_, stderr, err := runWithStderr(t, fake, "--cascade", "--wait-timeout", "20ms")
		require.EqualError(t, err, "stopped waiting after 20ms (--wait-timeout)")
		require.Contains(t, stderr, "The server is still deleting repo "+testDeleteULID+" and its 2 mirrors.")
		require.NotContains(t, stderr, "Check with", "repo view takes only paths; a ULID ref has none to suggest")
	})

	t.Run("a path ref names the repo by path and suggests repo view", func(t *testing.T) {
		fake := &repoDeleteFake{mirrors: twoMirrors(), deleteStatus: http.StatusAccepted, readsBeforeGone: 1 << 30}
		srv := httptest.NewServer(fake.handler(t))
		t.Cleanup(srv.Close)
		// No leading slash: the spelling that used to fall back to `entire api`.
		out, stderr, err := runCoreCmd(t, newRepoDeleteCmd, srv.URL, "et/"+testDeletePath, "--force", "--cascade", "--wait-timeout", "20ms")
		require.Error(t, err)
		require.Contains(t, out, "Deleting repo /et/"+testDeletePath+" and its 2 mirrors…")
		require.Contains(t, stderr, "The server is still deleting repo /et/"+testDeletePath+" and its 2 mirrors.")
		require.Contains(t, stderr, "Check with: entire repo view /et/"+testDeletePath+"\n")
		require.NotContains(t, out+stderr, testDeleteULID, "the ULID stays internal")
	})

	t.Run("an already-gone repo is idempotent under --cascade", func(t *testing.T) {
		fake := &repoDeleteFake{deleteStatus: http.StatusNotFound, deleteDetail: "not found"}
		out, err := run(t, fake, "--cascade")
		require.NoError(t, err)
		require.Contains(t, out, "not found; nothing to delete")
	})

	t.Run("--no-wait requires --cascade", func(t *testing.T) {
		fake := &repoDeleteFake{deleteStatus: http.StatusNoContent}
		_, err := run(t, fake, "--no-wait")
		require.ErrorContains(t, err, "--no-wait requires --cascade")
		require.Empty(t, fake.deletes, "refused before any request")
	})
}

// TestRepoDelete_MirrorConflictHint pins the one refusal the CLI rewords: the
// server's mirrors-exist 409 names --cascade as the remedy, or, when --cascade
// was passed, says the server did not take it. Every other refusal is the
// server's own detail.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoDelete_MirrorConflictHint(t *testing.T) {
	const detail = "delete native mirrors before deleting their primary"

	run := func(t *testing.T, detail string, args ...string) error {
		t.Helper()
		fake := &repoDeleteFake{deleteStatus: http.StatusConflict, deleteDetail: detail}
		srv := httptest.NewServer(fake.handler(t))
		t.Cleanup(srv.Close)
		_, _, err := runCoreCmd(t, newRepoDeleteCmd, srv.URL, append([]string{testDeleteULID, "--force"}, args...)...)
		return err
	}

	t.Run("the mirrors-exist 409 hints at --cascade", func(t *testing.T) {
		err := run(t, detail)
		require.EqualError(t, err, "repo "+testDeleteULID+" has mirrors on other clusters; add --cascade to delete them too")
	})

	t.Run("under --cascade it says the server did not take the cascade", func(t *testing.T) {
		err := run(t, detail, "--cascade")
		require.EqualError(t, err, "repo "+testDeleteULID+" has mirrors on other clusters and the server did not delete them with --cascade; remove them with `entire repo mirror remove`, then delete the repo")
	})

	t.Run("other conflicts keep the server's words", func(t *testing.T) {
		err := run(t, "repo is locked")
		require.EqualError(t, err, "repo is locked")
	})
}

// fakeRepoReader scripts the repo reads awaitRepoDeleted makes, holding the
// last answer once the script runs out.
type fakeRepoReader struct {
	answers []error
	calls   int
}

func (f *fakeRepoReader) GetRepo(context.Context, coreapi.GetRepoParams) (*coreapi.RepoHeaders, error) {
	err := f.answers[min(f.calls, len(f.answers)-1)]
	f.calls++
	if err != nil {
		return nil, err
	}
	return &coreapi.RepoHeaders{}, nil
}

// TestAwaitRepoDeleted pins the wait's three exits: the 404, a run of read
// errors, and the context. A 200 never ends it, whatever state it carries.
//
// Not parallel: shortens the package-level poll interval.
func TestAwaitRepoDeleted(t *testing.T) {
	prev := mirrorPollInterval
	mirrorPollInterval = time.Millisecond
	t.Cleanup(func() { mirrorPollInterval = prev })

	gone := &coreapi.ErrorModelStatusCode{StatusCode: http.StatusNotFound}
	glitch := errors.New("connection reset")

	t.Run("a 200 keeps polling and a 404 ends the wait", func(t *testing.T) {
		reader := &fakeRepoReader{answers: []error{nil, nil, gone}}
		require.NoError(t, awaitRepoDeleted(t.Context(), reader, testDeleteULID))
		require.Equal(t, 3, reader.calls)
	})

	t.Run("transient read errors are tolerated", func(t *testing.T) {
		answers := make([]error, 0, maxConsecutivePollErrors)
		for range maxConsecutivePollErrors - 1 {
			answers = append(answers, glitch)
		}
		reader := &fakeRepoReader{answers: append(answers, gone)}
		require.NoError(t, awaitRepoDeleted(t.Context(), reader, testDeleteULID))
	})

	t.Run("a run of read errors gives up", func(t *testing.T) {
		reader := &fakeRepoReader{answers: []error{glitch}}
		err := awaitRepoDeleted(t.Context(), reader, testDeleteULID)
		require.ErrorIs(t, err, glitch)
		require.True(t, strings.HasPrefix(err.Error(), "poll repository:"), err.Error())
		require.Equal(t, maxConsecutivePollErrors, reader.calls)
	})

	t.Run("a cancelled context is a silent exit", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := awaitRepoDeleted(ctx, &fakeRepoReader{answers: []error{nil}}, testDeleteULID)
		var silent *SilentError
		require.ErrorAs(t, err, &silent)
	})

	t.Run("a deadline reports a timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		err := awaitRepoDeleted(ctx, &fakeRepoReader{answers: []error{nil}}, testDeleteULID)
		require.ErrorContains(t, err, "timed out waiting for the repository to be deleted")
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// TestReportUnfinishedDelete pins the hint every early exit prints.
func TestReportUnfinishedDelete(t *testing.T) {
	t.Parallel()

	t.Run("an interrupt keeps its silent exit", func(t *testing.T) {
		t.Parallel()
		var w strings.Builder
		interrupted := NewSilentError(context.Canceled)
		err := reportUnfinishedDelete(&w, "repo web", "entire repo view /et/acme/web", time.Minute, interrupted)
		require.ErrorIs(t, err, interrupted)
		require.Equal(t, "The server is still deleting repo web.\nCheck with: entire repo view /et/acme/web\n", w.String())
	})

	t.Run("no check line without a command to suggest", func(t *testing.T) {
		t.Parallel()
		var w strings.Builder
		err := reportUnfinishedDelete(&w, "repo web", "", time.Minute, context.DeadlineExceeded)
		require.EqualError(t, err, "stopped waiting after 1m0s (--wait-timeout)")
		require.Equal(t, "The server is still deleting repo web.\n", w.String())
	})

	t.Run("a poll failure keeps its error", func(t *testing.T) {
		t.Parallel()
		glitch := errors.New("poll repository: connection reset")
		err := reportUnfinishedDelete(io.Discard, "repo web", "x", time.Minute, glitch)
		require.ErrorIs(t, err, glitch)
	})
}

func TestRepoCheckCommand(t *testing.T) {
	t.Parallel()
	require.Equal(t, "entire repo view /et/acme/web", repoCheckCommand("/et/acme/web", ""))
	require.Equal(t, "entire repo view /et/acme/web", repoCheckCommand("/et/acme/web", "acme"), "a path wins over the listing")
	require.Equal(t, "entire repo list --project "+testDeleteULID, repoCheckCommand("web", testDeleteULID), "a bare name with no path falls back to the project listing")
	require.Empty(t, repoCheckCommand(testDeleteULID, ""), "repo view takes only paths")
}
