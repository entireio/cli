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

func TestProjectAgentServeRetriesTransientClaims(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, 0} {
		for _, once := range []bool{false, true} {
			name := http.StatusText(status)
			if status == 0 {
				name = "network"
			}
			if once {
				name += "/once"
			} else {
				name += "/continuous"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var claims, polls, completions atomic.Int32
				claim := agentServeClaim()
				worker := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodGet:
						polls.Add(1)
						assert.NoError(t, json.NewEncoder(out).Encode(api.ProjectConfigPage[projectAgentJob]{Items: []projectAgentJob{claim.projectAgentJob}}))
					case strings.HasSuffix(r.URL.Path, "/claim"):
						assert.True(t, strings.HasSuffix(r.URL.Path, claim.RunID+"/claim"))
						if claims.Add(1) <= 3 {
							if status == 0 {
								hijacker, ok := out.(http.Hijacker)
								if !assert.True(t, ok) {
									out.WriteHeader(http.StatusInternalServerError)
									return
								}
								conn, _, err := hijacker.Hijack()
								if assert.NoError(t, err) {
									assert.NoError(t, conn.Close())
								}
							} else {
								out.WriteHeader(status)
							}
							return
						}
						assert.NoError(t, json.NewEncoder(out).Encode(claim))
					case strings.HasSuffix(r.URL.Path, "/complete"):
						completions.Add(1)
						writeAgentTestResponse(t, out, `{}`)
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						out.WriteHeader(http.StatusNotFound)
					}
				})
				executed := 0
				worker.execute = func(context.Context, projectAgentClaim) (json.RawMessage, error) {
					executed++
					cancel()
					return json.RawMessage(`{"findings":[]}`), nil
				}
				var waits []time.Duration
				worker.wait = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
				err := worker.serve(ctx, once, 7*time.Second)
				if once {
					require.ErrorIs(t, err, errAgentClaimRetry)
					require.EqualValues(t, 3, claims.Load())
					require.EqualValues(t, 1, polls.Load())
					require.Zero(t, executed)
					require.Zero(t, completions.Load())
					require.Equal(t, []time.Duration{time.Second, 2 * time.Second}, waits)
				} else {
					require.ErrorIs(t, err, context.Canceled)
					require.EqualValues(t, 4, claims.Load())
					require.EqualValues(t, 2, polls.Load())
					require.Equal(t, 1, executed)
					require.EqualValues(t, 1, completions.Load())
					require.Equal(t, []time.Duration{time.Second, 2 * time.Second, 7 * time.Second, 7 * time.Second}, waits)
				}
			})
		}
	}
}

func TestProjectAgentServeDoesNotRetryFatalRunErrors(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"unauthorized", "invalid-claim", "uncertain-submission"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			claim := agentServeClaim()
			var claims, submissions atomic.Int32
			worker := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					assert.NoError(t, json.NewEncoder(out).Encode(api.ProjectConfigPage[projectAgentJob]{Items: []projectAgentJob{claim.projectAgentJob}}))
				case strings.HasSuffix(r.URL.Path, "/claim"):
					claims.Add(1)
					if mode == "unauthorized" {
						out.WriteHeader(http.StatusUnauthorized)
						return
					}
					response := claim
					if mode == "invalid-claim" {
						response.ProjectID = "other-project"
					}
					assert.NoError(t, json.NewEncoder(out).Encode(response))
				case strings.HasSuffix(r.URL.Path, "/complete"):
					submissions.Add(1)
					out.WriteHeader(http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					out.WriteHeader(http.StatusNotFound)
				}
			})
			var pending *projectAgentSubmission
			executions := 0
			worker.save = func(s *projectAgentSubmission) error { pending = s; return nil }
			worker.execute = func(context.Context, projectAgentClaim) (json.RawMessage, error) {
				executions++
				return json.RawMessage(`{"findings":[]}`), nil
			}
			err := worker.serve(ctx, false, time.Second)
			require.Error(t, err)
			require.NotErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, errAgentClaimRetry)
			require.EqualValues(t, 1, claims.Load())
			if mode == "uncertain-submission" {
				require.Equal(t, 1, executions)
				require.EqualValues(t, 3, submissions.Load())
				require.NotNil(t, pending)
			} else {
				require.Zero(t, executions)
				require.Zero(t, submissions.Load())
				require.Nil(t, pending)
			}
		})
	}
}
