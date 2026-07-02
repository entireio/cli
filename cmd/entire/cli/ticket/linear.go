package ticket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// linearAPIURL is the Linear GraphQL endpoint.
const linearAPIURL = "https://api.linear.app/graphql"

// issueIDPattern matches a Linear issue identifier such as "ENG-142", including
// when embedded in a branch name like "amy/eng-142-title".
var issueIDPattern = regexp.MustCompile(`([A-Za-z][A-Za-z0-9]*)-(\d+)`)

// httpDoer is the subset of *http.Client the Linear provider needs; injectable
// so tests can stub responses without network access.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// linearProvider implements Provider against Linear's GraphQL API. The team is
// the workspace team key (e.g. "ENG"), used when resolving workflow states.
type linearProvider struct {
	token string
	team  string
	url   string
	http  httpDoer
}

// newLinearProvider builds a Linear provider with a real HTTP client.
func newLinearProvider(token, team string) *linearProvider {
	return &linearProvider{
		token: token,
		team:  team,
		url:   linearAPIURL,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Name implements Provider.
func (p *linearProvider) Name() string { return string(PlatformLinear) }

// ResolveFromBranch implements Provider, extracting a "TEAM-123" identifier
// from a branch name (Linear's branch format embeds one).
func (p *linearProvider) ResolveFromBranch(branch string) (string, bool) {
	m := issueIDPattern.FindString(branch)
	if m == "" {
		return "", false
	}
	return strings.ToUpper(m), true
}

// Fetch implements Provider.
func (p *linearProvider) Fetch(ctx context.Context, id string) (Task, error) {
	start := time.Now()
	iss, err := p.fetchIssue(ctx, id)
	if err != nil {
		logging.Debug(ctx, "ticket fetch failed",
			slog.String("platform", p.Name()),
			slog.String("ticket", id),
			slog.String("error", err.Error()))
		return Task{}, err
	}
	logging.Debug(ctx, "ticket fetched",
		slog.String("platform", p.Name()),
		slog.String("ticket", id),
		slog.Duration("duration", time.Since(start)))

	labels := make([]string, 0, len(iss.Labels.Nodes))
	for _, l := range iss.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	comments := make([]Comment, 0, len(iss.Comments.Nodes))
	for _, c := range iss.Comments.Nodes {
		comments = append(comments, Comment{Author: c.User.Name, Body: c.Body})
	}
	return Task{
		ID:         iss.Identifier,
		Title:      iss.Title,
		Intent:     iss.Description,
		State:      normalizeLinearState(iss.State.Type),
		URL:        iss.URL,
		BranchName: iss.BranchName,
		Labels:     labels,
		Comments:   comments,
	}, nil
}

// Comment implements Provider.
func (p *linearProvider) Comment(ctx context.Context, id, body string) error {
	iss, err := p.fetchIssue(ctx, id)
	if err != nil {
		return err
	}
	const mutation = `mutation($issueId: String!, $body: String!) {
  commentCreate(input: {issueId: $issueId, body: $body}) { success }
}`
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	if err := p.do(ctx, mutation, map[string]any{"issueId": iss.ID, "body": body}, &out); err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return fmt.Errorf("linear did not accept the comment on %s", id)
	}
	return nil
}

// SetState implements Provider by resolving a workflow state on the issue's
// team and updating the issue.
func (p *linearProvider) SetState(ctx context.Context, id string, state State) error {
	iss, err := p.fetchIssue(ctx, id)
	if err != nil {
		return err
	}
	teamKey, _, err := parseIssueID(id)
	if err != nil {
		return err
	}
	stateID, err := p.resolveWorkflowStateID(ctx, teamKey, state)
	if err != nil {
		return err
	}
	const mutation = `mutation($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: {stateId: $stateId}) { success }
}`
	var out struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	if err := p.do(ctx, mutation, map[string]any{"id": iss.ID, "stateId": stateID}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return fmt.Errorf("linear did not accept the state change on %s", id)
	}
	return nil
}

// linearIssue is the subset of a Linear issue this provider reads.
type linearIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	BranchName  string `json:"branchName"`
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Comments struct {
		Nodes []struct {
			Body string `json:"body"`
			User struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"nodes"`
	} `json:"comments"`
}

// fetchIssue resolves a "TEAM-123" identifier to a Linear issue.
func (p *linearProvider) fetchIssue(ctx context.Context, id string) (linearIssue, error) {
	teamKey, number, err := parseIssueID(id)
	if err != nil {
		return linearIssue{}, err
	}
	const query = `query($team: String!, $number: Float!) {
  issues(filter: {team: {key: {eq: $team}}, number: {eq: $number}}, first: 1) {
    nodes {
      id identifier title description url branchName
      state { name type }
      labels { nodes { name } }
      comments(first: 50) { nodes { body user { name } } }
    }
  }
}`
	var out struct {
		Issues struct {
			Nodes []linearIssue `json:"nodes"`
		} `json:"issues"`
	}
	if err := p.do(ctx, query, map[string]any{"team": teamKey, "number": float64(number)}, &out); err != nil {
		return linearIssue{}, err
	}
	if len(out.Issues.Nodes) == 0 {
		return linearIssue{}, fmt.Errorf("linear issue %s not found", id)
	}
	return out.Issues.Nodes[0], nil
}

// resolveWorkflowStateID finds the workflow-state UUID on a team that best
// matches the normalized target state: a name match first, then a type match.
func (p *linearProvider) resolveWorkflowStateID(ctx context.Context, teamKey string, target State) (string, error) {
	const query = `query($team: String!) {
  workflowStates(filter: {team: {key: {eq: $team}}}, first: 50) {
    nodes { id name type }
  }
}`
	var out struct {
		WorkflowStates struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}
	if err := p.do(ctx, query, map[string]any{"team": teamKey}, &out); err != nil {
		return "", err
	}

	wantName := stateDisplayName(target)
	for _, n := range out.WorkflowStates.Nodes {
		if strings.EqualFold(n.Name, wantName) {
			return n.ID, nil
		}
	}
	wantType := stateLinearType(target)
	for _, n := range out.WorkflowStates.Nodes {
		if n.Type == wantType {
			return n.ID, nil
		}
	}
	return "", fmt.Errorf("no linear workflow state found for %q on team %s", target, teamKey)
}

// do executes a GraphQL request and unmarshals the "data" field into out.
func (p *linearProvider) do(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("encode linear request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build linear request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", p.token)

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("linear request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read linear response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode linear response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("linear API error: %s", envelope.Errors[0].Message)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode linear data: %w", err)
		}
	}
	return nil
}

// CanonicalID implements Provider, normalizing a Linear issue URL or a
// "team-142" identifier into the canonical "TEAM-142" form. It parses locally
// and makes no API call.
func (p *linearProvider) CanonicalID(raw string) (string, bool) {
	teamKey, number, err := parseIssueID(raw)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s-%d", teamKey, number), true
}

// parseIssueID splits a Linear identifier like "ENG-142" into an uppercased
// team key and issue number.
func parseIssueID(id string) (teamKey string, number int, err error) {
	m := issueIDPattern.FindStringSubmatch(strings.TrimSpace(id))
	if m == nil {
		return "", 0, fmt.Errorf("invalid Linear issue id %q (expected e.g. ENG-142)", id)
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, fmt.Errorf("invalid Linear issue number in %q: %w", id, err)
	}
	return strings.ToUpper(m[1]), n, nil
}

// normalizeLinearState maps a Linear workflow-state type to a normalized State.
func normalizeLinearState(linearType string) State {
	switch linearType {
	case "completed", "canceled":
		return StateDone
	case "started":
		return StateInProgress
	case "unstarted", "backlog", "triage":
		return StateTodo
	default:
		return StateUnknown
	}
}

// stateDisplayName is the workflow-state name typically used for a State.
func stateDisplayName(s State) string {
	switch s {
	case StateTodo:
		return "Todo"
	case StateInProgress:
		return "In Progress"
	case StateInReview:
		return "In Review"
	case StateDone:
		return "Done"
	case StateUnknown:
		return ""
	default:
		return ""
	}
}

// stateLinearType is the Linear workflow-state type a State maps onto.
func stateLinearType(s State) string {
	switch s {
	case StateTodo:
		return "unstarted"
	case StateInProgress, StateInReview:
		return "started"
	case StateDone:
		return "completed"
	case StateUnknown:
		return ""
	default:
		return ""
	}
}
