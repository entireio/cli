package cli

import (
	"context"
	"strings"
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
}

// fetch returns the recap for [since, until). With a separate team cell it
// asks both cells in parallel and keeps each side from the cell that holds it.
// A home failure fails the fetch; a team failure only drops the team side.
func (s recapSource) fetch(ctx context.Context, since, until time.Time) (*recap.MeRecapResponse, error) {
	if s.team == nil || s.repo == "" {
		return recap.FetchMeRecap(ctx, s.home, since, until, s.repo, 0) //nolint:wrapcheck // callers render recap fetch errors
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
	if normalizeCellURL(homeBaseURL) == normalizeCellURL(repoCell.BaseURL) {
		return nil
	}
	return repoCell
}

// normalizeCellURL compares cell origins without trailing slashes or case.
// A false "different" costs one same-cell call; it cannot hide data.
func normalizeCellURL(u string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(u), "/"))
}
