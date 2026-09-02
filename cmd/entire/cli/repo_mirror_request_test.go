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
			case mirrorStatusAPIPath:
				writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
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
		// --no-wait still reads the mirror's status once: the placement
		// response cannot say whether an existing placement is suspended, and
		// reporting a suspended mirror as a plain success is what sends a
		// script on to a clone that then fails.
		require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath(), mirrorStatusAPIPath}, paths)
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
			case mirrorStatusAPIPath:
				writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
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

// TestCreateAndAwaitMirror_AsyncIgnoresLocation pins that the poll is driven by
// the 202 body's requestId, not the Location header. Location is optional in
// the spec, so a create whose body already identifies the request must not fail
// on a missing or unparseable header — and the header's host must never become
// a poll target, since re-pointing the client's base URL at a server-named
// origin would send the control-plane bearer there.
func TestCreateAndAwaitMirror_AsyncIgnoresLocation(t *testing.T) {
	useFastMirrorPolling(t)

	locations := []string{
		"",
		":",
		"/api/v1/mirrors/not-a-request",
		"/api/v1/mirror-requests/not-a-uuid",
		mirrorRequestPath() + "?extra=true",
		"http://evil.example/api/v1/mirror-requests/" + testMirrorRequestID.String(),
		"/some-prefix/api/v1/mirror-requests/" + testMirrorRequestID.String(),
	}
	for _, location := range locations {
		t.Run("Location "+location, func(t *testing.T) {
			var hosts []string
			client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
				hosts = append(hosts, r.Host)
				switch r.URL.Path {
				case mirrorRequestsAPIPath:
					if location != "" {
						w.Header().Set("Location", location)
					}
					writeJSONResponse(t, w, http.StatusAccepted, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusPending})
				case mirrorRequestPath():
					writeSuccessfulMirrorRequest(t, w)
				case mirrorStatusAPIPath:
					writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			})

			outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
				async: true, noWait: true, timeout: time.Second,
			})
			require.NoError(t, err)
			require.Equal(t, "mirror-1", outcome.created.MirrorId)
			// Every request stayed on the client's own base URL — nothing was
			// re-targeted at the host the Location named.
			require.NotEmpty(t, hosts)
			for _, host := range hosts {
				require.NotEqual(t, "evil.example", host)
			}
		})
	}
}

// TestCreateAndAwaitMirror_AsyncMissingRequestID pins the one thing the body
// genuinely must carry: without a request id there is nothing to poll.
func TestCreateAndAwaitMirror_AsyncMissingRequestID(t *testing.T) {
	useFastMirrorPolling(t)

	client := newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusAccepted, &coreapi.MirrorRequest{Status: coreapi.MirrorRequestStatusPending})
	})

	_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
		async: true, timeout: time.Second,
	})
	require.ErrorContains(t, err, "missing a request id")
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
			case mirrorStatusAPIPath:
				homeAuths = append(homeAuths, r.Header.Get("Authorization"))
				writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
			default:
				t.Errorf("unexpected home-core request %s %s", r.Method, r.URL.Path)
			}
		}))
		t.Cleanup(homeCore.Close)

		var redirected []string
		wrongCore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/entire-federation" {
				writeJSONResponse(t, w, http.StatusOK, map[string]any{"peer_auth_hosts": []string{homeCore.URL}})
				return
			}
			redirected = append(redirected, r.URL.Path)
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
		require.Equal(t, []string{"Bearer original-token", "Bearer home-token", "Bearer home-token", "Bearer home-token"}, homeAuths)
		// Every call — submission, placement poll, status read — went out to
		// the client's configured core and was redirected there. This is the
		// regression guard: short-circuiting the poll straight at the home
		// core (via WithServerURL from the Location header) strips the
		// afterRedirect provenance the transport's bare-401 re-exchange needs,
		// so the wait breaks unrecoverably once the exchanged token leaves the
		// cache. Polls must keep arriving here.
		require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath(), mirrorStatusAPIPath}, redirected)
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
			case mirrorStatusAPIPath:
				writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
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
		case mirrorStatusAPIPath:
			writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
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
	// One spinner for the whole create, so exactly one completion line — three
	// ✓ lines would be three success claims for one operation, and would mark
	// a phase successful merely because the next one superseded it.
	require.Equal(t, 1, strings.Count(stderr.String(), "✓"), "stderr: %q", stderr.String())
	require.Contains(t, stderr.String(), "mirror owner/repo into aws-us-east-2.entire.io")
	require.Equal(t, []string{mirrorRequestsAPIPath, mirrorRequestPath(), mirrorRequestPath(), mirrorStatusAPIPath}, paths)
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
		// The status read applyAsyncSuspension makes carries no body, so route
		// it out before decoding one.
		if r.URL.Path == mirrorStatusAPIPath {
			writeMirrorStatus(t, w, coreapi.MirrorStatusReady)
			return
		}
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

