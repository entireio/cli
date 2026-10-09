package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
)

// repoCreateCancelled names the flow in its cancellation line.
const repoCreateCancelled = "Repository create"

// Wizard defaults for the settings the direct path leaves to the server. The
// wizard always shows a value, so it needs one to start from.
const (
	repoCreateDefaultVisibility   = coreapi.SetRepoVisibilityInputBodyVisibilityPrivate
	repoCreateDefaultObjectFormat = coreapi.CreateRepoInputBodyObjectFormatSHA1
)

// repoProject is one row of the project picker. The user only ever sees name
// and the details after it; id is what the create request needs.
type repoProject struct {
	id     string
	name   string
	owner  string
	region string
	repos  int64
	counts bool // repos is known
}

// label is the project's picker row, padded so the details column lines up.
func (p repoProject) label(width int) string {
	details := make([]string, 0, 3)
	if p.owner != "" {
		details = append(details, p.owner)
	}
	if p.region != "" {
		details = append(details, p.region)
	}
	if p.counts {
		unit := "repos"
		if p.repos == 1 {
			unit = "repo"
		}
		details = append(details, strconv.FormatInt(p.repos, 10)+" "+unit)
	}
	if len(details) == 0 {
		return p.name
	}
	return fmt.Sprintf("%-*s  (%s)", width, p.name, strings.Join(details, ", "))
}

// repoCreateAnswers is what the wizard collects. The summary page binds to
// the summary text rather than to these fields (see summaryBinding), so it
// follows any change to them.
type repoCreateAnswers struct {
	ProjectID    string
	Name         string
	Visibility   coreapi.SetRepoVisibilityInputBodyVisibility
	Advanced     bool
	ObjectFormat coreapi.CreateRepoInputBodyObjectFormat
}

// repoCreateState is the wizard's model: the projects on offer, the answers
// so far, and the repo names the duplicate check reads.
type repoCreateState struct {
	// createWizard runs the forms and holds the summary's answer.
	createWizard

	projects []repoProject
	// hiddenProjects counts the projects left out because the caller cannot
	// create repositories in them.
	hiddenProjects int
	// names holds each project's existing repo names, loaded in the background
	// as projects are picked, because huh validates on the UI loop. Nil in
	// tests that need no duplicate check.
	names *repoNameIndex

	// conflict is the name a create was refused for (409) and the project it
	// was refused in; the name page says so only while that project is the
	// chosen one.
	conflict struct{ projectID, name, reason string }
	// pickedProject is the accessible project select's binding; see
	// projectGroup.
	pickedProject string

	// The live pages whose headings recap earlier answers; nil outside the
	// paged form.
	nameGrp, visibilityGrp, advancedGrp, formatGrp *huh.Group
	answers                                        repoCreateAnswers
}

// repoCreateProjects builds the picker rows from the visible projects, sorted
// by name, leaving out those that say the caller may not create repositories
// in them. It also returns how many were left out.
func repoCreateProjects(projects []coreapi.Project) ([]repoProject, int) {
	rows := make([]repoProject, 0, len(projects))
	for _, p := range projects {
		// A project that reports no capabilities is offered: the server still
		// refuses a create the caller may not make.
		if caps, ok := p.Capabilities.Get(); ok && !caps.CanCreateRepository {
			continue
		}
		n, counts := p.RepositoryCount.Get()
		rows = append(rows, repoProject{
			id:     p.ID,
			name:   p.Name,
			owner:  strings.TrimSpace(p.OwnerName.Or("")),
			region: strings.TrimSpace(p.Region),
			repos:  n,
			counts: counts,
		})
	}
	slices.SortStableFunc(rows, func(a, b repoProject) int {
		return cmp.Compare(strings.ToLower(a.name), strings.ToLower(b.name))
	})
	return rows, len(projects) - len(rows)
}

