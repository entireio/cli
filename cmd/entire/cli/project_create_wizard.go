package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

// projectCreateNameRe is the shape the server accepts for a NEW project's name:
// 3-32 lowercase letters, digits or hyphens, alphanumeric at both ends.
// nativeProjectRe (repo_clone.go) is the lookup pattern and allows uppercase,
// because lookups fold case; it must not stand in for this one.
var projectCreateNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

// The name rules, in the words the name page and the flag form show.
const (
	projectNameRule  = "project names are 3-32 lowercase letters, digits or hyphens, starting and ending with a letter or digit"
	projectNameNotID = "project names can't look like an id (26 letters and digits)"
)

// checkProjectName reports why name, exactly as given, can't be a new
// project's name. A ULID-shaped name is refused although it fits the
// pattern: every command would read it as an id (looksLikeULID folds case),
// so the project could never be found by its name.
func checkProjectName(name string) error {
	if !projectCreateNameRe.MatchString(name) {
		return errors.New(projectNameRule)
	}
	if looksLikeULID(name) {
		return errors.New(projectNameNotID)
	}
	return nil
}

// lowerProjectName is the name the wizard creates for what was typed: the
// wizard is lenient about case (it says so on the Name page and in the
// summary), the flag form is not.
func lowerProjectName(typed string) string {
	return strings.ToLower(strings.TrimSpace(typed))
}

// projectCreateCancelled names the flow in its cancellation line.
const projectCreateCancelled = "Project create"

// projectCreateInput is what `project create` was given on the command line.
// Only the direct path reads the flags: the wizard takes the name at most, as
// the name field's starting text.
type projectCreateInput struct {
	name      string
	owner     string
	ownerType string
	region    string
}

// complete reports whether the command line names everything a project needs,
// in which case it is created without prompting.
func (in projectCreateInput) complete() bool {
	return in.name != "" && in.owner != ""
}

// usesFlags reports whether any create flag was given, empty or not. Flags
// mean the flag form: the wizard never takes them as starting values, so a
// flag with a missing name or owner is refused rather than prompted for.
func (in projectCreateInput) usesFlags(cmd *cobra.Command) bool {
	f := cmd.Flags()
	return f.Changed("owner") || f.Changed("region") || f.Changed("owner-type")
}

// projectOwner is one row of the owner picker. The user only ever sees ref (an
// org name or a provider-qualified handle); key and id are internal, the id
// being what the create request needs.
type projectOwner struct {
	key  string
	kind coreapi.CreateProjectInputBodyOwnerType
	id   string
	ref  string
	// flagRef is how --owner can name this owner, or empty when nothing but
	// its id can: an account with no handle, or an org sharing its exact name
	// with another the caller sees (the direct path refuses such a name as
	// ambiguous).
	flagRef  string
	region   string // the owner's jurisdiction: the region picker's default
	personal bool
	// aside tells an org apart from visible rows with the same name, since
	// its id is never shown: "created 2025-03-01", or finer when that collides
	// (see sameNameAsides). Empty for a name no other visible row shares.
	aside string
}

// noFlagReason says why an owner has no --owner spelling (flagRef is empty),
// for the summary's Command row. Counted over every org, so the namesake may
// be one the picker hides.
func (o projectOwner) noFlagReason() string {
	if o.personal {
		return "your account has no handle"
	}
	return fmt.Sprintf("more than one of your organizations is named %q", o.ref)
}

// orgKind describes an org row: "organization, us", plus the creation day
// when its name is shared.
func (o projectOwner) orgKind() string {
	parts := []string{"organization"}
	if o.region != "" {
		parts = append(parts, o.region)
	}
	if o.aside != "" {
		parts = append(parts, o.aside)
	}
	return strings.Join(parts, ", ")
}

// label is the owner's picker row, padded so the kind column lines up.
func (o projectOwner) label(width int) string {
	kind := "you — personal project"
	if !o.personal {
		kind = o.orgKind()
	}
	return fmt.Sprintf("%-*s  (%s)", width, o.ref, kind)
}

