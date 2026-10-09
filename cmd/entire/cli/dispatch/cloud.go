package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// CloudClient generates dispatches on an entire-api cell: the caller's home
// cell, or the cell of the jurisdiction runServer was asked for.
type CloudClient struct {
	api          *api.Client
	pollInterval time.Duration
	budget       time.Duration
}

// The cell generates asynchronously: POST /me/dispatches answers 202 with a
// run still "generating", and GET /me/dispatches/{id} reports it until the
// run completes or fails.
const (
	dispatchesPath = "/api/v1/me/dispatches"

	dispatchStatusGenerating = "generating"
	dispatchStatusComplete   = "complete"
	dispatchStatusFailed     = "failed"

	defaultDispatchPollInterval = 2 * time.Second
	// dispatchBudget bounds the whole create-and-poll operation; api.Client
	// sets no timeout of its own. It outlasts the cell's two-minute stale
	// threshold, after which the cell reports a stuck run as failed, so it only
	// fires when the cell stops answering with a terminal status at all.
	dispatchBudget = 3 * time.Minute
)

func NewCloudClient(client *api.Client) *CloudClient {
	return &CloudClient{api: client, pollInterval: defaultDispatchPollInterval, budget: dispatchBudget}
}

// CreateDispatchRequest is the body of POST /me/dispatches. Repos must be
// forge-qualified (gh/owner/repo or et/project/repo).
type CreateDispatchRequest struct {
	Repos []string `json:"repos,omitempty"`
	Since string   `json:"since"`
	Until string   `json:"until"`
	Voice string   `json:"voice,omitempty"`
}

// APIRun is the cell's dispatch shape, served by both the create and the
// read route; only the fields the CLI renders are decoded.
type APIRun struct {
	ID                string    `json:"id"`
	Status            string    `json:"status"`
	CoveredRepos      []string  `json:"coveredRepos"`
	Window            APIWindow `json:"window"`
	Repos             []APIRepo `json:"repos"`
	GeneratedMarkdown string    `json:"generatedMarkdown"`
	ErrorMessage      *string   `json:"errorMessage"`
}

type APIWindow struct {
	NormalizedSince          string `json:"normalizedSince"`
	NormalizedUntil          string `json:"normalizedUntil"`
	FirstCheckpointCreatedAt string `json:"firstCheckpointCreatedAt"`
	LastCheckpointCreatedAt  string `json:"lastCheckpointCreatedAt"`
}

type APIRepo struct {
	FullName string       `json:"fullName"`
	Sections []APISection `json:"sections"`
}

type APISection struct {
	Label   string      `json:"label"`
	Bullets []APIBullet `json:"bullets"`
}

type APIBullet struct {
	CheckpointID string   `json:"checkpointId"`
	Text         string   `json:"text"`
	Source       string   `json:"source"`
	Branch       string   `json:"branch"`
	CreatedAt    string   `json:"createdAt"`
	Labels       []string `json:"labels"`
}

// RepoNotFoundError is the cell's 404 for a repo that is not placed in (or
// not visible in) the jurisdiction the request was routed to — a repo mirrored
// only in US, requested from an AU home, is simply unknown to the AU cell.
// Jurisdiction is the selector the caller sent ("" = home) and Home the
// caller's home jurisdiction when known (so callers know which cell answered
// when no selector was sent); Repos are the requested slugs the cell's
// message named; Message is its sentence; Cause is the underlying
// *api.HTTPError.
type RepoNotFoundError struct {
	Jurisdiction string
	Home         string
	Repos        []string
	Message      string
	Cause        error
}

// FailedJurisdiction is the jurisdiction whose cell answered "not found": the
// selector when one was sent, else the home jurisdiction ("" if unknown).
func (e *RepoNotFoundError) FailedJurisdiction() string {
	if j := strings.TrimSpace(e.Jurisdiction); j != "" {
		return j
	}
	return e.Home
}

const repoNotFoundPrefix = "repository not found"

func (e *RepoNotFoundError) Error() string {
	scope := "your home jurisdiction"
	if j := strings.TrimSpace(e.Jurisdiction); j != "" {
		scope = strings.ToUpper(j)
	}
	// Render our own sentence when the repos were parsed, so the wording does
	// not depend on (or double up with) the cell's prose; fall back to its
	// message only when parsing found nothing.
	sentence := e.Message
	if len(e.Repos) > 0 {
		sentence = repoNotFoundPrefix + ": " + strings.Join(e.Repos, ", ")
	}
	return "In " + scope + ": " + sentence + ". Pick a jurisdiction the repository is mirrored into (entire dispatch --jurisdiction <slug>), or mirror it there."
}

func (e *RepoNotFoundError) Unwrap() error { return e.Cause }

