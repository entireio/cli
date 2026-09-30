package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/auth"
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

// repoCreateAnswers is what the wizard collects. It is its own struct so the
// summary can bind to the answers alone.
type repoCreateAnswers struct {
	projectID    string
	name         string
	visibility   coreapi.SetRepoVisibilityInputBodyVisibility
	advanced     bool
	objectFormat coreapi.CreateRepoInputBodyObjectFormat
}

// repoCreateState is the wizard's model: the projects on offer, the answers
// so far, and the repo names the duplicate check reads.
type repoCreateState struct {
	projects []repoProject
	// hiddenProjects counts the projects left out because the caller cannot
	// create repositories in them.
	hiddenProjects int
	// names holds each project's existing repo names, loaded in the background
	// as projects are picked, because huh validates on the UI loop. Nil in
	// tests that need no duplicate check.
	names *repoNameIndex

	// projectNote is shown on the project page. conflict is the name a create
	// was refused for (409) and the project it was refused in; the name page
	// says so only while that project is the chosen one.
	projectNote string
	conflict    struct{ projectID, name, reason string }
	// pickedProject is the accessible project select's binding; see
	// projectGroup.
	pickedProject string

	// The live pages whose headings recap earlier answers; nil outside the
	// paged form.
	nameGrp, visibilityGrp, advancedGrp, formatGrp *huh.Group
	// nav keeps Shift+Tab working on a page that fails validation; nil
	// outside the paged form.
	nav *uiform.BackNav

	// flagFormat is --object-format as given, which a declined "advanced"
	// keeps; answers.objectFormat is only the format page's cursor.
	flagFormat coreapi.CreateRepoInputBodyObjectFormat

	answers   repoCreateAnswers
	confirmed bool
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

// newRepoCreateState assembles the wizard from the visible projects and what
// the command line said. A --project naming nothing on offer is an error
// rather than a silently different starting point.
func newRepoCreateState(projects []coreapi.Project, req repoCreateRequest, projectRef, defaultName string) (*repoCreateState, error) {
	rows, hidden := repoCreateProjects(projects)
	if len(rows) == 0 {
		return nil, errors.New("you have no project you can create repositories in; create one with `entire project create`")
	}
	s := &repoCreateState{projects: rows, hiddenProjects: hidden, flagFormat: req.objectFormat}
	s.answers = repoCreateAnswers{
		projectID:    rows[0].id,
		name:         cmp.Or(req.name, defaultName),
		visibility:   cmp.Or(req.visibility, repoCreateDefaultVisibility),
		advanced:     req.objectFormat != "",
		objectFormat: cmp.Or(req.objectFormat, repoCreateDefaultObjectFormat),
	}
	if projectRef != "" {
		p, ok := s.matchProject(projectRef)
		if !ok {
			return nil, fmt.Errorf("--project %q is not a project you can create repositories in", projectRef)
		}
		s.answers.projectID = p.id
	}
	return s, nil
}

// matchProject finds the row a --project value names, mirroring the ref
// resolvers: an id, else the name exactly, else case-folded. Project names are
// unique, so at most one row matches.
func (s *repoCreateState) matchProject(ref string) (repoProject, bool) {
	for _, match := range []func(repoProject) bool{
		func(p repoProject) bool { return p.id == ref },
		func(p repoProject) bool { return p.name == ref },
		func(p repoProject) bool { return strings.EqualFold(p.name, ref) },
	} {
		if i := slices.IndexFunc(s.projects, match); i >= 0 {
			return s.projects[i], true
		}
	}
	return repoProject{}, false
}

func (s *repoCreateState) project() repoProject {
	for _, p := range s.projects {
		if p.id == s.answers.projectID {
			return p
		}
	}
	return s.projects[0]
}

// setProject records the project and starts loading its repo names for the
// duplicate check. Re-setting the current project is a no-op: huh writes a
// select's value back after every message.
func (s *repoCreateState) setProject(id string) {
	if id == s.answers.projectID {
		return
	}
	s.answers.projectID = id
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
	if existing, ok := s.names.lookup(s.answers.projectID, name); ok {
		return fmt.Errorf("%s already has a repository named %q", s.project().name, existing)
	}
	return nil
}

func (s *repoCreateState) request() repoCreateRequest {
	req := repoCreateRequest{
		projectID:    s.answers.projectID,
		projectName:  s.project().name,
		name:         strings.TrimSpace(s.answers.name),
		visibility:   s.answers.visibility,
		objectFormat: s.flagFormat,
	}
	if s.answers.advanced {
		req.objectFormat = s.answers.objectFormat
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
	// front (see summaryGroup).
	rows := [][2]string{
		{"Project", req.projectName},
		{"Name", req.name},
		{"Path", "/" + nativeCloneForge + "/" + req.projectName + "/" + req.name},
		{"Visibility", string(req.visibility)},
		{"Object format", s.objectFormatDisplay()},
		{"Command", s.command()},
	}
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%-14s %s", r[0], r[1])
	}
	return b.String()
}

// repoCreatePrompt is the seam the wizard's forms sit behind. It fills in
// s.answers and s.confirmed, returning (false, nil) when the user cancelled
// after being told so. Command-level tests swap it, because the forms are
// unreachable under `go test`.
var repoCreatePrompt = runRepoCreateForms

// runRepoCreateWizard is the prompting path of `repo create`: load the
// projects, ask, then create what the summary showed.
//
// --wait-timeout bounds the loading before the form and, separately, the
// create and readiness wait after it: time spent answering is not the
// server's to spend.
func runRepoCreateWizard(cmd *cobra.Command, req repoCreateRequest, projectRef string, opts repoCreateOptions) error {
	// The "Using context" notice would sit above the form; the wizard shows
	// no login at all.
	auth.SilenceContextNotice()
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		stop := startSpinner(cmd.ErrOrStderr(), "Loading projects")
		loadCtx, cancelLoad := context.WithTimeout(ctx, opts.waitTimeout)
		projects, err := listAllProjects(loadCtx, c)
		cancelLoad()
		// Always erase the spinner line: the form replaces it, and a lingering
		// "✓ Loading…" above the form is noise.
		stop(false)
		if err != nil {
			return fmt.Errorf("list projects: %w", err)
		}
		s, err := newRepoCreateState(projects, req, projectRef, currentFolderName(ctx))
		if err != nil {
			return err
		}
		// The name index outlives no wizard: its loads stop with this call.
		namesCtx, cancelNames := context.WithCancel(ctx)
		defer cancelNames()
		s.names = newRepoNameIndex(namesCtx, c)
		s.names.load(s.answers.projectID)

		for {
			ok, err := repoCreatePrompt(cmd, s)
			if err != nil || !ok {
				return err
			}
			req := s.request()
			createCtx, cancel := context.WithTimeout(ctx, opts.waitTimeout)
			created, err := createRepo(createCtx, c, req)
			if err != nil {
				cancel()
				// Typically the name was free when checked and someone took it
				// since. Reopen the wizard on the same answers, with the
				// server's reason, rather than fail a run the user answered.
				if isRepoCreateConflict(err) {
					s.names.add(req.projectID, req.name)
					s.conflict.projectID, s.conflict.name = req.projectID, req.name
					s.conflict.reason = coreapi.APIError(err)
					continue
				}
				return err
			}
			err = finishRepoCreate(createCtx, cmd, c, req, created, opts)
			cancel()
			return err
		}
	})
}

