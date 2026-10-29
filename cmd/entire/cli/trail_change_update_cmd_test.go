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

// runTrailChangeUpdate runs a trail subcommand against a fake repository whose
// PATCH records each request body and answers with respond.
func runTrailChangeUpdate(t *testing.T, project bool, args []string, respond func(request map[string]any) api.TrailResource) (patches []map[string]any, stdout string, err error) {
	t.Helper()
	if project {
		setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
			if !serveWorkingProjectRead(t, w, r) {
				t.Errorf("unexpected project request: %s %s", r.Method, r.URL.Path)
				http.NotFound(w, r)
			}
		})
	}
	setupWorkingRepoClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/trails/gh/acme/widget/7":
			assert.False(t, project, "project mode must resolve through its owned route")
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailResource{ID: projectTrailTestChange, Number: 7, Branch: "feature/work"}))
		case "PATCH /api/v1/trails/gh/acme/widget/7":
			var request map[string]any
			assert.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			patches = append(patches, request)
			assert.NoError(t, json.NewEncoder(w).Encode(api.TrailUpdateResponse{Trail: respond(request)}))
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
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true
	cmd.SetArgs(append([]string{args[0], selector, "--repo", "gh/acme/widget"}, args[1:]...))
	err = cmd.Execute()
	return patches, out.String(), err
}

func echoReadiness(request map[string]any) api.TrailResource {
	ready, _ := request["ready_for_review"].(bool) //nolint:errcheck // absent means false
	return api.TrailResource{Number: 7, Branch: "feature/work", Base: "main", ReadyForReview: &ready}
}

func TestTrailReady_SendsReadinessToTheSelectedChange(t *testing.T) {
	// Serial: replaces global clients.
	for _, project := range []bool{false, true} {
		for _, tc := range []struct {
			args  []string
			ready bool
			says  string
		}{
			{[]string{"ready"}, true, "ready for review"},
			{[]string{"ready", "--undo"}, false, "to draft"},
		} {
			t.Run(fmt.Sprintf("project=%t/%v", project, tc.args), func(t *testing.T) {
				patches, out, err := runTrailChangeUpdate(t, project, tc.args, echoReadiness)
				require.NoError(t, err)
				require.Equal(t, []map[string]any{{"ready_for_review": tc.ready}}, patches, "only readiness is sent")
				assert.Contains(t, out, tc.says)
				assert.Contains(t, out, "trail #")
			})
		}
	}
}

func TestTrailReady_FailsWhenTheServerDoesNotRecordReadiness(t *testing.T) {
	// Serial: replaces global clients.
	notTracked := func(map[string]any) api.TrailResource {
		ready := false
		return api.TrailResource{Number: 7, Branch: "feature/work", ReadyForReview: &ready}
	}
	_, out, err := runTrailChangeUpdate(t, true, []string{"ready"}, notTracked)
	require.ErrorIs(t, err, errTrailNotMarkedReady)
	assert.Empty(t, out, "must not claim success")
}

func TestTrailReady_TrustsTheRequestWhenTheServerOmitsReadiness(t *testing.T) {
	// Serial: replaces global clients.
	omitted := func(map[string]any) api.TrailResource {
		return api.TrailResource{Number: 7, Branch: "feature/work"}
	}
	_, out, err := runTrailChangeUpdate(t, true, []string{"ready"}, omitted)
	require.NoError(t, err)
	assert.Contains(t, out, "ready for review")
}

func TestTrailRetarget_SendsOnlyTheBase(t *testing.T) {
	// Serial: replaces global clients.
	for _, project := range []bool{false, true} {
		t.Run(fmt.Sprintf("project=%t", project), func(t *testing.T) {
			patches, out, err := runTrailChangeUpdate(t, project, []string{"retarget", "--base", " release/1.2 "}, func(request map[string]any) api.TrailResource {
				base, _ := request["base"].(string) //nolint:errcheck // asserted below
				return api.TrailResource{Number: 7, Branch: "feature/work", Base: base}
			})
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{"base": "release/1.2"}}, patches)
			assert.Contains(t, out, "onto release/1.2")
		})
	}
}

func TestTrailRetarget_RequiresABase(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"retarget", "7"}, {"retarget", "7", "--base", "  "}} {
		cmd := newTrailCmdForMode(true)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SilenceUsage = true
		cmd.SetArgs(args)
		require.ErrorContains(t, cmd.Execute(), "--base is required", "args %v", args)
	}
}
