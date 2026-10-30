package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	findingReplyTestFinding    = "finding-one"
	findingReplyTestDiscussion = "discussion-one"
	findingReplyTestThreadPath = "/api/v1/trails/gh/acme/widget/7/discussions/" + findingReplyTestDiscussion
)

// setupFindingReplyTest serves change widget/7 of project trail 42 with one
// finding, whose discussion is discussionID (none when empty). thread answers
// requests under the finding's discussion; it records what it was sent.
func setupFindingReplyTest(t *testing.T, discussionID string, thread http.HandlerFunc) {
	t.Helper()
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		if !serveWorkingProjectRead(t, w, r) {
			t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	finding := map[string]any{"id": findingReplyTestFinding, "review_id": "review-one", "status": "open", "body": "Finding body",
		"location": map[string]any{"granularity": "whole_change"}}
	if discussionID != "" {
		finding["discussion_id"] = discussionID
	}
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case r.Method == http.MethodGet && path == "/api/v1/trails/gh/acme/widget/7":
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Status: "open",
				Parent: &api.TrailParentReference{ID: projectTrailTestID, Number: 42, ProjectID: projectTrailTestProject,
					Host: "gh", Project: "acme", Path: projectTrailTestPath, Jurisdiction: "eu", PrimaryProcessingCell: "project-cell"}}))
		case r.Method == http.MethodGet && path == "/api/v1/trails/gh/acme/widget/7/reviews/comments":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"comments": []any{finding}}))
		case r.Method == http.MethodGet && path == "/api/v1/trails/gh/acme/widget/7/reviews/review-one":
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"comments": []any{finding}}))
		case strings.HasPrefix(path, findingReplyTestThreadPath):
			thread(w, r)
		default:
			t.Errorf("unexpected repo request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
}

func TestTrailFindingReply_PostsToTheFindingsDiscussion(t *testing.T) {
	// Not parallel: replaces client constructors.
	var sent []string
	setupFindingReplyTest(t, findingReplyTestDiscussion, func(w http.ResponseWriter, r *http.Request) {
		var request api.TrailDiscussionMessageRequest
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		sent = append(sent, r.Method+" "+r.URL.Path+" "+request.Body)
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailDiscussionMessageResponse{Message: api.TrailDiscussionMessage{ID: "message-one", Body: request.Body}}))
	})

	out, _, err := executeProjectTrailTest(t, "finding", "reply", "widget/7", findingReplyTestFinding, "-m", "Fixed in abc123", "--project", "gh/acme")
	require.NoError(t, err)
	require.Equal(t, []string{"POST " + findingReplyTestThreadPath + "/messages Fixed in abc123"}, sent)
	assert.Contains(t, out, "Added reply message-one to finding "+findingReplyTestFinding)

	sent = nil
	out, _, err = executeProjectTrailTest(t, "finding", "edit-reply", "widget/7", findingReplyTestFinding, "message-one", "-m", "Fixed in def456", "--project", "gh/acme")
	require.NoError(t, err)
	require.Equal(t, []string{"PATCH " + findingReplyTestThreadPath + "/messages/message-one Fixed in def456"}, sent)
	assert.Contains(t, out, "Edited reply message-one")
}

func TestTrailFindingDeleteReply_DeletesTheMessage(t *testing.T) {
	// Not parallel: replaces client constructors.
	var sent []string
	setupFindingReplyTest(t, findingReplyTestDiscussion, func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})

	out, _, err := executeProjectTrailTest(t, "finding", "delete-reply", "widget/7", findingReplyTestFinding, "message-one", "--project", "gh/acme")
	require.NoError(t, err)
	require.Equal(t, []string{"DELETE " + findingReplyTestThreadPath + "/messages/message-one"}, sent)
	assert.Contains(t, out, "Deleted reply message-one")
}

func TestTrailFindingReply_RefusesAFindingWithoutAThread(t *testing.T) {
	// Not parallel: replaces client constructors.
	setupFindingReplyTest(t, "", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected thread request: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	_, _, err := executeProjectTrailTest(t, "finding", "reply", "widget/7", findingReplyTestFinding, "-m", "hello", "--project", "gh/acme")
	require.ErrorContains(t, err, "has no reply thread")
}

func TestTrailFindingReply_RequiresABody(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"finding", "reply", "7", findingReplyTestFinding},
		{"finding", "edit-reply", "7", findingReplyTestFinding, "message-one", "-m", " "},
	} {
		cmd := newTrailCmdForMode(true)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		cmd.SetArgs(args)
		require.ErrorContains(t, cmd.Execute(), "--body is required", "args %v", args)
	}
}

func TestTrailFindingShow_PrintsRepliesWithTheirIDs(t *testing.T) {
	// Not parallel: replaces client constructors.
	created := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	setupFindingReplyTest(t, findingReplyTestDiscussion, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, findingReplyTestThreadPath, r.URL.Path)
		assert.NoError(t, json.NewEncoder(w).Encode(api.TrailDiscussionDetailResponse{Messages: []api.TrailDiscussionMessage{
			{ID: "message-one", Author: "alice", CreatedAt: created, Body: "Is this still live?\nSecond line",
				Replies: []api.TrailDiscussionReply{{ID: "reply-one", Author: "bob", CreatedAt: created, Body: "Fixed"}}},
		}}))
	})

	out, _, err := executeProjectTrailTest(t, "finding", "show", "widget/7", findingReplyTestFinding, "--project", "gh/acme")
	require.NoError(t, err)
	assert.Contains(t, out, "Finding body")
	assert.Contains(t, out, "Replies:\n  message-one  alice  2026-10-29T12:00:00Z\n  Is this still live?\n  Second line\n")
	assert.Contains(t, out, "    ↳ reply-one  bob  2026-10-29T12:00:00Z\n    Fixed\n")
}

func TestTrailFindingShow_WarnsButSucceedsWhenRepliesFail(t *testing.T) {
	// Not parallel: replaces client constructors.
	setupFindingReplyTest(t, findingReplyTestDiscussion, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	out, errOut, err := executeProjectTrailTest(t, "finding", "show", "widget/7", findingReplyTestFinding, "--project", "gh/acme")
	require.NoError(t, err)
	assert.Contains(t, out, "Finding body")
	assert.NotContains(t, out, "Replies:")
	assert.Contains(t, errOut, "could not load replies")
}