// projectRegion is one jurisdiction the region picker offers.
type projectRegion struct {
	id    string
	label string
}

func (r projectRegion) display() string {
	if r.label == "" || strings.EqualFold(r.label, r.id) {
		return r.id
	}
	return fmt.Sprintf("%s (%s)", r.label, r.id)
}

// projectCreateAnswers is what the wizard collects. The summary page binds to
// the summary text rather than to these fields (see summaryBinding), so it
// follows any change to them.
type projectCreateAnswers struct {
	OwnerKey string
	Name     string
	Region   string
}

// projectCreateState is the wizard's model: the choices on offer, the answers
// so far, and the listing the duplicate-name check reads.
type projectCreateState struct {
	// createWizard runs the forms and holds the summary's answer.
	createWizard

	owners  []projectOwner
	regions []projectRegion
	// hiddenOrgs counts the orgs left out because the caller cannot create
	// projects in them.
	hiddenOrgs int
	// existing is every project the caller can see, fetched once up front
	// because huh validates on the UI loop. Nil when the listing failed; the
	// server's own conflict check still applies.
	existing []coreapi.Project
	// ownerChanges counts owner changes. The region select's options are
	// bound to it rather than to the owner, because huh caches options per
	// binding value and, on a cache hit, leaves the cursor where it was: going
	// back to an owner picked before would then keep the other owner's region.
	ownerChanges int

	// nameGrp and regionGrp are the live pages whose headings recap earlier
	// answers; nil outside the paged form.
	nameGrp, regionGrp *huh.Group

	// loginNote names the acting login on the owner page (wizardLoginNote),
	// in place of the "Using context" notice printed above the form.
	loginNote string
	// pickedOwner is the accessible owner select's binding; see ownerGroup.
	pickedOwner string
	answers     projectCreateAnswers
}

// projectCreateData is everything the wizard loads before it opens.
type projectCreateData struct {
	me       *coreapi.GetMeOutputBody
	orgs     []coreapi.Org
	regions  []coreapi.TopologyJurisdiction
	projects []coreapi.Project
}

// loadProjectCreateData fetches the caller, their orgs, the jurisdictions and
// the visible projects concurrently. Only the project listing may fail: it
// feeds a pre-check the server repeats anyway.
func loadProjectCreateData(ctx context.Context, c *coreapi.Client) (projectCreateData, error) {
	var d projectCreateData
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		me, err := c.GetMe(gctx)
		if err != nil {
			return fmt.Errorf("fetch profile: %w", err)
		}
		d.me = me
		return nil
	})
	g.Go(func() error {
		orgs, err := listAllOrgs(gctx, c)
		if err != nil {
			return fmt.Errorf("list orgs: %w", err)
		}
		d.orgs = orgs
		return nil
	})
	g.Go(func() error {
		top, err := c.GetTopology(gctx)
		if err != nil {
			return fmt.Errorf("list regions: %w", err)
		}
		d.regions = top.Jurisdictions
		return nil
	})
	g.Go(func() error {
		projects, err := listAllProjects(gctx, c)
		if err != nil {
			logging.Debug(gctx, "project create: skipping duplicate-name pre-check", "error", err.Error())
			return nil
		}
		d.projects = projects
		return nil
	})
	if err := g.Wait(); err != nil {
		return projectCreateData{}, err //nolint:wrapcheck // each branch already names what it fetched
	}
	return d, nil
}

// projectOwnerKeyPersonal is the personal row's picker value; org rows are
// keyed by id, which a user never sees.
const projectOwnerKeyPersonal = "account"