// runRepoCreateForms runs the wizard as one paged form, so Shift+Tab walks
// back through earlier answers. huh's accessible runner evaluates neither
// OptionsFunc nor DescriptionFunc, so there each stage is its own form, built
// once the answers it depends on are in.
func runRepoCreateForms(cmd *cobra.Command, s *repoCreateState) (bool, error) {
	s.confirmed = true
	if IsAccessibleMode() {
		// Each stage is built only when it runs: the summary's text is read at
		// build time, so building it up front showed stale answers.
		for _, stage := range []func() []*huh.Group{
			func() []*huh.Group { return []*huh.Group{s.projectGroup(true)} },
			func() []*huh.Group {
				return []*huh.Group{s.nameGroup(false), s.visibilityGroup(false), s.advancedGroup(false)}
			},
			func() []*huh.Group {
				if !s.answers.advanced {
					return nil
				}
				return []*huh.Group{s.formatGroup(false)}
			},
			func() []*huh.Group { return []*huh.Group{s.summaryGroup(false)} },
		} {
			groups := stage()
			if len(groups) == 0 {
				continue
			}
			if ok, err := runRepoCreateForm(cmd, s, groups...); !ok || err != nil {
				return ok, err
			}
			// Applied once the project stage has run (a no-op after the
			// others), so the name check reads the chosen project.
			s.setProject(s.pickedProject)
		}
		return true, nil
	}
	s.nav = uiform.NewBackNav()
	return runRepoCreateForm(cmd, s, s.projectGroup(false), s.nameGroup(true), s.visibilityGroup(true),
		s.advancedGroup(true), s.formatGroup(true), s.summaryGroup(true))
}

// runRepoCreateForm runs one form and classifies how it ended: a cancelled
// context is an interruption and comes back as an error, a user abort prints
// the cancellation line where the prompt was, and a declined summary does too.
func runRepoCreateForm(cmd *cobra.Command, s *repoCreateState, groups ...*huh.Group) (bool, error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("repository create: %w", err)
	}
	form := NewAccessibleForm(groups...)
	if s.nav != nil {
		form = form.WithProgramOptions(s.nav.ProgramOption())
	}
	render, err := runPromptForm(cmd, form)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("repository create: %w", ctxErr)
	}
	if err != nil {
		return false, handleFormCancellation(render, repoCreateCancelled, err)
	}
	return s.confirm(render), nil
}

