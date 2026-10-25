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
					"message":    api.TrailDiscussionMessage{ID: "message-one", Body: "Plan", ETag: `W/"message-version"`},
					"items":      []api.TrailDiscussionSummary{{ID: "discussion-one", Title: "Plan"}},
				}))
			})
			args := append([]string{"comment", "--project", "gh/acme", "--trail", projectTrailTestID}, tt.args...)
			out, _, err := executeProjectTrailTest(t, args...)
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.NotEmpty(t, out)
			// ETags drive conditional writes internally; they never reach output.
			assert.NotContains(t, out, "etag")
			assert.NotContains(t, out, "-version")
		})
	}
}

// Project trail discussions are written only from the web app, so the CLI
// offers no write verb an agent could reach for.
func TestProjectTrailCommentIsReadOnly(t *testing.T) {
	comment, _, err := newTrailCmdForMode(true).Find([]string{"comment"})
	require.NoError(t, err)
	var verbs []string
	for _, child := range comment.Commands() {
		verbs = append(verbs, child.Name())
	}
	assert.ElementsMatch(t, []string{"list", "show"}, verbs)
	assert.Contains(t, comment.Long, "web app")
}

// An empty discussion list names the trail by the number the user knows it by
// when resolution learned one, and by the ULID they passed otherwise.
func TestProjectTrailEmptyDiscussionsNameTheTrail(t *testing.T) {
	for _, tt := range []struct{ selector, want string }{
		{"42", "No discussions on trail #42"},
		{projectTrailTestID, "No discussions on trail " + projectTrailTestID},
	} {
		t.Run(tt.selector, func(t *testing.T) {
			setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/gh/acme/trails/42":
					assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
				case projectTrailTestPath + "/discussions":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"items": []any{}}))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})
			out, _, err := executeProjectTrailTest(t, "comment", "list", "--trail", tt.selector, "--project", "gh/acme")
			require.NoError(t, err)
			require.Equal(t, tt.want+"\n", out)
		})
	}
}
