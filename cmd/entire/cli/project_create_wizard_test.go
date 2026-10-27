package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

const (
	testWizardAccountULID = "01HZX7QACC0000000000000000"
	testWizardAcmeULID    = "01HZX7QACME000000000000000"
	testWizardBetaULID    = "01HZX7QBETA000000000000000"
	testWizardLockedULID  = "01HZX7QL0CKED0000000000000"
)

func wizardTestOrg(id, name, region string, canCreate bool) coreapi.Org {
	return coreapi.Org{
		ID: id, Name: name, Region: region,
		Capabilities: coreapi.NewOptOrgCapabilities(coreapi.OrgCapabilities{CanCreateProject: canCreate}),
	}
}

func wizardTestData() projectCreateData {
	return projectCreateData{
		me: &coreapi.GetMeOutputBody{
			Auth: coreapi.MeAuth{Provider: "github"},
			Global: coreapi.MeGlobal{
				AccountId:        testWizardAccountULID,
				Handle:           coreapi.NewOptString("alice"),
				HomeJurisdiction: coreapi.NewOptString("us"),
			},
		},
		orgs: []coreapi.Org{
			wizardTestOrg(testWizardBetaULID, "beta", "eu", true),
			wizardTestOrg(testWizardLockedULID, "locked", "us", false),
			wizardTestOrg(testWizardAcmeULID, "Acme", "us", true),
		},
		regions: []coreapi.TopologyJurisdiction{
			{ID: "us", Label: "United States"},
			{ID: "eu", Label: "Europe"},
		},
		projects: []coreapi.Project{
			{Name: "widgets", OwnerId: testWizardAcmeULID, OwnerType: coreapi.ProjectOwnerTypeOrg},
			{Name: "dotfiles", OwnerId: testWizardAccountULID, OwnerType: coreapi.ProjectOwnerTypeAccount},
		},
	}
}

// wizardState builds the wizard for d with the given starting name, then
// picks the owner whose row is named ref (when ref is set), the way a user
// moving the owner cursor would.
func wizardState(t *testing.T, d projectCreateData, name, ref string) *projectCreateState {
	t.Helper()
	s, err := newProjectCreateState(d, name, "")
	require.NoError(t, err)
	if ref != "" {
		i := slices.IndexFunc(s.owners, func(o projectOwner) bool { return o.ref == ref })
		require.GreaterOrEqual(t, i, 0, "no owner row named %q", ref)
		s.setOwner(s.owners[i].key)
	}
	return s
}

func TestProjectOwners_PersonalFirstThenCreatableOrgs(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	// An org whose capabilities are absent is offered; the server decides.
	d.orgs = append(d.orgs, coreapi.Org{ID: "01HZX7QN0CAPS0000000000000", Name: "nocaps"})

	owners, hidden := projectOwners(d.me, d.orgs)
	refs := make([]string, len(owners))
	for i, o := range owners {
		refs[i] = o.ref
	}
	assert.Equal(t, []string{"github:alice", "Acme", "beta", "nocaps"}, refs)
	assert.Equal(t, 1, hidden, "only the org without create permission is hidden")
	assert.True(t, owners[0].personal)
	assert.Equal(t, "us", owners[0].region, "the personal default is the home jurisdiction")
}

func TestProjectOwner_LabelsSayWhatEachRowIs(t *testing.T) {
	t.Parallel()
	owners, _ := projectOwners(wizardTestData().me, wizardTestData().orgs)
	assert.Equal(t, "github:alice  (you — personal project)", owners[0].label(12))
	assert.Equal(t, "Acme          (organization, us)", owners[1].label(12))
	for _, o := range owners {
		assert.NotContains(t, o.label(12), o.id, "a row never shows an id")
	}
}

func TestNewProjectCreateState_Defaults(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), "", "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "my-repo", s.answers.Name, "the folder name is suggested when no name was given")
	assert.True(t, s.owner().personal, "the personal row is the starting owner")
	assert.Equal(t, "us", s.answers.Region)
}

func TestNewProjectCreateState_NameArgumentBeatsTheFolderName(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), "widgets", "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "widgets", s.answers.Name)
	assert.True(t, s.owner().personal, "the owner still starts on the first row")
}

func TestNewProjectCreateState_NeedsARegion(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	d.regions = nil
	_, err := newProjectCreateState(d, "", "")
	require.ErrorContains(t, err, "no regions available")
}

