package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
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
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{}, "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "my-repo", s.answers.name, "the folder name is suggested when no name was given")
	assert.True(t, s.owner().personal, "the personal row is the starting owner")
	assert.Equal(t, "us", s.answers.region)
}

func TestNewProjectCreateState_PrefillsFromTheCommandLine(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: "widgets", owner: "beta"}, "my-repo")
	require.NoError(t, err)
	assert.Equal(t, "widgets", s.answers.name, "the argument beats the folder name")
	assert.Equal(t, "beta", s.owner().ref)
	assert.Equal(t, "eu", s.answers.region, "the region starts at the owner's")
}

func TestNewProjectCreateState_MatchesOwner(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"Acme", "acme", testWizardAcmeULID} {
		s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: ref}, "")
		require.NoError(t, err, ref)
		assert.Equal(t, "Acme", s.owner().ref, ref)
	}
	for _, ref := range []string{"github:alice", "GitHub:Alice", testWizardAccountULID} {
		s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: ref}, "")
		require.NoError(t, err, ref)
		assert.True(t, s.owner().personal, ref)
	}
}

func TestNewProjectCreateState_RejectsOwnersNotOnOffer(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"locked", "nope", "github:bob"} {
		_, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: ref}, "")
		require.ErrorContains(t, err, "is not an owner you can create projects under", ref)
	}
}

func TestNewProjectCreateState_Region(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "beta", region: "US"}, "")
	require.NoError(t, err)
	assert.Equal(t, "us", s.answers.region, "--region wins over the owner's region")
	s.setOwner(s.owners[0].key)
	assert.Equal(t, "us", s.answers.region, "and stays put when the owner changes")

	_, err = newProjectCreateState(wizardTestData(), projectCreateInput{region: "mars"}, "")
	require.ErrorContains(t, err, `unknown --region "mars": choose one of us, eu`)

	d := wizardTestData()
	d.regions = nil
	_, err = newProjectCreateState(d, projectCreateInput{}, "")
	require.ErrorContains(t, err, "no regions available")
}

// Moving the owner cursor goes through the accessor, which is what makes the
// region page follow the owner.
func TestProjectOwnerAccessor_MovesTheRegion(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	d.orgs = append(d.orgs, wizardTestOrg("01HZX7QAP0C00000000000000", "apac", "ap", true))
	s, err := newProjectCreateState(d, projectCreateInput{}, "")
	require.NoError(t, err)
	acc := projectOwnerAccessor{s: s}

	acc.Set("org:" + testWizardBetaULID)
	assert.Equal(t, "eu", s.answers.region)
	acc.Set(projectOwnerKeyPersonal)
	assert.Equal(t, "us", s.answers.region)
	acc.Set("org:01HZX7QAP0C00000000000000")
	assert.Equal(t, "us", s.answers.region, "an owner region not on offer falls back to the first region")
	assert.Equal(t, "org:01HZX7QAP0C00000000000000", acc.Get())

	// huh writes the value back after every message; that must not undo a
	// region picked by hand, nor count as an owner change.
	s.answers.region = "eu"
	changes := s.ownerChanges
	acc.Set(acc.Get())
	assert.Equal(t, "eu", s.answers.region)
	assert.Equal(t, changes, s.ownerChanges)
}

func TestProjectCreateState_ValidateName(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "acme"}, "")
	require.NoError(t, err)

	require.ErrorContains(t, s.validateName("  "), "enter a project name")
	require.ErrorContains(t, s.validateName(strings.Repeat("x", 101)), "at most 100 characters")
	require.NoError(t, s.validateName(strings.Repeat("x", 100)))
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
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: "my widgets", owner: "acme"}, "")
	require.NoError(t, err)
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
	assert.Equal(t, "✓ Created project acme/widgets in us\n", out)
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
	for _, args := range [][]string{{}, {"widgets"}, {"--owner", "acme"}} {
		_, err := execProjectCreate(t, args...)
		require.ErrorContains(t, err, "a project name and --owner are required without an interactive terminal", args)
		assert.NotContains(t, err.Error(), "ULID")
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
		assert.Equal(t, "widgets", s.answers.name, "the argument is the starting name")
		assert.True(t, s.owner().personal)
		assert.Equal(t, "us", s.answers.region)
		projectOwnerAccessor{s: s}.Set("org:" + testWizardAcmeULID)
		assert.Equal(t, "eu", s.answers.region, "the region follows the owner")
		return true, nil
	})

	out, err := execProjectCreate(t, "widgets")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project acme/widgets in eu\n", out)
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
	assert.Equal(t, "✓ Created project github:alice/widgets in us\n", out)
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

