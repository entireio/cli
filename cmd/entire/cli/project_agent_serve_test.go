package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const agentServeRunID = "01JABCDEFGHJKMNPQRSTVWXYZ0"

func agentServeClaim() projectAgentClaim {
	return projectAgentClaim{projectAgentJob: projectAgentJob{RunID: agentServeRunID, ProjectID: "project", Harness: "claude"}, GitRemoteURL: "entire://example.com/et/acme/repo", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Prompt: "review", OutputSchema: json.RawMessage(`{"type":"object"}`), DeadlineAt: time.Now().Add(time.Minute)}
}

func testAgentWorker(t *testing.T, h http.HandlerFunc) *projectAgentWorker {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return &projectAgentWorker{target: &projectAgentTarget{client: api.NewClientWithBaseURL("test", s.URL), projectID: "project"}, out: io.Discard, save: func(*projectAgentSubmission) error { return nil }, wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
}

func TestProjectAgentServePagination(t *testing.T) {
	t.Parallel()
	var paths []string
	w := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		out.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			writeAgentTestResponse(t, out, `{"items":[{"project_id":"other","harness":"claude"},{"project_id":"project","harness":"codex"}],"next_cursor":"next"}`)
		} else {
			writeAgentTestResponse(t, out, `{"items":[{"run_id":"`+agentServeRunID+`","project_id":"project","harness":"claude","state":"claimed"}]}`)
		}
	})
	job, err := w.scanJobs().next(t.Context())
	require.NoError(t, err)
	require.Equal(t, agentServeRunID, job.RunID)
	require.Equal(t, "claimed", job.State)
	require.Len(t, paths, 2)
	require.Contains(t, paths[1], "cursor=next")
}

func TestProjectAgentServeRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		fail, expired, saveFail bool
	}{
		{name: "complete"}, {name: "execution failure", fail: true}, {name: "expired", expired: true}, {name: "journal failure", saveFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			claim := agentServeClaim()
			if tc.expired {
				claim.DeadlineAt = time.Now().Add(-time.Minute)
			}
			var mu sync.Mutex
			var paths []string
			saved := false
			w := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				paths = append(paths, r.URL.Path)
				out.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/claim") {
					assert.NoError(t, json.NewEncoder(out).Encode(claim))
					return
				}
				assert.True(t, saved, "must durably save before publishing")
				var body map[string]json.RawMessage
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				if tc.fail || tc.expired {
					assert.Contains(t, body, "error")
					assert.NotContains(t, string(body["error"]), "secret")
				} else {
					assert.JSONEq(t, `{"findings":[]}`, string(body["result"]))
				}
				writeAgentTestResponse(t, out, `{}`)
			})
			w.save = func(s *projectAgentSubmission) error {
				if tc.saveFail {
					return errors.New("disk full")
				}
				mu.Lock()
				defer mu.Unlock()
				saved = s != nil
				return nil
			}
			calls := 0
			w.execute = func(ctx context.Context, _ projectAgentClaim) (json.RawMessage, error) {
				calls++
				if tc.expired {
					require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
					return nil, ctx.Err()
				}
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.False(t, deadline.After(claim.DeadlineAt.Add(-agentDeadlineReserve)))
				if tc.fail {
					return nil, errors.New("secret failure")
				}
				return json.RawMessage(`{"findings":[]}`), nil
			}
			processed, err := w.run(t.Context(), claim.projectAgentJob)
			require.True(t, processed)
			require.Equal(t, 1, calls)
			if tc.fail || tc.expired || tc.saveFail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.saveFail {
				require.Len(t, paths, 1)
			} else {
				require.Len(t, paths, 2)
				require.False(t, saved)
				action := "complete"
				if tc.fail || tc.expired {
					action = "fail"
				}
				require.True(t, strings.HasSuffix(paths[1], "/"+action))
			}
		})
	}
}

func TestProjectAgentServeRetriesExactSubmission(t *testing.T) {
	t.Parallel()
	calls := 0
	var bodies []string
	w := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
		calls++
		b, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		bodies = append(bodies, string(b))
		if calls < 3 {
			out.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeAgentTestResponse(t, out, `{}`)
	})
	cleared := false
	w.save = func(s *projectAgentSubmission) error { cleared = s == nil; return nil }
	p := projectAgentSubmission{RunID: agentServeRunID, Action: "complete", Body: json.RawMessage(`{"result":{"findings":[]},"harness":"claude"}`)}
	require.NoError(t, w.submit(t.Context(), p))
	require.True(t, cleared)
	require.Len(t, bodies, 3)
	require.Equal(t, bodies[0], bodies[1])
	require.Equal(t, bodies[0], bodies[2])
}

