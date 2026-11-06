package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestTrailReviewStartKey(t *testing.T) {
	t.Parallel()
	inputs := []api.TrailReviewCommentInput{{ClientID: "first"}, {ClientID: "second"}}
	key, err := trailReviewStartKey(inputs)
	require.NoError(t, err)
	require.NotEmpty(t, key)
	require.LessOrEqual(t, len(key), 255)
	reordered, err := trailReviewStartKey([]api.TrailReviewCommentInput{inputs[1], inputs[0]})
	require.NoError(t, err)
	require.Equal(t, key, reordered)
	inputs[0].Body = stringPtr("changed payload must conflict within the same review")
	changed, err := trailReviewStartKey(inputs)
	require.NoError(t, err)
	require.Equal(t, key, changed)
	for _, ids := range [][]string{{"firstsecond"}, {"first", "third"}, {"first\nsecond"}, {"first\x00second"}, {strings.Repeat("long", 1000)}} {
		var batch []api.TrailReviewCommentInput
		for _, id := range ids {
			batch = append(batch, api.TrailReviewCommentInput{ClientID: id})
		}
		other, err := trailReviewStartKey(batch)
		require.NoError(t, err)
		require.NotEqual(t, key, other)
		require.NotContains(t, other, "\n")
		require.LessOrEqual(t, len(other), 255)
	}
	_, err = trailReviewStartKey([]api.TrailReviewCommentInput{{}})
	require.ErrorContains(t, err, "client ID must not be empty")
}

// Full independent Cobra invocations share only the simulated server's replay
// store, not a command instance or CLI cache. The fixture mirrors the server's
// start-review Idempotency-Key and review-local client_id contracts.
// Not parallel: replaces routing constructors and CWD.
func TestProjectTrailFindingAddRetryUsesSameReview(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_batch_fails=%t", failFirst), func(t *testing.T) {
			t.Chdir(t.TempDir())
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if !serveWorkingProjectRead(t, w, r) {
					t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			const base = "/api/v1/trails/gh/acme/widget/7/reviews"
			reviews := map[string]string{}
			findings := map[string]api.TrailReviewComment{}
			var reviewCount, batchCalls int
			setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				if r.URL.Path == base {
					key := r.Header.Get("Idempotency-Key")
					assert.NotEmpty(t, key)
					reviewID, ok := reviews[key]
					if !ok || key == "" {
						reviewCount++
						reviewID = fmt.Sprintf("review-%d", reviewCount)
						reviews[key] = reviewID
					}
					assert.NoError(t, json.NewEncoder(w).Encode(api.TrailReviewStartResponse{ReviewID: reviewID, TrailID: projectTrailTestChange}))
					return
				}
				assert.True(t, strings.HasPrefix(r.URL.Path, base+"/review-"))
				assert.True(t, strings.HasSuffix(r.URL.Path, "/comments"))
				batchCalls++
				if failFirst && batchCalls == 1 {
					http.Error(w, `{"detail":"temporary failure"}`, http.StatusServiceUnavailable)
					return
				}
				var batch api.TrailReviewCommentBatchRequest
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&batch)) || !assert.Len(t, batch.Comments, 1) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				input := batch.Comments[0]
				reviewID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/"), "/comments")
				key := reviewID + ":" + input.ClientID
				comment, exists := findings[key]
				status := "created"
				if exists {
					status = "existing"
					if *comment.Body != *input.Body {
						assert.NoError(t, json.NewEncoder(w).Encode(api.TrailReviewCommentBatchResponse{Results: []api.TrailReviewCommentBatchResult{{Status: "error", Error: &api.TrailReviewCommentBatchError{Code: "client_id_conflict", Message: "different content"}}}}))
						return
					}
				} else {
					comment = api.TrailReviewComment{ID: fmt.Sprintf("finding-%d", len(findings)+1), ReviewID: reviewID, Body: input.Body, ClientID: &input.ClientID}
					findings[key] = comment
				}
				assert.NoError(t, json.NewEncoder(w).Encode(api.TrailReviewCommentBatchResponse{Results: []api.TrailReviewCommentBatchResult{{Status: status, Comment: &comment}}}))
			})
			var ids []string
			for i := range 3 {
				out, _, err := executeProjectTrailTest(t, "finding", "add", "42", "--project", "gh/acme", "--repo", "gh/acme/widget", "--branch", "feature/work", "--client-id", "stable-client-key", "-m", "Original finding", "--json")
				if failFirst && i == 0 {
					require.Error(t, err)
					continue
				}
				require.NoError(t, err)
				var finding struct {
					ID string `json:"id"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &finding))
				ids = append(ids, finding.ID)
			}
			require.Equal(t, 1, reviewCount, "retry must not open another review")
			require.Len(t, findings, 1)
			for _, id := range ids {
				require.Equal(t, ids[0], id)
			}
			_, _, err := executeProjectTrailTest(t, "finding", "add", "42", "--project", "gh/acme", "--repo", "gh/acme/widget", "--branch", "feature/work", "--client-id", "stable-client-key", "-m", "Changed finding", "--json")
			require.ErrorContains(t, err, "client_id_conflict")
			require.Equal(t, 1, reviewCount)
			require.Len(t, findings, 1)
		})
	}
}
