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

// requireSecureDispatchURL is the secure-base-URL guard used before the cloud
// client sends a bearer token. Tests swap it to allow httptest.NewServer
// (http://127.0.0.1:...) endpoints; production always routes through
// api.RequireSecureURL and rejects plain HTTP.
var requireSecureDispatchURL = api.RequireSecureURL

func runServer(ctx context.Context, opts Options) (*Dispatch, error) {
	baseURL := api.BaseURL()
	if opts.InsecureHTTPAuth {
		auth.EnableInsecureHTTP()
	} else {
		if err := requireSecureDispatchURL(baseURL); err != nil {
			return nil, fmt.Errorf("dispatch base URL: %w", err)
		}
	}

	token, err := lookupResourceToken(ctx, baseURL)
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return nil, errors.New("dispatch requires login — run `entire login`")
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}

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

		repoFullName, err := resolveRepoFullName(ctx, repo)
		if err != nil {
			return nil, err
		}
		repos = []string{repoFullName}
	}

	cloud := NewCloudClient(CloudConfig{BaseURL: baseURL, Token: token})
	reqBody := CreateDispatchRequest{
		Repos:    repos,
		Since:    normalizedSince.Format(time.RFC3339),
		Until:    normalizedUntil.Format(time.RFC3339),
		Generate: true,
		Voice:    resolvedDispatchVoicePreference(opts.Voice),
	}
	response, err := cloud.CreateDispatch(ctx, reqBody, opts.Jurisdiction)
	if err != nil {
		// With no selector the home cell answered; say which one, from the
		// token already in hand, so the hint can exclude it.
		var notFound *RepoNotFoundError
		if errors.As(err, &notFound) && notFound.Jurisdiction == "" {
			notFound.Home, _ = auth.HomeJurisdictionFromLoginJWT(token) //nolint:errcheck // best-effort label on an error already being returned
		}
		return nil, err
	}
	if err := checkDispatchJurisdiction(opts.Jurisdiction, response.Jurisdiction); err != nil {
		return nil, err
	}

	dispatch := apiToDispatch(response)
	if strings.TrimSpace(dispatch.GeneratedText) == "" {
		return nil, errDispatchMissingMarkdown
	}
	return dispatch, nil
}

// checkDispatchJurisdiction verifies the gateway generated the dispatch where
// --jurisdiction asked. The gateway echoes `jurisdiction` whenever the caller
// named one, so a missing echo means a gateway that ignored the selector and
// routed home — and a different echo means another region entirely. Both fail:
// a wrong-region result would be rendered labelled with a jurisdiction it did
// not come from, which is worse than no result. No selector, no check.
func checkDispatchJurisdiction(requested, stamped string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	stamped = strings.ToLower(strings.TrimSpace(stamped))
	switch {
	case stamped == "":
		return fmt.Errorf("the dispatch service ignored --jurisdiction %s (no jurisdiction in its response); it may predate jurisdiction-scoped dispatches — retry without the flag for a home dispatch", requested)
	case stamped != requested:
		return fmt.Errorf("dispatch was generated in jurisdiction %s, not the requested %s", strings.ToUpper(stamped), strings.ToUpper(requested))
	}
	return nil
}

func apiToDispatch(response *CreateDispatchResponse) *Dispatch {
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
	if generatedText == "" {
		generatedText = strings.TrimSpace(response.GeneratedText)
	}

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
