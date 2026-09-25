package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/coreapi"
)

const mirrorRequestsAPIPath = "/api/v1/mirror-requests"

var testMirrorRequestID = uuid.MustParse("67b477f3-97b7-4dfe-90c4-6365dbebd5bf")

func TestCreateAndAwaitMirror_AsyncSuccess(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("placement and clone succeed", func(t *testing.T) {
		var paths []string
		requestPolls := 0
		mirrorPolls := 0
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch {
			case r.Method == http.MethodPost && r.URL.Path == mirrorRequestsAPIPath:
				writeAcceptedMirrorRequest(t, w)
			case r.Method == http.MethodGet && r.URL.Path == mirrorRequestPath():
				requestPolls++
				if requestPolls == 1 {
					writeJSONResponse(t, w, http.StatusOK, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusProcessing})
					return
				}
				writeSuccessfulMirrorRequest(t, w)
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/mirrors/mirror-1":
				mirrorPolls++
				if mirrorPolls == 1 {
					writeCoreProblem(t, w, http.StatusNotFound, "mirror not found")
					return
				}
				writeJSONResponse(t, w, http.StatusOK, &coreapi.Mirror{Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)})
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		})

		var phases []mirrorAddPhase
		outcome, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			timeout: time.Second,
			onPhase: func(phase mirrorAddPhase) { phases = append(phases, phase) },
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.Equal(t, coreapi.MirrorStatusReady, outcome.status)
		require.Equal(t, []mirrorAddPhase{mirrorAddPhaseQueued, mirrorAddPhasePlacing, mirrorAddPhaseCloning}, phases)
		require.Equal(t, []string{
			mirrorRequestsAPIPath,
			mirrorRequestPath(),
			mirrorRequestPath(),
			"/api/v1/mirrors/mirror-1",
			"/api/v1/mirrors/mirror-1",
		}, paths)
	})

	t.Run("no-wait stops after placement", func(t *testing.T) {
		var paths []string
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			switch r.URL.Path {
			case mirrorRequestsAPIPath:
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				writeSuccessfulMirrorRequest(t, w)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		outcome, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.False(t, outcome.polled)
		require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath()}, paths)
	})
}

func TestRepoMirrorAdd_BrokenEntireDirUsesDefaultRoute(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		newRepoWithSymlinkedEntireDir(t)
		assertMirrorAddReachesArgumentValidation(t)
	})

	t.Run("regular file", func(t *testing.T) {
		repoDir := t.TempDir()
		testutil.InitRepo(t, repoDir)
		require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".entire"), []byte("broken"), 0o600))
		t.Chdir(repoDir)
		paths.ClearWorktreeRootCache()
		t.Cleanup(paths.ClearWorktreeRootCache)
		assertMirrorAddReachesArgumentValidation(t)
	})
}

func assertMirrorAddReachesArgumentValidation(t *testing.T) {
	t.Helper()
	cmd := newRepoMirrorAddCmd()
	cmd.SetArgs([]string{"not-a-github-url"})
	require.ErrorContains(t, cmd.Execute(), "invalid <repo>")
}

func TestCreateAndAwaitMirror_AsyncFailures(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("submission failure does not fall back", func(t *testing.T) {
		var paths []string
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			writeCoreProblem(t, w, http.StatusNotFound, "route unavailable")
		})

		outcome, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			timeout: time.Second,
		})
		require.Error(t, err)
		require.Nil(t, outcome.created)
		require.Equal(t, []string{mirrorRequestsAPIPath}, paths)
	})

	for _, tt := range []struct {
		name       string
		code       string
		message    string
		retryable  bool
		want       string
		wantDetail string
		wantRetry  bool
	}{
		{name: "known", code: "repo_inaccessible", message: "repository is not accessible", want: "repository is not accessible"},
		{name: "retryable", code: "github_unavailable", message: "GitHub is unavailable", retryable: true, want: "github_unavailable", wantRetry: true},
		{name: "unknown", code: "future_failure", message: "future detail", want: "unknown failure code", wantDetail: "future detail"},
	} {
		t.Run("terminal failure "+tt.name, func(t *testing.T) {
			client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case mirrorRequestsAPIPath:
					writeAcceptedMirrorRequest(t, w)
				case mirrorRequestPath():
					request := coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusFailed}
					request.Failure = coreapi.NewOptMirrorRequestFailure(coreapi.MirrorRequestFailure{
						Code: tt.code, Message: tt.message, Retryable: tt.retryable,
					})
					writeJSONResponse(t, w, http.StatusOK, &request)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})

			_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
				timeout: time.Second,
			})
			require.ErrorContains(t, err, tt.want)
			if tt.wantDetail != "" {
				require.ErrorContains(t, err, tt.wantDetail)
			}
			if tt.wantRetry {
				require.ErrorContains(t, err, "retry this command")
			} else {
				require.NotContains(t, err.Error(), "retry this command")
			}
		})
	}

	t.Run("transient request poll failures are retried", func(t *testing.T) {
		polls := 0
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case mirrorRequestsAPIPath:
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				polls++
				if polls < 3 {
					writeCoreProblem(t, w, http.StatusServiceUnavailable, "try again")
					return
				}
				writeSuccessfulMirrorRequest(t, w)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, 3, polls)
	})

	t.Run("persistent request poll failures stop at the cap", func(t *testing.T) {
		polls := 0
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == mirrorRequestsAPIPath {
				writeAcceptedMirrorRequest(t, w)
				return
			}
			polls++
			writeCoreProblem(t, w, http.StatusServiceUnavailable, "still unavailable")
		})

		_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			noWait: true, timeout: time.Second,
		})
		require.ErrorContains(t, err, "poll mirror request")
		require.Equal(t, maxConsecutivePollErrors, polls)
	})
}