// newRepoCreateState assembles the wizard from the visible projects. The
// positional name is the only thing the command line carries in (flags mean
// the flag form; see newRepoCreateCmd); without it the name starts as
// defaultName. Everything else starts at the wizard's own defaults: the first
// project, private, the server's object format.
func newRepoCreateState(projects []coreapi.Project, name, defaultName string) (*repoCreateState, error) {
	rows, hidden := repoCreateProjects(projects)
	if len(rows) == 0 {
		return nil, errors.New("you have no project you can create repositories in; create one with `entire project create`")
	}
	s := &repoCreateState{projects: rows, hiddenProjects: hidden, createWizard: createWizard{action: repoCreateCancelled}}
	s.answers = repoCreateAnswers{
		ProjectID:    rows[0].id,
		Name:         cmp.Or(name, defaultName),
		Visibility:   repoCreateDefaultVisibility,
		ObjectFormat: repoCreateDefaultObjectFormat,
	}
	return s, nil
}

func (s *repoCreateState) project() repoProject {
	for _, p := range s.projects {
		if p.id == s.answers.ProjectID {
			return p
		}
	}
	return s.projects[0]
}

// setProject records the project and starts loading its repo names for the
// duplicate check. Re-setting the current project is a no-op: huh writes a
// select's value back after every message.
func (s *repoCreateState) setProject(id string) {
	if id == s.answers.ProjectID {
		return
	}
	s.answers.ProjectID = id
	s.names.load(id)
	s.refreshPageTitles()
}

// validateName catches what can never be a repository name and, once the
// chosen project's names have loaded, one it already holds. It stops there:
// the server owns the naming rules, and a stricter client copy is how the CLI
// came to refuse names the API and the web app accept (COR-1891). Names are
// compared case-insensitively, as the API's own name lookup is.
func (s *repoCreateState) validateName(value string) error {
	name := strings.TrimSpace(value)
	switch {
	case name == "":
		return errors.New("enter a repository name")
	case strings.Contains(name, "/"):
		return errors.New("a repository name cannot contain '/'")
	case strings.IndexFunc(name, unicode.IsSpace) >= 0:
		return errors.New("a repository name cannot contain spaces")
	}
	// The same rule the direct path enforces (refuseGitSuffixRepoName), in any
	// case, said briefly enough for the page.
	if rest, had := gitremote.CutGitDirSuffix(name); had {
		if use, ok := suggestRepoName(rest); ok {
			return fmt.Errorf("a repository name cannot end in %s, in any case (use %q)", gitDirSuffix, use)
		}
		return fmt.Errorf("a repository name cannot end in %s, in any case", gitDirSuffix)
	}
	if existing, ok := s.names.lookup(s.answers.ProjectID, name); ok {
		return fmt.Errorf("%s already has a repository named %q", s.project().name, existing)
	}
	return nil
}

func (s *repoCreateState) request() repoCreateRequest {
	req := repoCreateRequest{
		projectID:   s.answers.ProjectID,
		projectName: s.project().name,
		name:        strings.TrimSpace(s.answers.Name),
		visibility:  s.answers.Visibility,
	}
	// A declined advanced step leaves the format to the server.
	if s.answers.Advanced {
		req.objectFormat = s.answers.ObjectFormat
	}
	return req
}

// objectFormatDisplay is the object format the create will use.
func (s *repoCreateState) objectFormatDisplay() string {
	if f := s.request().objectFormat; f != "" {
		return string(f)
	}
	return string(repoCreateDefaultObjectFormat) + " (server default)"
}

// command is the flag form of the answers, so the summary teaches the
// non-interactive spelling.
func (s *repoCreateState) command() string {
	req := s.request()
	parts := []string{"entire repo create", shellArg(req.name), "--project", shellArg(req.projectName), "--visibility", string(req.visibility)}
	if req.objectFormat != "" {
		parts = append(parts, "--object-format", string(req.objectFormat))
	}
	return strings.Join(parts, " ")
}