// mirrorStatusAPIPath is the status route every async create now reads once,
// so applyAsyncSuspension can fill in CreatedMirror.Suspended (the placement
// response carries no such field).
const mirrorStatusAPIPath = "/api/v1/mirrors/mirror-1"

func writeMirrorStatus(t *testing.T, w http.ResponseWriter, status coreapi.MirrorStatus) {
	t.Helper()
	writeJSONResponse(t, w, http.StatusOK, &coreapi.Mirror{Status: coreapi.NewOptMirrorStatus(status)})
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

// TestCreateAndAwaitMirror_AsyncSuspendedPlacement pins that the async route
// reports a suspended placement the same way the synchronous one does.
// MirrorRequestResult has no `suspended` field, so without a status read the
// async create returns Suspended=false and every downstream branch treats an
// unusable mirror as a success — exiting 0 from `create --no-wait`, which sends
// a script chaining `&& git clone` on to a clone that then fails.
func TestCreateAndAwaitMirror_AsyncSuspendedPlacement(t *testing.T) {
	useFastMirrorPolling(t)

	newSuspendedClient := func(t *testing.T) *coreapi.Client {
		return newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case mirrorRequestsAPIPath:
				writeAcceptedMirrorRequest(t, w)
			case mirrorRequestPath():
				writeSuccessfulMirrorRequest(t, w)
			case mirrorStatusAPIPath:
				writeMirrorStatus(t, w, coreapi.MirrorStatusSuspended)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
	}

	t.Run("no-wait surfaces the suspension and exits non-zero", func(t *testing.T) {
		outcome, err := createAndAwaitMirror(t.Context(), newSuspendedClient(t), "owner", "repo", "cluster", mirrorCreateOptions{
			async: true, noWait: true, timeout: time.Second,
		})
		require.NoError(t, err, "a suspended re-create is a non-fatal create")
		require.True(t, outcome.created.Suspended)

		var stdout, stderr bytes.Buffer
		reportErr := reportOneShotMirror(&stdout, &stderr, outcome, err)
		require.ErrorIs(t, reportErr, errMirrorSuspended)
		require.Contains(t, stderr.String(), "suspended by an admin")
		require.NotContains(t, stdout.String(), "will work once it completes")
	})

	t.Run("the wizard classifies it as suspended", func(t *testing.T) {
		target := mirrorTarget{owner: "owner", repo: "repo", region: regionChoice{host: "cluster"}}
		result := createOneMirror(t.Context(), target, newSuspendedClient(t), nil,
			mirrorCreateOptions{async: true, noWait: true, timeout: time.Second}, nil)
		require.Equal(t, mirrorStatusSuspended, result.status)
		require.Error(t, result.err)
	})
}

// TestCreateAndAwaitMirror_AsyncTimeoutKeepsRequestID pins that a placement
// that outlives --wait-timeout still hands back the request id. It is the only
// handle on a placement that may still be progressing server-side, and there is
// no `mirror request get` subcommand to look one up after the fact, so dropping
// it leaves the user with nothing but a blind resubmit.
func TestCreateAndAwaitMirror_AsyncTimeoutKeepsRequestID(t *testing.T) {
	useFastMirrorPolling(t)

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

	outcome, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
		async: true, timeout: 20 * time.Millisecond,
	})
	require.ErrorContains(t, err, "timed out waiting for mirror placement")
	require.Nil(t, outcome.created)
	require.Equal(t, testMirrorRequestID, outcome.requestID)

	var stdout, stderr bytes.Buffer
	require.Error(t, reportOneShotMirror(&stdout, &stderr, outcome, err))
	require.Contains(t, stderr.String(), testMirrorRequestID.String())
	require.Contains(t, stderr.String(), "idempotent")
}

// TestCreateOneMirror_AsyncPlacementTimeoutIsTimedOut pins that the wizard
// renders a placement timeout as "timed out", not "error". Both halves of the
// single --wait-timeout describe the same user-visible condition, so which side
// of the placement/clone boundary it expired on must not change the label.
func TestCreateOneMirror_AsyncPlacementTimeoutIsTimedOut(t *testing.T) {
	useFastMirrorPolling(t)

	client := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mirrorRequestsAPIPath {
			writeAcceptedMirrorRequest(t, w)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, &coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusProcessing})
	})

	target := mirrorTarget{owner: "owner", repo: "repo", region: regionChoice{host: "cluster"}}
	result := createOneMirror(t.Context(), target, client, nil,
		mirrorCreateOptions{async: true, timeout: 20 * time.Millisecond}, nil)
	require.Equal(t, mirrorStatusTimedOut, result.status)
	require.ErrorIs(t, result.err, context.DeadlineExceeded)
}

