package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestResolveTrailBySelector_FindsBySelector(t *testing.T) {
	// Not t.Parallel(): the subtests share one httptest server closed on
	// return, so they must run synchronously before the deferred Close.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{
			Trails: []api.TrailResource{
				{ID: "trl_a", Number: 1, Branch: "feature/a", Title: "Alpha"},
				{ID: "trl_b", Number: 575, Branch: "feature/b", Title: "Bravo"},
			},
			Total: 2,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)

	cases := []struct {
		name     string
		selector string
		wantID   string
	}{
		{"by number", "575", "trl_b"},
		{"by id", "trl_a", "trl_a"},
		{"by branch", "feature/b", "trl_b"},
		{"trims whitespace", "  feature/a  ", "trl_a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, err := resolveTrailBySelector(context.Background(), client, "gh", "acme", "repo", tc.selector, "")
			if err != nil {
				t.Fatalf("resolveTrailBySelector: %v", err)
			}
			if found == nil || found.ID != tc.wantID {
				t.Fatalf("found = %#v, want ID %q", found, tc.wantID)
			}
		})
	}
}

func TestResolveTrailBySelector_NotFoundIsAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(api.TrailListResponse{Trails: []api.TrailResource{}}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := resolveTrailBySelector(context.Background(), client, "gh", "acme", "repo", "does-not-exist", "")
	if err == nil {
		t.Fatalf("expected error for missing trail, got found = %#v", found)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil on error", found)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error %q should name the selector", err)
	}
}

func TestDescribeTrailRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   api.TrailResource
		want string
	}{
		{"number and title", api.TrailResource{Number: 575, Title: "Add foo"}, "trail #575 (Add foo)"},
		{"number without title", api.TrailResource{Number: 575}, "trail #575"},
		{"title without number", api.TrailResource{Title: "Add foo"}, `trail "Add foo"`},
		{"neither", api.TrailResource{}, "trail"},
		{"title trimmed", api.TrailResource{Number: 1, Title: "  Add foo  "}, "trail #1 (Add foo)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Copy the input into a local so the parallel subtest never takes the
			// address of the shared range variable.
			in := tc.in
			got := describeTrailRef(&in)
			if got != tc.want {
				t.Fatalf("describeTrailRef(%#v) = %q, want %q", in, got, tc.want)
			}
		})
	}
}

func TestTrailCheckoutRejectsArgWithTrailFlag(t *testing.T) {
	t.Parallel()

	cmd := newTrailCheckoutCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"feature/b", "--trail", "575"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error combining a positional arg with --trail, got nil")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Fatalf("error = %q, want it to mention 'cannot combine'", err)
	}
}