func (s *repoCreateState) summary() string {
	req := s.request()
	// The row count must not change while the form runs: huh sizes pages up
	// front (see createWizard.summaryPage).
	rows := []wizardRow{
		{"Project", req.projectName},
		{"Name", req.name},
		{"Path", "/" + nativeCloneForge + "/" + req.projectName + "/" + req.name},
		{"Visibility", string(req.visibility)},
		{"Object format", s.objectFormatDisplay()},
		{"Command", s.command()},
	}
	return wizardRows(rows, wizardLabelWidth("Object format")+2)
}

// repoCreatePrompt is the seam the wizard's forms sit behind. It fills in
// s.answers and s.confirmed, returning (false, nil) when the user cancelled
// after being told so. Command-level tests swap it, because the forms are
// unreachable under `go test`.
var repoCreatePrompt = runRepoCreateForms

// runRepoCreateWizard is the prompting path of `repo create`: load the
// projects, ask, then create what the summary showed.
//
// --wait-timeout is one budget for the server's work, as on the direct path:
// the loading before the form and the create and readiness wait after it
// share it (see repoCreateBudget). Time spent answering is not the server's
// to spend, so it is not counted.
func runRepoCreateWizard(cmd *cobra.Command, name string, opts repoCreateOptions) error {
	// The "Using context" notice would sit above the form; the wizard shows
	// no login at all.
	auth.SilenceContextNotice()
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		budget := &repoCreateBudget{left: opts.waitTimeout}
		stop := startSpinner(cmd.ErrOrStderr(), "Loading projects")
		loadCtx, doneLoading := budget.phase(ctx)
		projects, err := listAllProjects(loadCtx, c)
		doneLoading()
		// Always erase the spinner line: the form replaces it, and a lingering
		// "✓ Loading…" above the form is noise.
		stop(false)
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		s, err := newRepoCreateState(projects, name, repoCreateFolderName(ctx))
		if err != nil {
			return err
		}
		// The name index outlives no wizard: its loads stop with this call.
		namesCtx, cancelNames := context.WithCancel(ctx)
		defer cancelNames()
		s.names = newRepoNameIndex(namesCtx, c)

		for {
			// Every (re)opening loads the chosen project's names: a no-op
			// once they are in, and a retry when an earlier load failed — a
			// user with one project never moves the cursor to trigger it.
			s.names.load(s.answers.ProjectID)
			ok, err := repoCreatePrompt(cmd, s)
			if err != nil || !ok {
				return err
			}
			req := s.request()
			createCtx, done := budget.phase(ctx)
			created, err := createRepo(createCtx, c, req)
			if err != nil {
				done()
				// Typically the name was taken since it was checked (409), or
				// the server judged it invalid (400/422; it owns the naming
				// rules). Reopen the wizard on the same answers, with the
				// server's reason, rather than fail a run the user answered.
				if isRepoCreateRefusal(err) {
					if isRepoCreateConflict(err) {
						s.names.add(req.projectID, req.name)
					}
					s.conflict.projectID, s.conflict.name = req.projectID, req.name
					s.conflict.reason = coreapi.APIError(err)
					continue
				}
				return err
			}
			err = finishRepoCreate(createCtx, cmd, c, req, created, opts)
			done()
			return err
		}
	})
}

// repoCreateFolderName is the name the wizard suggests when none was given:
// the current folder's, as one the server would accept (suggestRepoName
// lowercases it, since create refuses uppercase), or nothing when the folder's
// name cannot become one. It only fills the field; what is created stays the
// server's to judge.
func repoCreateFolderName(ctx context.Context) string {
	// Spaces and underscores, common in folder names, are not allowed in a
	// repo name: they become hyphens before the suggestion is shape-checked.
	folder := strings.Trim(strings.Map(func(r rune) rune {
		if r == '_' || unicode.IsSpace(r) {
			return '-'
		}
		return r
	}, currentFolderName(ctx)), "-")
	if use, ok := suggestRepoName(folder); ok {
		return use
	}
	return ""
}