func TestProjectCreate_WizardRejectsUnknownOwnerBeforePrompting(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)
	_, err := execProjectCreate(t, "--owner", "nope")
	require.ErrorContains(t, err, `--owner "nope" is not an owner you can create projects under`)
	assert.Nil(t, fake.created)
}

// A declined summary is the user's answer: it prints the cancellation line on
// the prompt's writer and reports no error.
func TestProjectCreateState_DeclinedSummary(t *testing.T) {
	t.Parallel()
	var w bytes.Buffer
	s := &projectCreateState{confirmed: true}
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
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: " widgets ", owner: "acme"}, "")
	require.NoError(t, err)
	assert.Equal(t, "✓ Owner  Acme (organization)", s.decided(projectStageOwner))
	assert.Equal(t, "✓ Owner  Acme (organization)\n✓ Name   widgets", s.decided(projectStageName))

	s.setOwner(projectOwnerKeyPersonal)
	s.answers.name = "tools"
	assert.Equal(t, "✓ Owner  github:alice (you)\n✓ Name   tools", s.decided(projectStageName))
	assert.NotContains(t, s.decided(projectStageName), testWizardAccountULID)

	// The settled answers come first, the page heading last.
	lines := strings.Split(ansi.Strip(s.pageTitle(projectStageName, projectHeadingRegion)), "\n")
	assert.Equal(t, []string{"✓ Owner  github:alice (you)", "✓ Name   tools", "Region"}, lines)
}

// The live page headings follow an owner or name change.
func TestProjectCreateState_PageTitlesFollowAnswers(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: "widgets", owner: "acme"}, "")
	require.NoError(t, err)
	s.nameGroup(true)
	s.regionGroup(true)
	heading := func(g *huh.Group) string { return ansi.Strip(g.Header()) }
	assert.Contains(t, heading(s.regionGrp), "✓ Owner  Acme (organization)\n✓ Name   widgets")

	projectOwnerAccessor{s: s}.Set(projectOwnerKeyPersonal)
	projectNameAccessor{s: s}.Set("tools")
	assert.Contains(t, heading(s.nameGrp), "✓ Owner  github:alice (you)")
	assert.Contains(t, heading(s.regionGrp), "✓ Owner  github:alice (you)\n✓ Name   tools")
	assert.Equal(t, "tools", s.answers.name)
}

// A ULID --owner is accepted, but when the server does not name the owner the
// success line leaves it out rather than echo the id.
func TestProjectCreate_ULIDOwnerIsNeverEchoed(t *testing.T) {
	fake := newProjectCoreFixture(t)
	fake.omitOwnerName = true

	out, err := execProjectCreate(t, "widgets", "--owner", testWizardAcmeULID)
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project widgets in us\n", out)

	out, err = execProjectCreate(t, "widgets", "--owner", "acme")
	require.NoError(t, err)
	assert.Equal(t, "✓ Created project acme/widgets in us\n", out, "a typed name is still a fine fallback")
}

// An account with no handle is shown as "you", but "you" is not something
// --owner accepts, so it is neither matched nor offered as a command.
func TestProjectCreateState_AccountWithoutHandle(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	d.me.Global.Handle = coreapi.OptString{}
	s, err := newProjectCreateState(d, projectCreateInput{name: "widgets"}, "")
	require.NoError(t, err)

	o := s.owner()
	assert.Equal(t, "you", o.ref)
	assert.Empty(t, o.shownRef(), "the success line leaves the owner out")
	assert.Empty(t, s.command())
	assert.Contains(t, s.summary(), "Command  (none: this owner can only be picked here)")
	assert.NotContains(t, s.summary(), "--owner you")

	_, err = newProjectCreateState(d, projectCreateInput{owner: "you"}, "")
	require.ErrorContains(t, err, "is not an owner you can create projects under")
}