// projectOwners builds the owner rows: the caller's own account first, then
// the orgs they may create projects in, sorted by name. It also returns how
// many orgs were left out.
func projectOwners(me *coreapi.GetMeOutputBody, orgs []coreapi.Org) ([]projectOwner, int) {
	profile := profileFromMe(me)
	handle := authIdentityLabel(profile)
	owners := []projectOwner{{
		key:      projectOwnerKeyPersonal,
		kind:     coreapi.CreateProjectInputBodyOwnerTypeAccount,
		id:       me.Global.AccountId,
		ref:      cmp.Or(handle, "you"),
		flagRef:  handle,
		region:   profile.Jurisdiction,
		personal: true,
	}}
	// Counted over every org, creatable or not: that is the listing
	// resolveOrgRef reads when the summary's command is run.
	named := make(map[string]int, len(orgs))
	for _, o := range orgs {
		named[o.Name]++
	}
	creatable := make([]coreapi.Org, 0, len(orgs))
	for _, o := range orgs {
		// An org that reports no capabilities is offered: the server still
		// refuses a create the caller may not make.
		if caps, ok := o.Capabilities.Get(); !ok || caps.CanCreateProject {
			creatable = append(creatable, o)
		}
	}
	// Same-named orgs sort oldest first, which is also the order their
	// sameNameAsides ordinals follow.
	slices.SortStableFunc(creatable, func(a, b coreapi.Org) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)),
			a.CreatedAt.Compare(b.CreatedAt),
		)
	})
	asides := sameNameAsides(creatable)
	for _, o := range creatable {
		owner := projectOwner{
			key:    "org:" + o.ID,
			kind:   coreapi.CreateProjectInputBodyOwnerTypeOrg,
			id:     o.ID,
			ref:    o.Name,
			region: o.Region,
		}
		// The flag spelling counts hidden namesakes too (resolveOrgRef would
		// refuse the name), but the aside only visible ones: a row alone in
		// the picker needs nothing to tell it apart.
		if named[o.Name] == 1 {
			owner.flagRef = o.Name
		}
		owner.aside = asides[o.ID]
		owners = append(owners, owner)
	}
	return owners, len(orgs) - len(creatable)
}

// sameNameAsides tells apart visible orgs sharing an exact name, keyed by id,
// using the least detail that works for the whole group: the creation day,
// else the creation minute (UTC), else an oldest-first ordinal. orgs is in
// picker order, so same-named ones are oldest first already.
func sameNameAsides(orgs []coreapi.Org) map[string]string {
	groups := make(map[string][]coreapi.Org)
	for _, o := range orgs {
		groups[o.Name] = append(groups[o.Name], o)
	}
	asides := make(map[string]string)
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		for _, format := range []func(coreapi.Org) string{
			func(o coreapi.Org) string { return "created " + o.CreatedAt.UTC().Format(time.DateOnly) },
			func(o coreapi.Org) string { return "created " + o.CreatedAt.UTC().Format("2006-01-02 15:04") + " UTC" },
			nil,
		} {
			labels := make([]string, len(group))
			for i, o := range group {
				if format == nil {
					labels[i] = fmt.Sprintf("#%d", i+1)
				} else {
					labels[i] = format(o)
				}
			}
			if distinct(labels) {
				for i, o := range group {
					asides[o.ID] = labels[i]
				}
				break
			}
		}
	}
	return asides
}

// distinct reports whether no label repeats.
func distinct(labels []string) bool {
	seen := make(map[string]bool, len(labels))
	for _, l := range labels {
		if seen[l] {
			return false
		}
		seen[l] = true
	}
	return true
}

// projectRegions maps the topology's jurisdictions to picker rows.
func projectRegions(jurisdictions []coreapi.TopologyJurisdiction) []projectRegion {
	out := make([]projectRegion, 0, len(jurisdictions))
	for _, j := range jurisdictions {
		if j.ID == "" {
			continue
		}
		out = append(out, projectRegion{id: j.ID, label: j.Label})
	}
	return out
}

// newProjectCreateState assembles the wizard from what was loaded. name is
// the name field's starting text: the command's argument, else defaultName.
// The owner starts on the personal row, always the first, and the region on
// that owner's.
func newProjectCreateState(d projectCreateData, name, defaultName string) (*projectCreateState, error) {
	owners, hidden := projectOwners(d.me, d.orgs)
	s := &projectCreateState{
		owners:       owners,
		regions:      projectRegions(d.regions),
		hiddenOrgs:   hidden,
		existing:     d.projects,
		createWizard: createWizard{action: projectCreateCancelled},
	}
	if len(s.regions) == 0 {
		return nil, errors.New("no regions available to create a project in")
	}
	s.answers.Name = cmp.Or(name, defaultName)
	s.setOwner(s.owners[0].key)
	return s, nil
}