func TestCreateAndAwaitMirror_AsyncLocationValidation(t *testing.T) {
	useFastMirrorPolling(t)

	for _, location := range []string{"", ":", "/api/v1/mirrors/not-a-request", "/api/v1/mirror-requests/not-a-uuid", mirrorRequestPath() + "?extra=true"} {
		t.Run("invalid Location "+location, func(t *testing.T) {
			client := newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if location != "" {
					w.Header().Set("Location", location)
				}
				writeJSONResponse(t, w, http.StatusAccepted, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusPending})
			})

			_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
				timeout: time.Second,
			})
			require.ErrorContains(t, err, "Location")
		})
	}
}

// pollPhaseTimeout is the deadline for the subtests that assert WHICH phase a
// timeout lands in when that phase is a poll loop. It has to outlast one real
// httptest round trip — the submission, which must NOT time out — while the
// loop below it spins on the 1ms mirrorPollInterval and consumes whatever is
// left. At 10ms a loaded `-race` run could spend the whole budget on the
// submission and report the wrong phase, which is a flake in the assertion
// rather than in the code.
const pollPhaseTimeout = 250 * time.Millisecond

func TestCreateAndAwaitMirror_AsyncTimeout(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("operation timeout covers submission", func(t *testing.T) {
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusAccepted)
		})

		_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			timeout: 10 * time.Millisecond,
		})
		require.ErrorContains(t, err, "timed out submitting mirror request")
		require.NotContains(t, err.Error(), "waiting for mirror placement")
	})

	t.Run("operation timeout covers clone polling", func(t *testing.T) {
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == mirrorRequestsAPIPath:
				w.Header().Set("Location", mirrorRequestPath())
				writeSuccessfulMirrorRequestWithStatus(t, w, http.StatusAccepted)
			case strings.HasPrefix(r.URL.Path, "/api/v1/mirrors/"):
				writeJSONResponse(t, w, http.StatusOK, &coreapi.Mirror{Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusProcessing)})
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			timeout: pollPhaseTimeout,
		})
		require.ErrorContains(t, err, "timed out waiting for initial clone")
	})

	t.Run("operation timeout stops placement polling", func(t *testing.T) {
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case mirrorRequestsAPIPath:
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				writeJSONResponse(t, w, http.StatusOK, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusProcessing})
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			timeout: pollPhaseTimeout,
		})
		require.ErrorContains(t, err, "timed out waiting for mirror placement")
	})
}

func TestCreateAndAwaitMirror_AsyncCrossJurisdiction(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("cross-jurisdiction submission and polling use the home core", func(t *testing.T) {
		var homeAuths []string
		homeCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/oauth/token":
				writeJSONResponse(t, w, http.StatusOK, map[string]any{
					"access_token": "home-token", "expires_in": 300, "token_type": "Bearer",
				})
			case mirrorRequestsAPIPath:
				homeAuths = append(homeAuths, r.Header.Get("Authorization"))
				if r.Header.Get("Authorization") != "Bearer home-token" {
					writeCoreProblem(t, w, http.StatusUnauthorized, "invalid token")
					return
				}
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				homeAuths = append(homeAuths, r.Header.Get("Authorization"))
				writeSuccessfulMirrorRequest(t, w)
			default:
				t.Errorf("unexpected home-core request %s %s", r.Method, r.URL.Path)
			}
		}))
		t.Cleanup(homeCore.Close)

		wrongCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/entire-federation" {
				writeJSONResponse(t, w, http.StatusOK, map[string]any{"peer_auth_hosts": []string{homeCore.URL}})
				return
			}
			w.WriteHeader(http.StatusMisdirectedRequest)
			if _, err := fmt.Fprintf(w, `{"home_core_url":%q}`, homeCore.URL); err != nil {
				t.Errorf("write 421 response: %v", err)
			}
		}))
		t.Cleanup(wrongCore.Close)
		client, err := coreapi.NewWithBearer(wrongCore.URL, "original-token")
		require.NoError(t, err)

		outcome, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.Equal(t, []string{"Bearer original-token", "Bearer home-token", "Bearer home-token"}, homeAuths)
	})
}