// Moving the owner cursor goes through the accessor, which is what makes the
// region page follow the owner.
func TestProjectOwnerAccessor_MovesTheRegion(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	d.orgs = append(d.orgs, wizardTestOrg("01HZX7QAP0C00000000000000", "apac", "ap", true))
	s, err := newProjectCreateState(d, "", "")
	require.NoError(t, err)
	acc := projectOwnerAccessor{s: s}

	acc.Set("org:" + testWizardBetaULID)
	assert.Equal(t, "eu", s.answers.Region)
	acc.Set(projectOwnerKeyPersonal)
	assert.Equal(t, "us", s.answers.Region)
	acc.Set("org:01HZX7QAP0C00000000000000")
	assert.Equal(t, "us", s.answers.Region, "an owner region not on offer falls back to the first region")
	assert.Equal(t, "org:01HZX7QAP0C00000000000000", acc.Get())

	// huh writes the value back after every message; that must not undo a
	// region picked by hand, nor count as an owner change.
	s.answers.Region = "eu"
	changes := s.ownerChanges
	acc.Set(acc.Get())
	assert.Equal(t, "eu", s.answers.Region)
	assert.Equal(t, changes, s.ownerChanges)
}

func TestProjectCreateState_ValidateName(t *testing.T) {
	t.Parallel()
	s := wizardState(t, wizardTestData(), "", "Acme")

	require.ErrorContains(t, s.validateName("  "), "enter a project name")
	// The server's shape: 3-32 letters, digits or hyphens, alphanumeric ends.
	for _, bad := range []string{"ui", strings.Repeat("x", 33), "my_app", "my app", "-widgets", "widgets-", "wid.gets"} {
		require.EqualError(t, s.validateName(bad), projectNameRule, bad)
	}
	require.NoError(t, s.validateName(strings.Repeat("x", 32)))
	require.NoError(t, s.validateName("my-app-2"))
	require.EqualError(t, s.validateName("Widgets"), `Acme already has a project named "widgets"`)
	// Project names are unique across owners: another owner's name is taken
	// too, named by the server's owner name when the listing has one.
	require.EqualError(t, s.validateName("dotfiles"), `a project named "dotfiles" already exists; project names are unique`)
	s.existing[1].OwnerName = coreapi.NewOptString("github:alice")
	require.EqualError(t, s.validateName("dotfiles"), `"dotfiles" is taken by github:alice's project; project names are unique`)
	require.NoError(t, s.validateName("gadgets"))

	s.setOwner(projectOwnerKeyPersonal)
	require.EqualError(t, s.validateName("dotfiles"), `you already have a project named "dotfiles"`)
	require.ErrorContains(t, s.validateName("widgets"), `a project named "widgets" already exists`)

	s.existing = nil // the listing failed: only the length checks remain
	require.NoError(t, s.validateName("dotfiles"))
}

func TestProjectCreateState_SummaryNamesNoIDs(t *testing.T) {
	t.Parallel()
	s := wizardState(t, wizardTestData(), "my widgets", "Acme")
	assert.Equal(t, "Name     my widgets\n"+
		"Owner    Acme (organization)\n"+
		"Region   United States (us)\n"+
		`Command  entire project create 'my widgets' --owner Acme --region us`, s.summary())

	s.setOwner(projectOwnerKeyPersonal)
	assert.Equal(t, `entire project create 'my widgets' --owner github:alice --owner-type account --region us`, s.command())
	assert.Contains(t, s.summary(), "github:alice (you)")
	assert.NotContains(t, s.summary(), testWizardAccountULID)
}

// --- command level ----------------------------------------------------------

// fakeProjectCore serves the calls `project create` makes and records the
// create request.
type fakeProjectCore struct {
	mu       sync.Mutex
	requests []string
	created  *coreapi.CreateProjectInputBody
	// omitOwnerName leaves the optional ownerName out of the create response.
	omitOwnerName bool
}