// confirm turns a declined summary into the cancellation line, written where
// the prompt was drawn.
func (s *repoCreateState) confirm(render io.Writer) bool {
	if !s.confirmed {
		fmt.Fprintln(render, repoCreateCancelled+" cancelled.")
	}
	return s.confirmed
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
		s.pickedProject = s.answers.projectID
		sel.Value(&s.pickedProject)
	} else {
		sel.Accessor(repoProjectAccessor{s: s})
	}
	var notes []string
	if s.projectNote != "" {
		notes = append(notes, s.projectNote)
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
	lines := []string{"✓ Project     " + s.project().name}
	if stages >= repoStageName {
		lines = append(lines, "✓ Name        "+strings.TrimSpace(s.answers.name))
	}
	if stages >= repoStageVisibility {
		lines = append(lines, "✓ Visibility  "+string(s.answers.visibility))
	}
	return strings.Join(lines, "\n")
}

// pageTitle is a page's heading with the recap, dimmed, above it.
func (s *repoCreateState) pageTitle(stages int, heading string) string {
	// Line by line: rendering the block at once pads every line to the
	// widest one.
	lines := strings.Split(s.decided(stages), "\n")
	for i, l := range lines {
		lines[i] = decidedDim.Render(l)
	}
	return strings.Join(append(lines, heading), "\n")
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

// nameNote explains a create refused with a conflict (typically the name was
// taken meanwhile), while the project it was refused in is the chosen one. Elsewhere that name
// may well be free, so the note would mislead; the name index still refuses
// it if the user goes back to that project.
func (s *repoCreateState) nameNote() string {
	if s.conflict.name == "" || s.conflict.projectID != s.answers.projectID {
		return ""
	}
	// The endpoint's 409 is not documented as a name clash alone, so the
	// server's own words are shown rather than a guess at the cause.
	if s.conflict.reason != "" {
		return fmt.Sprintf("Creating %q was refused (%s); pick another name.", s.conflict.name, s.conflict.reason)
	}
	return fmt.Sprintf("Creating %q was refused as a conflict; pick another name.", s.conflict.name)
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
		in.Description(note()).DescriptionFunc(note, &s.answers.projectID)
	}
	// A name page that fails validation must still let Shift+Tab leave it.
	in.Validate(uiform.Lenient(s.nav, s.validateName))
	s.nameGrp = huh.NewGroup(in.Accessor(repoNameAccessor{s: s}))
	s.refreshPageTitles()
	return s.nameGrp
}

// accessibleName adapts the name input to huh's accessible runner, which keeps
// the current value on an empty answer but validates the empty answer first,
// so a pre-filled name could not be accepted. It also never shows the value it
// would keep, so the question names it. Descriptions are dropped there too, so
// a name note leads the question.
func (s *repoCreateState) accessibleName(in *huh.Input) *huh.Input {
	in.Value(&s.answers.name)
	title := "Repository name"
	if note := s.nameNote(); note != "" {
		title = note + " " + title
	}
	current := strings.TrimSpace(s.answers.name)
	if current == "" {
		return in.Title(title)
	}
	return in.
		Title(fmt.Sprintf("%s (press Enter for %q)", title, current)).
		Validate(func(v string) error {
			return s.validateName(cmp.Or(strings.TrimSpace(v), current))
		})
}

func (s *repoCreateState) visibilityGroup(dynamic bool) *huh.Group {
	sel := huh.NewSelect[coreapi.SetRepoVisibilityInputBodyVisibility]().
		Title("Who can see it?").
		Options(
			huh.NewOption("private  (only people you grant access)", coreapi.SetRepoVisibilityInputBodyVisibilityPrivate),
			huh.NewOption("public   (any signed-in Entire user can read)", coreapi.SetRepoVisibilityInputBodyVisibilityPublic),
		)
	if !dynamic {
		return huh.NewGroup(sel.Value(&s.answers.visibility)).Title(repoHeadingVisibility)
	}
	s.visibilityGrp = huh.NewGroup(sel.Accessor(repoVisibilityAccessor{s: s}))
	s.refreshPageTitles()
	return s.visibilityGrp
}

// advancedGroup gates the settings most people leave alone behind one
// question. Declining keeps whatever is already chosen — a flag's value, or
// the server default — rather than resetting it.
func (s *repoCreateState) advancedGroup(dynamic bool) *huh.Group {
	confirm := huh.NewConfirm().
		Title("Customize advanced options?").
		Affirmative("Yes").
		Negative("No").
		Value(&s.answers.advanced)
	if !dynamic {
		return huh.NewGroup(confirm).Title(repoHeadingAdvanced)
	}
	s.advancedGrp = huh.NewGroup(confirm)
	s.refreshPageTitles()
	return s.advancedGrp
}

