package cli

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestProjectTrailStatusExcludesDraft(t *testing.T) {
	// If validation regresses, repository discovery must still stay isolated.
	t.Chdir(t.TempDir())
	for _, verb := range []string{"create", "update", "link", "list"} {
		t.Run(verb, func(t *testing.T) {
			args := []string{verb, "--status", "draft"}
			switch verb {
			case "create":
				args = append(args, "--title", "Intent", "--no-branch")
			case "update", "link":
				args = append(args, projectTrailTestID)
			}
			_, _, err := executeProjectTrailTest(t, args...)
			require.ErrorContains(t, err, "project trail status must be open or closed")
			cmd, _, err := newTrailCmdForMode(true).Find([]string{verb})
			require.NoError(t, err)
			require.NotContains(t, cmd.Flags().Lookup("status").Usage, "draft")
		})
	}
	for _, status := range []string{"open", "closed"} {
		require.True(t, validProjectTrailStatus(status))
	}
	require.False(t, validProjectTrailStatus("draft"))
}

// Not parallel: replaces project-client constructors through the fixture.
func TestProjectTrailMessageOnlyWriteJSON(t *testing.T) {
	for _, verb := range []string{"reply", "edit"} {
		t.Run(verb, func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					assert.NoError(t, json.NewEncoder(w).Encode(projectDiscussionResponse{
						Discussion: api.TrailDiscussionSummary{ID: "discussion-one"},
						Messages:   []api.TrailDiscussionMessage{{ID: "message-one", ETag: `W/"message-version"`}},
					}))
					return
				}
				if verb == "edit" {
					assert.Equal(t, http.MethodPatch, r.Method)
					assert.Equal(t, `W/"message-version"`, r.Header.Get("If-Match"))
				} else {
					assert.Equal(t, http.MethodPost, r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := w.Write([]byte(`{"message":{"id":"message-one","body":"Updated","created_at":"2026-10-21T09:00:00Z","replies":[],"etag":"private-token"}}`))
				assert.NoError(t, err)
			})
			args := []string{"comment", verb, "discussion-one"}
			if verb == "edit" {
				args = append(args, "message-one")
			}
			args = append(args, "--trail", projectTrailTestID, "--project", "gh/acme", "-m", "Updated", "--json")
			out, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			var body map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(out), &body))
			require.NotContains(t, body, "discussion", "message-only responses must not invent a discussion")
			require.Contains(t, string(body["message"]), `"id": "message-one"`)
			require.NotContains(t, out, "0001-01-01")
			require.NotContains(t, out, "etag")
			require.NotContains(t, out, "private-token")
		})
	}
}
