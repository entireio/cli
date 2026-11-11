package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectAgentServeSkipsRefusedClaims(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusConflict} {
		for _, laterJob := range []bool{false, true} {
			name := http.StatusText(status) + "/only-refused"
			if laterJob {
				name = http.StatusText(status) + "/later-page-job"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				blocked := agentServeClaim().projectAgentJob
				later := agentServeClaim()
				later.RunID = "01JABCDEFGHJKMNPQRSTVWXYZ1"
				var refused, completed, executed atomic.Int32
				w := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
					out.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodGet:
						page := api.ProjectConfigPage[projectAgentJob]{}
						if r.URL.Query().Get("cursor") == "" {
							cursor := "page-two"
							page.Items = []projectAgentJob{blocked}
							page.NextCursor = &cursor
						} else if laterJob {
							page.Items = []projectAgentJob{later.projectAgentJob}
						}
						assert.NoError(t, json.NewEncoder(out).Encode(page))
					case strings.HasSuffix(r.URL.Path, blocked.RunID+"/claim"):
						refused.Add(1)
						out.WriteHeader(status)
					case strings.HasSuffix(r.URL.Path, later.RunID+"/claim"):
						assert.NoError(t, json.NewEncoder(out).Encode(later))
					case strings.HasSuffix(r.URL.Path, later.RunID+"/complete"):
						completed.Add(1)
						writeAgentTestResponse(t, out, `{}`)
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						out.WriteHeader(http.StatusNotFound)
					}
				})
				w.execute = func(_ context.Context, c projectAgentClaim) (json.RawMessage, error) {
					executed.Add(1)
					require.Equal(t, later.RunID, c.RunID)
					return json.RawMessage(`{"findings":[]}`), nil
				}
				w.wait = func(context.Context, time.Duration) error {
					t.Error("once must scan past refusals without sleeping")
					return context.Canceled
				}
				require.NoError(t, w.serve(ctx, true, time.Second))
				require.EqualValues(t, 1, refused.Load())
				want := int32(0)
				if laterJob {
					want = 1
				}
				require.Equal(t, want, executed.Load())
				require.Equal(t, want, completed.Load())
			})
		}
	}
}

func TestProjectAgentServeRetriesRefusalOnNextScan(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	claim := agentServeClaim()
	var available atomic.Bool
	var claims, completed atomic.Int32
	w := testAgentWorker(t, func(out http.ResponseWriter, r *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			assert.NoError(t, json.NewEncoder(out).Encode(api.ProjectConfigPage[projectAgentJob]{Items: []projectAgentJob{claim.projectAgentJob}}))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claims.Add(1)
			if !available.Load() {
				out.WriteHeader(http.StatusConflict)
				return
			}
			assert.NoError(t, json.NewEncoder(out).Encode(claim))
		case strings.HasSuffix(r.URL.Path, "/complete"):
			completed.Add(1)
			writeAgentTestResponse(t, out, `{}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			out.WriteHeader(http.StatusNotFound)
		}
	})
	w.out = io.Discard
	w.wait = func(ctx context.Context, _ time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		available.Store(true)
		return nil
	}
	w.execute = func(context.Context, projectAgentClaim) (json.RawMessage, error) {
		cancel()
		return json.RawMessage(`{"findings":[]}`), nil
	}
	require.ErrorIs(t, w.serve(ctx, false, time.Second), context.Canceled)
	require.EqualValues(t, 2, claims.Load())
	require.EqualValues(t, 1, completed.Load())
}