// repoCreateBudget is what is left of --wait-timeout for the server's work.
// The wizard spends it in phases separated by prompts — loading, then each
// create attempt — and a prompt's time is never charged.
type repoCreateBudget struct{ left time.Duration }

// phase starts a phase bounded by the budget left; done ends it, charging the
// time it took. A spent budget yields an already-expired context, so the
// phase fails on the deadline as the direct path would.
func (b *repoCreateBudget) phase(ctx context.Context) (context.Context, func()) {
	start := time.Now()
	phaseCtx, cancel := context.WithTimeout(ctx, b.left)
	return phaseCtx, func() {
		cancel()
		b.left -= time.Since(start)
	}
}

// runRepoCreateForms runs the wizard as one paged form, so Shift+Tab walks
// back through earlier answers; in accessible mode, as one form per stage
// (see createWizard.runStages).
func runRepoCreateForms(cmd *cobra.Command, s *repoCreateState) (bool, error) {
	if IsAccessibleMode() {
		return s.runStages(cmd,
			// Applied once the project stage has run (a no-op after the
			// others), so the name check reads the chosen project.
			func() { s.setProject(s.pickedProject) },
			func() []*huh.Group { return []*huh.Group{s.projectGroup(true)} },
			func() []*huh.Group {
				return []*huh.Group{s.nameGroup(false), s.visibilityGroup(false), s.advancedGroup(false)}
			},
			func() []*huh.Group {
				if !s.answers.Advanced {
					return nil
				}
				return []*huh.Group{s.formatGroup(false)}
			},
			func() []*huh.Group { return []*huh.Group{s.summaryGroup()} },
		)
	}
	s.startPaged()
	return s.runForm(cmd, s.projectGroup(false), s.nameGroup(true), s.visibilityGroup(true),
		s.advancedGroup(true), s.formatGroup(true), s.summaryGroup())
}

// projectGroup offers the projects. In accessible mode huh drops a select's
// description, so the notes go into the title, and it keeps the current
// choice as the default only for a plain pointer binding, so the select binds
// pickedProject and the caller applies it through setProject afterwards.
func (s *repoCreateState) projectGroup(accessible bool) *huh.Group {
	width := 0
	for _, p := range s.projects {
		width = max(width, utf8.RuneCountInString(p.name))
	}
	opts := make([]huh.Option[string], len(s.projects))
	for i, p := range s.projects {
		opts[i] = huh.NewOption(p.label(width), p.id)
	}
	const question = "Which project should hold it?"
	sel := huh.NewSelect[string]().Title(question).Options(opts...)
	if accessible {
		s.pickedProject = s.answers.ProjectID
		sel.Value(&s.pickedProject)
	} else {
		sel.Accessor(repoProjectAccessor{s: s})
	}
	var notes []string
	// A reopened wizard starts here, so this is where it says why: the name
	// page repeats it while the refused project is chosen.
	if note := s.refusalNote(); note != "" {
		notes = append(notes, note)
	}
	switch s.hiddenProjects {
	case 0:
	case 1:
		notes = append(notes, "1 project hidden: you can't create repositories in it.")
	default:
		notes = append(notes, fmt.Sprintf("%d projects hidden: you can't create repositories in them.", s.hiddenProjects))
	}
	if len(notes) > 0 {
		// huh's accessible runner drops descriptions, so there the notes join
		// the question instead.
		if accessible {
			sel.Title(question + " (" + strings.Join(notes, " ") + ")")
		} else {
			sel.Description(strings.Join(notes, "\n"))
		}
	}
	return huh.NewGroup(sel).Title("Project")
}

// Stages whose answers a later page recaps, in the order the wizard asks them.
const (
	repoStageProject = iota + 1
	repoStageName
	repoStageVisibility
)

// Page headings, which the recap sits above.
const (
	repoHeadingName       = "Name"
	repoHeadingVisibility = "Visibility"
	repoHeadingAdvanced   = "Advanced"
	repoHeadingFormat     = "Object format"
)

