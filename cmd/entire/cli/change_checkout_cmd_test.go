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

func TestResolveChangeBySelector_FindsBySelector(t *testing.T) {
	// Not t.Parallel(): the subtests share one httptest server closed on
	// return, so they must run synchronously before the deferred Close.
	alpha := api.ChangeResource{ID: "trl_a", Number: 1, Branch: "feature/a", Title: "Alpha"}
	bravo := api.ChangeResource{ID: "trl_b", Number: 575, Branch: "feature/b", Title: "Bravo"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A numeric selector resolves through the direct number route; the other
		// selectors fall back to scanning the list.
		var payload any = api.ChangeListResponse{Changes: []api.ChangeResource{alpha, bravo}, Total: 2}
		switch r.URL.Path {
		case changeTestBasePath + "/1":
			payload = alpha
		case changeTestBasePath + "/575":
			payload = bravo
		}
		if err := json.NewEncoder(w).Encode(payload); err != nil {
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
			found, err := resolveChangeBySelector(context.Background(), client, "gh", "acme", "repo", tc.selector, "")
			if err != nil {
				t.Fatalf("resolveChangeBySelector: %v", err)
			}
			if found == nil || found.ID != tc.wantID {
				t.Fatalf("found = %#v, want ID %q", found, tc.wantID)
			}
		})
	}
}

func TestResolveChangeBySelector_NotFoundIsAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(api.ChangeListResponse{Changes: []api.ChangeResource{}}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	found, err := resolveChangeBySelector(context.Background(), client, "gh", "acme", "repo", "does-not-exist", "")
	if err == nil {
		t.Fatalf("expected error for missing change, got found = %#v", found)
	}
	if found != nil {
		t.Fatalf("found = %#v, want nil on error", found)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Fatalf("error %q should name the selector", err)
	}
}

func TestDescribeChangeRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   api.ChangeResource
		want string
	}{
		{"number and title", api.ChangeResource{Number: 575, Title: "Add foo"}, "change #575 (Add foo)"},
		{"number without title", api.ChangeResource{Number: 575}, "change #575"},
		{"title without number", api.ChangeResource{Title: "Add foo"}, `change "Add foo"`},
		{"neither", api.ChangeResource{}, "change"},
		{"title trimmed", api.ChangeResource{Number: 1, Title: "  Add foo  "}, "change #1 (Add foo)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Copy the input into a local so the parallel subtest never takes the
			// address of the shared range variable.
			in := tc.in
			got := describeChangeRef(&in)
			if got != tc.want {
				t.Fatalf("describeChangeRef(%#v) = %q, want %q", in, got, tc.want)
			}
		})
	}
}

func TestChangeCheckoutRejectsArgWithChangeFlag(t *testing.T) {
	t.Parallel()

	cmd := newChangeCheckoutCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"feature/b", "--change", "575"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error combining a positional arg with --change, got nil")
	}
	if !strings.Contains(err.Error(), "cannot combine") {
		t.Fatalf("error = %q, want it to mention 'cannot combine'", err)
	}
}

func TestChangeCheckoutHasWorktreeFlag(t *testing.T) {
	t.Parallel()

	cmd := newChangeCheckoutCmd()
	flag := cmd.Flags().Lookup("worktree")
	if flag == nil {
		t.Fatal("worktree flag not registered")
	}
	if flag.Value.Type() != "bool" {
		t.Fatalf("worktree flag type = %q, want bool", flag.Value.Type())
	}
}
