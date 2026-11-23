package cli

import (
	"context"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/recap"
)

// recapTeamTimeout bounds the call to the repo's processing cell. Its team
// side is best-effort, so a slow foreign cell must not hold up the personal
// side recap already has.
const recapTeamTimeout = 5 * time.Second

// recapSource fetches /me/recap for a window. home is the caller's home cell,
// which holds their own activity. team is the repo's processing cell when it is
// a different cell, which holds the repo's team rollups; nil when home is that
// cell or the repo is unknown. Immutable after construction, so the TUI's
// concurrent fetches can share it.
type recapSource struct {
	home        *api.Client
	team        *api.Client
	repo        string
	teamTimeout time.Duration
	// teamUnavailable is set when the repo is processed on another cell but
	// no client for it could be built: the home cell's team side would be
	// the wrong cell's zeros, so fetch drops it instead.
	teamUnavailable bool
}

// fetch returns the recap for [since, until). With a separate team cell it
// asks both cells in parallel and keeps each side from the cell that holds it.
// A home failure fails the fetch; a team failure only drops the team side.
func (s recapSource) fetch(ctx context.Context, since, until time.Time) (*recap.MeRecapResponse, error) {
	if s.team == nil || s.repo == "" {
		resp, err := recap.FetchMeRecap(ctx, s.home, since, until, s.repo, 0)
		if err != nil {
			return nil, err //nolint:wrapcheck // callers render recap fetch errors
		}
		if s.teamUnavailable {
			recap.ClearTeam(resp)
		}
		return resp, nil
	}
	var teamResp *recap.MeRecapResponse
	var teamErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		teamCtx, cancel := context.WithTimeout(ctx, s.teamTimeout)
		defer cancel()
		teamResp, teamErr = recap.FetchMeRecap(teamCtx, s.team, since, until, s.repo, 0)
	}()
	home, err := recap.FetchMeRecap(ctx, s.home, since, until, s.repo, 0)
	<-done
	if err != nil {
		return nil, err //nolint:wrapcheck // callers render recap fetch errors
	}
	if teamErr != nil {
		logging.Debug(ctx, "recap team side unavailable from the repo's cell", "error", teamErr.Error())
		teamResp = nil
	}
	recap.MergeTeam(home, teamResp)
	return home, nil
}

// recapTeamCell returns the repo cell to read the team side from, or nil when
// recap should keep to the home cell: the repo is unknown, it is processed on
// the home cell, or ENTIRE_API_BASE_URL pins one cell (the same rule as entire
// api's repo routing; see auth.BaseURLOverridePinsCell).
func recapTeamCell(homeBaseURL string, repoCell *auth.CellTarget, baseURLPinsCell bool) *auth.CellTarget {
	if baseURLPinsCell || repoCell == nil || repoCell.BaseURL == "" {
		return nil
	}
	// A false "different" costs one same-cell call; it cannot hide data.
	if api.OriginOnly(homeBaseURL) == api.OriginOnly(repoCell.BaseURL) {
		return nil
	}
	return repoCell
}

// withTeamCell points source's team side at cell, the repo's processing cell
// when recapTeamCell chose one (nil keeps the home cell). When no client for
// it can be built, the team side is marked unavailable rather than left to
// the home cell, which would show the wrong cell's zeros.
func withTeamCell(ctx context.Context, source recapSource, cell *auth.CellTarget, clientFor func(context.Context, *auth.CellTarget) (*api.Client, error)) recapSource {
	if cell == nil || source.repo == "" {
		return source
	}
	team, err := clientFor(ctx, cell)
	if err != nil {
		logging.Debug(ctx, "recap cannot reach the repo's cell; showing the personal side only", "error", err.Error())
		source.teamUnavailable = true
		return source
	}
	source.team = team
	return source
}