func (s *projectCreateState) owner() projectOwner {
	for _, o := range s.owners {
		if o.key == s.answers.OwnerKey {
			return o
		}
	}
	return s.owners[0]
}

// setOwner records the owner and moves the region to that owner's
// jurisdiction (the first region when it has none on offer).
// Re-setting the current owner is a no-op: huh writes a select's value back
// after every message, which must not undo a region the user picked.
func (s *projectCreateState) setOwner(key string) {
	if key == s.answers.OwnerKey {
		return
	}
	s.answers.OwnerKey = key
	s.ownerChanges++
	defer s.refreshPageTitles()
	if r, ok := s.regionByID(s.owner().region); ok {
		s.answers.Region = r.id
		return
	}
	s.answers.Region = s.regions[0].id
}

func (s *projectCreateState) regionByID(id string) (projectRegion, bool) {
	for _, r := range s.regions {
		if strings.EqualFold(r.id, id) {
			return r, true
		}
	}
	return projectRegion{}, false
}

// validateName checks the length the API enforces and, when the listing
// loaded, that no visible project already has the name. Names are compared
// case-insensitively, as the API's own name lookup is.
func (s *projectCreateState) validateName(name string) error {
	// Uppercase is lowered rather than refused (nameHint says so); the rest
	// of the server's shape is checked here so a bad name stops on this page
	// rather than after the summary was confirmed.
	name = lowerProjectName(name)
	if name == "" {
		return errors.New("enter a project name")
	}
	if err := checkProjectName(name); err != nil {
		return err
	}
	// Project names are unique across owners (see resolveProjectByName), so
	// any visible project of that name is a conflict, not only the chosen
	// owner's. Only visible ones can be checked; the server has the last word.
	o := s.owner()
	for _, p := range s.existing {
		if !strings.EqualFold(p.Name, name) {
			continue
		}
		switch {
		case string(p.OwnerType) == string(o.kind) && p.OwnerId == o.id && o.personal:
			return fmt.Errorf("you already have a project named %q", p.Name)
		case string(p.OwnerType) == string(o.kind) && p.OwnerId == o.id:
			return fmt.Errorf("%s already has a project named %q", o.ref, p.Name)
		case p.OwnerName.Or("") != "":
			return fmt.Errorf("%q is taken by %s's project; project names are unique", p.Name, p.OwnerName.Or(""))
		default:
			return fmt.Errorf("a project named %q already exists; project names are unique", p.Name)
		}
	}
	return nil
}

// createName is the name the project is created under: what was typed,
// trimmed and lowercased.
func (s *projectCreateState) createName() string {
	return lowerProjectName(s.answers.Name)
}

// nameHint tells the user, under the name field, that what they typed will be
// lowercased; empty when it already is.
func (s *projectCreateState) nameHint() string {
	if strings.TrimSpace(s.answers.Name) == s.createName() {
		return ""
	}
	return fmt.Sprintf("Will be created as %q: project names are lowercase.", s.createName())
}

// summaryName is the summary's Name row: the name that will be created, plus
// what was typed when the two differ. The accessible runner drops the Name
// page's hint, so this is where it learns of the lowercasing.
func (s *projectCreateState) summaryName() string {
	typed := strings.TrimSpace(s.answers.Name)
	if typed == s.createName() {
		return typed
	}
	return fmt.Sprintf("%s (lowercased from %q)", s.createName(), typed)
}

// ownerDisplay names the chosen owner in the summary.
func (s *projectCreateState) ownerDisplay() string {
	o := s.owner()
	if o.personal {
		return o.ref + " (you)"
	}
	if o.aside != "" {
		// A shared name alone would not say which org this is.
		return o.ref + " (organization, " + o.aside + ")"
	}
	return o.ref + " (organization)"
}

