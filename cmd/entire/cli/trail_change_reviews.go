package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

const (
	// trailChangeReviewTimeout bounds the review lookups behind a change list.
	// The list is complete without them, so a slow cell only drops the state.
	trailChangeReviewTimeout = 3 * time.Second
	// trailChangeReviewLimit caps lookups for one list; changes beyond it are
	// listed without review state.
	trailChangeReviewLimit = 20
)

// trailChangeReviews is each reviewer's latest decision on one change. The
// server appends a record per approve/request-changes and never replaces one,
// so earlier records by the same reviewer are superseded, not cumulative.
type trailChangeReviews struct {
	Approved         []string
	ChangesRequested []string
}

func summarizeTrailApprovals(approvals []api.TrailApproval) trailChangeReviews {
	latest := map[string]api.TrailApproval{}
	for _, a := range approvals {
		if a.Author == "" {
			continue
		}
		if prev, ok := latest[a.Author]; !ok || !a.CreatedAt.Before(prev.CreatedAt) {
			latest[a.Author] = a
		}
	}
	var out trailChangeReviews
	for author, a := range latest {
		switch a.Event {
		case "approve":
			out.Approved = append(out.Approved, tuiutil.SanitizeTerminalLabel(author))
		case "request_changes":
			out.ChangesRequested = append(out.ChangesRequested, tuiutil.SanitizeTerminalLabel(author))
		}
	}
	slices.Sort(out.Approved)
	slices.Sort(out.ChangesRequested)
	return out
}

func (r trailChangeReviews) String() string {
	var parts []string
	if len(r.Approved) > 0 {
		parts = append(parts, "approved by "+strings.Join(r.Approved, ", "))
	}
	if len(r.ChangesRequested) > 0 {
		parts = append(parts, "changes requested by "+strings.Join(r.ChangesRequested, ", "))
	}
	if len(parts) == 0 {
		return "no reviews yet"
	}
	return strings.Join(parts, "; ")
}

// trailRepoAccess is an already-opened repository client, reused for changes
// in that repository instead of resolving its cell again.
type trailRepoAccess struct {
	Client   *api.Client
	BasePath string
	RepoID   string
}

// lookupTrailChangeReviews reads each change's approvals through its
// repository's trail route, keyed by change ID. Failures leave a change out:
// callers list it without state rather than failing the list.
func lookupTrailChangeReviews(ctx context.Context, insecure bool, target *projectTrailTarget, local trailRepoAccess, changes []api.ChangeSummary) map[string]trailChangeReviews {
	ctx, cancel := context.WithTimeout(ctx, trailChangeReviewTimeout)
	defer cancel()
	var (
		mu    sync.Mutex
		out   = map[string]trailChangeReviews{}
		repos = map[string]*trailRepoAccess{}
		wg    sync.WaitGroup
	)
	if local.Client != nil {
		repos[local.RepoID] = &local
	}
	byRepo := map[string][]api.ChangeSummary{}
	looked := 0
	for _, item := range changes {
		if item.Number <= 0 || looked >= trailChangeReviewLimit {
			continue
		}
		looked++
		byRepo[item.RepositoryID] = append(byRepo[item.RepositoryID], item)
	}
	for repoID, items := range byRepo {
		wg.Go(func() {
			access := repos[repoID]
			if access == nil {
				opened, err := openTrailChangeRepo(ctx, insecure, target, items[0])
				if err != nil {
					logging.Debug(ctx, "trail change review lookup skipped", "repository_id", repoID, "error", err.Error())
					return
				}
				access = opened
			}
			for _, item := range items {
				wg.Go(func() {
					reviews, err := readTrailChangeReviews(ctx, access, item.Number)
					if err != nil {
						logging.Debug(ctx, "trail change review lookup failed", "change_id", item.ID, "error", err.Error())
						return
					}
					mu.Lock()
					out[item.ID] = reviews
					mu.Unlock()
				})
			}
		})
	}
	wg.Wait()
	return out
}

func openTrailChangeRepo(ctx context.Context, insecure bool, target *projectTrailTarget, item api.ChangeSummary) (*trailRepoAccess, error) {
	if !validTrailChangeRepoName(item.Repository) {
		return nil, fmt.Errorf("repository name not resolved for change %s", item.ID)
	}
	client, repoID, err := newTrailAPIClient(ctx, insecure, target.Host, target.Project, item.Repository)
	if err != nil {
		return nil, err
	}
	if repoID != item.RepositoryID {
		return nil, fmt.Errorf("repository %s/%s does not match the trail's change", target.Project, item.Repository)
	}
	base, err := trailRepoBasePath(target.Host, target.Project, item.Repository, repoID)
	if err != nil {
		return nil, err
	}
	return &trailRepoAccess{Client: client, BasePath: base, RepoID: repoID}, nil
}

func readTrailChangeReviews(ctx context.Context, access *trailRepoAccess, number int) (trailChangeReviews, error) {
	resp, err := access.Client.Get(ctx, trailApprovalsPath(access.BasePath, number))
	if err != nil {
		return trailChangeReviews{}, fmt.Errorf("list trail approvals: %w", err)
	}
	defer resp.Body.Close()
	if err := checkTrailResponse(resp); err != nil {
		return trailChangeReviews{}, err
	}
	var out api.TrailApprovalsResponse
	if err := api.DecodeJSON(resp, &out); err != nil {
		return trailChangeReviews{}, fmt.Errorf("decode trail approvals: %w", err)
	}
	return summarizeTrailApprovals(out.Approvals), nil
}
