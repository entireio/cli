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

		var phases []mirrorCreatePhase
		outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, timeout: time.Second,
			onPhase: func(phase mirrorCreatePhase) { phases = append(phases, phase) },
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.False(t, outcome.created.Created)
		require.True(t, outcome.createdStateUnknown)
		require.Equal(t, coreapi.MirrorStatusReady, outcome.status)
		require.Equal(t, []mirrorCreatePhase{mirrorCreatePhaseQueued, mirrorCreatePhasePlacing, mirrorCreatePhaseCloning}, phases)
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

		outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.False(t, outcome.polled)
		require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath()}, paths)
	})
}

func TestRepoMirrorCreate_BrokenEntireDirUsesDefaultRoute(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		newRepoWithSymlinkedEntireDir(t)
		assertMirrorCreateReachesArgumentValidation(t)
	})

	t.Run("regular file", func(t *testing.T) {
		repoDir := t.TempDir()
		testutil.InitRepo(t, repoDir)
		require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".entire"), []byte("broken"), 0o600))
		t.Chdir(repoDir)
		paths.ClearWorktreeRootCache()
		t.Cleanup(paths.ClearWorktreeRootCache)
		assertMirrorCreateReachesArgumentValidation(t)
	})
}

func assertMirrorCreateReachesArgumentValidation(t *testing.T) {
	t.Helper()
	cmd := newRepoMirrorCreateCmd()
	cmd.SetArgs([]string{"not-a-github-url"})
	require.ErrorContains(t, cmd.Execute(), "not a recognized GitHub URL")
}

func TestCreateAndAwaitMirror_AsyncFailures(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("submission failure does not fall back", func(t *testing.T) {
		var paths []string
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			writeCoreProblem(t, w, http.StatusNotFound, "route unavailable")
		})

		outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, timeout: time.Second,
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

			_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
				async: true, timeout: time.Second,
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

		_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
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

		_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
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

			_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
				async: true, timeout: time.Second,
			})
			require.ErrorContains(t, err, "Location")
		})
	}
}

func TestCreateAndAwaitMirror_AsyncTimeout(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("operation timeout covers submission", func(t *testing.T) {
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(100 * time.Millisecond)
			w.WriteHeader(http.StatusAccepted)
		})

		_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, timeout: 10 * time.Millisecond,
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

		_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, timeout: 10 * time.Millisecond,
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

		_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, timeout: 10 * time.Millisecond,
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

		outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.Equal(t, []string{"Bearer original-token", "Bearer home-token", "Bearer home-token"}, homeAuths)
	})
}

func TestCreateAndAwaitMirror_AsyncResubmission(t *testing.T) {
	useFastMirrorPolling(t)

	t.Run("resubmission after transport failure reuses the placement", func(t *testing.T) {
		submissions := 0
		placements := 0
		client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case mirrorRequestsAPIPath:
				submissions++
				if submissions == 1 {
					placements++
					panic(http.ErrAbortHandler)
				}
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				writeSuccessfulMirrorRequest(t, w)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
		})
		require.Error(t, err)
		outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
		})
		require.NoError(t, err)
		require.Equal(t, "mirror-1", outcome.created.MirrorId)
		require.Equal(t, 1, placements)
		require.Equal(t, 2, submissions)
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
				_, err := createAndAwaitMirror(ctx, client, "owner", "repo", "cluster", mirrorCreateOptions{
					async: true, timeout: time.Second,
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

func TestRepoMirrorCreate_AsyncDefaultWhenSettingsFail(t *testing.T) {
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

	cmd := newRepoCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"mirror", "create", "--no-wait", "github.com/owner/repo", "aws-us-east-2.entire.io"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.Contains(t, stdout.String(), "Mirror placed at entire://cluster/gh/owner/repo")
	require.Contains(t, stdout.String(), "Mirror ID: mirror-1")
	require.NotContains(t, stdout.String(), "Registered mirror")
	require.NotContains(t, stdout.String(), "Mirror exists")
	require.Contains(t, stderr.String(), "Queued mirror owner/repo")
	require.Contains(t, stderr.String(), "Placing mirror owner/repo")
	require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath(), mirrorRequestPath()}, paths)
}

func TestRepoMirrorCreate_SynchronousOptOut(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, `{"async_mirror_requests":false}`)

	var paths []string
	client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodPost || r.URL.Path != mirrorsAPIPath {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSONResponse(t, w, http.StatusCreated, &coreapi.CreatedMirror{
			Created:   true,
			MirrorId:  "mirror-1",
			MirrorUrl: "entire://cluster/gh/owner/repo",
		})
	})
	previousClient := clusterCoreClient
	clusterCoreClient = func(context.Context, string) (*coreapi.Client, error) { return client, nil }
	t.Cleanup(func() { clusterCoreClient = previousClient })

	cmd := newRepoCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"mirror", "create", "--no-wait", "github.com/owner/repo", "aws-us-east-2.entire.io"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	require.Contains(t, stdout.String(), "Registered mirror mirror-1")
	require.Contains(t, stdout.String(), "entire://cluster/gh/owner/repo")
	require.Equal(t, []string{mirrorsAPIPath}, paths)
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
	result := createOneMirror(t.Context(), target, client, nil, mirrorCreateOptions{async: true, timeout: time.Second},
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
		resultsCh <- createMirrors(t.Context(), &bytes.Buffer{}, targets, mirrorCreateOptions{async: true, noWait: true, timeout: time.Second})
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
