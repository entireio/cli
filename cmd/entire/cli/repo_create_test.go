package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/internal/coreapi"
)

const (
	testCreateProjectAcme   = "01ARZ3NDEKTSV4RRFFQ69G5FA1"
	testCreateProjectBeta   = "01ARZ3NDEKTSV4RRFFQ69G5FA2"
	testCreateProjectLocked = "01ARZ3NDEKTSV4RRFFQ69G5FA3"
	testCreatedRepoID       = "01KS6KFJR2XS6PZ188MVYE07AN"
)

// fakeRepoCreateCore is a control plane for `repo create`: project listing and
// lookup, the by-name repo check, the create itself, the readiness read and
// the visibility write. It records the create and visibility bodies.
type fakeRepoCreateCore struct {
	t *testing.T

	projects []fakeCreateProject
	// taken names already exist in every project, as the by-name check sees.
	taken map[string]bool
	// conflicts is how many creates are refused with a 409 before one succeeds.
	conflicts int
	// withPath adds the server's /et/<project>/<repo> path to the created repo.
	withPath bool
	// lookupFails makes GET /projects/{id} fail; projectGets counts those reads.
	lookupFails bool
	projectGets int
	// invalids is how many creates are refused with a 422 (after any
	// conflicts) before one succeeds.
	invalids int
	// createdVisibility is the visibility the create response reports.
	createdVisibility string
	// omitFullName leaves fullName out of the created repo.
	omitFullName   bool
	failVisibility bool
	// snapshotOmitsVisibility leaves visibility and fullName out of the
	// readiness read.
	snapshotOmitsVisibility bool
	// snapshotHangs makes the readiness read wait until the client gives up,
	// so --wait-timeout runs out.
	snapshotHangs bool
	// snapshotState overrides the lifecycle state the readiness read reports.
	snapshotState string

	mu           sync.Mutex
	requests     int
	lastCreated  map[string]any
	createBodies []map[string]any
	visBodies    []string
}

type fakeCreateProject struct {
	id, name  string
	canCreate bool
}

func (p fakeCreateProject) json() map[string]any {
	return map[string]any{
		"id": p.id, "name": p.name, "ownerType": "org", "ownerId": "01ARZ3NDEKTSV4RRFFQ69G5FOW", "ownerName": "acme-inc",
		"region": "us", "createdAt": "2026-01-01T00:00:00Z", "repositoryCount": 3,
		"capabilities": map[string]any{"canCreateRepository": p.canCreate, "canDelete": false, "canManageAccess": false, "canManageTrails": false},
	}
}

func defaultCreateProjects() []fakeCreateProject {
	// Deliberately out of name order: the picker sorts.
	return []fakeCreateProject{
		{id: testCreateProjectBeta, name: "beta", canCreate: true},
		{id: testCreateProjectAcme, name: "acme", canCreate: true},
		{id: testCreateProjectLocked, name: "locked", canCreate: false},
	}
}

// serve starts the fake and points the active-context client seam at it.
// Callers must not be parallel.
func (f *fakeRepoCreateCore) serve() {
	f.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	f.t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	f.t.Cleanup(func() { activeCoreClient = prev })
}

func (f *fakeRepoCreateCore) project(id string) (fakeCreateProject, bool) {
	for _, p := range f.projects {
		if p.id == id {
			return p, true
		}
	}
	return fakeCreateProject{}, false
}