func (s *projectCreateState) regionDisplay() string {
	if r, ok := s.regionByID(s.answers.Region); ok {
		return r.display()
	}
	return s.answers.Region
}

// command is the flag form of the answers, so the summary teaches the
// non-interactive spelling. Empty when the owner has no --owner spelling
// short of its id, which the wizard never shows.
func (s *projectCreateState) command() string {
	o := s.owner()
	if o.flagRef == "" {
		return ""
	}
	parts := []string{"entire project create", shellArg(s.createName()), "--owner", shellArg(o.flagRef)}
	if o.kind == coreapi.CreateProjectInputBodyOwnerTypeAccount {
		parts = append(parts, "--owner-type", ownerTypeAccount)
	}
	parts = append(parts, "--region", shellArg(s.answers.Region))
	return strings.Join(parts, " ")
}

func (s *projectCreateState) summary() string {
	rows := []wizardRow{
		{"Name", s.summaryName()},
		{"Owner", s.ownerDisplay()},
		{"Region", s.regionDisplay()},
	}
	// The row count must not change while the form runs (huh sizes pages up
	// front; see createWizard.summaryPage), so a missing command keeps its row.
	rows = append(rows, wizardRow{"Command", cmp.Or(s.command(), "(none: "+s.owner().noFlagReason()+")")})
	return wizardRows(rows, wizardLabelWidth("Command")+2)
}

// request is the create body the answers describe. The region is always sent:
// the wizard showed one, so that is the one the project gets.
func (s *projectCreateState) request() *coreapi.CreateProjectInputBody {
	o := s.owner()
	return &coreapi.CreateProjectInputBody{
		Name:      s.createName(),
		OwnerId:   o.id,
		OwnerType: o.kind,
		Region:    coreapi.NewOptString(s.answers.Region),
	}
}

// projectCreatePrompt is the seam the wizard's forms sit behind. It fills in
// s.answers and s.confirmed, returning (false, nil) when the user cancelled
// after being told so. Command-level tests swap it, because the forms are
// unreachable under `go test`.
var projectCreatePrompt = runProjectCreateForms

// runProjectCreateWizard is the prompting path of `project create`: load the
// choices, ask, then create what the summary showed.
func runProjectCreateWizard(cmd *cobra.Command, name string) error {
	// The wizard names the acting login on its first page instead, so nothing
	// is printed above the form.
	loginNote := wizardLoginNote()
	auth.SilenceContextNotice()
	return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		stop := startSpinner(cmd.ErrOrStderr(), "Loading owners and regions")
		d, err := loadProjectCreateData(ctx, c)
		// Always erase the spinner line: the form replaces it, and a lingering
		// "✓ Loading…" above the form is noise.
		stop(false)
		if err != nil {
			return err
		}
		s, err := newProjectCreateState(d, name, suggestProjectName(currentFolderName(ctx)))
		if err != nil {
			return err
		}
		s.loginNote = loginNote
		ok, err := projectCreatePrompt(cmd, s)
		if err != nil || !ok {
			return err
		}
		created, err := c.CreateProject(ctx, s.request())
		if err != nil {
			return err
		}
		return printProjectCreated(cmd, &created.Response)
	})
}

// wizardLoginNote is the "Using context 'x'." line a wizard shows on its first
// page in place of the notice printed above the form, which the wizard
// silences. Same rule as that notice: only when several logins are saved and
// none was picked for this invocation with --context; empty otherwise.
func wizardLoginNote() string {
	// ENTIRE_TOKEN wins over every saved login (coreapi.New never resolves a
	// context then), so naming one would name the wrong identity.
	if contexts.Requested() || os.Getenv(auth.EnvTokenVar) != "" {
		return ""
	}
	all, _, err := auth.StoredContexts()
	if err != nil || len(all) < 2 {
		return ""
	}
	c, ok, err := auth.ActiveContext()
	if err != nil || !ok {
		return ""
	}
	return fmt.Sprintf("Using context '%s'.", c.Name)
}