// decided lists the answers given before a page, one per line, so each page
// shows what is already settled:
//
//	✓ Project     acme
//	✓ Name        web
//	✓ Visibility  private
func (s *repoCreateState) decided(stages int) string {
	rows := []wizardRow{{"✓ Project", s.project().name}}
	if stages >= repoStageName {
		rows = append(rows, wizardRow{"✓ Name", strings.TrimSpace(s.answers.Name)})
	}
	if stages >= repoStageVisibility {
		rows = append(rows, wizardRow{"✓ Visibility", string(s.answers.Visibility)})
	}
	return wizardRows(rows, wizardLabelWidth("✓ Project", "✓ Name", "✓ Visibility")+2)
}

// pageTitle is a page's heading with the recap above it.
func (s *repoCreateState) pageTitle(stages int, heading string) string {
	return wizardPageTitle(s.decided(stages), heading)
}

// refreshPageTitles rewrites the recapping headings after an answer changes.
// huh reads a group's title afresh on every render, and the line count never
// changes, so the page heights huh measured up front stay right.
func (s *repoCreateState) refreshPageTitles() {
	for _, page := range []struct {
		grp     *huh.Group
		stages  int
		heading string
	}{
		{s.nameGrp, repoStageProject, repoHeadingName},
		{s.visibilityGrp, repoStageName, repoHeadingVisibility},
		{s.advancedGrp, repoStageVisibility, repoHeadingAdvanced},
		{s.formatGrp, repoStageVisibility, repoHeadingFormat},
	} {
		if page.grp != nil {
			page.grp.Title(s.pageTitle(page.stages, page.heading))
		}
	}
}

// nameNote explains a create the server refused (a 409, or a 400/422 over
// what was asked for), while the project it was refused in is the chosen
// one. Elsewhere that name may well be free, so the note would mislead; the
// name index still refuses a taken name if the user goes back to that
// project. It sits on the name page because that is the first answer after
// the project, but it does not claim the name is the cause: the refusal may
// concern another answer (the object format, say), so the server's own words
// say which, and the note asks for whichever answer they name.
func (s *repoCreateState) nameNote() string {
	if s.conflict.projectID != s.answers.ProjectID {
		return ""
	}
	return s.refusalNote()
}

// refusalNote says why the wizard reopened, or nothing when it did not.
func (s *repoCreateState) refusalNote() string {
	if s.conflict.name == "" {
		return ""
	}
	if s.conflict.reason != "" {
		return fmt.Sprintf("Creating %q was refused: %s. Change the answer it concerns, or cancel.", s.conflict.name, s.conflict.reason)
	}
	return fmt.Sprintf("Creating %q was refused; change an answer and try again, or cancel.", s.conflict.name)
}

// nameGroup asks for the name. dynamic recaps the chosen project above the
// heading, kept current through refreshPageTitles; the accessible runner
// leaves earlier answers on screen already, so it gets none.
func (s *repoCreateState) nameGroup(dynamic bool) *huh.Group {
	in := huh.NewInput().
		Title("Repository name").
		Placeholder("my-repo").
		Validate(s.validateName)
	if !dynamic {
		return huh.NewGroup(s.accessibleName(in)).Title(repoHeadingName)
	}
	if s.conflict.name != "" {
		// Follows the project cursor: the note belongs to the project the
		// create was refused in. A blank line stands in elsewhere, because huh
		// sizes the page once and the height must not change.
		note := func() string { return cmp.Or(s.nameNote(), " ") }
		in.Description(note()).DescriptionFunc(note, &s.answers.ProjectID)
	}
	// A name page that fails validation must still let Shift+Tab leave it.
	in.Validate(uiform.Lenient(s.nav, s.validateName))
	s.nameGrp = huh.NewGroup(in.Accessor(repoNameAccessor{s: s}))
	s.refreshPageTitles()
	return s.nameGrp
}