func (f *fakeProjectCore) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		reply := func(body string) {
			if _, err := io.WriteString(w, body); err != nil {
				t.Errorf("write response: %v", err)
			}
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/me":
			reply(`{"auth":{"provider":"github","providerUserId":"1"},
				"global":{"accountId":"` + testWizardAccountULID + `","createdAt":"2026-01-01T00:00:00Z",
				"handle":"alice","handles":[],"homeJurisdiction":"us"}}`)
		case "GET /api/v1/orgs":
			reply(`{"orgs":[{"id":"` + testWizardAcmeULID + `","name":"acme","region":"eu","createdAt":"2026-01-01T00:00:00Z",
				"capabilities":{"canCreateProject":true,"canManageMembers":true,"canDelete":true,"canChangeOwners":true}}]}`)
		case "GET /api/v1/topology":
			reply(`{"jurisdictions":[{"id":"us","label":"United States","regions":[]},{"id":"eu","label":"Europe","regions":[]}]}`)
		case "GET /api/v1/projects":
			reply(`{"projects":[]}`)
		case "POST /api/v1/projects":
			var body coreapi.CreateProjectInputBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.created = &body
			w.WriteHeader(http.StatusCreated)
			ownerName := "acme"
			if body.OwnerType == coreapi.CreateProjectInputBodyOwnerTypeAccount {
				ownerName = "github:alice" // the server's own spelling for an account
			}
			ownerField := `"ownerName":"` + ownerName + `",`
			if f.omitOwnerName {
				ownerField = ""
			}
			reply(`{"id":"01HZX7QPR0JECT000000000000","name":"` + body.Name + `","ownerId":"` + body.OwnerId + `",` +
				ownerField + `"ownerType":"` + string(body.OwnerType) + `","region":"` + body.Region.Or("us") +
				`","createdAt":"2026-01-01T00:00:00Z"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// newProjectCoreFixture installs the fake as the active core client and a
// prompt that fails the test unless replaced.
// Not parallel: swaps package-level seams.
func newProjectCoreFixture(t *testing.T) *fakeProjectCore {
	t.Helper()
	fake := &fakeProjectCore{}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	prevClient := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prevClient })
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) {
		t.Error("the wizard must not open")
		return false, nil
	})
	return fake
}

func stubProjectCreatePrompt(t *testing.T, fn func(*cobra.Command, *projectCreateState) (bool, error)) {
	t.Helper()
	prev := projectCreatePrompt
	projectCreatePrompt = fn
	t.Cleanup(func() { projectCreatePrompt = prev })
}

func execProjectCreate(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newProjectCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append([]string{"create"}, args...))
	err := root.ExecuteContext(t.Context())
	return out.String(), err
}

// With a name and an owner the project is created straight away, even in a
// terminal, and an omitted --region stays omitted.
func TestProjectCreate_CompleteFlagsSkipTheWizard(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)

	out, err := execProjectCreate(t, "widgets", "--owner", "acme")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project widgets in us\n", out)
	require.NotNil(t, fake.created)
	assert.Equal(t, testWizardAcmeULID, fake.created.OwnerId)
	assert.False(t, fake.created.Region.IsSet(), "the server picks the region")
	assert.NotContains(t, fake.requests, "GET /api/v1/topology")
}

func TestProjectCreate_CompleteFlagsJSON(t *testing.T) {
	newProjectCoreFixture(t)
	out, err := execProjectCreate(t, "widgets", "--owner", "acme", "--region", "eu", "--json")
	require.NoError(t, err)
	var got coreapi.CreatedProject
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, "eu", got.Region)
}

// Without a terminal a missing name or owner is refused before any request.
func TestProjectCreate_NonInteractiveNeedsNameAndOwner(t *testing.T) {
	fake := newProjectCoreFixture(t)
	for _, tc := range []struct {
		args    []string
		missing string
	}{
		{nil, "a project name and --owner are required:"},
		{[]string{"widgets"}, "--owner is required:"},
		{[]string{"--owner", "acme"}, "a project name is required:"},
	} {
		_, err := execProjectCreate(t, tc.args...)
		require.ErrorContains(t, err, tc.missing, tc.args)
		// Both spellings of --owner, since a handle needs --owner-type account.
		require.ErrorContains(t, err, "entire project create <name> --owner <org>\n", tc.args)
		require.ErrorContains(t, err, "--owner github:<handle> --owner-type account", tc.args)
		assert.NotContains(t, err.Error(), "ULID")
		assert.NotContains(t, err.Error(), "[<name>]", "nothing that does not paste")
	}
	assert.Empty(t, fake.requests)
}

// Flags mean the flag form: in a terminal too, a flag with a missing name or
// owner is refused before any request rather than seeding the wizard.
func TestProjectCreate_FlagsNeverSeedTheWizard(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	for _, args := range [][]string{
		{"--owner", "acme"},
		{"widgets", "--region", "eu"},
		{"widgets", "--owner-type", "org"},
		{"--owner-type", "account", "--region", "us"},
		{"--owner", ""},
		{"widgets", "--region", ""},
	} {
		_, err := execProjectCreate(t, args...)
		require.ErrorContains(t, err, "required:\n  entire project create <name> --owner <org>", args)
	}
	assert.Empty(t, fake.requests)
}

func TestProjectCreate_InvalidOwnerTypeFailsFirst(t *testing.T) {
	fake := newProjectCoreFixture(t)
	_, err := execProjectCreate(t, "widgets", "--owner-type", "team")
	require.ErrorContains(t, err, `invalid --owner-type "team"`)
	assert.Empty(t, fake.requests)
}

func TestProjectCreate_WizardCreatesWhatTheSummaryShowed(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	stubProjectCreatePrompt(t, func(_ *cobra.Command, s *projectCreateState) (bool, error) {
		assert.Equal(t, "widgets", s.answers.Name, "the argument is the starting name")
		assert.True(t, s.owner().personal)
		assert.Equal(t, "us", s.answers.Region)
		projectOwnerAccessor{s: s}.Set("org:" + testWizardAcmeULID)
		assert.Equal(t, "eu", s.answers.Region, "the region follows the owner")
		return true, nil
	})

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project widgets in eu\n", out)
	require.NotNil(t, fake.created)
	assert.Equal(t, testWizardAcmeULID, fake.created.OwnerId)
	assert.Equal(t, coreapi.CreateProjectInputBodyOwnerTypeOrg, fake.created.OwnerType)
	assert.Equal(t, "eu", fake.created.Region.Or(""), "the wizard sends the region it showed")
}

func TestProjectCreate_WizardPersonalProject(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) { return true, nil })

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project widgets in us\n", out)
	require.NotNil(t, fake.created)
	assert.Equal(t, testWizardAccountULID, fake.created.OwnerId)
	assert.Equal(t, coreapi.CreateProjectInputBodyOwnerTypeAccount, fake.created.OwnerType)
}

func TestProjectCreate_WizardCancelledCreatesNothing(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) { return false, nil })

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Nil(t, fake.created)
}

// A declined summary is the user's answer: it prints the cancellation line on
// the prompt's writer and reports no error.
func TestProjectCreateState_DeclinedSummary(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	s := &projectCreateState{createWizard: createWizard{action: projectCreateCancelled, confirmed: true}}
	assert.True(t, s.confirm(&w))
	assert.Empty(t, w.String())
	s.confirmed = false
	assert.False(t, s.confirm(&w))
	assert.Equal(t, "Project create cancelled.\n", w.String())
}

// Each page recaps what was already decided, one answer per line, and follows
// a changed answer.
func TestProjectCreateState_Decided(t *testing.T) {
	t.Parallel()
	s := wizardState(t, wizardTestData(), " widgets ", "Acme")
	assert.Equal(t, "✓ Owner  Acme (organization)", s.decided(projectStageOwner))
	assert.Equal(t, "✓ Owner  Acme (organization)\n✓ Name   widgets", s.decided(projectStageName))

	s.setOwner(projectOwnerKeyPersonal)
	s.answers.Name = "tools"
	assert.Equal(t, "✓ Owner  github:alice (you)\n✓ Name   tools", s.decided(projectStageName))
	assert.NotContains(t, s.decided(projectStageName), testWizardAccountULID)

	// The settled answers come first, the page heading last.
	lines := strings.Split(ansi.Strip(s.pageTitle(projectStageName, projectHeadingRegion)), "\n")
	assert.Equal(t, []string{"✓ Owner  github:alice (you)", "✓ Name   tools", "Region"}, lines)
}

// The live page headings follow an owner or name change.
func TestProjectCreateState_PageTitlesFollowAnswers(t *testing.T) {
	t.Parallel()
	s := wizardState(t, wizardTestData(), "widgets", "Acme")
	s.nameGroup(true)
	s.regionGroup(true)
	heading := func(g *huh.Group) string { return ansi.Strip(g.Header()) }
	assert.Contains(t, heading(s.regionGrp), "✓ Owner  Acme (organization)\n✓ Name   widgets")

	projectOwnerAccessor{s: s}.Set(projectOwnerKeyPersonal)
	projectNameAccessor{s: s}.Set("tools")
	assert.Contains(t, heading(s.nameGrp), "✓ Owner  github:alice (you)")
	assert.Contains(t, heading(s.regionGrp), "✓ Owner  github:alice (you)\n✓ Name   tools")
	assert.Equal(t, "tools", s.answers.Name)
}

// The success line names the project the way every command takes it, plus the
// region it landed in: never by id, and the same whatever --owner looked like.
func TestProjectCreate_SuccessLineNamesTheProject(t *testing.T) {
	fake := newProjectCoreFixture(t)
	fake.omitOwnerName = true
	for _, owner := range []string{testWizardAcmeULID, "acme"} {
		out, err := execProjectCreate(t, "widgets", "--owner", owner)
		require.NoError(t, err)
		assert.Equal(t, "✓ Created project widgets in us\n", out, owner)
	}
}

// An account with no handle is shown as "you", but "you" is not something
// --owner accepts, so it is not offered as a command.
func TestProjectCreateState_AccountWithoutHandle(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	d.me.Global.Handle = coreapi.OptString{}
	s, err := newProjectCreateState(d, "widgets", "")
	require.NoError(t, err)

	o := s.owner()
	assert.Equal(t, "you", o.ref)
	assert.Empty(t, s.command())
	assert.Contains(t, s.summary(), "Command  (none: your account has no handle)")
	assert.NotContains(t, s.summary(), "--owner you")
}

// Org names are not unique: the summary offers no command for a shared name,
// which the direct path would refuse as ambiguous.
func TestProjectCreateState_SameNamedOrgs(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	const otherAcme = "01HZX7QACME200000000000000"
	d.orgs = append(d.orgs, wizardTestOrg(otherAcme, "Acme", "eu", true))
	s, err := newProjectCreateState(d, "", "")
	require.NoError(t, err)

	s.setOwner("org:" + otherAcme)
	assert.Empty(t, s.command())

	s.setOwner("org:" + testWizardBetaULID)
	assert.Contains(t, s.command(), "--owner beta", "a unique name keeps its command")
}

// huh's accessible runner validates an empty answer before keeping the current
// value, so the pre-filled name has to validate as itself.
func TestProjectCreateState_AccessibleNameKeepsTheSuggestion(t *testing.T) {
	t.Parallel()
	s := wizardState(t, wizardTestData(), "tools", "Acme")
	in := s.accessibleName(huh.NewInput())
	require.NoError(t, in.RunAccessible(io.Discard, strings.NewReader("\n")))
	assert.Equal(t, "tools", s.answers.Name)

	// The kept value is still checked: a taken name is refused.
	s.answers.Name = "widgets"
	var out bytes.Buffer
	in = s.accessibleName(huh.NewInput())
	require.NoError(t, in.RunAccessible(&out, strings.NewReader("\nfresh\n")))
	assert.Contains(t, out.String(), `Project name (press Enter for "widgets")`)
	assert.Contains(t, out.String(), `Acme already has a project named "widgets"`)
	assert.Equal(t, "fresh", s.answers.Name)
}

// In accessible mode huh prints a select's title but not its description, and
// keeps a default only for a plain pointer binding: the owner page's notes move
// into the title there, and the current owner is still what Enter keeps (here
// one the user picked and then came back to).
//
// Not parallel: sets ACCESSIBLE.
func TestProjectCreateState_AccessibleOwnerPage(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	s := wizardState(t, wizardTestData(), "", "beta")
	s.loginNote = "Using context 'eu'."

	var out bytes.Buffer
	sel := s.ownerGroup(true)
	require.NoError(t, NewAccessibleForm(sel).WithOutput(&out).WithInput(strings.NewReader("\n")).Run())
	assert.Contains(t, out.String(), "Using context 'eu'.")
	assert.Contains(t, out.String(), "1 organization hidden: you can't create projects in it.")

	assert.Equal(t, "org:"+testWizardBetaULID, s.pickedOwner, "Enter keeps the pre-selected owner")
	s.setOwner(s.pickedOwner)
	assert.Equal(t, "eu", s.answers.Region, "and applying it moves the region")
}

// Two orgs sharing a name and a region would read identically with ids never
// shown, so their rows and the summary add the day each was created, and the
// oldest comes first (the one --owner pre-selects).
func TestProjectCreateState_SameNamedOrgsAreToldApartByCreationDay(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	const newer = "01HZX7QACME300000000000000"
	older := wizardTestOrg(testWizardAcmeULID, "Acme", "us", true)
	older.CreatedAt = time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	later := wizardTestOrg(newer, "Acme", "us", true)
	later.CreatedAt = time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	d.orgs = []coreapi.Org{later, older, wizardTestOrg(testWizardBetaULID, "beta", "eu", true)}

	s, err := newProjectCreateState(d, "", "")
	require.NoError(t, err)

	labels := make([]string, len(s.owners))
	for i, o := range s.owners {
		labels[i] = o.label(12)
	}
	assert.Equal(t, []string{
		"github:alice  (you — personal project)",
		"Acme          (organization, us, created 2025-03-01)",
		"Acme          (organization, us, created 2026-07-14)",
		"beta          (organization, eu)",
	}, labels, "only the shared name gets a date")
	s.setOwner("org:" + testWizardAcmeULID)
	assert.Contains(t, s.summary(), "Owner    Acme (organization, created 2025-03-01)")

	s.setOwner("org:" + newer)
	assert.Contains(t, s.summary(), "Owner    Acme (organization, created 2026-07-14)")
	assert.Contains(t, s.decided(projectStageOwner), "created 2026-07-14", "the recap too")
	for _, id := range []string{testWizardAcmeULID, newer} {
		assert.NotContains(t, s.summary(), id)
	}
}

// When the creation day does not tell same-named orgs apart, the minute is
// used, and when that collides too, an oldest-first ordinal. Never an id.
func TestSameNameAsides_FallBackToFinerDetail(t *testing.T) {
	t.Parallel()
	at := func(id string, ts time.Time) coreapi.Org {
		o := wizardTestOrg(id, "Acme", "us", true)
		o.CreatedAt = ts
		return o
	}
	day := func(h, m int) time.Time { return time.Date(2025, 3, 1, h, m, 0, 0, time.UTC) }
	const a, b = "01HZX7QACMEA00000000000000", "01HZX7QACMEB00000000000000"

	assert.Equal(t, map[string]string{a: "created 2025-03-01", b: "created 2025-03-02"},
		sameNameAsides([]coreapi.Org{at(a, day(9, 0)), at(b, day(9, 0).AddDate(0, 0, 1))}))
	assert.Equal(t, map[string]string{a: "created 2025-03-01 09:00 UTC", b: "created 2025-03-01 14:30 UTC"},
		sameNameAsides([]coreapi.Org{at(a, day(9, 0)), at(b, day(14, 30))}), "same day: the minute")
	assert.Equal(t, map[string]string{a: "#1", b: "#2"},
		sameNameAsides([]coreapi.Org{at(a, day(9, 0)), at(b, day(9, 0).Add(10*time.Second))}), "same minute: an ordinal")

	// Wired through: rows and the summary read differently, with no ids.
	d := wizardTestData()
	d.orgs = []coreapi.Org{at(a, day(9, 0)), at(b, day(9, 0).Add(10*time.Second))}
	s, err := newProjectCreateState(d, "", "")
	require.NoError(t, err)
	s.setOwner(s.owners[1].key)
	assert.Equal(t, "Acme  (organization, us, #1)", s.owners[1].label(4))
	assert.Equal(t, "Acme  (organization, us, #2)", s.owners[2].label(4))
	assert.Contains(t, s.summary(), "Owner    Acme (organization, #1)")
	assert.NotContains(t, s.summary(), a)
}

// A hidden namesake still makes the name ambiguous for --owner, so the summary
// offers no command, but the row is alone in the picker and gets no date.
func TestProjectOwners_HiddenNamesakeNeedsNoAside(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	hidden := wizardTestOrg("01HZX7QS0L0HIDDEN000000000", "Solo", "us", false)
	hidden.CreatedAt = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	visible := wizardTestOrg("01HZX7QS0L0VISIBLE00000000", "Solo", "us", true)
	visible.CreatedAt = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	d.orgs = []coreapi.Org{hidden, visible}

	s := wizardState(t, d, "", "Solo")
	assert.Equal(t, "Solo  (organization, us)", s.owner().label(4), "no date on a row alone in the picker")
	assert.Contains(t, s.summary(), "Owner    Solo (organization)\n")
	assert.Empty(t, s.command(), "resolveOrgRef would still find two orgs named Solo")
	assert.Contains(t, s.summary(), `Command  (none: more than one of your organizations is named "Solo")`,
		"the summary says why, though the namesake is hidden")
}

// --json does not stop the wizard: like `grant add`, a terminal still gets the
// prompts (on stderr or the controlling terminal) and stdout carries only the
// created project, so `project create --json | jq` works interactively.
func TestProjectCreate_WizardUnderJSONPrintsOnlyTheObject(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	prompted := false
	stubProjectCreatePrompt(t, func(*cobra.Command, *projectCreateState) (bool, error) {
		prompted = true
		return true, nil
	})

	out, err := execProjectCreate(t, "widgets", "--json")
	require.NoError(t, err)
	assert.True(t, prompted, "--json is not a reason to refuse the wizard")
	require.NotNil(t, fake.created)
	var got coreapi.CreatedProject
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout is the JSON object and nothing else")
	assert.Equal(t, "widgets", got.Name)
}

// The name argument is trimmed before either path runs, so the direct path
// sends what the wizard would, and a blank name counts as missing.
func TestProjectCreate_NameArgumentIsTrimmed(t *testing.T) {
	fake := newProjectCoreFixture(t)
	out, err := execProjectCreate(t, "  widgets  ", "--owner", "acme")
	require.NoError(t, err)
	require.NotNil(t, fake.created)
	assert.Equal(t, "widgets", fake.created.Name)
	assert.Equal(t, "✓ Created project widgets in us\n", out)

	fake.created = nil
	_, err = execProjectCreate(t, "   ", "--owner", "acme")
	require.ErrorContains(t, err, "a project name is required:")
	assert.Nil(t, fake.created)
}

// --owner is not cobra-required (the wizard asks for it), so its help text has
// to say it is required: agents read the flag list and never have a terminal.
func TestProjectCreate_OwnerFlagSaysRequired(t *testing.T) {
	t.Parallel()
	usage := newProjectCreateCmd().Flags().Lookup("owner").Usage
	assert.Contains(t, usage, "required")
	assert.Contains(t, usage, "github:handle")
	assert.NotContains(t, usage, "ULID")
}

// The summary re-renders only when the hashstructure hash of its binding
// (projectCreateAnswers) changes, and hashstructure ignores unexported fields.
// An unexported field would leave the summary showing a stale answer after a
// Shift+Tab revisit while the create used the new one.
func TestProjectCreateAnswers_AllFieldsCountForTheSummaryRefresh(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[projectCreateAnswers]()
	for i := range typ.NumField() {
		assert.True(t, typ.Field(i).IsExported(), "%s must be exported to reach the summary's binding hash", typ.Field(i).Name)
	}
}

// A folder name becomes a suggestion only once it has the server's shape.
func TestSuggestProjectName(t *testing.T) {
	t.Parallel()
	for folder, want := range map[string]string{
		"widgets":               "widgets",
		"MyApp":                 "myapp",
		"my_app":                "my-app",
		"my app.v2":             "my-app-v2",
		"_private_":             "private",
		testWizardAcmeULID:      "",
		"ui":                    "",
		"":                      "",
		strings.Repeat("x", 33): "",
		"café":                  "",
	} {
		assert.Equal(t, want, suggestProjectName(folder), folder)
	}
}

// ENTIRE_TOKEN beats every saved login, so the wizard must not name one. With
// two logins saved the note names the active one, and the token silences it.
//
// Not parallel: sets ENTIRE_CONFIG_DIR and ENTIRE_TOKEN.
func TestWizardLoginNote_SilentUnderEntireToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ENTIRE_CONFIG_DIR", dir)
	t.Setenv(contexts.EnvContextVar, "")
	os.Unsetenv(contexts.EnvContextVar)
	contexts.SetFlagOverrideForTest(t, "")
	t.Setenv(auth.EnvTokenVar, "")
	os.Unsetenv(auth.EnvTokenVar)
	require.NoError(t, contexts.Save(dir, &contexts.File{
		CurrentContext: "work",
		Contexts: []*contexts.Context{
			{Name: "work", CoreURL: "https://core.work.example", Handle: "me", KeychainService: "kc:work"},
			{Name: "home", CoreURL: "https://core.home.example", Handle: "me", KeychainService: "kc:home"},
		},
	}))
	require.Equal(t, "Using context 'work'.", wizardLoginNote(), "two logins saved: the active one is named")

	t.Setenv(auth.EnvTokenVar, "tok")
	assert.Empty(t, wizardLoginNote(), "the token, not 'work', is what the wizard acts as")
}

// The summary follows a revisited answer. Driving the paged form itself:
// through to the summary, Shift+Tab back to the region page, a different
// region, forward again: the summary must show what will be created. It used
// to keep its first render, because huh re-renders a DescriptionFunc only when
// its binding's hash changes and the answers' fields were unexported. Unlike
// TestProjectCreateAnswers_AllFieldsCountForTheSummaryRefresh this catches the
// regression whatever its cause.
func TestProjectCreateWizard_SummaryFollowsARevisit(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), "gadgets", "") // free: "widgets" is Acme's
	require.NoError(t, err)
	s.startPaged()
	form := huh.NewForm(s.ownerGroup(false), s.nameGroup(true), s.regionGroup(true), s.summaryGroup())

	send, run := driveForm(form)
	enter := tea.KeyPressMsg{Code: tea.KeyEnter}
	send(tea.WindowSizeMsg{Width: 120, Height: 40}, run(form.Init()))
	send(enter, enter, enter) // owner (personal), name, region (the owner's: us)
	require.Contains(t, ansi.Strip(form.View()), "--region us", "on the summary")

	send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}) // back to the region page
	send(tea.KeyPressMsg{Code: tea.KeyDown}, enter)            // eu, forward
	require.Equal(t, "eu", s.request().Region.Or(""))
	view := ansi.Strip(form.View())
	require.Contains(t, view, "--region eu", "the summary shows what will be created")
	require.Contains(t, view, "Europe (eu)")
	require.NotContains(t, view, "--region us")
}

// The flag form checks the name rule before any request, as the wizard's Name
// page does, and the help states the rule, so scripts and agents learn it
// without a server 400.
func TestProjectCreate_FlagFormChecksTheNameRule(t *testing.T) {
	fake := newProjectCoreFixture(t)
	// The flag form creates exactly what it is given, so uppercase is refused
	// here, unlike in the wizard.
	for _, name := range []string{"my_app", "ui", "My.App", "MyApp", "WIDGETS"} {
		_, err := execProjectCreate(t, name, "--owner", "acme")
		require.EqualError(t, err, `invalid project name "`+name+`": `+projectNameRule, name)
	}
	// A ULID-shaped name: uppercase already breaks the case rule; lowercase
	// fits the pattern but would be read as an id by every command.
	_, err := execProjectCreate(t, testWizardAcmeULID, "--owner", "acme")
	require.EqualError(t, err, `invalid project name "`+testWizardAcmeULID+`": `+projectNameRule)
	lowerID := strings.ToLower(testWizardAcmeULID)
	_, err = execProjectCreate(t, lowerID, "--owner", "acme")
	require.EqualError(t, err, `invalid project name "`+lowerID+`": `+projectNameNotID)
	assert.Empty(t, fake.requests, "refused before resolving the owner")
	assert.Contains(t, newProjectCreateCmd().Long, "3-32 lowercase letters, digits or hyphens")
	assert.Contains(t, newProjectCreateCmd().Long, "can't look like an id")
}

// The wizard is lenient about case: it accepts a typed name with uppercase,
// says on the Name page and in the summary that it will be lowercased, and
// creates the lowercased name. A ULID-shaped name is refused in any case.
func TestProjectCreateState_WizardLowercasesTypedNames(t *testing.T) {
	t.Parallel()
	s := wizardState(t, wizardTestData(), "MyApp", "Acme")
	require.NoError(t, s.validateName("MyApp"))
	assert.Equal(t, "myapp", s.createName())
	assert.Equal(t, `Will be created as "myapp": project names are lowercase.`, s.nameHint())
	assert.Contains(t, s.summary(), `Name     myapp (lowercased from "MyApp")`)
	assert.Contains(t, s.decided(projectStageName), "✓ Name   myapp")
	assert.Contains(t, s.command(), "entire project create myapp ")
	assert.Equal(t, "myapp", s.request().Name)

	s.answers.Name = "myapp"
	assert.Empty(t, s.nameHint(), "no hint once it is lowercase")
	assert.Contains(t, s.summary(), "Name     myapp\n")

	require.EqualError(t, s.validateName("WIDGETS"), `Acme already has a project named "widgets"`, "checked as lowercased")
	for _, id := range []string{testWizardAcmeULID, strings.ToLower(testWizardAcmeULID)} {
		require.EqualError(t, s.validateName(id), projectNameNotID, id)
	}
}