// Org names are not unique. A shared name is not guessed at: --owner leaves
// the choice to the picker, and the summary offers no command the direct path
// would refuse as ambiguous.
func TestProjectCreateState_SameNamedOrgs(t *testing.T) {
	t.Parallel()
	d := wizardTestData()
	const otherAcme = "01HZX7QACME200000000000000"
	d.orgs = append(d.orgs, wizardTestOrg(otherAcme, "Acme", "eu", true))

	s, err := newProjectCreateState(d, projectCreateInput{owner: "Acme"}, "")
	require.NoError(t, err)
	assert.Equal(t, "org:"+testWizardAcmeULID, s.owner().key, "starts on the first of the two, never the personal row")
	assert.Equal(t, `2 organizations are named "Acme"; pick the one you mean.`, s.ownerNote)
	assert.NotContains(t, s.ownerNote, otherAcme)

	s.setOwner("org:" + otherAcme)
	assert.Empty(t, s.command())
	assert.Equal(t, "Acme", s.owner().shownRef(), "the success line still names it")

	s.setOwner("org:" + testWizardBetaULID)
	assert.Contains(t, s.command(), "--owner beta", "a unique name keeps its command")

	// The ULID still picks exactly one.
	s, err = newProjectCreateState(d, projectCreateInput{owner: otherAcme}, "")
	require.NoError(t, err)
	assert.Equal(t, "org:"+otherAcme, s.owner().key)
	assert.Empty(t, s.ownerNote)
}

// huh's accessible runner validates an empty answer before keeping the current
// value, so the pre-filled name has to validate as itself.
func TestProjectCreateState_AccessibleNameKeepsTheSuggestion(t *testing.T) {
	t.Parallel()
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{name: "tools", owner: "acme"}, "")
	require.NoError(t, err)
	in := s.accessibleName(huh.NewInput())
	require.NoError(t, in.RunAccessible(io.Discard, strings.NewReader("\n")))
	assert.Equal(t, "tools", s.answers.name)

	// The kept value is still checked: a taken name is refused.
	s.answers.name = "widgets"
	var out bytes.Buffer
	in = s.accessibleName(huh.NewInput())
	require.NoError(t, in.RunAccessible(&out, strings.NewReader("\nfresh\n")))
	assert.Contains(t, out.String(), `Project name (press Enter for "widgets")`)
	assert.Contains(t, out.String(), `Acme already has a project named "widgets"`)
	assert.Equal(t, "fresh", s.answers.name)
}

// An explicit --owner-type limits --owner to rows of that kind; left at its
// default it does not stop --owner naming the caller's own account.
func TestNewProjectCreateState_OwnerTypeFiltersTheMatch(t *testing.T) {
	t.Parallel()
	org, account := coreapi.CreateProjectInputBodyOwnerTypeOrg, coreapi.CreateProjectInputBodyOwnerTypeAccount

	_, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "acme", ownerKind: account}, "")
	require.EqualError(t, err, `--owner "acme" is not your account`)
	_, err = newProjectCreateState(wizardTestData(), projectCreateInput{owner: "github:alice", ownerKind: org}, "")
	require.EqualError(t, err, `--owner "github:alice" is not an organization you can create projects in`)

	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "acme", ownerKind: org}, "")
	require.NoError(t, err)
	assert.Equal(t, "Acme", s.owner().ref)
	s, err = newProjectCreateState(wizardTestData(), projectCreateInput{owner: "github:alice", ownerKind: account}, "")
	require.NoError(t, err)
	assert.True(t, s.owner().personal)

	// No explicit type: any kind matches.
	s, err = newProjectCreateState(wizardTestData(), projectCreateInput{owner: "github:alice"}, "")
	require.NoError(t, err)
	assert.True(t, s.owner().personal)
}

