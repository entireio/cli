package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	job, err := w.next(t.Context())
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
				require.True(t, deadline.Before(claim.DeadlineAt))
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