// TestCreateAndAwaitMirror_SyncTimeoutCoversCreate pins that --wait-timeout
// bounds the synchronous route's create call too. It previously wrapped only
// the clone poll, so a CreateMirror that hung ignored the flag entirely — and
// the flag's help now promises it covers mirror creation.
func TestCreateAndAwaitMirror_SyncTimeoutCoversCreate(t *testing.T) {
	client := newMirrorRequestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	start := time.Now()
	_, err := createAndAwaitMirror(t.Context(), client, "owner", "repo", "cluster", mirrorCreateOptions{
		timeout: 20 * time.Millisecond,
	})
	require.ErrorContains(t, err, "timed out registering the mirror")
	require.Less(t, time.Since(start), 150*time.Millisecond)
}

// TestResolveAsyncMirrorRequests pins the env var's precedence over repo
// settings, in both directions. `repo mirror create` names a repo the caller
// has usually not cloned, so a switch readable only from the cwd's
// .entire/settings.json has no effect where the command is most used; and
// because that file is version-controlled, a repo must not be able to pin the
// route for everyone standing in it with no way out.
func TestResolveAsyncMirrorRequests(t *testing.T) {
	for _, tt := range []struct {
		name     string
		settings string
		env      string
		want     bool
	}{
		{name: "unset and unspecified is the default", settings: `{}`, want: true},
		{name: "unset honours an explicit opt-in", settings: `{"async_mirror_requests":true}`, want: true},
		{name: "unset honours an explicit opt-out", settings: `{"async_mirror_requests":false}`},
		{name: "env enables over an opt-out", settings: `{"async_mirror_requests":false}`, env: "1", want: true},
		{name: "env true enables over an opt-out", settings: `{"async_mirror_requests":false}`, env: asyncEnvWordOn, want: true},
		{name: "env disables over the default", settings: `{}`, env: "0"},
		{name: "env false disables over an explicit opt-in", settings: `{"async_mirror_requests":true}`, env: asyncEnvWordOff},
	} {
		t.Run(tt.name, func(t *testing.T) {
			setupTestRepo(t)
			writeSettings(t, tt.settings)
			t.Setenv(asyncMirrorRequestsEnv, tt.env)

			got, err := resolveAsyncMirrorRequests(t.Context())
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	// The command is routinely run from outside any repository, so that must
	// not become a warning on every invocation.
	t.Run("no repository and no env is a clean default", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv(asyncMirrorRequestsEnv, "")
		got, err := resolveAsyncMirrorRequests(t.Context())
		require.NoError(t, err)
		require.True(t, got)
	})

	// A settings read that failed says nothing about whether the repo opted
	// out, so the default must stand — the caller reports the error, but
	// nobody gets silently downgraded onto the synchronous route by a broken
	// .entire.
	t.Run("a settings error keeps the default route", func(t *testing.T) {
		setupTestRepo(t)
		writeSettings(t, `{"async_mirror_requests":`)
		t.Setenv(asyncMirrorRequestsEnv, "")
		got, err := resolveAsyncMirrorRequests(t.Context())
		require.Error(t, err)
		require.True(t, got)
	})

	t.Run("env wins with no repository at all", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv(asyncMirrorRequestsEnv, "1")
		got, err := resolveAsyncMirrorRequests(t.Context())
		require.NoError(t, err)
		require.True(t, got, "the switch must work outside a repo, which is where this command is normally run")
	})
}

// TestReportOneShotMirror_TerminalFailureIsNotCalledInFlight pins that the
// "may still be progressing" notice is scoped to waits that ended without a
// verdict. A terminal placement failure carries a request id too, and pairing
// e.g. repo_inaccessible with "re-run it to pick it up" sends the user in
// circles over work the server has already finished with.
func TestReportOneShotMirror_TerminalFailureIsNotCalledInFlight(t *testing.T) {
	t.Parallel()

	failed := coreapi.MirrorRequest{RequestId: testMirrorRequestID, Status: coreapi.MirrorRequestStatusFailed}
	failed.Failure = coreapi.NewOptMirrorRequestFailure(coreapi.MirrorRequestFailure{
		Code: "repo_inaccessible", Message: "the upstream is not reachable",
	})
	outcome := mirrorCreateOutcome{requestID: testMirrorRequestID, createdStateUnknown: true}

	var stdout, stderr bytes.Buffer
	err := reportOneShotMirror(&stdout, &stderr, outcome, mirrorRequestFailureError(failed))
	require.ErrorContains(t, err, "repo_inaccessible")
	require.NotContains(t, stderr.String(), "may still be progressing")
	require.NotContains(t, stderr.String(), "Re-run")

	// A wait that ended with no verdict still names the request.
	var timedOutOut, timedOutErr bytes.Buffer
	require.Error(t, reportOneShotMirror(&timedOutOut, &timedOutErr, outcome,
		classifyWaitContextErr(context.DeadlineExceeded, "waiting for mirror placement")))
	require.Contains(t, timedOutErr.String(), testMirrorRequestID.String())
	require.Contains(t, timedOutErr.String(), "may still be progressing")
}