// The command passes an explicit --owner-type through to the wizard, and only
// an explicit one.
func TestProjectCreate_WizardHonoursExplicitOwnerType(t *testing.T) {
	t.Setenv(interactive.EnvTestTTY, "1")
	fake := newProjectCoreFixture(t)

	_, err := execProjectCreate(t, "--owner", "acme", "--owner-type", "account")
	require.EqualError(t, err, `--owner "acme" is not your account`)
	assert.Nil(t, fake.created)

	var seeded projectOwner
	stubProjectCreatePrompt(t, func(_ *cobra.Command, s *projectCreateState) (bool, error) {
		seeded = s.owner()
		return false, nil
	})
	_, err = execProjectCreate(t, "--owner", "github:alice")
	require.NoError(t, err)
	assert.True(t, seeded.personal, "the default --owner-type does not exclude the account")
}

// In accessible mode huh prints a select's title but not its description, and
// keeps a default only for a plain pointer binding: the owner page's notes move
// into the title there, and the pre-selected owner is still what Enter keeps.
//
// Not parallel: sets ACCESSIBLE.
func TestProjectCreateState_AccessibleOwnerPage(t *testing.T) {
	t.Setenv("ACCESSIBLE", "1")
	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{owner: "beta"}, "")
	require.NoError(t, err)
	s.loginNote = "Using context 'eu'."

	var out bytes.Buffer
	sel := s.ownerGroup(true)
	require.NoError(t, NewAccessibleForm(sel).WithOutput(&out).WithInput(strings.NewReader("\n")).Run())
	assert.Contains(t, out.String(), "Using context 'eu'.")
	assert.Contains(t, out.String(), "1 organization hidden: you can't create projects in it.")

	assert.Equal(t, "org:"+testWizardBetaULID, s.pickedOwner, "Enter keeps the pre-selected owner")
	s.setOwner(s.pickedOwner)
	assert.Equal(t, "eu", s.answers.region, "and applying it moves the region")
}

// An explicit --owner-type org with no --owner starts on the first org, and on
// the personal row only when no org is offered. The accessible picker starts
// from the same owner.
func TestNewProjectCreateState_OwnerTypeOrgPreselectsAnOrg(t *testing.T) {
	t.Parallel()
	org := coreapi.CreateProjectInputBodyOwnerTypeOrg

	s, err := newProjectCreateState(wizardTestData(), projectCreateInput{ownerKind: org}, "")
	require.NoError(t, err)
	assert.Equal(t, "Acme", s.owner().ref, "the first org in picker order")
	assert.Equal(t, "us", s.answers.region, "with its region")
	s.ownerGroup(true)
	assert.Equal(t, "org:"+testWizardAcmeULID, s.pickedOwner, "the accessible picker starts there too")

	d := wizardTestData()
	d.orgs = nil
	s, err = newProjectCreateState(d, projectCreateInput{ownerKind: org}, "")
	require.NoError(t, err)
	assert.True(t, s.owner().personal, "no org offered: the personal row")

	s, err = newProjectCreateState(wizardTestData(), projectCreateInput{}, "")
	require.NoError(t, err)
	assert.True(t, s.owner().personal, "without --owner-type the personal row stays first")
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

	s, err := newProjectCreateState(d, projectCreateInput{owner: "Acme"}, "")
	require.NoError(t, err)
	assert.Equal(t, "org:"+testWizardAcmeULID, s.owner().key, "the oldest of the two")

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
	s, err := newProjectCreateState(d, projectCreateInput{owner: "Acme"}, "")
	require.NoError(t, err)
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

	s, err := newProjectCreateState(d, projectCreateInput{owner: "Solo"}, "")
	require.NoError(t, err)
	assert.Equal(t, "Solo", s.owner().ref)
	assert.Empty(t, s.ownerNote, "only one row matches")
	assert.Equal(t, "Solo  (organization, us)", s.owner().label(4), "no date on a row alone in the picker")
	assert.Contains(t, s.summary(), "Owner    Solo (organization)\n")
	assert.Empty(t, s.command(), "resolveOrgRef would still find two orgs named Solo")
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
	assert.Equal(t, "✓ Created project acme/widgets in us\n", out)

	fake.created = nil
	_, err = execProjectCreate(t, "   ", "--owner", "acme")
	require.ErrorContains(t, err, "a project name and --owner are required without an interactive terminal")
	assert.Nil(t, fake.created)
}
