package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubmitTrailApprovalUsesHTTPStatusNotResponseBody(t *testing.T) {
	// Serial: replaces global clients.
	for _, project := range []bool{false, true} {
		for _, verb := range []string{"approve", "request-changes"} {
			for _, response := range []struct {
				name   string
				status int
				body   string
			}{
				{"empty", http.StatusNoContent, ""},
				{"empty OK", http.StatusOK, ""},
				{"non-JSON", http.StatusCreated, "accepted"},
				{"rejected", http.StatusForbidden, "forbidden"},
			} {
				t.Run(fmt.Sprintf("project=%t/%s/%s", project, verb, response.name), func(t *testing.T) {
					if project {
						setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
							if !serveWorkingProjectRead(t, w, r) {
								t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
								http.NotFound(w, r)
							}
						})
					}
					posts := 0
					setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
						switch r.Method + " " + r.URL.Path {
						case "GET /api/v1/trails/gh/acme/widget/7":
							assert.False(t, project, "project mode must resolve through its owned route")
							assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work"}))
						case "POST /api/v1/trails/gh/acme/widget/7/approvals":
							posts++
							var request api.TrailApprovalRequest
							assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
							event := "approve"
							if verb == "request-changes" {
								event = "request_changes"
							}
							assert.Equal(t, event, request.Event)
							w.WriteHeader(response.status)
							_, err := fmt.Fprint(w, response.body)
							assert.NoError(t, err)
						default:
							t.Errorf("unexpected repository request: %s %s", r.Method, r.URL.Path)
							http.NotFound(w, r)
						}
					})
					selector := "7"
					if project {
						selector = "42"
					}
					cmd := newTrailCmdForMode(project)
					var out, errOut bytes.Buffer
					cmd.SetOut(&out)
					cmd.SetErr(&errOut)
					cmd.SilenceUsage = true
					cmd.SetArgs([]string{verb, selector, "--repo", "gh/acme/widget", "-m", "Review decision"})
					err := cmd.Execute()
					require.Equal(t, 1, posts, "must not retry a successful mutation because of its body")
					if response.status == http.StatusForbidden {
						require.Error(t, err)
						require.Empty(t, out.String())
					} else {
						require.NoError(t, err)
						require.Contains(t, out.String(), "trail #"+selector)
					}
				})
			}
		}
	}
}
