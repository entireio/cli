package cli

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

// Not parallel: these command tests replace the project routing constructors.
func TestProjectTrailDiscussionsUseProjectRoutes(t *testing.T) {
	for _, tt := range []struct {
		args           []string
		method, suffix string
	}{
		{[]string{"list", "--json"}, http.MethodGet, ""},
		{[]string{"show", "discussion-one", "--json"}, http.MethodGet, "/discussion-one"},
		{[]string{"add", "-m", "Plan", "--json"}, http.MethodPost, ""},
		{[]string{"reply", "discussion-one", "-m", "Reply"}, http.MethodPost, "/discussion-one/messages"},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			calls := 0
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, tt.method, r.Method)
				assert.Equal(t, projectTrailTestPath+"/discussions"+tt.suffix, r.URL.Path)
				assert.Empty(t, r.Header.Get("If-Match"))
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
					"discussion": api.TrailDiscussionSummary{ID: "discussion-one", Title: "Plan", TrailID: projectTrailTestID},
					"message":    api.TrailDiscussionMessage{ID: "message-one", Body: "Plan"},
					"items":      []api.TrailDiscussionSummary{{ID: "discussion-one", Title: "Plan"}},
				}))
			})
			args := append([]string{"comment", "--project", "gh/acme", "--trail", projectTrailTestID}, tt.args...)
			out, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.NotEmpty(t, out)
		})
	}
}

func TestProjectTrailDiscussionWritesUseResourceETags(t *testing.T) {
	for _, tt := range []struct {
		args                 []string
		method, suffix, etag string
	}{
		{[]string{"resolve", "discussion-one"}, http.MethodPatch, "", `W/"discussion-version"`},
		{[]string{"unresolve", "discussion-one"}, http.MethodPatch, "", `W/"discussion-version"`},
		{[]string{"edit", "discussion-one", "message-one", "-m", "New body"}, http.MethodPatch, "/messages/message-one", `W/"message-version"`},
		{[]string{"delete", "discussion-one", "message-one", "--force"}, http.MethodDelete, "/messages/message-one", `W/"message-version"`},
	} {
		t.Run(tt.args[0], func(t *testing.T) {
			calls := 0
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method == http.MethodGet {
					assert.Equal(t, projectTrailTestPath+"/discussions/discussion-one", r.URL.Path)
					w.Header().Set("ETag", `W/"discussion-version"`)
					assert.NoError(t, json.NewEncoder(w).Encode(projectDiscussionResponse{
						Discussion: api.TrailDiscussionSummary{ID: "discussion-one"},
						Messages:   []api.TrailDiscussionMessage{{ID: "message-one", ETag: `W/"message-version"`}},
					}))
					return
				}
				assert.Equal(t, tt.method, r.Method)
				assert.Equal(t, projectTrailTestPath+"/discussions/discussion-one"+tt.suffix, r.URL.Path)
				assert.Equal(t, tt.etag, r.Header.Get("If-Match"))
				// A stale write fails once, never with an unconditional retry.
				w.WriteHeader(http.StatusPreconditionFailed)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"detail": "stale"}))
			})
			args := append([]string{"comment", "--project", "gh/acme", "--trail", projectTrailTestID}, tt.args...)
			_, _, err := executeProjectTrailTest(t, args...)
			require.ErrorContains(t, err, "changed since it was read")
			require.Equal(t, 2, calls)
		})
	}
}

func TestProjectTrailDiscussionMissingETagRefusesWrite(t *testing.T) {
	setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.NoError(t, json.NewEncoder(w).Encode(projectDiscussionResponse{Discussion: api.TrailDiscussionSummary{ID: "discussion-one"}}))
	})
	_, _, err := executeProjectTrailTest(t, "comment", "resolve", "discussion-one", "--project", "gh/acme", "--trail", projectTrailTestID)
	require.ErrorContains(t, err, "no ETag")
}
