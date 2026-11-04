package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
)

const (
	agentTestRepoID      = "01JREPO0000000000000000000"
	agentTestOtherRepoID = "01JREPO0000000000000000001"
	agentTestBase        = "/api/v1/gh/acme"
	agentTestPresets     = "/api/v1/agents/presets"
)

// fakeAgentsAPI is an in-memory project agents API: enough of agents,
// environments and presets for the commands, recording every write.
type fakeAgentsAPI struct {
	t *testing.T

	mu      sync.Mutex
	agents  []api.ProjectAgent
	presets []api.ProjectAgentPreset
	envs    []api.ProjectEnvironment
	writes  []fakeAgentWrite
	// forbidReplace answers every replace 403, as for a caller without push
	// on a repository of the agent's selection.
	forbidReplace bool
	// url is the fake's base URL, for tests that build their own target.
	url string
	// created replays creates by Idempotency-Key, as the API does.
	created map[string]fakeCreate
}

type fakeAgentWrite struct {
	Method, Path, IfMatch, IdempotencyKey string
	Body                                  map[string]any
}

type fakeCreate struct {
	body     string
	response []byte
}

func (f *fakeAgentsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Errorf("encode: %v", err)
		}
	}
	var body map[string]any
	if r.Body != nil && r.Method != http.MethodGet {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode %s %s: %v", r.Method, r.URL.Path, err)
		}
		f.writes = append(f.writes, fakeAgentWrite{Method: r.Method, Path: r.URL.Path, IfMatch: r.Header.Get("If-Match"), IdempotencyKey: r.Header.Get("Idempotency-Key"), Body: body})
	}
	// The API's write contract: every create carries an Idempotency-Key, a
	// replayed key returns what the first create saved, and a reused key with
	// another body is a conflict; every replace carries If-Match.
	switch r.Method {
	case http.MethodPost:
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			http.Error(w, `{"title":"Idempotency-Key is required"}`, http.StatusUnprocessableEntity)
			return
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			f.t.Errorf("encode body: %v", err)
		}
		if prior, ok := f.created[r.URL.Path+" "+key]; ok {
			if prior.body != string(encoded) {
				http.Error(w, `{"title":"Idempotency-Key already used with a different body"}`, http.StatusConflict)
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			_, _ = w.Write(prior.response) //nolint:errcheck // test server
			return
		}
		rec := httptest.NewRecorder()
		f.create(rec, r.URL.Path, body)
		if rec.Code == http.StatusOK {
			if f.created == nil {
				f.created = map[string]fakeCreate{}
			}
			f.created[r.URL.Path+" "+key] = fakeCreate{body: string(encoded), response: rec.Body.Bytes()}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes()) //nolint:errcheck // test server
		return
	case http.MethodPut:
		if r.Header.Get("If-Match") == "" {
			http.Error(w, `{"title":"If-Match is required"}`, http.StatusPreconditionRequired)
			return
		}
		if f.forbidReplace {
			http.Error(w, `{"title":"repository push required"}`, http.StatusForbidden)
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == agentTestPresets:
		write(api.ProjectConfigPage[api.ProjectAgentPreset]{Items: f.presets})
	case r.Method == http.MethodGet && r.URL.Path == agentTestBase+"/agents":
		repo := r.URL.Query().Get("repository_id")
		items := []api.ProjectAgent{}
		for _, a := range f.agents {
			if repo == "" || slices.Contains(a.RepositoryIDs, repo) {
				items = append(items, a)
			}
		}
		write(api.ProjectConfigPage[api.ProjectAgent]{Items: items})
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, agentTestBase+"/agents/"):
		id := strings.TrimPrefix(r.URL.Path, agentTestBase+"/agents/")
		for i, a := range f.agents {
			if a.ID != id {
				continue
			}
			if r.Header.Get("If-Match") != a.ETag {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if _, ok := body["preset"]; !ok {
				f.agents[i].Preset = nil
			}
			if repos, ok := body["repository_ids"].([]any); ok {
				f.agents[i].RepositoryIDs = nil
				for _, id := range repos {
					f.agents[i].RepositoryIDs = append(f.agents[i].RepositoryIDs, fmt.Sprint(id))
				}
			}
			f.agents[i].Instructions, _ = body["instructions"].(string) //nolint:errcheck // checked by the tests
			// Every saved version advances the etag, so a stale one is refused.
			f.agents[i].Version++
			f.agents[i].ETag = fmt.Sprintf(`"%d"`, f.agents[i].Version)
			write(f.agents[i])
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodGet && r.URL.Path == agentTestBase+"/environments":
		write(api.ProjectConfigPage[api.ProjectEnvironment]{Items: f.envs})
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		w.WriteHeader(http.StatusNotFound)
	}
}

// create saves one agent or environment; ServeHTTP has checked its key.
func (f *fakeAgentsAPI) create(w http.ResponseWriter, path string, body map[string]any) {
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Errorf("encode: %v", err)
		}
	}
	switch path {
	case agentTestBase + "/agents":
		a := api.ProjectAgent{ID: fmt.Sprintf("01JNEW%020d", len(f.agents)), ETag: `"1"`}
		if p, ok := body["preset"].(string); ok {
			a.Preset = &api.ProjectAgentPresetRef{ID: p, Version: 1}
			a.Name = p
		}
		if n, ok := body["name"].(string); ok {
			a.Name = n
		}
		a.IsEnabled, _ = body["is_enabled"].(bool) //nolint:errcheck // absent means false
		f.agents = append(f.agents, a)
		write(a)
	case agentTestBase + "/environments":
		env := api.ProjectEnvironment{ID: "01JENV0000000000000000000", Name: fmt.Sprint(body["name"])}
		f.envs = append(f.envs, env)
		write(env)
	default:
		f.t.Errorf("unexpected create %s", path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAgentsAPI) writesTo(method string) []fakeAgentWrite {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeAgentWrite
	for _, w := range f.writes {
		if w.Method == method {
			out = append(out, w)
		}
	}
	return out
}

// stubProjectAgentAPI points the agent commands at fake, as project gh/acme
// resolved from repository gh/acme/widget.
func stubProjectAgentAPI(t *testing.T, fake *fakeAgentsAPI) {
	t.Helper()
	fake.t = t
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.url = srv.URL

	oldTarget, oldRepo := resolveProjectAgentTarget, resolveProjectAgentRepoID
	t.Cleanup(func() { resolveProjectAgentTarget, resolveProjectAgentRepoID = oldTarget, oldRepo })
	resolveProjectAgentTarget = func(*cobra.Command) (*projectAgentTarget, error) {
		return &projectAgentTarget{
			client: api.NewClientWithBaseURL("token", srv.URL),
			host:   "gh", project: "acme", base: agentTestBase,
			repoForge: "gh", repoOwner: "acme", repoName: "widget",
		}, nil
	}
	resolveProjectAgentRepoID = func(context.Context, bool, string, string, string) (string, error) {
		return agentTestRepoID, nil
	}
	oldNames := resolveProjectRepoNames
	t.Cleanup(func() { resolveProjectRepoNames = oldNames })
	resolveProjectRepoNames = func(context.Context, string) map[string]string {
		return map[string]string{agentTestRepoID: "widget", agentTestOtherRepoID: "frontend"}
	}
}

func executeProjectAgent(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newProjectAgentCmd()
	cmd.PersistentFlags().Bool("insecure-http-auth", false, "")
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SetContext(t.Context())
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func samplePreset(id string) api.ProjectAgentPreset {
	return api.ProjectAgentPreset{ID: id, Version: 1, Name: id, Description: id + " review",
		Agent: api.ProjectAgentDefinition{Name: id, Harness: "claude", TimeoutMS: 60000, Instructions: "Review {{branch}}."}}
}

// sampleRiskAgent is the project's risk preset agent (01JA, version 3),
// enabled and reviewing the current repository.
func sampleRiskAgent() api.ProjectAgent {
	return api.ProjectAgent{
		ID: "01JA", Version: 3, ETag: `"3"`,
		ProjectAgentDefinition: api.ProjectAgentDefinition{
			Name: "Risk", Harness: "claude", TimeoutMS: 60000,
			Instructions: "Review {{branch}} against {{base_branch}} for risk.",
			Signal:       api.ProjectAgentSignal{ValueType: "size", Polarity: "lower_is_better"},
		},
		Preset:    &api.ProjectAgentPresetRef{ID: "risk", Version: 2},
		IsEnabled: true, EnvironmentID: "01JENV0000000000000000000",
		RepositoryIDs: []string{agentTestRepoID},
		Triggers:      []api.ProjectAgentTrigger{{ID: "push", Kind: "push", When: "ready"}},
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentList(t *testing.T) {
	fake := &fakeAgentsAPI{
		agents:  []api.ProjectAgent{sampleRiskAgent()},
		presets: []api.ProjectAgentPreset{samplePreset("risk"), samplePreset("security-review")},
	}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"01JA", "Risk", "risk@2", "claude"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	out, _, err = executeProjectAgent(t, "list", "--presets", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []projectAgentPresetJSON
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("presets JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 || !rows[0].Added || rows[1].Added {
		t.Errorf("presets = %+v, want risk added and security-review not", rows)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_CreatesPresetInANewDefaultEnvironment(t *testing.T) {
	fake := &fakeAgentsAPI{presets: []api.ProjectAgentPreset{samplePreset("risk"), samplePreset("security-review")}}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, "add", "security-review")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added security-review") {
		t.Errorf("output = %q", out)
	}
	posts := fake.writesTo(http.MethodPost)
	if len(posts) != 2 || posts[0].Path != agentTestBase+"/environments" || posts[1].Path != agentTestBase+"/agents" {
		t.Fatalf("writes = %+v, want an environment then an agent", posts)
	}
	body := posts[1].Body
	if body["preset"] != "security-review" || body["environment_id"] != "01JENV0000000000000000000" || body["is_enabled"] != true {
		t.Errorf("agent body = %v", body)
	}
	if repos, _ := body["repository_ids"].([]any); len(repos) != 1 || repos[0] != agentTestRepoID { //nolint:errcheck // asserted
		t.Errorf("repository_ids = %v, want the current repo", body["repository_ids"])
	}
	// A preset create sends only placement; the definition is the catalog's.
	for _, field := range []string{"name", "instructions", "harness"} {
		if _, ok := body[field]; ok {
			t.Errorf("preset create sent %q", field)
		}
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_SkipsAPresetTheProjectRuns(t *testing.T) {
	fake := &fakeAgentsAPI{
		agents:  []api.ProjectAgent{sampleRiskAgent()},
		presets: []api.ProjectAgentPreset{samplePreset("risk")},
		envs:    []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}},
	}
	stubProjectAgentAPI(t, fake)

	_, errOut, err := executeProjectAgent(t, "add", "risk")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "skip risk") {
		t.Errorf("stderr = %q, want a skip note", errOut)
	}
	if writes := fake.writesTo(http.MethodPost); len(writes) != 0 {
		t.Errorf("writes = %+v, want none", writes)
	}

	if _, _, err := executeProjectAgent(t, "add", "nope"); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("err = %v, want unknown preset", err)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_SecondRepositoryExtendsThePresetAgent(t *testing.T) {
	// risk was added from frontend; adding it from widget must make it review
	// widget too, not report success while widget stays unreviewed.
	risk := sampleRiskAgent()
	risk.RepositoryIDs = []string{agentTestOtherRepoID}
	fake := &fakeAgentsAPI{agents: []api.ProjectAgent{risk}, presets: []api.ProjectAgentPreset{samplePreset("risk")}}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, "add", "risk")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "extended Risk") {
		t.Errorf("output = %q", out)
	}
	if posts := fake.writesTo(http.MethodPost); len(posts) != 0 {
		t.Errorf("POSTs = %+v; a project runs each preset once, so this must extend, not create", posts)
	}
	puts := fake.writesTo(http.MethodPut)
	if len(puts) != 1 || puts[0].Path != agentTestBase+"/agents/01JA" || puts[0].IfMatch != `"3"` {
		t.Fatalf("PUTs = %+v, want one compare-and-swap replace", puts)
	}
	body := puts[0].Body
	if body["preset"] != "risk" || body["environment_id"] != risk.EnvironmentID {
		t.Errorf("replace body = %v, want the preset and its placement", body)
	}
	if _, ok := body["instructions"]; ok {
		t.Error("a preset agent's replace must not send its definition")
	}
	repos, _ := body["repository_ids"].([]any) //nolint:errcheck // asserted
	if len(repos) != 2 || repos[0] != agentTestOtherRepoID || repos[1] != agentTestRepoID {
		t.Errorf("repository_ids = %v, want both repositories", repos)
	}

	// Run again from widget: it is covered now, so nothing changes.
	_, errOut, err := executeProjectAgent(t, "add", "risk")
	if err != nil || !strings.Contains(errOut, "already reviews this repository") {
		t.Errorf("second add: err=%v stderr=%q", err, errOut)
	}
	if len(fake.writesTo(http.MethodPut)) != 1 {
		t.Error("a covered preset must not be replaced again")
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_ExtensionWithoutAccessNamesTheRepositories(t *testing.T) {
	risk := sampleRiskAgent()
	risk.RepositoryIDs = []string{agentTestOtherRepoID}
	fake := &fakeAgentsAPI{agents: []api.ProjectAgent{risk}, presets: []api.ProjectAgentPreset{samplePreset("risk")}, forbidReplace: true}
	stubProjectAgentAPI(t, fake)

	_, _, err := executeProjectAgent(t, "add", "risk")
	if err == nil || !strings.Contains(err.Error(), "frontend") || !strings.Contains(err.Error(), "push access") {
		t.Fatalf("err = %v, want one naming frontend and the push requirement", err)
	}
	if !api.IsHTTPErrorStatus(err, http.StatusForbidden) {
		t.Error("the 403 must stay inspectable through the wrap")
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_NoopJSONIsAnEmptyArray(t *testing.T) {
	fake := &fakeAgentsAPI{agents: []api.ProjectAgent{sampleRiskAgent()}, presets: []api.ProjectAgentPreset{samplePreset("risk")}}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, "add", "risk", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("stdout = %q, want []", out)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_ExtendJSONCarriesTheSavedVersion(t *testing.T) {
	risk := sampleRiskAgent()
	risk.RepositoryIDs = []string{agentTestOtherRepoID}
	fake := &fakeAgentsAPI{agents: []api.ProjectAgent{risk}, presets: []api.ProjectAgentPreset{samplePreset("risk")}}
	stubProjectAgentAPI(t, fake)

	out, _, err := executeProjectAgent(t, "add", "risk", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var saved []api.ProjectAgent
	if err := json.Unmarshal([]byte(out), &saved); err != nil || len(saved) != 1 {
		t.Fatalf("JSON = %q (%v)", out, err)
	}
	if saved[0].Version != 4 || saved[0].ETag != `"4"` || len(saved[0].RepositoryIDs) != 2 {
		t.Fatalf("emitted %+v, want the server's version 4 with both repositories", saved[0])
	}
	// The emitted etag is the one the next write needs.
	target, err := resolveProjectAgentTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.replaceAgent(t.Context(), "01JA", saved[0].ETag, api.ProjectAgentWrite{Preset: "risk", RepositoryIDs: saved[0].RepositoryIDs, EnvironmentID: saved[0].EnvironmentID}); err != nil {
		t.Errorf("replace with the emitted etag: %v", err)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_RefusesFlagsAnExistingAgentCannotHonor(t *testing.T) {
	risk := sampleRiskAgent() // enabled, in 01JENV...
	risk.RepositoryIDs = []string{agentTestOtherRepoID}
	fake := &fakeAgentsAPI{
		agents:  []api.ProjectAgent{risk},
		presets: []api.ProjectAgentPreset{samplePreset("risk")},
		envs:    []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}, {ID: "01JENV0000000000000000009", Name: "GPU"}},
	}
	stubProjectAgentAPI(t, fake)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"add", "risk", "--disabled"}, "--disabled"},
		{[]string{"add", "risk", "--disabled", "--json"}, "--disabled"},
		{[]string{"add", "risk", "--environment", "GPU"}, "--environment"},
	} {
		if _, _, err := executeProjectAgent(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want one naming %s", tc.args, err, tc.want)
		}
	}
	if n := len(fake.writesTo(http.MethodPut)) + len(fake.writesTo(http.MethodPost)); n != 0 {
		t.Errorf("refused requests made %d write(s)", n)
	}

	// The environment the agent already runs in is no conflict.
	if _, _, err := executeProjectAgent(t, "add", "risk", "--environment", "Default"); err != nil {
		t.Errorf("matching --environment: %v", err)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_RefusesFlagsForAPresetThatAlreadyCoversTheRepo(t *testing.T) {
	// risk already reviews widget, so add has nothing to do — but the flags
	// still ask for something the agent is not, and are refused, not ignored.
	fake := &fakeAgentsAPI{
		agents:  []api.ProjectAgent{sampleRiskAgent()},
		presets: []api.ProjectAgentPreset{samplePreset("risk")},
		envs:    []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}, {ID: "01JENV0000000000000000009", Name: "GPU"}},
	}
	stubProjectAgentAPI(t, fake)

	for _, args := range [][]string{
		{"add", "risk", "--disabled"},
		{"add", "risk", "--disabled", "--json"},
		{"add", "risk", "--environment", "GPU"},
	} {
		if _, _, err := executeProjectAgent(t, args...); err == nil {
			t.Errorf("%v: want a refusal", args)
		}
	}
	if n := len(fake.writesTo(http.MethodPut)) + len(fake.writesTo(http.MethodPost)); n != 0 {
		t.Errorf("refused requests made %d write(s)", n)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_DisabledSendsAnExplicitFalse(t *testing.T) {
	fake := &fakeAgentsAPI{presets: []api.ProjectAgentPreset{samplePreset("risk")}, envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)

	if _, _, err := executeProjectAgent(t, "add", "risk", "--disabled"); err != nil {
		t.Fatal(err)
	}
	posts := fake.writesTo(http.MethodPost)
	if len(posts) != 1 {
		t.Fatalf("POSTs = %+v", posts)
	}
	if v, ok := posts[0].Body["is_enabled"]; !ok || v != false {
		t.Errorf("is_enabled = %v (sent: %v), want an explicit false", v, ok)
	}
}

// Not parallel: replaces the package-level API resolvers.
func TestProjectAgentAdd_MembershipIsTheProjectsNotTheNamespace(t *testing.T) {
	fake := &fakeAgentsAPI{presets: []api.ProjectAgentPreset{samplePreset("risk")}, envs: []api.ProjectEnvironment{{ID: "01JENV0000000000000000000", Name: "Default"}}}
	stubProjectAgentAPI(t, fake)
	// The project is et/acme while the repository's remote is gh/acme/widget.
	resolveProjectAgentTarget = func(*cobra.Command) (*projectAgentTarget, error) {
		return &projectAgentTarget{
			client: api.NewClientWithBaseURL("token", fake.url), projectID: "01JPROJ",
			host: "gh", project: "acme", base: agentTestBase,
			repoForge: "et", repoOwner: "acme", repoName: "widget",
		}, nil
	}

	// Listed among the project's repositories: a member despite the namespace.
	if _, _, err := executeProjectAgent(t, "add", "risk"); err != nil {
		t.Fatalf("listed repository refused: %v", err)
	}

	// Not listed (or the list is unreadable): refused before any write.
	resolveProjectRepoNames = func(context.Context, string) map[string]string { return map[string]string{} }
	before := len(fake.writesTo(http.MethodPost))
	if _, _, err := executeProjectAgent(t, "add", "risk"); err == nil || !strings.Contains(err.Error(), "is not in project") {
		t.Fatalf("err = %v, want a membership refusal", err)
	}
	if len(fake.writesTo(http.MethodPost)) != before {
		t.Error("a refused repository was written")
	}
}

func TestReplaceProjectConfig_KeepsThePreconditionFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	t.Cleanup(srv.Close)
	err := api.NewClientWithBaseURL("token", srv.URL).ReplaceProjectConfig(t.Context(), "/x", `"1"`, map[string]string{}, nil)
	if !api.IsHTTPErrorStatus(err, http.StatusPreconditionFailed) {
		t.Errorf("err = %v, want the 412 kept for status inspection", err)
	}
}