// formatGroup offers the object format, only when advanced options were asked
// for. It starts from the flag's value, else the server's default.
func (s *repoCreateState) formatGroup(dynamic bool) *huh.Group {
	sel := huh.NewSelect[coreapi.CreateRepoInputBodyObjectFormat]().
		Title("Which git object format?").
		Options(
			huh.NewOption("sha1    (the default, supported by every git tool)", coreapi.CreateRepoInputBodyObjectFormatSHA1),
			huh.NewOption("sha256  (not yet supported by every git tool)", coreapi.CreateRepoInputBodyObjectFormatSHA256),
		).
		Value(&s.answers.objectFormat)
	if !dynamic {
		return huh.NewGroup(sel).Title(repoHeadingFormat)
	}
	s.formatGrp = huh.NewGroup(sel).WithHideFunc(func() bool { return !s.answers.advanced })
	s.refreshPageTitles()
	return s.formatGrp
}

// summaryGroup shows what will be created and asks to go ahead. dynamic keeps
// the summary current as earlier pages are revisited.
func (s *repoCreateState) summaryGroup(dynamic bool) *huh.Group {
	// The static text matters even when dynamic: huh sizes every page from
	// the first render, before a DescriptionFunc has run, so an empty
	// description left the page too short. The row count never changes, so
	// the initial text sizes it right.
	note := huh.NewNote().Description(s.summary())
	if dynamic {
		note.DescriptionFunc(s.summary, &s.answers)
	}
	confirm := huh.NewConfirm().
		Title("Create this repository?").
		Affirmative("Create").
		Negative("Cancel").
		Value(&s.confirmed)
	if dynamic {
		confirm.Description("Shift+Tab goes back to change an answer.")
	}
	return huh.NewGroup(note, confirm).Title("Summary")
}

// repoProjectAccessor routes the project select through setProject, so a
// cursor move also loads that project's names and refreshes the recaps.
type repoProjectAccessor struct{ s *repoCreateState }

func (a repoProjectAccessor) Get() string  { return a.s.answers.projectID }
func (a repoProjectAccessor) Set(v string) { a.s.setProject(v) }

// repoNameAccessor keeps later pages' recaps in step with the name.
type repoNameAccessor struct{ s *repoCreateState }

func (a repoNameAccessor) Get() string { return a.s.answers.name }
func (a repoNameAccessor) Set(v string) {
	if v == a.s.answers.name {
		return
	}
	a.s.answers.name = v
	a.s.refreshPageTitles()
}

// repoVisibilityAccessor keeps later pages' recaps in step with the visibility.
type repoVisibilityAccessor struct{ s *repoCreateState }

func (a repoVisibilityAccessor) Get() coreapi.SetRepoVisibilityInputBodyVisibility {
	return a.s.answers.visibility
}

func (a repoVisibilityAccessor) Set(v coreapi.SetRepoVisibilityInputBodyVisibility) {
	if v == a.s.answers.visibility {
		return
	}
	a.s.answers.visibility = v
	a.s.refreshPageTitles()
}

// repoNameIndex holds each project's existing repo names for the duplicate
// check. huh validates on its UI loop, so nothing there may wait on the
// network: a project's names are loaded in the background when it is picked,
// and a check made before they arrive (or after the load failed) passes —
// the server's 409 is the backstop. A nil index checks nothing.
type repoNameIndex struct {
	ctx  context.Context //nolint:containedctx // bounds the background loads to the wizard's lifetime
	list func(ctx context.Context, projectID string) ([]string, error)

	mu      sync.Mutex
	started map[string]bool
	names   map[string]map[string]string // project id → folded name → name
}

func newRepoNameIndex(ctx context.Context, c *coreapi.Client) *repoNameIndex {
	return &repoNameIndex{
		ctx: ctx,
		list: func(ctx context.Context, projectID string) ([]string, error) {
			return listProjectRepoNames(ctx, c, projectID)
		},
		started: map[string]bool{},
		names:   map[string]map[string]string{},
	}
}

// load starts fetching a project's names unless that already happened.
func (x *repoNameIndex) load(projectID string) {
	if x == nil {
		return
	}
	x.mu.Lock()
	if x.started[projectID] {
		x.mu.Unlock()
		return
	}
	x.started[projectID] = true
	x.mu.Unlock()
	go func() {
		names, err := x.list(x.ctx, projectID)
		if err != nil {
			logging.Debug(x.ctx, "repo create: skipping duplicate-name pre-check", "error", err.Error())
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