func (f *fakeRepoCreateCore) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakeRepoCreateCore) handle(w http.ResponseWriter, r *http.Request) {
	t := f.t
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	writeJSON := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	problem := func(status int) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"status":%d,"detail":"%s"}`, status, http.StatusText(status))
	}
	switch {
	case r.Method == http.MethodGet && path == "/projects":
		if name := r.URL.Query().Get("name"); name != "" {
			for _, p := range f.projects {
				if strings.EqualFold(p.name, name) {
					writeJSON(http.StatusOK, map[string]any{"project": p.json()})
					return
				}
			}
			problem(http.StatusNotFound)
			return
		}
		rows := make([]map[string]any, 0, len(f.projects))
		for _, p := range f.projects {
			rows = append(rows, p.json())
		}
		writeJSON(http.StatusOK, map[string]any{"projects": rows})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/projects/") && strings.HasSuffix(path, "/repos"):
		name := r.URL.Query().Get("name")
		if name != "" {
			t.Errorf("the wizard reads a project's repos once, not by name (asked for %q)", name)
		}
		repos := []any{}
		for taken := range f.taken {
			repos = append(repos, createdRepoJSON("01KS6KFJR2XS6PZ188MVYE0TKN", taken, testCreateProjectAcme, ""))
		}
		writeJSON(http.StatusOK, map[string]any{"repos": repos})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/projects/"):
		f.mu.Lock()
		f.projectGets++
		f.mu.Unlock()
		if f.lookupFails {
			problem(http.StatusInternalServerError)
			return
		}
		p, ok := f.project(strings.TrimPrefix(path, "/projects/"))
		if !ok {
			problem(http.StatusNotFound)
			return
		}
		writeJSON(http.StatusOK, p.json())
	case r.Method == http.MethodPost && path == "/repos":
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read create body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode create body: %v", err)
		}
		f.mu.Lock()
		f.createBodies = append(f.createBodies, body)
		conflict := f.conflicts > 0
		if conflict {
			f.conflicts--
		}
		invalid := !conflict && f.invalids > 0
		if invalid {
			f.invalids--
		}
		f.mu.Unlock()
		if conflict {
			problem(http.StatusConflict)
			return
		}
		if invalid {
			problem(http.StatusUnprocessableEntity)
			return
		}
		projectID, _ := body["projectId"].(string) //nolint:errcheck // absent is caught by the assertions
		name, _ := body["name"].(string)           //nolint:errcheck // absent is caught by the assertions
		fullName := ""
		if p, ok := f.project(projectID); ok && !f.omitFullName {
			fullName = p.name + "/" + name
		}
		repo := createdRepoJSON(testCreatedRepoID, name, projectID, fullName)
		if f.createdVisibility != "" {
			repo["visibility"] = f.createdVisibility
		}
		if p, ok := f.project(projectID); ok && f.withPath {
			repo["path"] = "/et/" + p.name + "/" + name
		}
		f.mu.Lock()
		f.lastCreated = repo
		f.mu.Unlock()
		writeJSON(http.StatusCreated, repo)
	case r.Method == http.MethodGet && path == "/repos/"+testCreatedRepoID:
		if f.snapshotHangs {
			<-r.Context().Done()
			return
		}
		// The readiness read is authoritative: it answers with the repo as
		// created, which the command adopts.
		f.mu.Lock()
		repo := make(map[string]any, len(f.lastCreated))
		for k, v := range f.lastCreated {
			repo[k] = v
		}
		f.mu.Unlock()
		if f.snapshotOmitsVisibility {
			delete(repo, "visibility")
			delete(repo, "fullName")
		}
		if f.snapshotState != "" {
			repo["state"] = f.snapshotState
		}
		writeJSON(http.StatusOK, repo)
	case r.Method == http.MethodPut && path == "/repos/"+testCreatedRepoID+"/visibility":
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read visibility body: %v", err)
		}
		f.mu.Lock()
		f.visBodies = append(f.visBodies, strings.TrimSpace(string(raw)))
		f.mu.Unlock()
		if f.failVisibility {
			problem(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw) //nolint:errcheck // test fixture echo
	default:
		t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
		problem(http.StatusNotFound)
	}
}

func createdRepoJSON(id, name, projectID, fullName string) map[string]any {
	repo := map[string]any{
		"id": id, "name": name, "owningProjectId": projectID, "provider": "entire", "state": "active",
		"capabilities": map[string]any{"canManage": true, "canPush": true, "canPull": true},
	}
	if fullName != "" {
		repo["fullName"] = fullName
	}
	return repo
}

// execRepoCreateArgs runs `repo create <args...>` under a parent carrying the
// control-plane persistent flags.
func execRepoCreateArgs(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	parent := &cobra.Command{Use: "repo", SilenceErrors: true}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoCreateCmd())
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs(append([]string{"create"}, args...))
	err = parent.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

// answerRepoCreatePrompts makes the command promptable and answers its
// prompts in order, one line each, through the prompt-terminal seam in
// accessible mode. It returns what the prompts rendered, and fails the test
// if a prompt goes unanswered or an answer goes unused.
func answerRepoCreatePrompts(t *testing.T, answers ...string) *bytes.Buffer {
	t.Helper()
	// The wizard suggests the current folder's name, which runs git: keep it
	// off the developer's checkout.
	t.Chdir(t.TempDir())
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv(interactive.EnvTestTTY, "1")
	var terminal bytes.Buffer
	next := 0
	prev := openPromptTerminal
	openPromptTerminal = func() (promptTerminal, error) {
		if next >= len(answers) {
			t.Errorf("unexpected prompt #%d; terminal so far:\n%s", next+1, terminal.String())
			return promptTerminal{}, errors.New("no answer scripted")
		}
		answer := answers[next]
		next++
		// One byte per read, as a terminal delivers typed lines: each field
		// scans the reader afresh, and a buffered read would hand the first
		// field every line.
		return promptTerminal{in: iotest.OneByteReader(strings.NewReader(answer + "\n")), out: &terminal}, nil
	}
	t.Cleanup(func() {
		openPromptTerminal = prev
		if next != len(answers) {
			t.Errorf("%d of %d scripted answers were used; terminal:\n%s", next, len(answers), terminal.String())
		}
	})
	return &terminal
}

// TestRepoCreate_MissingInputsWithoutTerminal pins the non-interactive
// contract: without a terminal the wizard is not an option, so a missing name
// or --project is refused before any request, naming the flag form.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_MissingInputsWithoutTerminal(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		missing string
	}{
		{nil, "a repository name and --project are required"},
		{[]string{"web"}, "--project is required"},
		{[]string{"web", "--json"}, "--project is required"},
		{[]string{"--project", "acme"}, "a repository name is required"},
	} {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		_, _, err := execRepoCreateArgs(t, tc.args...)
		require.ErrorIs(t, err, errRepoCreateNeedsInput, tc.args)
		require.ErrorContains(t, err, tc.missing+" without an interactive terminal: entire repo create <name> --project <project>", "names what is missing: %v", tc.args)
		require.ErrorContains(t, err, "'entire project list' shows project names")
		require.NotContains(t, err.Error(), "ULID")
		require.Zero(t, f.requestCount(), "refused before any request")
	}
}

// Flags mean the flag form: the wizard takes only the positional name, so a
// create flag given with the name or --project missing is refused before any
// request — even in a terminal — rather than silently dropped.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreate_FlagsMeanTheFlagForm(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	stubRepoCreatePrompt(t, func(*cobra.Command, *repoCreateState) (bool, error) {
		t.Error("the wizard must not open when create flags are given")
		return false, nil
	})
	for _, args := range [][]string{
		{"--project", "acme"},
		{"web", "--visibility", "public"},
		{"--object-format", "sha256"},
		{"web", "--project", ""},
	} {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		_, _, err := execRepoCreateArgs(t, args...)
		require.ErrorIs(t, err, errRepoCreateFlagsNeedInput, args)
		require.ErrorContains(t, err, "required when create flags are given: entire repo create <name> --project <project> (or, in a terminal and without flags, run 'entire repo create' or 'entire repo create <name>' to be asked)")
		require.Zero(t, f.requestCount(), "refused before any request: %v", args)
	}
}

// TestRepoCreate_VisibilityFlag pins the second call a visibility needs: the
// create endpoint takes none, so it is set right after, skipped when the
// create already reports it, and refused client-side when misspelled.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_VisibilityFlag(t *testing.T) {
	t.Run("public is set after the create", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private"}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "public", "--json")
		require.NoError(t, err)
		require.Len(t, f.createBodies, 1)
		require.NotContains(t, f.createBodies[0], "visibility", "the create endpoint takes no visibility")
		require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(stdout), &obj))
		require.Equal(t, "public", obj["visibility"], "--json reports the visibility that now holds")
	})

	t.Run("a visibility the create already reports is not set again", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private"}
		f.serve()
		_, _, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "private")
		require.NoError(t, err)
		require.Empty(t, f.visBodies)
	})

	t.Run("no flag leaves the server default alone", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		_, _, err := execRepoCreateArgs(t, "web", "--project", "acme")
		require.NoError(t, err)
		require.Empty(t, f.visBodies)
	})

	t.Run("an unknown value fails before any request", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		_, _, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "internal")
		require.ErrorContains(t, err, "invalid visibility")
		require.Empty(t, f.createBodies)
	})

	t.Run("a failed visibility write keeps the repo and says how to finish", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), failVisibility: true}
		f.serve()
		stdout, stderr, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "public")
		var silent *SilentError
		require.ErrorAs(t, err, &silent, "the failure is explained on stderr, not reprinted")
		require.Contains(t, stdout, "✓ Created repo /et/acme/web")
		require.NotContains(t, stdout, "Next steps", "next steps would read as success")
		require.Contains(t, stderr, "was created, but setting its visibility to public failed")
		require.Contains(t, stderr, "entire repo edit /et/acme/web --visibility public")
		require.NotContains(t, stderr, testCreatedRepoID, "the repo is addressed by its path, not its id")
		require.Len(t, f.createBodies, 1)
	})
}

// TestRepoCreate_NextSteps pins the commands printed after a create: they
// address the repo by its /et/ ref, which comes from the server's full name,
// or failing that the project name the command resolved — and are left out
// when neither is known rather than printed with a ref that cannot work.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_NextSteps(t *testing.T) {
	t.Run("from the server's full name", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", testCreateProjectAcme)
		require.NoError(t, err)
		require.Contains(t, stdout, "Next steps")
		require.Contains(t, stdout, "entire repo clone /et/acme/web")
		require.Contains(t, stdout, "entire repo remote add origin /et/acme/web --override")
		require.Contains(t, stdout, "entire repo mirror add /et/acme/web")
	})

	t.Run("from the resolved project name", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), omitFullName: true}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", "acme")
		require.NoError(t, err)
		require.Contains(t, stdout, "entire repo clone /et/acme/web")
	})

	t.Run("from the server's path, for a project given as a ULID", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), omitFullName: true, withPath: true}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", testCreateProjectAcme)
		require.NoError(t, err)
		require.Contains(t, stdout, "✓ Created repo /et/acme/web")
		require.Contains(t, stdout, "entire repo clone /et/acme/web")
		require.NotContains(t, stdout, testCreatedRepoID)
	})

	t.Run("left out of --json", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", "acme", "--json")
		require.NoError(t, err)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(stdout), &obj), "stdout stays one JSON object")
	})
}

// --- wizard state ------------------------------------------------------------

func wizardTestProjects() []coreapi.Project {
	project := func(id, name, owner string, repos int64, canCreate bool) coreapi.Project {
		return coreapi.Project{
			ID: id, Name: name, OwnerName: coreapi.NewOptString(owner), Region: "us",
			RepositoryCount: coreapi.NewOptInt64(repos),
			Capabilities:    coreapi.NewOptProjectCapabilities(coreapi.ProjectCapabilities{CanCreateRepository: canCreate}),
		}
	}
	return []coreapi.Project{
		project(testCreateProjectBeta, "beta", "acme-inc", 1, true),
		project(testCreateProjectLocked, "locked", "acme-inc", 9, false),
		project(testCreateProjectAcme, "Acme", "acme-inc", 3, true),
	}
}

// fixedNames is a name index preloaded with names, loading nothing itself.
func fixedNames(t *testing.T, byProject map[string][]string) *repoNameIndex {
	t.Helper()
	x := &repoNameIndex{
		ctx:     t.Context(),
		list:    func(context.Context, string) ([]string, error) { return nil, nil },
		started: map[string]bool{},
		names:   map[string]map[string]string{},
	}
	for id, names := range byProject {
		x.started[id] = true
		for _, n := range names {
			x.add(id, n)
		}
	}
	return x
}

func TestRepoCreateProjects_OnlyCreatableSortedByName(t *testing.T) {
	t.Parallel()
	projects := append(wizardTestProjects(), coreapi.Project{ID: "01ARZ3NDEKTSV4RRFFQ69G5FA4", Name: "nocaps"})
	rows, hidden := repoCreateProjects(projects)
	names := make([]string, len(rows))
	for i, r := range rows {
		names[i] = r.name
	}
	require.Equal(t, []string{"Acme", "beta", "nocaps"}, names, "a project reporting no capabilities is offered; the server decides")
	require.Equal(t, 1, hidden)
}

func TestRepoProject_LabelsAlign(t *testing.T) {
	t.Parallel()
	rows, _ := repoCreateProjects(wizardTestProjects())
	require.Equal(t, "Acme    (acme-inc, us, 3 repos)", rows[0].label(6))
	require.Equal(t, "beta    (acme-inc, us, 1 repo)", rows[1].label(6))
	require.Equal(t, "bare", repoProject{name: "bare"}.label(6))
	for _, r := range rows {
		require.NotContains(t, r.label(6), r.id, "a row never shows an id")
	}
}

func TestNewRepoCreateState_Defaults(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "", "my-repo")
	require.NoError(t, err)
	require.Equal(t, "my-repo", s.answers.Name, "the folder name is suggested when no name was given")
	require.Equal(t, "Acme", s.project().name, "the first project is the starting one")
	require.Equal(t, coreapi.SetRepoVisibilityInputBodyVisibilityPrivate, s.answers.Visibility)
	require.False(t, s.answers.Advanced)

	req := s.request()
	require.Empty(t, req.objectFormat, "a declined advanced step leaves the format to the server")
	require.Equal(t, coreapi.SetRepoVisibilityInputBodyVisibilityPrivate, req.visibility)
}

// The positional name is the only thing carried into the wizard.
func TestNewRepoCreateState_TakesOnlyTheName(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "web", "my-repo")
	require.NoError(t, err)
	require.Equal(t, "web", s.answers.Name, "the argument beats the folder name")
	require.Equal(t, "Acme", s.project().name)
	require.Equal(t, coreapi.SetRepoVisibilityInputBodyVisibilityPrivate, s.answers.Visibility)
	require.False(t, s.answers.Advanced)

	_, err = newRepoCreateState(wizardTestProjects()[1:2], "", "")
	require.ErrorContains(t, err, "no project you can create repositories in")
}

func TestRepoCreateState_ValidateName(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "", "")
	require.NoError(t, err)
	s.names = fixedNames(t, map[string][]string{testCreateProjectAcme: {"web"}})

	require.ErrorContains(t, s.validateName("  "), "enter a repository name")
	require.ErrorContains(t, s.validateName("acme/web"), "cannot contain '/'")
	require.ErrorContains(t, s.validateName("my repo"), "cannot contain spaces")
	require.NoError(t, s.validateName("web.site"), "the server owns the naming rules")
	require.EqualError(t, s.validateName("web.git"), `a repository name cannot end in .git, in any case (use "web")`, "the direct path's rule, on the page")
	require.EqualError(t, s.validateName("Web.GIT"), `a repository name cannot end in .git, in any case (use "web")`)
	require.EqualError(t, s.validateName(".git"), "a repository name cannot end in .git, in any case")
	require.EqualError(t, s.validateName("WEB"), `Acme already has a repository named "web"`)

	s.setProject(testCreateProjectBeta)
	require.NoError(t, s.validateName("web"), "another project's repo name is free here")

	s.names = nil // no index: only the shape checks remain
	require.NoError(t, s.validateName("web"))
}

func TestRepoCreateState_SummaryNamesNoIDs(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "my web", "")
	require.NoError(t, err)
	require.Equal(t, "Project        Acme\n"+
		"Name           my web\n"+
		"Path           /et/Acme/my web\n"+
		"Visibility     private\n"+
		"Object format  sha1 (server default)\n"+
		"Command        entire repo create 'my web' --project Acme --visibility private", s.summary())

	s.answers.Advanced = true
	s.answers.ObjectFormat = coreapi.CreateRepoInputBodyObjectFormatSHA256
	require.Equal(t, "entire repo create 'my web' --project Acme --visibility private --object-format sha256", s.command())
	require.NotContains(t, s.summary(), testCreateProjectAcme)
}

// Each page recaps what was already decided, one answer per line, dimmed above
// the heading, and follows a changed answer.
func TestRepoCreateState_PageTitlesFollowAnswers(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "web", "")
	require.NoError(t, err)
	s.nameGroup(true)
	s.visibilityGroup(true)
	s.advancedGroup(true)
	s.formatGroup(true)
	heading := func(g *huh.Group) []string {
		lines := strings.Split(ansi.Strip(g.Header()), "\n")
		for i, l := range lines {
			lines[i] = strings.TrimRight(l, " ")
		}
		return lines
	}
	require.Equal(t, []string{"✓ Project     Acme", "✓ Name        web", "Visibility"}, heading(s.visibilityGrp)[:3])

	repoProjectAccessor{s: s}.Set(testCreateProjectBeta)
	repoNameAccessor{s: s}.Set("tools")
	repoVisibilityAccessor{s: s}.Set(coreapi.SetRepoVisibilityInputBodyVisibilityPublic)
	require.Contains(t, heading(s.nameGrp), "✓ Project     beta")
	require.Equal(t, []string{"✓ Project     beta", "✓ Name        tools", "✓ Visibility  public", "Advanced"}, heading(s.advancedGrp)[:4])
	require.Equal(t, []string{"✓ Project     beta", "✓ Name        tools", "✓ Visibility  public", "Object format"}, heading(s.formatGrp)[:4])
}

// huh's accessible runner validates an empty answer before keeping the current
// value, so the pre-filled name has to validate as itself.
func TestRepoCreateState_AccessibleNameKeepsTheSuggestion(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "tools", "")
	require.NoError(t, err)
	require.NoError(t, s.accessibleName(huh.NewInput()).RunAccessible(io.Discard, strings.NewReader("\n")))
	require.Equal(t, "tools", s.answers.Name)

	// The kept value is still checked: a taken name is refused.
	s.names = fixedNames(t, map[string][]string{testCreateProjectAcme: {"web"}})
	s.answers.Name = "web"
	var out bytes.Buffer
	require.NoError(t, s.accessibleName(huh.NewInput()).RunAccessible(&out, strings.NewReader("\nfresh\n")))
	require.Contains(t, out.String(), `Repository name (press Enter for "web")`)
	require.Contains(t, out.String(), `Acme already has a repository named "web"`)
	require.Equal(t, "fresh", s.answers.Name)
}

func TestRepoCreateState_DeclinedSummary(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	s, err := newRepoCreateState(wizardTestProjects(), "", "")
	require.NoError(t, err)
	s.confirmed = true
	require.True(t, s.confirm(&w))
	require.Empty(t, w.String())
	s.confirmed = false
	require.False(t, s.confirm(&w))
	require.Equal(t, "Repository create cancelled.\n", w.String())
}

// The name index loads a project once, in the background, and a check before
// the load lands passes rather than waits.
func TestRepoNameIndex_LoadsEachProjectOnce(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var mu sync.Mutex
	loads := map[string]int{}
	x := &repoNameIndex{
		ctx: t.Context(),
		list: func(_ context.Context, id string) ([]string, error) {
			mu.Lock()
			loads[id]++
			mu.Unlock()
			<-release
			return []string{"Web"}, nil
		},
		started: map[string]bool{},
		names:   map[string]map[string]string{},
	}
	x.load("p1")
	x.load("p1")
	_, ok := x.lookup("p1", "web")
	require.False(t, ok, "not loaded yet: the check passes")
	close(release)
	require.Eventually(t, func() bool { _, ok := x.lookup("p1", "web"); return ok }, time.Second, time.Millisecond)
	existing, _ := x.lookup("p1", "WEB")
	require.Equal(t, "Web", existing)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, loads["p1"])
}

// --- command level -----------------------------------------------------------

func stubRepoCreatePrompt(t *testing.T, fn func(*cobra.Command, *repoCreateState) (bool, error)) {
	t.Helper()
	// The wizard suggests the current folder's name, which runs git: keep it
	// off the developer's checkout.
	t.Chdir(t.TempDir())
	prev := repoCreatePrompt
	repoCreatePrompt = fn
	t.Cleanup(func() { repoCreatePrompt = prev })
}

// With a name and a project the repo is created straight away, even in a
// terminal, named by its path rather than its id.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreate_CompleteFlagsSkipTheWizard(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
	f.serve()
	stubRepoCreatePrompt(t, func(*cobra.Command, *repoCreateState) (bool, error) {
		t.Error("the wizard must not open")
		return false, nil
	})
	stdout, _, err := execRepoCreateArgs(t, "web", "--project", "acme")
	require.NoError(t, err)
	require.Contains(t, stdout, "✓ Created repo /et/acme/web\n")
	require.NotContains(t, stdout, testCreatedRepoID)
	require.Len(t, f.createBodies, 1)
	require.NotContains(t, f.createBodies[0], "objectFormat", "omitted flags stay out of the request")
	require.Empty(t, f.visBodies)
}

// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_CreatesWhatTheSummaryShowed(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private"}
	f.serve()
	stubRepoCreatePrompt(t, func(_ *cobra.Command, s *repoCreateState) (bool, error) {
		require.Equal(t, "web", s.answers.Name, "the argument is the starting name")
		require.Equal(t, "acme", s.project().name, "sorted first")
		require.NotContains(t, s.summary(), "locked")
		repoProjectAccessor{s: s}.Set(testCreateProjectBeta)
		s.answers.Visibility = coreapi.SetRepoVisibilityInputBodyVisibilityPublic
		s.answers.Advanced = true
		s.answers.ObjectFormat = coreapi.CreateRepoInputBodyObjectFormatSHA256
		return true, nil
	})
	stdout, _, err := execRepoCreateArgs(t, "web")
	require.NoError(t, err)
	require.Len(t, f.createBodies, 1)
	require.Equal(t, testCreateProjectBeta, f.createBodies[0]["projectId"])
	require.Equal(t, "sha256", f.createBodies[0]["objectFormat"])
	require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies)
	require.Contains(t, stdout, "✓ Created repo /et/beta/web")
	require.Contains(t, stdout, "entire repo mirror add /et/beta/web")
}

// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_CancelledCreatesNothing(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
	f.serve()
	stubRepoCreatePrompt(t, func(*cobra.Command, *repoCreateState) (bool, error) { return false, nil })
	stdout, _, err := execRepoCreateArgs(t, "web")
	require.NoError(t, err)
	require.Empty(t, stdout)
	require.Empty(t, f.createBodies)
}

// A name taken between the check and the create reopens the wizard on the same
// answers, saying why, instead of failing a run the user already answered.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_ConflictReopensTheWizard(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), conflicts: 1}
	f.serve()
	runs := 0
	stubRepoCreatePrompt(t, func(_ *cobra.Command, s *repoCreateState) (bool, error) {
		runs++
		if runs == 2 {
			require.Equal(t, `Creating "web" was refused: Conflict. Change the answer it concerns, or cancel.`, s.nameNote(), "the server's reason, not a guess")
			require.Equal(t, s.nameNote(), s.refusalNote(), "the reopened wizard's first page says why")
			repoProjectAccessor{s: s}.Set(testCreateProjectBeta)
			require.Empty(t, s.nameNote(), "the note belongs to the project the create was refused in")
			require.NotEmpty(t, s.refusalNote(), "the project page still says why it reopened")
			repoProjectAccessor{s: s}.Set(testCreateProjectAcme)
			require.NotEmpty(t, s.nameNote())
			require.Error(t, s.validateName("web"), "the taken name is now refused on the page")
			s.answers.Name = "web2"
		}
		return true, nil
	})
	_, _, err := execRepoCreateArgs(t, "web")
	require.NoError(t, err)
	require.Equal(t, 2, runs)
	require.Len(t, f.createBodies, 2)
	require.Equal(t, "web2", f.createBodies[1]["name"])
}

// The real forms, in accessible mode: each stage is its own form, answered
// line by line, and the prompts stay off stdout.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_AccessibleRun(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), taken: map[string]bool{"taken": true}, createdVisibility: "private"}
	f.serve()
	terminal := answerRepoCreatePrompts(t,
		"2",         // project: beta (acme is first)
		"web\n2\ny", // name, visibility public, customize advanced
		"2",         // object format sha256
		"y",         // create
	)
	stdout, _, err := execRepoCreateArgs(t)
	require.NoError(t, err)

	shown := terminal.String()
	require.Contains(t, shown, "1. acme")
	require.Contains(t, shown, "2. beta")
	require.NotContains(t, shown, "locked")
	require.Contains(t, shown, "1 project hidden: you can't create repositories in it.")
	require.Contains(t, shown, "Command        entire repo create web --project beta --visibility public --object-format sha256")
	require.NotContains(t, shown, testCreateProjectBeta, "no id is ever shown")

	require.Contains(t, stdout, "✓ Created repo /et/beta/web")
	require.NotContains(t, stdout, "Which project", "prompts stay off stdout")
	require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies)
	require.Len(t, f.createBodies, 1)
	require.Equal(t, testCreateProjectBeta, f.createBodies[0]["projectId"])
	require.Equal(t, "sha256", f.createBodies[0]["objectFormat"])
}

// huh's accessible Select keeps a default only for a plain pointer binding, so
// the starting project must survive Enter on the picker — and Enter on every
// later question keeps its own default too.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_AccessibleEnterKeepsDefaults(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
	f.serve()
	answerRepoCreatePrompts(t,
		"",        // project: keep the first (acme)
		"web\n\n", // name, visibility (private), advanced (no)
		"",        // create (default)
	)
	_, _, err := execRepoCreateArgs(t)
	require.NoError(t, err)
	require.Len(t, f.createBodies, 1)
	require.Equal(t, testCreateProjectAcme, f.createBodies[0]["projectId"])
	require.Equal(t, "web", f.createBodies[0]["name"])
	require.NotContains(t, f.createBodies[0], "objectFormat")
	require.Equal(t, []string{`{"visibility":"private"}`}, f.visBodies)
}

// Commands the user pastes into a shell quote the server-derived ref.
func TestPrintRepoCreateNextSteps_QuotesTheRef(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printRepoCreateNextSteps(&out, "/et/acme/web")
	require.Contains(t, out.String(), "entire repo clone /et/acme/web\n", "a plain ref needs no quotes")
	out.Reset()
	printRepoCreateNextSteps(&out, "/et/acme/$(boom)")
	require.Contains(t, out.String(), "entire repo clone '/et/acme/$(boom)'\n")
	require.NotContains(t, out.String(), "clone /et/acme/$(boom)")
}

// The usage line advertises the one optional name the command accepts.
func TestRepoCreate_UsageNamesOneOptionalName(t *testing.T) {
	t.Parallel()
	require.Equal(t, "create [<name>]", newRepoCreateCmd().Use)
}

// A readiness snapshot that omits the visibility must not erase what the
// create reported: asking for the visibility the repo already has sends no
// second request.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_VisibilitySurvivesTheReadinessSnapshot(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private", snapshotOmitsVisibility: true}
	f.serve()
	stdout, _, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "private")
	require.NoError(t, err)
	require.Empty(t, f.visBodies)
	require.Contains(t, stdout, "✓ Created repo /et/acme/web")
}

// A readiness wait that runs out --wait-timeout still leaves the requested
// visibility to set on the repo that now exists: the write gets its own
// budget rather than failing on the spent deadline.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_VisibilityAfterWaitTimeout(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), snapshotHangs: true}
	f.serve()
	_, stderr, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "public", "--wait-timeout", "300ms")
	require.ErrorIs(t, err, context.DeadlineExceeded, "readiness was not confirmed")
	require.Contains(t, stderr, "Readiness was not confirmed")
	require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies, "the visibility was still set")
	require.NotContains(t, stderr, "setting its visibility to public failed")
}

// --json still runs the wizard in a terminal, as `grant add` prompts: the
// form renders off stdout, which carries the repository object and nothing
// else.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_UnderJSONPrintsOnlyTheObject(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private"}
	f.serve()
	answerRepoCreatePrompts(t, "1", "web\n2\n", "")
	stdout, _, err := execRepoCreateArgs(t, "--json")
	require.NoError(t, err)
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &obj), "stdout is the repository object and nothing else:\n%s", stdout)
	require.Equal(t, testCreatedRepoID, obj["id"])
	require.Equal(t, "public", obj["visibility"])
	require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies)
}

// A repo whose provisioning failed gets no visibility write: that would pile a
// second error onto a repo that may never become usable. The user is told how
// to finish once it is active.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_NoVisibilityWhenProvisioningFailed(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), snapshotState: "failed"}
	f.serve()
	_, stderr, err := execRepoCreateArgs(t, "web", "--project", "acme", "--visibility", "public")
	require.ErrorContains(t, err, "provisioning failed")
	require.Empty(t, f.visBodies)
	require.Contains(t, stderr, "Visibility was not set. Once the repository is active, set it with: entire repo edit /et/acme/web --visibility public")
	require.NotContains(t, stderr, "setting its visibility to public failed")
}

// The name argument is trimmed for the direct path as the wizard trims typed
// names, so ' web ' creates the same repo either way; a blank one is missing.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_NameArgumentIsTrimmed(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
	f.serve()
	_, _, err := execRepoCreateArgs(t, " web ", "--project", "acme")
	require.NoError(t, err)
	require.Len(t, f.createBodies, 1)
	require.Equal(t, "web", f.createBodies[0]["name"])

	_, _, err = execRepoCreateArgs(t, "   ")
	require.ErrorIs(t, err, errRepoCreateNeedsInput, "a blank name is a missing one")
	require.Len(t, f.createBodies, 1, "no second create")
}

// Without a terminal, a create flag with an input missing gets the
// no-terminal refusal: the flag-form one suggests running without flags to be
// prompted, which nothing can do there.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_NoTerminalRefusalWinsOverFlagForm(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
	f.serve()
	_, _, err := execRepoCreateArgs(t, "--project", "acme")
	require.ErrorIs(t, err, errRepoCreateNeedsInput)
	require.NotContains(t, err.Error(), "to be asked", "nothing can ask without a terminal")
	require.Zero(t, f.requestCount())
}

// A name ending in .git is refused on the direct path before any request,
// naming the spelling to use — and before the wizard opens.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreate_RefusesGitSuffixName(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	stubRepoCreatePrompt(t, func(*cobra.Command, *repoCreateState) (bool, error) {
		t.Error("the wizard must not open for a refused name")
		return false, nil
	})
	for _, args := range [][]string{{"web.git", "--project", "acme"}, {" web.git "}} {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		_, _, err := execRepoCreateArgs(t, args...)
		require.ErrorContains(t, err, `repo name "web.git" must not end in .git`, args)
		require.ErrorContains(t, err, `(use "web")`)
		require.Zero(t, f.requestCount(), "refused before any request: %v", args)
	}
}

// --wait-timeout is one budget across the wizard's phases, as on the direct
// path: each phase is charged what it took, and a spent budget expires the
// next phase at once rather than granting it the full timeout again.
func TestRepoCreateBudget_PhasesShareOneTimeout(t *testing.T) {
	t.Parallel()
	b := &repoCreateBudget{left: time.Second}

	ctx, done := b.phase(t.Context())
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, time.Until(deadline), time.Second)
	time.Sleep(50 * time.Millisecond)
	done()
	require.Less(t, b.left, 960*time.Millisecond, "the first phase was charged")

	ctx, done = b.phase(t.Context())
	deadline, _ = ctx.Deadline()
	require.Less(t, time.Until(deadline), 960*time.Millisecond, "the second phase gets only what is left")
	done()

	b.left = 0
	ctx, done = b.phase(t.Context())
	defer done()
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded, "a spent budget expires the next phase at once")
}

// A name the server judges invalid (it owns the naming rules) reopens the
// wizard on the same answers with the server's reason, as a conflict does,
// rather than ending a run the user answered.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreateWizard_InvalidNameReopensTheWizard(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), invalids: 1}
	f.serve()
	runs := 0
	stubRepoCreatePrompt(t, func(_ *cobra.Command, s *repoCreateState) (bool, error) {
		runs++
		if runs == 2 {
			require.Contains(t, s.nameNote(), `Creating "MyApp" was refused: Unprocessable Entity. Change the answer it concerns, or cancel.`)
			require.NoError(t, s.validateName("MyApp"), "an invalid name is not a taken one: the server judges again")
			s.answers.Name = "myapp"
		}
		return true, nil
	})
	_, _, err := execRepoCreateArgs(t, "MyApp")
	require.NoError(t, err)
	require.Equal(t, 2, runs)
	require.Len(t, f.createBodies, 2)
	require.Equal(t, "myapp", f.createBodies[1]["name"])
}

// The folder-derived name is suggested as one the server would accept:
// lowercased, since create refuses uppercase.
//
// Not parallel: changes the working directory.
func TestRepoCreateFolderName_IsLowercased(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "MyApp")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	testutil.InitRepo(t, dir)
	t.Chdir(dir)
	require.Equal(t, "myapp", repoCreateFolderName(t.Context()))

	odd := filepath.Join(t.TempDir(), "my..app")
	require.NoError(t, os.MkdirAll(odd, 0o755))
	testutil.InitRepo(t, odd)
	t.Chdir(odd)
	require.Empty(t, repoCreateFolderName(t.Context()), "a folder name that cannot become a repo name suggests nothing")
}

// Scrolling past projects loads only the one the cursor rests on.
func TestRepoNameIndex_LoadsOnlyWhereTheCursorRests(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var loaded []string
	x := &repoNameIndex{
		ctx:    t.Context(),
		settle: 50 * time.Millisecond,
		list: func(_ context.Context, id string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			loaded = append(loaded, id)
			return []string{"web"}, nil
		},
		started: map[string]bool{},
		names:   map[string]map[string]string{},
	}
	for _, id := range []string{"p1", "p2", "p3"} {
		x.load(id)
	}
	require.Eventually(t, func() bool { _, ok := x.lookup("p3", "web"); return ok }, time.Second, 5*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"p3"}, loaded)
}

// --project is not cobra-required (the wizard asks for it), so its help says
// it is required: agents read the flag list and never have a terminal.
func TestRepoCreate_ProjectFlagSaysRequired(t *testing.T) {
	t.Parallel()
	usage := newRepoCreateCmd().Flags().Lookup(projectFlagName).Usage
	require.Contains(t, usage, "(required; run in a terminal without --project, --visibility or --object-format to be asked instead)")
}

// When nothing names the new repo — a project given as a ULID, and no full
// name or /et/ path in the response — one project lookup names it for the
// output. It is skipped when the output would not use it, and a failed lookup
// falls back to the ID rather than failing a create that succeeded.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_NamesTheProjectOnlyWhenNothingElseDoes(t *testing.T) {
	t.Run("one lookup names it", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), omitFullName: true}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", testCreateProjectAcme)
		require.NoError(t, err)
		require.Contains(t, stdout, "✓ Created repo /et/acme/web")
		require.Contains(t, stdout, "entire repo clone /et/acme/web")
		require.Equal(t, 1, f.projectGets)
	})

	t.Run("no lookup when the server names it", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects()}
		f.serve()
		_, _, err := execRepoCreateArgs(t, "web", "--project", testCreateProjectAcme)
		require.NoError(t, err)
		require.Zero(t, f.projectGets)
	})

	t.Run("no lookup when --json on a ready repo would not use it", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), omitFullName: true}
		f.serve()
		_, _, err := execRepoCreateArgs(t, "web", "--project", testCreateProjectAcme, "--json")
		require.NoError(t, err)
		require.Zero(t, f.projectGets)
	})

	t.Run("a failed lookup falls back to the ID", func(t *testing.T) {
		f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), omitFullName: true, lookupFails: true}
		f.serve()
		stdout, _, err := execRepoCreateArgs(t, "web", "--project", testCreateProjectAcme)
		require.NoError(t, err, "the create succeeded")
		require.Contains(t, stdout, "✓ Created repo web ("+testCreatedRepoID+")")
		require.NotContains(t, stdout, "Next steps")
	})
}

// The summary follows a revisited answer. Driving the paged form itself:
// through to the summary, Shift+Tab back to the visibility page, a different
// choice, forward again — the summary must show what will be created. It used
// to keep its first render, because huh re-renders a DescriptionFunc only when
// its binding's hash changes and the answers' fields were unexported.
func TestRepoCreateWizard_SummaryFollowsARevisit(t *testing.T) {
	t.Parallel()
	s, err := newRepoCreateState(wizardTestProjects(), "web", "")
	require.NoError(t, err)
	s.startPaged()
	form := huh.NewForm(s.projectGroup(false), s.nameGroup(true), s.visibilityGroup(true),
		s.advancedGroup(true), s.formatGroup(true), s.summaryGroup())

	send, run := driveForm(form)
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	send(tea.WindowSizeMsg{Width: 120, Height: 40}, run(form.Init()))
	send(enter, enter, enter, enter) // project, name, visibility (private), advanced (no)
	require.Contains(t, ansi.Strip(form.View()), "--visibility private", "on the summary")

	send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}) // back past the hidden format page
	send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}) // to visibility
	send(tea.KeyPressMsg{Code: tea.KeyDown}, enter, enter)     // public, then advanced (no)
	require.Equal(t, coreapi.SetRepoVisibilityInputBodyVisibilityPublic, s.request().visibility)
	view := ansi.Strip(form.View())
	require.Contains(t, view, "--visibility public", "the summary shows what will be created")
	require.NotContains(t, view, "--visibility private")
}

// A failed visibility write is explained after the success report, so the last
// thing on screen is the problem the nonzero exit is about.
//
// Not parallel: swaps the package-level activeCoreClient seam.
func TestRepoCreate_VisibilityFailureComesLast(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), failVisibility: true}
	f.serve()
	parent := &cobra.Command{Use: "repo", SilenceErrors: true}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoCreateCmd())
	var screen bytes.Buffer // stdout and stderr as a terminal interleaves them
	parent.SetOut(&screen)
	parent.SetErr(&screen)
	parent.SetArgs([]string{"create", "web", "--project", "acme", "--visibility", "public"})
	require.Error(t, parent.ExecuteContext(t.Context()))
	out := screen.String()
	created := strings.Index(out, "✓ Created repo ")
	failed := strings.Index(out, "setting its visibility to public failed")
	require.GreaterOrEqual(t, created, 0)
	require.Greater(t, failed, created, "the failure is the last word:\n%s", out)
}

// The folder suggestion maps the separators folder names use and repo names
// refuse onto hyphens before it is shape-checked.
//
// Not parallel: changes the working directory.
func TestRepoCreateFolderName_MapsSeparators(t *testing.T) {
	for folder, want := range map[string]string{"my_app": "my-app", "My App": "my-app", "_tools_": "tools"} {
		dir := filepath.Join(t.TempDir(), folder)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		testutil.InitRepo(t, dir)
		t.Chdir(dir)
		require.Equal(t, want, repoCreateFolderName(t.Context()), folder)
	}
}

// A failed load does not mark the project as loaded: coming back to it tries
// again, so a one-off failure does not disable the duplicate check for the
// rest of the run.
func TestRepoNameIndex_RetriesAfterAFailedLoad(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	calls := 0
	x := &repoNameIndex{
		ctx: t.Context(),
		list: func(context.Context, string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				return nil, errors.New("connection reset")
			}
			return []string{"web"}, nil
		},
		started: map[string]bool{},
		names:   map[string]map[string]string{},
	}
	x.load("p1")
	require.Eventually(t, func() bool {
		x.mu.Lock()
		defer x.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		return calls == 1 && !x.started["p1"]
	}, time.Second, 5*time.Millisecond, "the failed load is forgotten")

	x.load("p1") // the user comes back to the project
	require.Eventually(t, func() bool { _, ok := x.lookup("p1", "web"); return ok }, time.Second, 5*time.Millisecond)
}

// core accepts a visibility change on a provisioning repo, so --no-wait sets
// the requested visibility straight after the create, and runs the wizard.
//
// Not parallel: sets env vars and swaps package-level seams.
func TestRepoCreate_NoWaitSetsTheVisibility(t *testing.T) {
	f := &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private"}
	f.serve()
	_, _, err := execRepoCreateArgs(t, "web", "--project", "acme", "--no-wait", "--visibility", "public")
	require.NoError(t, err)
	require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies)

	t.Setenv(interactive.EnvTestTTY, "1")
	f = &fakeRepoCreateCore{t: t, projects: defaultCreateProjects(), createdVisibility: "private"}
	f.serve()
	stubRepoCreatePrompt(t, func(_ *cobra.Command, s *repoCreateState) (bool, error) {
		s.answers.Visibility = coreapi.SetRepoVisibilityInputBodyVisibilityPublic
		return true, nil
	})
	_, _, err = execRepoCreateArgs(t, "web", "--no-wait")
	require.NoError(t, err, "the wizard runs with --no-wait")
	require.Equal(t, []string{`{"visibility":"public"}`}, f.visBodies)
}
