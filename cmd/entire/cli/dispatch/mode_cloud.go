package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// newDispatchCellClient builds the client for the entire-api cell that
// generates the dispatch — jurisdiction's, or the caller's home cell when it is
// empty — and reports the caller's home jurisdiction. Tests swap in a client
// for an httptest server.
var newDispatchCellClient = defaultNewDispatchCellClient

func defaultNewDispatchCellClient(ctx context.Context, insecureHTTP bool, jurisdiction string) (*api.Client, string, error) {
	factory, err := auth.NewEntireAPICellClientFactory(ctx, insecureHTTP)
	if err != nil {
		return nil, "", err //nolint:wrapcheck // runServer wraps; ErrNotLoggedIn must stay matchable
	}
	var target *auth.CellTarget
	if j := strings.TrimSpace(jurisdiction); j != "" {
		target = &auth.CellTarget{Jurisdiction: j}
	}
	client, err := factory.ClientFor(ctx, target)
	if err != nil {
		return nil, "", err //nolint:wrapcheck // runServer wraps
	}
	home, _ := factory.HomeJurisdiction() //nolint:errcheck // best-effort label for a repo-not-found error
	return client, home, nil
}

func runServer(ctx context.Context, opts Options) (*Dispatch, error) {
	now := nowUTC()
	sinceInput := strings.TrimSpace(opts.Since)
	if sinceInput == "" {
		sinceInput = "7d"
	}
	since, err := ParseSinceAtNow(sinceInput, now)
	if err != nil {
		return nil, err
	}
	until, err := ParseUntilAtNow(opts.Until, now)
	if err != nil {
		return nil, err
	}
	normalizedSince, normalizedUntil := NormalizeWindow(since, until)
	if !normalizedSince.Before(normalizedUntil) {
		return nil, errors.New("--since must be before --until")
	}

	repos := append([]string(nil), opts.RepoPaths...)
	if len(repos) == 0 {
		repoRoot, err := paths.WorktreeRoot(ctx)
		if err != nil {
			return nil, fmt.Errorf("not in a git repository: %w", err)
		}
		repo, err := gitrepo.OpenPath(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("open repository: %w", err)
		}
		defer repo.Close()

		// The slug names the origin's forge (gh/ or et/), so a same-named
		// repo on the other forge cannot answer for this checkout.
		repoSlug, err := resolveOriginRepoSlug(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("%w (or pass --repos %s to name the repo)", err, RepoSlugShapes)
		}
		repos = []string{repoSlug}
	}

	// Local inputs are settled first: building the client already dials (login
	// refresh, cell catalog).
	client, home, err := newDispatchCellClient(ctx, opts.InsecureHTTPAuth, opts.Jurisdiction)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return nil, errors.New("dispatch requires login — run `entire login`")
	}
	if err != nil {
		return nil, fmt.Errorf("dispatch service: %w", err)
	}

	reqBody := CreateDispatchRequest{
		Repos: repos,
		Since: normalizedSince.Format(time.RFC3339),
		Until: normalizedUntil.Format(time.RFC3339),
		Voice: resolvedDispatchVoicePreference(opts.Voice),
	}
	response, err := NewCloudClient(client).CreateDispatch(ctx, reqBody, opts.Jurisdiction)
	if err != nil {
		// With no selector the home cell answered; say which one so the hint
		// can exclude it.
		var notFound *RepoNotFoundError
		if errors.As(err, &notFound) && notFound.Jurisdiction == "" {
			notFound.Home = home
		}
		return nil, err
	}

	dispatch := apiToDispatch(response)
	if strings.TrimSpace(dispatch.GeneratedText) == "" {
		return nil, errDispatchMissingMarkdown
	}
	return dispatch, nil
}

func apiToDispatch(response *APIRun) *Dispatch {
	if response == nil {
		return &Dispatch{}
	}

	repos := make([]RepoGroup, 0, len(response.Repos))
	for _, repo := range response.Repos {
		sections := make([]Section, 0, len(repo.Sections))
		for _, section := range repo.Sections {
			bullets := make([]Bullet, 0, len(section.Bullets))
			for _, bullet := range section.Bullets {
				bullets = append(bullets, Bullet{
					CheckpointID: bullet.CheckpointID,
					Text:         bullet.Text,
					Source:       bullet.Source,
					Branch:       bullet.Branch,
					CreatedAt:    parseAPITime(bullet.CreatedAt),
					Labels:       append([]string(nil), bullet.Labels...),
				})
			}
			sections = append(sections, Section{
				Label:   section.Label,
				Bullets: bullets,
			})
		}
		repos = append(repos, RepoGroup{
			FullName: repo.FullName,
			URL:      githubRepoURL(repo.FullName),
			Sections: sections,
		})
	}

	generatedText := strings.TrimSpace(response.GeneratedMarkdown)

	return &Dispatch{
		Window: Window{
			NormalizedSince:   parseAPITime(response.Window.NormalizedSince),
			NormalizedUntil:   parseAPITime(response.Window.NormalizedUntil),
			FirstCheckpointAt: parseAPITime(response.Window.FirstCheckpointCreatedAt),
			LastCheckpointAt:  parseAPITime(response.Window.LastCheckpointCreatedAt),
		},
		CoveredRepos:  append([]string(nil), response.CoveredRepos...),
		Repos:         repos,
		GeneratedText: generatedText,
	}
}

func parseAPITime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