func TestProjectAgentServeRetainsUncertainSubmission(t *testing.T) {
	t.Parallel()
	for _, status := range []int{422, 503, 401} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			w := testAgentWorker(t, func(out http.ResponseWriter, _ *http.Request) { out.WriteHeader(status) })
			w.save = func(*projectAgentSubmission) error { t.Error("must not clear uncertain/refused result"); return nil }
			require.ErrorContains(t, w.submit(t.Context(), projectAgentSubmission{RunID: agentServeRunID, Action: "complete", Body: json.RawMessage(`{}`)}), "payload retained")
		})
	}
}

func TestProjectAgentServeClaimConflictDoesNotExecute(t *testing.T) {
	t.Parallel()
	w := testAgentWorker(t, func(out http.ResponseWriter, _ *http.Request) { out.WriteHeader(http.StatusConflict) })
	w.execute = func(context.Context, projectAgentClaim) (json.RawMessage, error) {
		t.Error("must not execute")
		return nil, nil
	}
	processed, err := w.run(t.Context(), agentServeClaim().projectAgentJob)
	require.NoError(t, err)
	require.False(t, processed)
}

func TestProjectAgentServeOnceEmpty(t *testing.T) {
	t.Parallel()
	w := testAgentWorker(t, func(out http.ResponseWriter, _ *http.Request) { writeAgentTestResponse(t, out, `{"items":[]}`) })
	w.wait = func(context.Context, time.Duration) error { t.Error("once must not wait when empty"); return nil }
	require.NoError(t, w.serve(t.Context(), true, time.Second))
}

func TestProjectAgentServeExit(t *testing.T) {
	t.Parallel()
	job := `{"items":[{"run_id":"` + agentServeRunID + `","project_id":"project","harness":"claude"}]}`
	for _, tc := range []struct {
		name        string
		stopInClaim bool
		submitFails bool
		wantStopped bool
	}{
		{name: "stop while polling", wantStopped: true},
		{name: "stop during run keeps execution error", stopInClaim: true},
		{name: "unsent submission keeps error", stopInClaim: true, submitFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
				out.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet:
					writeAgentTestResponse(t, out, job)
				case strings.HasSuffix(r.URL.Path, "/claim"):
					assert.NoError(t, json.NewEncoder(out).Encode(agentServeClaim()))
				case tc.submitFails:
					out.WriteHeader(http.StatusUnprocessableEntity)
				default:
					writeAgentTestResponse(t, out, `{}`)
				}
			})
			w.execute = func(ctx context.Context, _ projectAgentClaim) (json.RawMessage, error) {
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			}
			if !tc.stopInClaim {
				w.wait = func(context.Context, time.Duration) error {
					cancel()
					return fmt.Errorf("worker stopped: %w", context.Canceled)
				}
				w.execute = func(context.Context, projectAgentClaim) (json.RawMessage, error) {
					return json.RawMessage(`{}`), nil
				}
			}
			var out strings.Builder
			err := projectAgentServeExit(ctx, &out, w.serve(ctx, tc.stopInClaim, time.Second))
			if tc.wantStopped {
				require.NoError(t, err)
				require.Contains(t, out.String(), "Worker stopped.")
				return
			}
			require.Error(t, err)
			require.NotContains(t, out.String(), "Worker stopped.")
		})
	}
}

func TestProjectAgentRetryClassification(t *testing.T) {
	t.Parallel()
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "https://example.com", Err: err} }
	refused := errors.New("ENTIRE_TOKEN service account secret refused by core: HTTP 401")
	for _, tc := range []struct {
		err   error
		retry bool
	}{
		{wrap(fmt.Errorf("resolve bearer: %w", refused)), false},
		{refused, false},
		{wrap(io.EOF), true},
		{fmt.Errorf("read response body: %w", io.ErrUnexpectedEOF), true},
		{wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), true},
		{&api.HTTPError{StatusCode: http.StatusServiceUnavailable}, true},
		{&api.HTTPError{StatusCode: http.StatusForbidden}, false},
	} {
		require.Equal(t, tc.retry, retryProjectAgent(tc.err), tc.err.Error())
	}
}
