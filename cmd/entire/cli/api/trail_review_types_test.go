package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewSnapshotDecodesSnakeCase(t *testing.T) {
	t.Parallel()
	const payload = `{
  "review":{"id":"review-example","trail_id":"trail-example","code_version_id":"cv-example","actor_id":"actor-example","started_at":"2026-09-01T00:00:00Z"},
  "code_version":{"id":"cv-example","trail_id":"trail-example","repo_id":"repo_example","base_ref":"main","head_ref":"feature/example","base_sha":"base","head_sha":"head","captured_at":"2026-09-01T00:00:00Z"},
  "counts":{"open":1,"resolved":2,"dismissed":3,"stale":4,"total":6},
  "comments":[{"id":"comment-example","trail_id":"trail-example","repo_id":"repo_example","review_id":"review-example","code_version_id":"cv-example","actor_id":"actor-example","status":"open","status_reason":"Needs work","stale_outcome":"current","stale_checked_at":"2026-09-01T00:01:00Z","stale_checked_code_version_id":"cv-example","client_id":"client-example","client_id_hash":"hash-example","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:01:00Z",
   "location":{"id":"location-example","review_comment_id":"comment-example","code_version_id":"cv-example","granularity":"range","file_path":"example.go","start_line":2,"start_column":3,"end_line":4,"end_column":5,"selected_text":"old","nearby_text":"context"},
   "suggested_changes":[{"id":"change-example","review_comment_id":"comment-example","code_version_id":"cv-example","change_type":"unified_diff","patch":"patch","expected_file_path":"example.go","expected_file_hash":"blob","expected_start_line":2,"expected_end_line":4,"expected_lines":"old","created_by":"actor-example","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-01T00:01:00Z"}],
   "discussion_id":"thread-example","discussion_message_count":2,"outgoing_links":[{"source_comment_id":"comment-example","target_comment_id":"other-example","link_type":"related"}]}],
  "next_cursor":"opaque-example","event_cursor":"42"
 }`
	var got TrailReviewStateResponse
	require.NoError(t, json.Unmarshal([]byte(payload), &got))
	require.Equal(t, "trail-example", got.Review.TrailID)
	require.Equal(t, "actor-example", got.Review.ActorID)
	require.False(t, got.Review.StartedAt.IsZero())
	require.Equal(t, "repo_example", got.CodeVersion.RepositoryID)
	require.Equal(t, "head", *got.CodeVersion.HeadSHA)
	require.Equal(t, "main", *got.CodeVersion.BaseRef)
	require.False(t, got.CodeVersion.CapturedAt.IsZero())
	require.Equal(t, 6, got.Counts.Total)
	require.Equal(t, "opaque-example", *got.NextCursor)
	require.Equal(t, "42", got.EventCursor)
	require.Len(t, got.Comments, 1)
	comment := got.Comments[0]
	require.Equal(t, "repo_example", comment.RepositoryID)
	require.Equal(t, "review-example", comment.ReviewID)
	require.Equal(t, "Needs work", *comment.StatusReason)
	require.Equal(t, "current", comment.StaleOutcome)
	require.NotNil(t, comment.StaleCheckedAt)
	require.Equal(t, "cv-example", *comment.StaleCheckedCodeVersionID)
	require.Equal(t, "client-example", *comment.ClientID)
	require.Equal(t, "hash-example", *comment.ClientIDHash)
	require.False(t, comment.CreatedAt.IsZero())
	require.False(t, comment.UpdatedAt.IsZero())
	require.Equal(t, "example.go", *comment.Location.FilePath)
	require.Equal(t, 2, *comment.Location.StartLine)
	require.Equal(t, 5, *comment.Location.EndColumn)
	require.Equal(t, "context", *comment.Location.NearbyText)
	require.Len(t, comment.SuggestedChanges, 1)
	change := comment.SuggestedChanges[0]
	require.Equal(t, "unified_diff", change.ChangeType)
	require.Equal(t, "blob", *change.ExpectedFileHash)
	require.Equal(t, 4, *change.ExpectedEndLine)
	require.Equal(t, "actor-example", change.CreatedBy)
	require.False(t, change.CreatedAt.IsZero())
	require.NotNil(t, comment.DiscussionID)
	require.Equal(t, "thread-example", *comment.DiscussionID)
	require.Equal(t, 2, comment.DiscussionMessageCount)
	require.Len(t, comment.OutgoingLinks, 1)
	require.Equal(t, "other-example", comment.OutgoingLinks[0].TargetCommentID)
}

func TestReviewWritesUseSnakeCase(t *testing.T) {
	t.Parallel()
	head, base, file, text, reason := "head", "base", "example.go", "old", "fixed"
	line := 2
	for _, tc := range []struct {
		name string
		body any
		want string
	}{
		{"start", TrailReviewStartRequest{HeadSHA: &head, BaseSHA: &base, HeadRef: &head, BaseRef: &base}, `{"head_sha":"head","base_sha":"base","head_ref":"head","base_ref":"base"}`},
		{"batch", TrailReviewCommentBatchRequest{Comments: []TrailReviewCommentInput{{ClientID: "client-example", StatusReason: &reason, Location: TrailReviewLocationCreateRequest{Granularity: "line", FilePath: &file, StartLine: &line, SelectedText: &text}, SuggestedChange: &TrailReviewSuggestedChangeCreateRequest{ChangeType: "manual_instruction", Instruction: &reason}}}}, `{"comments":[{"client_id":"client-example","status_reason":"fixed","location":{"granularity":"line","file_path":"example.go","start_line":2,"selected_text":"old"},"suggested_change":{"change_type":"manual_instruction","instruction":"fixed"}}]}`},
		{"patch", TrailReviewCommentPatchRequest{Status: "resolved", StatusReason: &reason}, `{"status":"resolved","status_reason":"fixed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.body)
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestReviewStartAndBatchResponsesDecodeSnakeCase(t *testing.T) {
	t.Parallel()
	var start TrailReviewStartResponse
	require.NoError(t, json.Unmarshal([]byte(`{"review_id":"review-example","trail_id":"trail-example","repo_id":"repo_example","code_version_id":"cv-example","base_sha":"base","head_sha":"head","event_stream_url":"/events","diff_url":"/diff","files_url":"/files","limits":{"max_comments_per_batch":10}}`), &start))
	require.Equal(t, "review-example", start.ReviewID)
	require.Equal(t, "repo_example", start.RepositoryID)
	require.Equal(t, "/events", start.EventStreamURL)
	require.Equal(t, "/diff", start.DiffURL)
	require.Equal(t, "/files", start.FilesURL)
	require.Equal(t, 10, start.Limits.MaxCommentsPerBatch)
	var batch TrailReviewCommentBatchResponse
	require.NoError(t, json.Unmarshal([]byte(`{"results":[{"client_id":"client-example","status":"created","comment":{"id":"comment-example","review_id":"review-example"},"suggested_change":{"id":"change-example","change_type":"manual_instruction"}}]}`), &batch))
	require.Len(t, batch.Results, 1)
	require.Equal(t, "client-example", batch.Results[0].ClientID)
	require.Equal(t, "review-example", batch.Results[0].Comment.ReviewID)
	require.Equal(t, "manual_instruction", batch.Results[0].SuggestedChange.ChangeType)
}
