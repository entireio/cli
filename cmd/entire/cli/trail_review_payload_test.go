package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestFindingWritesUseCellContract(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"/api/v1/trails/gh/acme/widget/7", "/api/v1/repos/repo_example/trails/7"} {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				bodies = append(bodies, string(body))
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "POST " + base + "/reviews":
					w.WriteHeader(http.StatusCreated)
					_, _ = fmt.Fprint(w, `{"review_id":"review-example","repo_id":"repo_example","limits":{"max_comments_per_batch":10}}`)
				case "POST " + base + "/reviews/review-example/comments":
					_, _ = fmt.Fprint(w, `{"results":[{"client_id":"client-example","status":"created","comment":{"id":"comment-example","review_id":"review-example","status":"open","location":{"file_path":"example.go","start_line":2}}}]}`)
				case "PATCH " + base + "/reviews/review-example/comments/comment-example":
					_, _ = fmt.Fprint(w, `{"id":"comment-example","review_id":"review-example","status":"resolved","status_reason":"fixed"}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			client := api.NewClientWithBaseURL("tok", srv.URL)
			client.SetTrailRoute("trail-example", base)
			review, err := startTrailReview(t.Context(), client, "trail-example")
			require.NoError(t, err)
			file, line := "example.go", 2
			comments, err := postTrailReviewFindingBatch(t.Context(), client, "trail-example", review.ReviewID, []api.TrailReviewCommentInput{{ClientID: "client-example", Location: api.TrailReviewLocationCreateRequest{Granularity: "line", FilePath: &file, StartLine: &line}}})
			require.NoError(t, err)
			require.Len(t, comments, 1)
			require.Equal(t, "example.go", *comments[0].Location.FilePath)
			updated, err := patchTrailReviewCommentStatus(t.Context(), client, "trail-example", comments[0], "resolved", "fixed")
			require.NoError(t, err)
			require.Equal(t, "fixed", *updated.StatusReason)
			require.Len(t, bodies, 3)
			require.JSONEq(t, `{}`, bodies[0])
			require.JSONEq(t, `{"comments":[{"client_id":"client-example","location":{"granularity":"line","file_path":"example.go","start_line":2}}]}`, bodies[1])
			require.JSONEq(t, `{"status":"resolved","status_reason":"fixed"}`, bodies[2])
		})
	}
}

func TestFindingEmptyPageDisplaysNextCursor(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printTrailReviewComments(&out, nil, "opaque-next")
	require.Contains(t, out.String(), `--cursor "opaque-next"`)
}

func TestFindingContinuationUsesOnlyExplicitFilters(t *testing.T) {
	t.Parallel()
	opts := defaultTrailReviewListOptions()
	opts.Cursor = "opaque+/=page"
	parsed, err := url.Parse(trailReviewCommentsPath("trail-example", opts))
	require.NoError(t, err)
	require.Equal(t, url.Values{"cursor": {"opaque+/=page"}, "per_page": {"100"}}, parsed.Query())
	opts.Status = "resolved"
	opts.StatusChanged = true
	opts.Freshness = "any"
	opts.FreshnessChanged = true
	opts.IncludeDismissedChanged = true
	parsed, err = url.Parse(trailReviewCommentsPath("trail-example", opts))
	require.NoError(t, err)
	require.Equal(t, "resolved", parsed.Query().Get("status[eq]"))
	require.Equal(t, "any", parsed.Query().Get("stale"))
	require.Equal(t, "false", parsed.Query().Get("include_dismissed"))
}

func TestFindingJSONPreservesKeysAndReturnsNextCursor(t *testing.T) {
	t.Parallel()
	var comment api.TrailReviewComment
	require.NoError(t, json.Unmarshal([]byte(`{"id":"comment-example","repo_id":"repo_example","review_id":"review-example","discussion_id":"thread-example","discussion_message_count":2,"location":{"file_path":"example.go","start_line":2},"suggested_changes":[{"change_type":"manual_instruction","expected_file_path":"example.go"}],"outgoing_links":[{"source_comment_id":"comment-example","target_comment_id":"other-example","link_type":"related"}]}`), &comment))
	target := trailReviewTarget{Trail: api.TrailResource{OriginalBranch: "feature/example", BodyDocument: &api.TrailBodyDocument{TextSnapshot: "Description"}}}
	for _, cursor := range []string{"opaque-next", ""} {
		t.Run(cursor, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			require.NoError(t, encodeTrailReviewJSON(&out, target, []api.TrailReviewComment{comment}, cursor, trailReviewCommentCounts{}))
			var got map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(out.Bytes(), &got))
			if cursor != "" {
				require.JSONEq(t, `"opaque-next"`, string(got["next_cursor"]))
				require.JSONEq(t, `true`, string(got["has_more"]))
			} else {
				require.NotContains(t, got, "next_cursor")
				require.JSONEq(t, `false`, string(got["has_more"]))
			}
			for _, key := range []string{`"discussionId": "thread-example"`, `"discussionMessageCount": 2`, `"repositoryId": "repo_example"`, `"reviewId": "review-example"`, `"filePath": "example.go"`, `"startLine": 2`, `"suggestedChanges"`, `"changeType": "manual_instruction"`, `"expectedFilePath": "example.go"`, `"targetCommentId": "other-example"`} {
				require.Contains(t, string(got["findings"]), key)
			}
			requireCamelCaseKeys(t, got["findings"])
			requireCamelCaseKeys(t, got["trail"])
			require.Contains(t, string(got["trail"]), `"originalBranch": "feature/example"`)
			require.Contains(t, string(got["trail"]), `"textSnapshot": "Description"`)
		})
	}
}

func TestFindingCommandsAcceptCursor(t *testing.T) {
	t.Parallel()
	for _, cmd := range []*cobra.Command{newTrailFindingCmd(), newTrailFindingListCmd(&trailReviewTargetOptions{})} {
		require.NotNil(t, cmd.Flags().Lookup("cursor"))
		require.NoError(t, cmd.ParseFlags([]string{"--cursor", "opaque+/=page", "--limit", "1"}))
	}
}

func TestFindingScansFollowCellCursor(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"/api/v1/trails/gh/acme/widget/7", "/api/v1/repos/repo_example/trails/7"} {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			for _, operation := range []string{"list", "lookup"} {
				t.Run(operation, func(t *testing.T) {
					t.Parallel()
					requests := 0
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests++
						if r.URL.Path != base+"/reviews/comments" {
							t.Errorf("path = %s", r.URL.Path)
						}
						q := r.URL.Query()
						if q.Get("per_page") != "100" {
							t.Errorf("query = %s", r.URL.RawQuery)
						}
						w.Header().Set("Content-Type", "application/json")
						if requests == 1 {
							if q.Get("stale") != "any" || q.Get("include_dismissed") != "true" {
								t.Errorf("initial query = %s", r.URL.RawQuery)
							}
							_, _ = fmt.Fprint(w, `{"comments":[{"id":"comment-first","review_id":"review-example"}],"next_cursor":"opaque+/=page","event_cursor":"42"}`)
						} else {
							if requests != 2 || q.Get("cursor") != "opaque+/=page" || len(q) != 2 {
								t.Errorf("continuation = %s", r.URL.RawQuery)
							}
							_, _ = fmt.Fprint(w, `{"comments":[{"id":"comment-second","review_id":"review-example"}],"event_cursor":"43"}`)
						}
					}))
					defer srv.Close()
					client := api.NewClientWithBaseURL("tok", srv.URL)
					client.SetTrailRoute("trail-example", base)
					if operation == "lookup" {
						comment, err := resolveTrailReviewComment(t.Context(), client, "trail-example", "comment-second")
						require.NoError(t, err)
						require.Equal(t, "review-example", comment.ReviewID)
					} else {
						comments, err := fetchAllTrailReviewComments(t.Context(), client, "trail-example", trailReviewSummaryOptions())
						require.NoError(t, err)
						require.Len(t, comments, 2)
					}
					require.Equal(t, 2, requests)
				})
			}
		})
	}
}