// statusError keeps the dispatch command's established non-2xx wording while
// exposing the shared *api.HTTPError underneath (errors.As /
// api.IsHTTPErrorStatus), so dispatch failures classify like any other API call.
type statusError struct {
	*api.HTTPError
}

func (e *statusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("dispatch service returned status %d", e.StatusCode)
	}
	return fmt.Sprintf("dispatch service returned status %d: %s", e.StatusCode, strconv.Quote(e.Message))
}

func (e *statusError) Unwrap() error { return e.HTTPError }

// CreateDispatch starts a dispatch on the client's cell and waits for it to
// finish. jurisdiction is the --jurisdiction selector the cell was picked by
// ("" = home); it only labels a repo-not-found error.
func (c *CloudClient) CreateDispatch(ctx context.Context, reqBody CreateDispatchRequest, jurisdiction string) (*APIRun, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budget)
	defer cancel()
	var run APIRun
	if err := c.doJSON(ctx, http.MethodPost, dispatchesPath, reqBody, &run); err != nil {
		var httpErr *api.HTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound &&
			strings.HasPrefix(strings.ToLower(httpErr.Message), repoNotFoundPrefix) {
			return nil, &RepoNotFoundError{
				Jurisdiction: jurisdiction,
				Repos:        parseNotFoundRepos(httpErr.Message, reqBody.Repos),
				Message:      httpErr.Message,
				Cause:        err,
			}
		}
		return nil, err
	}
	return c.waitForDispatch(ctx, &run)
}

// waitForDispatch polls a generating run until the cell reports it complete
// or failed.
func (c *CloudClient) waitForDispatch(ctx context.Context, run *APIRun) (*APIRun, error) {
	timer := time.NewTimer(c.pollInterval)
	defer timer.Stop()
	for run.Status == dispatchStatusGenerating {
		if run.ID == "" {
			return nil, errors.New("dispatch service returned a generating dispatch without an id")
		}
		select {
		case <-ctx.Done():
			return nil, stillGeneratingError(ctx, run.ID)
		case <-timer.C:
		}
		// Go 1.23+ timers: Reset after a receive needs no drain.
		timer.Reset(c.pollInterval)
		var next APIRun
		if err := c.doJSON(ctx, http.MethodGet, dispatchesPath+"/"+url.PathEscape(run.ID), nil, &next); err != nil {
			// The budget can run out mid-poll as well as between polls.
			if ctx.Err() != nil {
				return nil, stillGeneratingError(ctx, run.ID)
			}
			return nil, err
		}
		run = &next
	}
	switch run.Status {
	case dispatchStatusComplete:
		return run, nil
	case dispatchStatusFailed:
		if run.ErrorMessage != nil && strings.TrimSpace(*run.ErrorMessage) != "" {
			return nil, fmt.Errorf("dispatch generation failed: %s", strconv.Quote(*run.ErrorMessage))
		}
		return nil, errors.New("dispatch generation failed")
	default:
		return nil, fmt.Errorf("dispatch service returned unknown status %s", strconv.Quote(run.Status))
	}
}

func stillGeneratingError(ctx context.Context, id string) error {
	return fmt.Errorf("dispatch %s is still generating: %w", id, ctx.Err())
}

// parseNotFoundRepos pulls the slugs out of a "repository not found: a/b, c/d"
// message, keeping only the repos this request asked for (in the request's
// spelling) so downstream lookups are bounded by CloudRepoLimit and never fan
// out over arbitrary prose. The cell may echo a slug bare or forge-prefixed;
// both match. Best-effort: an unexpected format yields nil and the caller
// still has the message.
func parseNotFoundRepos(message string, requested []string) []string {
	_, rest, ok := strings.Cut(message, ":")
	if !ok {
		return nil
	}
	named := normalizeScopeValues(strings.Split(rest, ","))
	var repos []string
	for _, repo := range requested {
		for _, candidate := range named {
			if echoedSlugMatches(candidate, repo) {
				repos = append(repos, repo)
				break
			}
		}
	}
	return repos
}

func (c *CloudClient) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	var (
		resp *http.Response
		err  error
	)
	if reqBody != nil {
		resp, err = c.api.Post(ctx, path, reqBody)
	} else {
		resp, err = c.api.Get(ctx, path)
	}
	if err != nil {
		return err //nolint:wrapcheck // api.Client already names the method and path
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("dispatch requires login — run `entire login`")
	}
	if checkErr := api.CheckResponse(resp); checkErr != nil {
		logging.Warn(ctx, "dispatch request failed", "method", method, "path", path, "status_code", resp.StatusCode)
		httpErr, ok := checkErr.(*api.HTTPError) //nolint:errorlint // CheckResponse returns the concrete type unwrapped
		if !ok {
			return fmt.Errorf("dispatch service: %w", checkErr)
		}
		return &statusError{HTTPError: httpErr}
	}
	if err := api.DecodeJSON(resp, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