func TestCreateAndAwaitMirror_AsyncResubmission(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("resubmission after transport failure reuses the placement", func(t *testing.T) {
		// Atomic, not plain ints: httptest serves each request on its own
		// goroutine, and the aborted first request's handler can still be
		// unwinding when the resubmission's handler runs, so two handler
		// goroutines touch these counters. The race detector flagged exactly
		// that in CI.
		var submissions, placements atomic.Int32
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case mirrorRequestsAPIPath:
				if submissions.Add(1) == 1 {
					placements.Add(1)
					panic(http.ErrAbortHandler)
				}
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				writeSuccessfulMirrorRequest(t, w)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		_, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			noWait: true, timeout: time.Second,
		})
		require.Error(t, err)
		outcome, err := addAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorAddOptions{
			noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.Equal(t, int32(1), placements.Load())
		require.Equal(t, int32(2), submissions.Load())
	})
}

func TestCreateAndAwaitMirror_AsyncCancellation(t *testing.T) {
	useFastMirrorPolling(t)

	for _, phase := range []string{"placement", "clone"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			pollStarted := make(chan struct{})
			client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == mirrorRequestsAPIPath:
					w.Header().Set("Location", mirrorRequestPath())
					if phase == "clone" {
						writeSuccessfulMirrorRequestWithStatus(t, w, http.StatusAccepted)
					} else {
						writeJSONResponse(t, w, http.StatusAccepted, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusPending})
					}
				case phase == "placement" && r.URL.Path == mirrorRequestPath():
					close(pollStarted)
					<-r.Context().Done()
				case phase == "clone" && strings.HasPrefix(r.URL.Path, "/api/v1/mirrors/"):
					close(pollStarted)
					<-r.Context().Done()
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})

			result := make(chan error, 1)
			go func() {
				_, err := addAndAwaitMirror(ctx, client, "owner", "repo", "cluster", mirrorAddOptions{
					timeout: time.Second,
				})
				result <- err
			}()
			<-pollStarted
			cancel()
			err := <-result
			var silent *SilentError
			require.ErrorAs(t, err, &silent)
		})
	}
}

func TestRepoMirrorAdd_AsyncDefaultWhenSettingsFail(t *testing.T) {
	useFastMirrorPolling(t)

	setupTestRepo(t)
	writeSettings(t, `{`)

	requestPolls := 0
	var paths []string
	client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case mirrorRequestsAPIPath:
			writeAcceptedMirrorRequest(t, w)
		case mirrorRequestPath():
			requestPolls++
			if requestPolls == 1 {
				writeJSONResponse(t, w, http.StatusOK, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusProcessing})
				return
			}
			writeSuccessfulMirrorRequest(t, w)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	previousClient := clusterCoreClient
	clusterCoreClient = func(context.Context, string) (*coreapi.Client, error) { return client, nil }
	t.Cleanup(func() { clusterCoreClient = previousClient })
	// --cluster names a cluster host; the catalog is still read, because the
	// native-mirror API is keyed by slug and one lookup serves both forges.
	serveClusters(t, testClusterCatalog)

	cmd := newRepoCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"mirror", "add", "--no-wait", "--cluster", defaultClusterHost, "/gh/owner/repo"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	// A one-shot add reports through the same summary table as the wizard, so
	// one repo on three clusters reads like three repos on three clusters.
	require.Contains(t, stdout.String(), "/gh/owner/repo")
	require.Contains(t, stdout.String(), "aws-us-east-2")
	require.Contains(t, stdout.String(), mirrorStatusRegistered)
	require.Contains(t, stdout.String(), "entire://cluster/gh/owner/repo")
	require.NotContains(t, stdout.String(), mirrorStatusReady, "--no-wait does not wait for the clone")
	require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath(), mirrorRequestPath()}, paths)
	_ = stderr
}