// accessibleName is the name input for huh's accessible runner (see
// wizardDefaultInput). Descriptions are dropped there, so a name note leads
// the question.
func (s *repoCreateState) accessibleName(in *huh.Input) *huh.Input {
	title := "Repository name"
	if note := s.nameNote(); note != "" {
		title = note + " " + title
	}
	return wizardDefaultInput(in, &s.answers.Name, title, s.validateName)
}

func (s *repoCreateState) visibilityGroup(dynamic bool) *huh.Group {
	sel := huh.NewSelect[coreapi.SetRepoVisibilityInputBodyVisibility]().
		Title("Who can see it?").
		Options(
			huh.NewOption("private  (only people you grant access)", coreapi.SetRepoVisibilityInputBodyVisibilityPrivate),
			huh.NewOption("public   (any signed-in Entire user can read)", coreapi.SetRepoVisibilityInputBodyVisibilityPublic),
		)
	if !dynamic {
		return huh.NewGroup(sel.Value(&s.answers.Visibility)).Title(repoHeadingVisibility)
	}
	s.visibilityGrp = huh.NewGroup(sel.Accessor(repoVisibilityAccessor{s: s}))
	s.refreshPageTitles()
	return s.visibilityGrp
}

// advancedGroup gates the settings most people leave alone behind one
// question. Declining leaves them to the server.
func (s *repoCreateState) advancedGroup(dynamic bool) *huh.Group {
	confirm := huh.NewConfirm().
		Title("Customize advanced options?").
		Affirmative("Yes").
		Negative("No").
		Value(&s.answers.Advanced)
	if !dynamic {
		return huh.NewGroup(confirm).Title(repoHeadingAdvanced)
	}
	s.advancedGrp = huh.NewGroup(confirm)
	s.refreshPageTitles()
	return s.advancedGrp
}

// formatGroup offers the object format, only when advanced options were asked
// for. It starts at the server's default.
func (s *repoCreateState) formatGroup(dynamic bool) *huh.Group {
	sel := huh.NewSelect[coreapi.CreateRepoInputBodyObjectFormat]().
		Title("Which git object format?").
		Options(
			huh.NewOption("sha1    (the default, supported by every git tool)", coreapi.CreateRepoInputBodyObjectFormatSHA1),
			huh.NewOption("sha256  (not yet supported by every git tool)", coreapi.CreateRepoInputBodyObjectFormatSHA256),
		).
		Value(&s.answers.ObjectFormat)
	if !dynamic {
		return huh.NewGroup(sel).Title(repoHeadingFormat)
	}
	s.formatGrp = huh.NewGroup(sel).WithHideFunc(func() bool { return !s.answers.Advanced })
	s.refreshPageTitles()
	return s.formatGrp
}

// summaryGroup shows what will be created and asks to go ahead (see
// createWizard.summaryPage). In the paged form it follows revisited answers.
func (s *repoCreateState) summaryGroup() *huh.Group {
	return s.summaryPage(s.summary, "Create this repository?", "Shift+Tab goes back to change an answer.")
}

// repoProjectAccessor routes the project select through setProject, so a
// cursor move also loads that project's names and refreshes the recaps.
type repoProjectAccessor struct{ s *repoCreateState }

func (a repoProjectAccessor) Get() string  { return a.s.answers.ProjectID }
func (a repoProjectAccessor) Set(v string) { a.s.setProject(v) }

// repoNameAccessor keeps later pages' recaps in step with the name.
type repoNameAccessor struct{ s *repoCreateState }

func (a repoNameAccessor) Get() string { return a.s.answers.Name }
func (a repoNameAccessor) Set(v string) {
	if v == a.s.answers.Name {
		return
	}
	a.s.answers.Name = v
	a.s.refreshPageTitles()
}

// repoVisibilityAccessor keeps later pages' recaps in step with the visibility.
type repoVisibilityAccessor struct{ s *repoCreateState }

func (a repoVisibilityAccessor) Get() coreapi.SetRepoVisibilityInputBodyVisibility {
	return a.s.answers.Visibility
}

