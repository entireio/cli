package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Keep real command routing and journal locking, but isolate config, auth and API.
// Empty PATH makes executor probing fail deterministically without subprocesses.
func projectAgentCommandFixture(t *testing.T, h http.HandlerFunc) (*projectAgentTarget, func(context.Context, ...string) *cobra.Command) {
	t.Helper()
	t.Setenv("ENTIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("ENTIRE_TOKEN", makeTestJWT(t, `{"sub":"test-service-account","iss":"https://auth.example.com","aud":"https://example.com"}`))
	t.Setenv("PATH", "")
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	target := &projectAgentTarget{client: api.NewClientWithBaseURL("test", server.URL), host: "et", project: "acme", projectID: saTestProjectID}
	require.NoError(t, saveProjectAgentConnection(target))
	old := newProjectTrailCellClient
	t.Cleanup(func() { newProjectTrailCellClient = old })
	newProjectTrailCellClient = func(_ context.Context, _ bool, cell *auth.CellTarget) (*api.Client, error) {
		require.Equal(t, server.URL, cell.BaseURL)
		return target.client, nil
	}
	return target, func(ctx context.Context, args ...string) *cobra.Command {
		cmd := newProjectAgentServeCmd()
		cmd.Flags().String("project", "et/acme", "")
		cmd.SetContext(ctx)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		return cmd
	}
}

func TestProjectAgentServeCommandValidation(t *testing.T) {
	for _, tc := range []struct{ name, token, interval, want string }{
		{"missing token", "", "1s", "set ENTIRE_TOKEN"},
		{"TLS verification disabled", "test", "1s", "unset ENTIRE_TLS_SKIP_VERIFY"},
		{"zero interval", "test", "0s", "--poll-interval must be at least 1s"},
		{"negative interval", "test", "-1s", "--poll-interval must be at least 1s"},
		{"short interval", "test", "999ms", "--poll-interval must be at least 1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, command := projectAgentCommandFixture(t, func(out http.ResponseWriter, _ *http.Request) {
				t.Error("validation must precede API calls")
				out.WriteHeader(http.StatusInternalServerError)
			})
			t.Setenv("ENTIRE_TOKEN", tc.token)
			skipTLS := ""
			if tc.want == "unset ENTIRE_TLS_SKIP_VERIFY" {
				skipTLS = "true"
			}
			t.Setenv("ENTIRE_TLS_SKIP_VERIFY", skipTLS)
			cmd := command(t.Context(), "--poll-interval", tc.interval)
			require.ErrorContains(t, cmd.Execute(), tc.want)
		})
	}
}

func TestProjectAgentServeCommandPendingReplay(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity} {
		for _, once := range []bool{false, true} {
			name := http.StatusText(status) + "/continuous"
			if once {
				name = http.StatusText(status) + "/once"
			}
			t.Run(name, func(t *testing.T) {
				pending := projectAgentSubmission{RunID: agentServeRunID, Action: "complete", Body: json.RawMessage(`{"result":{"findings":[]}}`)}
				var requests atomic.Int32
				target, command := projectAgentCommandFixture(t, func(out http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "/api/v1/self-hosted-runner/runs/"+agentServeRunID+"/complete", r.URL.Path)
					body, err := io.ReadAll(r.Body)
					assert.NoError(t, err)
					assert.Equal(t, string(pending.Body), string(body))
					out.WriteHeader(status)
				})
				journal, err := openProjectAgentJournal(t.Context(), target)
				require.NoError(t, err)
				require.NoError(t, journal.save(&pending))
				journal.unlock()
				args := []string{}
				if once {
					args = append(args, "--once")
				}
				err = command(t.Context(), args...).Execute()
				switch {
				case status == http.StatusUnprocessableEntity:
					require.True(t, api.IsHTTPErrorStatus(err, status), "%v", err)
					require.ErrorContains(t, err, journal.path())
				case once:
					require.NoError(t, err, "recovery with --once must not probe or poll")
				default:
					require.ErrorContains(t, err, "git is required", "probe must happen only after replay")
				}
				require.EqualValues(t, 1, requests.Load())
				// Both successful and failing command exits must release the real lock.
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				reopened, err := openProjectAgentJournal(ctx, target)
				require.NoError(t, err)
				defer reopened.unlock()
				saved, err := reopened.load()
				require.NoError(t, err)
				if status == http.StatusUnprocessableEntity {
					require.Equal(t, &pending, saved)
				} else {
					require.Nil(t, saved)
				}
			})
		}
	}
}

func TestProjectAgentServeContinuesAfterExecutorFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := agentServeClaim()
	second := agentServeClaim()
	second.RunID = "01JABCDEFGHJKMNPQRSTVWXYZ1"
	var failures, completions atomic.Int32
	worker := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			job := first.projectAgentJob
			if failures.Load() > 0 {
				job = second.projectAgentJob
			}
			assert.NoError(t, json.NewEncoder(out).Encode(api.ProjectConfigPage[projectAgentJob]{Items: []projectAgentJob{job}}))
		case strings.HasSuffix(r.URL.Path, first.RunID+"/claim"):
			assert.NoError(t, json.NewEncoder(out).Encode(first))
		case strings.HasSuffix(r.URL.Path, second.RunID+"/claim"):
			assert.NoError(t, json.NewEncoder(out).Encode(second))
		case strings.HasSuffix(r.URL.Path, first.RunID+"/fail"):
			failures.Add(1)
			writeAgentTestResponse(t, out, `{}`)
		case strings.HasSuffix(r.URL.Path, second.RunID+"/complete"):
			completions.Add(1)
			writeAgentTestResponse(t, out, `{}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			out.WriteHeader(http.StatusNotFound)
		}
	})
	executions := 0
	worker.execute = func(_ context.Context, c projectAgentClaim) (json.RawMessage, error) {
		executions++
		if c.RunID == first.RunID {
			return nil, errors.New("fake executor failed")
		}
		require.Equal(t, second.RunID, c.RunID)
		cancel()
		return json.RawMessage(`{"findings":[]}`), nil
	}
	var saved []string
	worker.save = func(s *projectAgentSubmission) error {
		if s != nil {
			saved = append(saved, s.Action)
		}
		return nil
	}
	require.ErrorIs(t, worker.serve(ctx, false, time.Second), context.Canceled)
	require.Equal(t, 2, executions)
	require.EqualValues(t, 1, failures.Load())
	require.EqualValues(t, 1, completions.Load())
	require.Equal(t, []string{agentSubmitFail, "complete"}, saved)
}

func TestProjectAgentServeCommandWaitsForJournalLock(t *testing.T) {
	target, command := projectAgentCommandFixture(t, func(out http.ResponseWriter, _ *http.Request) {
		t.Error("locked command must not call API")
		out.WriteHeader(http.StatusInternalServerError)
	})
	held, err := openProjectAgentJournal(t.Context(), target)
	require.NoError(t, err)
	defer held.unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = command(ctx, "--once").Execute()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "lock worker state")
	held.unlock()
	// A canceled waiter must not leave the lock held or damage the shared root.
	ctx2, cancel2 := context.WithTimeout(t.Context(), time.Second)
	defer cancel2()
	reopened, err := openProjectAgentJournal(ctx2, target)
	require.NoError(t, err)
	reopened.unlock()
}