// suggestProjectName turns a folder name into a project name the server
// accepts (lowercased, with "_", "." and spaces as "-"), or "" when it still
// would not fit, so the name field starts empty rather than wrong.
func suggestProjectName(folder string) string {
	name := strings.Trim(strings.Map(func(r rune) rune {
		switch r {
		case '_', '.', ' ':
			return '-'
		}
		return r
	}, strings.ToLower(folder)), "-")
	if checkProjectName(name) != nil {
		return ""
	}
	return name
}

// currentFolderName is the name the wizard suggests when none was given: the
// current repository's folder, or nothing outside one.
func currentFolderName(ctx context.Context) string {
	root, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return ""
	}
	return filepath.Base(root)
}

// runProjectCreateForms runs the wizard as one paged form, so Shift+Tab walks
// back through earlier answers and the region follows the owner; in
// accessible mode, as one form per stage (see createWizard.runStages).
func runProjectCreateForms(cmd *cobra.Command, s *projectCreateState) (bool, error) {
	if IsAccessibleMode() {
		return s.runStages(cmd,
			// Applied once the owner stage has run (a no-op after the
			// others), so the region default follows it.
			func() { s.setOwner(s.pickedOwner) },
			func() []*huh.Group { return []*huh.Group{s.ownerGroup(true)} },
			func() []*huh.Group { return []*huh.Group{s.nameGroup(false), s.regionGroup(false)} },
			func() []*huh.Group { return []*huh.Group{s.summaryGroup()} },
		)
	}
	s.startPaged()
	return s.runForm(cmd, s.ownerGroup(false), s.nameGroup(true), s.regionGroup(true), s.summaryGroup())
}

// ownerGroup offers the owners. In accessible mode huh drops a select's
// description, so the notes go into the title, and it keeps the current
// choice as the default only for a plain pointer binding, so the select binds
// pickedOwner and the caller applies it through setOwner afterwards.
func (s *projectCreateState) ownerGroup(accessible bool) *huh.Group {
	width := 0
	for _, o := range s.owners {
		width = max(width, utf8.RuneCountInString(o.ref))
	}
	opts := make([]huh.Option[string], len(s.owners))
	for i, o := range s.owners {
		opts[i] = huh.NewOption(o.label(width), o.key)
	}
	const question = "Who will own this project?"
	sel := huh.NewSelect[string]().Title(question)
	if accessible {
		s.pickedOwner = s.answers.OwnerKey
		sel.Options(opts...).Value(&s.pickedOwner)
	} else {
		sel.Options(opts...).Accessor(projectOwnerAccessor{s: s})
	}
	var notes []string
	if s.loginNote != "" {
		notes = append(notes, s.loginNote)
	}
	switch s.hiddenOrgs {
	case 0:
	case 1:
		notes = append(notes, "1 organization hidden: you can't create projects in it.")
	default:
		notes = append(notes, fmt.Sprintf("%d organizations hidden: you can't create projects in them.", s.hiddenOrgs))
	}
	switch {
	case len(notes) == 0:
	case accessible:
		sel.Title(question + "\n" + strings.Join(notes, "\n"))
	default:
		sel.Description(strings.Join(notes, "\n"))
	}
	return huh.NewGroup(sel).Title("Owner")
}

// Stages whose answers a later page recaps, in the order the wizard asks them.
const (
	projectStageOwner = iota + 1
	projectStageName
)

// decided lists the answers given before a page, one per line, so each page
// shows what is already settled:
//
//	✓ Owner  acme (organization)
//	✓ Name   widgets
func (s *projectCreateState) decided(stages int) string {
	rows := []wizardRow{{"✓ Owner", s.ownerDisplay()}}
	if stages >= projectStageName {
		rows = append(rows, wizardRow{"✓ Name", s.createName()})
	}
	return wizardRows(rows, wizardLabelWidth("✓ Owner", "✓ Name")+2)
}

// pageTitle is a page's heading with the recap above it.
func (s *projectCreateState) pageTitle(stages int, heading string) string {
	return wizardPageTitle(s.decided(stages), heading)
}