func (a repoVisibilityAccessor) Set(v coreapi.SetRepoVisibilityInputBodyVisibility) {
	if v == a.s.answers.Visibility {
		return
	}
	a.s.answers.Visibility = v
	a.s.refreshPageTitles()
}

// repoNameIndex holds each project's existing repo names for the duplicate
// check. huh validates on its UI loop, so nothing there may wait on the
// network: a project's names are loaded in the background when it is picked,
// and a check made before they arrive (or after the load failed) passes —
// the server's 409 is the backstop. A nil index checks nothing.
//
// huh sets a select's value on every cursor move, not only on submit, so
// "picked" means the cursor came to rest: a load starts only once settle has
// passed with no further move, and scrolling past N projects costs one load,
// not N.
type repoNameIndex struct {
	ctx    context.Context //nolint:containedctx // bounds the background loads to the wizard's lifetime
	list   func(ctx context.Context, projectID string) ([]string, error)
	settle time.Duration

	mu      sync.Mutex
	pending *time.Timer
	started map[string]bool
	names   map[string]map[string]string // project id → folded name → name
}

// repoNameIndexSettle is how long the project cursor must rest before that
// project's names are loaded.
const repoNameIndexSettle = 300 * time.Millisecond

func newRepoNameIndex(ctx context.Context, c *coreapi.Client) *repoNameIndex {
	return &repoNameIndex{
		ctx: ctx,
		list: func(ctx context.Context, projectID string) ([]string, error) {
			return listProjectRepoNames(ctx, c, projectID)
		},
		settle:  repoNameIndexSettle,
		started: map[string]bool{},
		names:   map[string]map[string]string{},
	}
}

// load asks for a project's names: a later call before settle passes
// replaces it, and a project already fetched is not fetched again.
func (x *repoNameIndex) load(projectID string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.pending != nil {
		x.pending.Stop()
	}
	x.pending = time.AfterFunc(x.settle, func() { x.fetch(projectID) })
}

// fetch loads a project's names unless that already happened.
func (x *repoNameIndex) fetch(projectID string) {
	x.mu.Lock()
	if x.started[projectID] || x.ctx.Err() != nil {
		x.mu.Unlock()
		return
	}
	x.started[projectID] = true
	x.mu.Unlock()
	go func() {
		names, err := x.list(x.ctx, projectID)
		if err != nil {
			logging.Debug(x.ctx, "repo create: skipping duplicate-name pre-check", "error", err.Error())
			// Not loaded after all: a later visit to the project tries
			// again, so a one-off failure does not disable the check for
			// the rest of the run.
			x.mu.Lock()
			delete(x.started, projectID)
			x.mu.Unlock()
			return
		}
		for _, n := range names {
			x.add(projectID, n)
		}
	}()
}

// add records a name the project is known to hold.
func (x *repoNameIndex) add(projectID, name string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.names[projectID] == nil {
		x.names[projectID] = map[string]string{}
	}
	x.names[projectID][strings.ToLower(name)] = name
}

// lookup reports the existing name a new one collides with, if known.
func (x *repoNameIndex) lookup(projectID, name string) (string, bool) {
	if x == nil {
		return "", false
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	existing, ok := x.names[projectID][strings.ToLower(name)]
	return existing, ok
}

// listProjectRepoNames reads a project's repo names, bounded like every
// picker pool: a project past the budget is only partly checked, which the
// server's own conflict check covers.
func listProjectRepoNames(ctx context.Context, c *coreapi.Client, projectID string) ([]string, error) {
	repos, _, err := boundedList(ctx, coreListFetchBudget, func(ctx context.Context, pageToken coreapi.OptString) ([]coreapi.Repo, coreapi.OptString, error) {
		out, err := c.ListProjectRepos(ctx, coreapi.ListProjectReposParams{ProjectId: projectID, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, fmt.Errorf("list project repos: %w", err)
		}
		return out.Repos, out.NextPageToken, nil
	})
	if err != nil {
		return nil, err
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return names, nil
}
