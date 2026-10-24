package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

// recapServer serves one canned /me/recap response (or status) and counts
// calls, standing in for one entire-api cell.
func recapServer(t *testing.T, status int, resp *recap.MeRecapResponse) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("repo") != "01REPO" {
			t.Errorf("repo param = %q, want 01REPO", r.URL.Query().Get("repo"))
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func homeRecap() *recap.MeRecapResponse {
	return &recap.MeRecapResponse{
		Summary: recap.Summary{Me: recap.SummaryTotals{Sessions: 3}, Team: &recap.SummaryTotals{}},
	}
}

func teamRecap() *recap.MeRecapResponse {
	return &recap.MeRecapResponse{Summary: recap.Summary{Team: &recap.SummaryTotals{Sessions: 40}}}
}

func TestRecapSource_SameCellMakesOneCall(t *testing.T) {
	t.Parallel()
	home, homeCalls := recapServer(t, http.StatusOK, homeRecap())
	src := recapSource{home: api.NewClientWithBaseURL("tok", home.URL), repo: "01REPO", teamTimeout: time.Second}

	resp, err := src.fetch(t.Context(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if homeCalls.Load() != 1 || resp.Summary.Team == nil {
		t.Errorf("calls=%d team=%+v, want one call with the home cell's team side kept", homeCalls.Load(), resp.Summary.Team)
	}
}

func TestRecapSource_TeamFromRepoCell(t *testing.T) {
	t.Parallel()
	home, _ := recapServer(t, http.StatusOK, homeRecap())
	team, teamCalls := recapServer(t, http.StatusOK, teamRecap())
	src := recapSource{
		home: api.NewClientWithBaseURL("tok", home.URL), team: api.NewClientWithBaseURL("tok", team.URL),
		repo: "01REPO", teamTimeout: time.Second,
	}

	resp, err := src.fetch(t.Context(), time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if teamCalls.Load() != 1 {
		t.Errorf("team cell calls = %d, want 1", teamCalls.Load())
	}
	if resp.Summary.Me.Sessions != 3 || resp.Summary.Team == nil || resp.Summary.Team.Sessions != 40 {
		t.Errorf("summary = %+v, want me from home and team from the repo cell", resp.Summary)
	}
}

func TestRecapSource_TeamCellFailureShowsPersonalSideOnly(t *testing.T) {
	t.Parallel()
	for name, status := range map[string]int{"server error": http.StatusInternalServerError, "forbidden": http.StatusForbidden} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			home, _ := recapServer(t, http.StatusOK, homeRecap())
			team, _ := recapServer(t, status, nil)
			src := recapSource{
				home: api.NewClientWithBaseURL("tok", home.URL), team: api.NewClientWithBaseURL("tok", team.URL),
				repo: "01REPO", teamTimeout: time.Second,
			}
			resp, err := src.fetch(t.Context(), time.Now().Add(-time.Hour), time.Now())
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if resp.Summary.Me.Sessions != 3 || resp.Summary.Team != nil {
				t.Errorf("summary = %+v, want the personal side and no team side (not zeros)", resp.Summary)
			}
		})
	}
}

func TestRecapSource_HomeFailureFails(t *testing.T) {
	t.Parallel()
	home, _ := recapServer(t, http.StatusInternalServerError, nil)
	team, _ := recapServer(t, http.StatusOK, teamRecap())
	src := recapSource{
		home: api.NewClientWithBaseURL("tok", home.URL), team: api.NewClientWithBaseURL("tok", team.URL),
		repo: "01REPO", teamTimeout: time.Second,
	}
	if _, err := src.fetch(t.Context(), time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("fetch succeeded with a failing home cell")
	}
}

func TestRecapTeamCell(t *testing.T) {
	t.Parallel()
	eu := &auth.CellTarget{BaseURL: "https://aws-eu-central-1.api.entire.io", Jurisdiction: "eu"}
	tests := []struct {
		name     string
		home     string
		cell     *auth.CellTarget
		pinsCell bool
		want     *auth.CellTarget
	}{
		{"repo on another cell", "https://aws-us-east-2.api.entire.io", eu, false, eu},
		{"same cell", "https://aws-eu-central-1.api.entire.io", eu, false, nil},
		{"same cell, different spelling", "HTTPS://aws-eu-central-1.api.entire.io/", eu, false, nil},
		{"unknown repo", "https://aws-us-east-2.api.entire.io", nil, false, nil},
		{"override pins a cell", "http://127.0.0.1:8082", eu, true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := recapTeamCell(tt.home, tt.cell, tt.pinsCell); got != tt.want {
				t.Errorf("recapTeamCell = %+v, want %+v", got, tt.want)
			}
		})
	}
}
