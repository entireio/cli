package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

func TestFetchTrailMergeability(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		if err := json.NewEncoder(w).Encode(api.TrailMergeabilityResponse{
			ApprovalGatePassed: true,
			ChecksPassed:       true,
			ChecksStatus:       "success",
			ComparisonStatus:   "available",
			BehindBy:           0,
			Mergeable:          true,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	m, err := fetchTrailMergeability(t.Context(), client, "gh", "acme", "repo", 575)
	if err != nil {
		t.Fatalf("fetchTrailMergeability: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Fatalf("method = %q, want GET", gotMethod)
	}
	if want := "/api/v1/trails/gh/acme/repo/575/mergeability"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if !m.Mergeable {
		t.Fatalf("mergeable = false, want true")
	}
}

func TestFetchTrailMergeability_SurfacesServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		if err := json.NewEncoder(w).Encode(map[string]string{"error": "Trail not found"}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	client := api.NewClientWithBaseURL("tok", srv.URL)
	if _, err := fetchTrailMergeability(t.Context(), client, "gh", "acme", "repo", 999); err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestMergeTrailByNumber(t *testing.T) {
	t.Parallel()

	t.Run("merges via the integer number path and accepts ok:true", func(t *testing.T) {
		t.Parallel()
		var gotMethod, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			if err := json.NewEncoder(w).Encode(api.TrailMergeResponse{OK: true, MergeCommitSHA: "abc1234"}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		res, err := mergeTrailByNumber(t.Context(), client, "gh", "acme", "repo", 575)
		if err != nil {
			t.Fatalf("mergeTrailByNumber: %v", err)
		}
		if gotMethod != http.MethodPost {
			t.Fatalf("method = %q, want POST", gotMethod)
		}
		if want := "/api/v1/trails/gh/acme/repo/575/merge"; gotPath != want {
			t.Fatalf("path = %q, want %q", gotPath, want)
		}
		if res.MergeCommitSHA != "abc1234" {
			t.Fatalf("merge_commit_sha = %q, want abc1234", res.MergeCommitSHA)
		}
	})

	t.Run("treats a 2xx without ok:true as failure", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if err := json.NewEncoder(w).Encode(api.TrailMergeResponse{OK: false}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		if _, err := mergeTrailByNumber(t.Context(), client, "gh", "acme", "repo", 575); err == nil {
			t.Fatal("expected error for 2xx without ok:true, got nil")
		}
	})

	t.Run("surfaces the server's 422 gate-failure message", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			if err := json.NewEncoder(w).Encode(map[string]string{"error": "Branch is out of date with the base branch"}); err != nil {
				t.Errorf("encode response: %v", err)
			}
		}))
		defer srv.Close()

		client := api.NewClientWithBaseURL("tok", srv.URL)
		_, err := mergeTrailByNumber(t.Context(), client, "gh", "acme", "repo", 575)
		if err == nil {
			t.Fatal("expected error for 422, got nil")
		}
		if !strings.Contains(err.Error(), "out of date") {
			t.Fatalf("error = %v, want it to surface the server message", err)
		}
	})
}

func TestDescribeMergeBlockers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   api.TrailMergeabilityResponse
		want []string
	}{
		{
			name: "missing approvals",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: false, ChecksPassed: true, ComparisonStatus: "available"},
			want: []string{"required approvals are missing"},
		},
		{
			name: "failed checks",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: true, ChecksPassed: false, ChecksStatus: "failure", ComparisonStatus: "available"},
			want: []string{"CI checks failed"},
		},
		{
			name: "pending checks",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: true, ChecksPassed: false, ChecksStatus: "pending", ComparisonStatus: "available"},
			want: []string{"CI checks are still running"},
		},
		{
			name: "behind base",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: true, ChecksPassed: true, ComparisonStatus: "available", BehindBy: 3},
			want: []string{"branch is 3 commits behind the base branch"},
		},
		{
			name: "behind base by one",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: true, ChecksPassed: true, ComparisonStatus: "available", BehindBy: 1},
			want: []string{"branch is 1 commit behind the base branch"},
		},
		{
			name: "unknown comparison",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: true, ChecksPassed: true, ComparisonStatus: "unknown"},
			want: []string{"could not compare the branch with its base"},
		},
		{
			name: "multiple blockers in gate order",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: false, ChecksPassed: false, ChecksStatus: "failure", ComparisonStatus: "available", BehindBy: 2},
			want: []string{"required approvals are missing", "CI checks failed", "branch is 2 commits behind the base branch"},
		},
		{
			name: "defensive generic reason when nothing recognized",
			in:   api.TrailMergeabilityResponse{ApprovalGatePassed: true, ChecksPassed: true, ComparisonStatus: "available", BehindBy: 0},
			want: []string{"the trail is not in a mergeable state"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := describeMergeBlockers(&tt.in)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Fatalf("describeMergeBlockers() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPrintTrailMergeability(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printTrailMergeability(&buf, &api.TrailResource{Number: 7, Branch: "feature", Base: "main"}, &api.TrailMergeabilityResponse{
		ApprovalGatePassed: true,
		ChecksPassed:       false,
		ChecksStatus:       "pending",
		ComparisonStatus:   "available",
		BehindBy:           0,
		Mergeable:          false,
	})
	out := buf.String()
	for _, want := range []string{"Trail #7 (feature → main)", "Approvals:  ✓", "Checks:     ✗ (pending)", "Mergeable:  ✗"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