func TestCreateOneMirror_AsyncProgress(t *testing.T) {
	useFastMirrorPolling(t)

	requestPolls := 0
	client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == mirrorRequestsAPIPath:
			writeAcceptedMirrorRequest(t, w)
		case r.URL.Path == mirrorRequestPath():
			requestPolls++
			if requestPolls == 1 {
				writeJSONResponse(t, w, http.StatusOK, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusProcessing})
				return
			}
			writeSuccessfulMirrorRequest(t, w)
		case strings.HasPrefix(r.URL.Path, "/api/v1/mirrors/"):
			writeJSONResponse(t, w, http.StatusOK, &coreapi.Mirror{Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	var progress []string
	target := mirrorTarget{owner: "owner", repo: "repo", region: regionChoice{host: "cluster"}}
	result := createOneMirror(t.Context(), target, client, nil, mirrorAddOptions{timeout: time.Second},
		func(status string, _ bool, _ bool) { progress = append(progress, status) })
	require.NoError(t, result.err)
	require.Equal(t, mirrorStatusReady, result.status)
	require.Equal(t, "entire://cluster/gh/owner/repo", result.cloneURL)
	require.Equal(t, []string{"queued", "placing", "cloning", "ready"}, progress)
}

func TestCreateMirrors_AsyncKeepsConcurrencyAndFailuresIndependent(t *testing.T) {
	var active atomic.Int32
	var maxActive atomic.Int32
	var started atomic.Int32
	reachedLimit := make(chan struct{})
	release := make(chan struct{})
	client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repo string `json:"repo"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maxActive.Load()
			if current <= previous || maxActive.CompareAndSwap(previous, current) {
				break
			}
		}
		if started.Add(1) == mirrorCreateConcurrency {
			close(reachedLimit)
		}
		<-release
		if body.Repo == "fails" {
			writeCoreProblem(t, w, http.StatusBadRequest, "repository rejected")
			return
		}
		w.Header().Set("Location", mirrorRequestPath())
		writeSuccessfulMirrorRequestWithStatus(t, w, http.StatusAccepted)
	})
	previousClient := clusterCoreClient
	clusterCoreClient = func(_ context.Context, host string) (*coreapi.Client, error) {
		if host == "bad-region" {
			return nil, errors.New("region unavailable")
		}
		return client, nil
	}
	t.Cleanup(func() { clusterCoreClient = previousClient })

	targets := make([]mirrorTarget, 0, mirrorCreateConcurrency+4)
	for i := range mirrorCreateConcurrency + 2 {
		targets = append(targets, mirrorTarget{owner: "owner", repo: fmt.Sprintf("repo-%d", i), region: regionChoice{host: "good-region"}})
	}
	targets = append(targets,
		mirrorTarget{owner: "owner", repo: "fails", region: regionChoice{host: "good-region"}},
		mirrorTarget{owner: "owner", repo: "region-fails", region: regionChoice{host: "bad-region"}},
	)

	resultsCh := make(chan []mirrorResult, 1)
	go func() {
		resultsCh <- createMirrors(t.Context(), &bytes.Buffer{}, targets, mirrorAddOptions{noWait: true, timeout: time.Second})
	}()
	<-reachedLimit
	time.Sleep(20 * time.Millisecond)
	observedMax := maxActive.Load()
	close(release)
	results := <-resultsCh

	require.Equal(t, int32(mirrorCreateConcurrency), observedMax)
	require.Len(t, results, len(targets))
	for i, result := range results {
		switch targets[i].repo {
		case "fails", "region-fails":
			require.Error(t, result.err, targets[i].repo)
		case "":
			t.Fatal("empty fixture repo")
		default:
			require.NoError(t, result.err, targets[i].repo)
			require.Equal(t, mirrorStatusRegistered, result.status, targets[i].repo)
		}
	}
	require.Equal(t, int32(mirrorCreateConcurrency+3), started.Load())
}

func mirrorRequestPath() string {
	return mirrorRequestsAPIPath + "/" + testMirrorRequestID.String()
}

func useFastMirrorPolling(t *testing.T) {
	t.Helper()
	previousInterval := mirrorPollInterval
	mirrorPollInterval = time.Millisecond
	t.Cleanup(func() { mirrorPollInterval = previousInterval })
}

func newMirrorRequestClient(t *testing.T, handler http.HandlerFunc) *coreapi.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client, err := coreapi.NewWithBearer(srv.URL, "token")
	require.NoError(t, err)
	return client
}

func writeAcceptedMirrorRequest(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Location", mirrorRequestPath())
	writeJSONResponse(t, w, http.StatusAccepted, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusPending})
}

func writeSuccessfulMirrorRequest(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	writeSuccessfulMirrorRequestWithStatus(t, w, http.StatusOK)
}

func writeSuccessfulMirrorRequestWithStatus(t *testing.T, w http.ResponseWriter, status int) {
	t.Helper()
	request := coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusSucceeded}
	request.Result = coreapi.NewOptMirrorRequestResult(coreapi.MirrorRequestResult{
		MirrorId: "mirror-1", MirrorUrl: "entire://cluster/gh/owner/repo", PublicUrl: "https://cluster/gh/owner/repo",
	})
	writeJSONResponse(t, w, status, &request)
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := printJSON(w, value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func writeCoreProblem(t *testing.T, w http.ResponseWriter, status int, detail string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if _, err := fmt.Fprintf(w, `{"title":"request failed","detail":%q,"status":%d}`, detail, status); err != nil {
		t.Errorf("write problem response: %v", err)
	}
}