// Page headings, which the recap sits above.
const (
	projectHeadingName   = "Name"
	projectHeadingRegion = "Region"
)

// refreshPageTitles rewrites the recapping headings after an answer changes.
// A group title has no TitleFunc, but huh reads it afresh on every render, so
// rewriting it on the live group is enough. The line count never changes, so
// the page heights huh measured up front stay right.
func (s *projectCreateState) refreshPageTitles() {
	if s.nameGrp != nil {
		s.nameGrp.Title(s.pageTitle(projectStageOwner, projectHeadingName))
	}
	if s.regionGrp != nil {
		s.regionGrp.Title(s.pageTitle(projectStageName, projectHeadingRegion))
	}
}

// nameGroup asks for the name. dynamic recaps the chosen owner above the
// heading, kept current through refreshPageTitles; the accessible runner
// leaves earlier answers on screen already, so it gets none.
func (s *projectCreateState) nameGroup(dynamic bool) *huh.Group {
	in := huh.NewInput().
		Title("Project name").
		Validate(s.validateName)
	if !dynamic {
		return huh.NewGroup(s.accessibleName(in)).Title(projectHeadingName)
	}
	// Shift+Tab off an invalid name must not strand the page; see
	// uiform.BackNav. Enter still validates going forward.
	in.Validate(uiform.Lenient(s.nav, s.validateName))
	// Live as the user types: the hint appears once the name has uppercase.
	in.DescriptionFunc(s.nameHint, &s.answers.Name)
	s.nameGrp = huh.NewGroup(in.Accessor(projectNameAccessor{s: s}))
	s.refreshPageTitles()
	return s.nameGrp
}

// accessibleName is the name input for huh's accessible runner (see
// wizardDefaultInput).
func (s *projectCreateState) accessibleName(in *huh.Input) *huh.Input {
	return wizardDefaultInput(in, &s.answers.Name, "Project name", s.validateName)
}

// regionGroup offers the jurisdictions. dynamic recaps the owner and name
// above the heading and re-selects the owner's region whenever the owner
// changes: huh re-runs an OptionsFunc when its binding changes and then moves
// the cursor to the bound value, which setOwner has already moved. See
// ownerChanges for why the binding is a counter.
func (s *projectCreateState) regionGroup(dynamic bool) *huh.Group {
	opts := make([]huh.Option[string], len(s.regions))
	for i, r := range s.regions {
		opts[i] = huh.NewOption(r.display(), r.id)
	}
	sel := huh.NewSelect[string]().
		Title("Where should its data live?").
		Value(&s.answers.Region)
	if !dynamic {
		return huh.NewGroup(sel.Options(opts...)).Title(projectHeadingRegion)
	}
	// An OptionsFunc otherwise gets huh's fixed default height, padding the
	// list with empty rows. The options never change, only the cursor does, so
	// size it to them: the height counts the title line too.
	sel.Height(len(opts)+1).
		OptionsFunc(func() []huh.Option[string] { return opts }, &s.ownerChanges)
	s.regionGrp = huh.NewGroup(sel)
	s.refreshPageTitles()
	return s.regionGrp
}

// summaryGroup shows what will be created and asks to go ahead (see
// createWizard.summaryPage). In the paged form it follows revisited answers.
func (s *projectCreateState) summaryGroup() *huh.Group {
	return s.summaryPage(s.summary, "Create this project?", "")
}

// projectOwnerAccessor routes the owner select through setOwner, so moving the
// cursor also moves the region default.
type projectOwnerAccessor struct{ s *projectCreateState }

func (a projectOwnerAccessor) Get() string  { return a.s.answers.OwnerKey }
func (a projectOwnerAccessor) Set(v string) { a.s.setOwner(v) }

// projectNameAccessor keeps the region page's recap in step with the name.
type projectNameAccessor struct{ s *projectCreateState }

func (a projectNameAccessor) Get() string { return a.s.answers.Name }
func (a projectNameAccessor) Set(v string) {
	if v == a.s.answers.Name {
		return
	}
	a.s.answers.Name = v
	a.s.refreshPageTitles()
}
