package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectAgentMalformedClaimReportsFailure(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"harness", "deadline"} {
		for _, once := range []bool{false, true} {
			for _, failStatus := range []int{http.StatusOK, http.StatusServiceUnavailable} {
				name := invalid + "/continuous/" + http.StatusText(failStatus)
				if once {
					name = invalid + "/once/" + http.StatusText(failStatus)
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					malformed := agentServeClaim()
					listed := malformed.projectAgentJob
					if invalid == "harness" {
						malformed.Harness = "unsupported"
					} else {
						malformed.DeadlineAt = time.Time{}
					}
					later := agentServeClaim()
					later.RunID = "01JABCDEFGHJKMNPQRSTVWXYZ1"
					var failed, savedBeforeSend atomic.Bool
					var failures, completions atomic.Int32
					worker := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet:
							job := listed
							if failed.Load() {
								job = later.projectAgentJob
							}
							assert.NoError(t, json.NewEncoder(out).Encode(api.ProjectConfigPage[projectAgentJob]{Items: []projectAgentJob{job}}))
						case strings.HasSuffix(r.URL.Path, malformed.RunID+"/claim"):
							assert.NoError(t, json.NewEncoder(out).Encode(malformed))
						case strings.HasSuffix(r.URL.Path, later.RunID+"/claim"):
							assert.NoError(t, json.NewEncoder(out).Encode(later))
						case strings.HasSuffix(r.URL.Path, malformed.RunID+"/fail"):
							failures.Add(1)
							assert.True(t, savedBeforeSend.Load(), "failure must be persisted before sending")
							var body map[string]string
							assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
							assert.Equal(t, map[string]string{"error": "CLI could not execute the review; inspect the worker's local error"}, body)
							out.WriteHeader(failStatus)
							if failStatus == http.StatusOK {
								failed.Store(true)
							}
						case strings.HasSuffix(r.URL.Path, later.RunID+"/complete"):
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
						require.Equal(t, later.RunID, c.RunID, "never execute the malformed claim")
						cancel()
						return json.RawMessage(`{"findings":[]}`), nil
					}
					var pending *projectAgentSubmission
					var saved []projectAgentSubmission
					worker.save = func(s *projectAgentSubmission) error {
						pending = s
						savedBeforeSend.Store(s != nil)
						if s != nil {
							saved = append(saved, *s)
						}
						return nil
					}
					err := worker.serve(ctx, once, time.Second)
					require.NotEmpty(t, saved)
					require.Equal(t, malformed.RunID, saved[0].RunID)
					require.Equal(t, agentSubmitFail, saved[0].Action)
					switch {
					case failStatus != http.StatusOK:
						require.True(t, api.IsHTTPErrorStatus(err, failStatus), "%v", err)
						require.EqualValues(t, 3, failures.Load())
						require.NotNil(t, pending)
						require.Len(t, saved, 1)
						require.Zero(t, executions)
						require.Zero(t, completions.Load())
					case once:
						require.ErrorIs(t, err, errAgentExecution)
						require.EqualValues(t, 1, failures.Load())
						require.Nil(t, pending)
						require.Zero(t, executions)
						require.Zero(t, completions.Load())
					default:
						require.ErrorIs(t, err, context.Canceled)
						require.EqualValues(t, 1, failures.Load())
						require.Nil(t, pending)
						require.Equal(t, 1, executions)
						require.EqualValues(t, 1, completions.Load())
					}
				})
			}
		}
	}
}

func TestProjectAgentClaimIdentityMismatchNeverPublishes(t *testing.T) {
	t.Parallel()
	for _, mismatch := range []string{"run", "project"} {
		t.Run(mismatch, func(t *testing.T) {
			t.Parallel()
			claim := agentServeClaim()
			job := claim.projectAgentJob
			if mismatch == "run" {
				claim.RunID = "01JABCDEFGHJKMNPQRSTVWXYZ1"
			} else {
				claim.ProjectID = "unrelated-project"
			}
			worker := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
				assert.Equal(t, agentJobsPath+"/"+job.RunID+"/claim", r.URL.Path)
				assert.NoError(t, json.NewEncoder(out).Encode(claim))
			})
			worker.execute = func(context.Context, projectAgentClaim) (json.RawMessage, error) {
				t.Error("must not execute")
				return nil, nil
			}
			worker.save = func(*projectAgentSubmission) error { t.Error("must not save a submission"); return nil }
			processed, err := worker.run(t.Context(), job)
			require.False(t, processed)
			require.ErrorContains(t, err, "claim does not match the requested project or run")
		})
	}
}
