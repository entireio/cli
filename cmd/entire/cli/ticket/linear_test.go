package ticket

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDoer returns a canned HTTP response for the Linear provider tests.
type fakeDoer struct {
	status int
	body   string
}

func (f fakeDoer) Do(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

func TestParseIssueID(t *testing.T) {
	t.Parallel()

	team, num, err := parseIssueID("ENG-142")
	require.NoError(t, err)
	assert.Equal(t, "ENG", team)
	assert.Equal(t, 142, num)

	team, num, err = parseIssueID("eng-7")
	require.NoError(t, err)
	assert.Equal(t, "ENG", team)
	assert.Equal(t, 7, num)

	_, _, err = parseIssueID("nope")
	require.Error(t, err)
}

func TestLinearResolveFromBranch(t *testing.T) {
	t.Parallel()

	p := newLinearProvider("t", "ENG")

	id, ok := p.ResolveFromBranch("amy/eng-142-add-rate-limiting")
	assert.True(t, ok)
	assert.Equal(t, "ENG-142", id)

	_, ok = p.ResolveFromBranch("main")
	assert.False(t, ok)
}

func TestLinearCanonicalID(t *testing.T) {
	t.Parallel()

	p := newLinearProvider("t", "MOH")

	cases := map[string]struct {
		in   string
		want string
		ok   bool
	}{
		"bare id":   {"MOH-57", "MOH-57", true},
		"lowercase": {"moh-57", "MOH-57", true},
		"full url":  {"https://linear.app/mohit-personal/issue/MOH-57/print-hello-world", "MOH-57", true},
		"no id":     {"just some words", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := p.CanonicalID(tc.in)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestNormalizeLinearState(t *testing.T) {
	t.Parallel()

	assert.Equal(t, StateDone, normalizeLinearState("completed"))
	assert.Equal(t, StateInProgress, normalizeLinearState("started"))
	assert.Equal(t, StateTodo, normalizeLinearState("backlog"))
	assert.Equal(t, StateUnknown, normalizeLinearState("mystery"))
}

func TestLinearFetch(t *testing.T) {
	t.Parallel()

	const body = `{"data":{"issues":{"nodes":[{
	  "id":"uuid-1","identifier":"ENG-142","title":"Add rate limiting",
	  "description":"throttle /export","url":"https://linear.app/x/ENG-142",
	  "state":{"name":"In Progress","type":"started"},
	  "labels":{"nodes":[{"name":"backend"}]},
	  "comments":{"nodes":[{"body":"looks good","user":{"name":"amy"}}]}
	}]}}}`

	p := &linearProvider{token: "t", team: "ENG", url: "https://example.test", http: fakeDoer{status: http.StatusOK, body: body}}

	task, err := p.Fetch(context.Background(), "ENG-142")
	require.NoError(t, err)
	assert.Equal(t, "ENG-142", task.ID)
	assert.Equal(t, "Add rate limiting", task.Title)
	assert.Equal(t, "throttle /export", task.Intent)
	assert.Equal(t, StateInProgress, task.State)
	assert.Equal(t, []string{"backend"}, task.Labels)
	require.Len(t, task.Comments, 1)
	assert.Equal(t, "amy", task.Comments[0].Author)
}

func TestLinearFetch_NotFound(t *testing.T) {
	t.Parallel()

	p := &linearProvider{token: "t", team: "ENG", url: "https://example.test", http: fakeDoer{status: http.StatusOK, body: `{"data":{"issues":{"nodes":[]}}}`}}

	_, err := p.Fetch(context.Background(), "ENG-999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
