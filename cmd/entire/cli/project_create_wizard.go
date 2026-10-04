package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/uiform"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/internal/entireclient/contexts"
)

// projectNameMaxLen mirrors CreateProjectInputBody.name's maxLength, so the
// wizard rejects an over-long name before the server does.
const projectNameMaxLen = 100

// projectCreateCancelled names the flow in its cancellation line.
const projectCreateCancelled = "Project create"

// projectCreateInput is what `project create` was given on the command line.
// In the wizard every field is only a starting value.
type projectCreateInput struct {
	name      string
	owner     string
	ownerType string
	region    string
	// ownerKind is the parsed --owner-type, set only when the flag was given:
	// the wizard then offers --owner only rows of that kind. Left unset, the
	// flag's "org" default does not stop --owner naming the caller's account.
	ownerKind coreapi.CreateProjectInputBodyOwnerType
}

// complete reports whether the command line names everything a project needs,
// in which case it is created without prompting.
func (in projectCreateInput) complete() bool {
	return in.name != "" && in.owner != ""
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

// shownRef names the owner in the success line, or empty when all there is to
// show is the "you" stand-in for an account with no handle.
func (o projectOwner) shownRef() string {
	if o.personal && o.flagRef == "" {
		return ""
	}
	return o.ref
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

// projectCreateAnswers is what the wizard collects. It is its own struct so
// the summary can bind to the answers alone rather than to every listing.
type projectCreateAnswers struct {
	ownerKey string
	name     string
	region   string
}

// projectCreateState is the wizard's model: the choices on offer, the answers
// so far, and the listing the duplicate-name check reads.
type projectCreateState struct {
	owners  []projectOwner
	regions []projectRegion
	// hiddenOrgs counts the orgs left out because the caller cannot create
	// projects in them.
	hiddenOrgs int
	// existing is every project the caller can see, fetched once up front
	// because huh validates on the UI loop. Nil when the listing failed; the
	// server's own conflict check still applies.
	existing []coreapi.Project
	// regionPinned is set when --region was given: the region then stays put
	// instead of following the owner.
	regionPinned bool
	// ownerChanges counts owner changes. The region select's options are
	// bound to it rather than to the owner, because huh caches options per
	// binding value and, on a cache hit, leaves the cursor where it was: going
	// back to an owner picked before would then keep the other owner's region.
	ownerChanges int

	// nameGrp and regionGrp are the live pages whose headings recap earlier
	// answers; nil outside the paged form.
	nameGrp, regionGrp *huh.Group

	// ownerNote explains on the owner page why no owner was pre-selected.
	ownerNote string

	// loginNote names the acting login on the owner page (wizardLoginNote),
	// in place of the "Using context" notice printed above the form.
	loginNote string
	// pickedOwner is the accessible owner select's binding; see ownerGroup.
	pickedOwner string
	// nav keeps Shift+Tab working off an invalid page in the paged form;
	// nil in accessible mode, which has no back key.
	nav *uiform.BackNav

	answers   projectCreateAnswers
	confirmed bool
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
	// Same-named orgs sort oldest first, so the one --owner pre-selects
	// among them is predictable.
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

// newProjectCreateState assembles the wizard from what was loaded and what the
// command line said. A --owner or --region that names nothing on offer is an
// error rather than a silently different starting point.
func newProjectCreateState(d projectCreateData, in projectCreateInput, defaultName string) (*projectCreateState, error) {
	owners, hidden := projectOwners(d.me, d.orgs)
	s := &projectCreateState{
		owners:     owners,
		regions:    projectRegions(d.regions),
		hiddenOrgs: hidden,
		existing:   d.projects,
	}
	if len(s.regions) == 0 {
		return nil, errors.New("no regions available to create a project in")
	}

	s.answers.name = cmp.Or(in.name, defaultName)

	if in.region != "" {
		r, ok := s.regionByID(in.region)
		if !ok {
			return nil, fmt.Errorf("unknown --region %q: choose one of %s", in.region, strings.Join(s.regionIDs(), ", "))
		}
		s.answers.region = r.id
		s.regionPinned = true
	}

	ownerKey := projectOwnerKeyPersonal
	if in.owner == "" && in.ownerKind == coreapi.CreateProjectInputBodyOwnerTypeOrg {
		// An explicit --owner-type org asks for an org: start on the first
		// one offered, or on the personal row when there is none.
		if i := slices.IndexFunc(s.owners, func(o projectOwner) bool { return !o.personal }); i >= 0 {
			ownerKey = s.owners[i].key
		}
	}
	if in.owner != "" {
		matches := s.matchOwner(in.owner, in.ownerKind)
		switch len(matches) {
		case 0:
			switch in.ownerKind {
			case coreapi.CreateProjectInputBodyOwnerTypeOrg:
				return nil, fmt.Errorf("--owner %q is not an organization you can create projects in", in.owner)
			case coreapi.CreateProjectInputBodyOwnerTypeAccount:
				return nil, fmt.Errorf("--owner %q is not your account", in.owner)
			}
			return nil, fmt.Errorf("--owner %q is not an owner you can create projects under", in.owner)
		case 1:
			ownerKey = matches[0].key
		default:
			// Same-named orgs: the picker is where they can be told apart
			// (their rows and the summary add sameNameAsides), so start
			// there on the first of them, the oldest, which is what was asked
			// for either way. Never the personal row, which Enter would then
			// create under.
			ownerKey = matches[0].key
			s.ownerNote = fmt.Sprintf("%d organizations are named %q; pick the one you mean.", len(matches), in.owner)
		}
	}
	s.setOwner(ownerKey)
	return s, nil
}

// matchOwner finds the rows a --owner value names, mirroring resolveOrgRef: an
// id, else names matched exactly, else case-folded. Several rows sharing the
// name are all returned for the caller to treat as ambiguous. A non-empty kind
// (an explicit --owner-type) limits the match to rows of that kind.
func (s *projectCreateState) matchOwner(ref string, kind coreapi.CreateProjectInputBodyOwnerType) []projectOwner {
	owners := s.owners
	if kind != "" {
		owners = slices.DeleteFunc(slices.Clone(owners), func(o projectOwner) bool { return o.kind != kind })
	}
	for _, o := range owners {
		if o.id == ref {
			return []projectOwner{o}
		}
	}
	name := func(o projectOwner) string {
		if o.personal {
			return o.flagRef
		}
		return o.ref
	}
	var exact, folded []projectOwner
	for _, o := range owners {
		switch n := name(o); {
		case n == "":
		case n == ref:
			exact = append(exact, o)
		case strings.EqualFold(n, ref):
			folded = append(folded, o)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return folded
}

func (s *projectCreateState) owner() projectOwner {
	for _, o := range s.owners {
		if o.key == s.answers.ownerKey {
			return o
		}
	}
	return s.owners[0]
}

// setOwner records the owner and, unless --region pinned it, moves the region
// to that owner's jurisdiction (the first region when it has none on offer).
// Re-setting the current owner is a no-op: huh writes a select's value back
// after every message, which must not undo a region the user picked.
func (s *projectCreateState) setOwner(key string) {
	if key == s.answers.ownerKey {
		return
	}
	s.answers.ownerKey = key
	s.ownerChanges++
	defer s.refreshPageTitles()
	if s.regionPinned {
		return
	}
	if r, ok := s.regionByID(s.owner().region); ok {
		s.answers.region = r.id
		return
	}
	s.answers.region = s.regions[0].id
}

func (s *projectCreateState) regionByID(id string) (projectRegion, bool) {
	for _, r := range s.regions {
		if strings.EqualFold(r.id, id) {
			return r, true
		}
	}
	return projectRegion{}, false
}

func (s *projectCreateState) regionIDs() []string {
	ids := make([]string, len(s.regions))
	for i, r := range s.regions {
		ids[i] = r.id
	}
	return ids
}

// validateName checks the length the API enforces and, when the listing
// loaded, that no visible project already has the name. Names are compared
// case-insensitively, as the API's own name lookup is.
func (s *projectCreateState) validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("enter a project name")
	}
	if utf8.RuneCountInString(name) > projectNameMaxLen {
		return fmt.Errorf("project names are at most %d characters", projectNameMaxLen)
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
	if r, ok := s.regionByID(s.answers.region); ok {
		return r.display()
	}
	return s.answers.region
}

// command is the flag form of the answers, so the summary teaches the
// non-interactive spelling. Empty when the owner has no --owner spelling
// short of its id, which the wizard never shows.
func (s *projectCreateState) command() string {
	o := s.owner()
	if o.flagRef == "" {
		return ""
	}
	parts := []string{"entire project create", shellArg(strings.TrimSpace(s.answers.name)), "--owner", shellArg(o.flagRef)}
	if o.kind == coreapi.CreateProjectInputBodyOwnerTypeAccount {
		parts = append(parts, "--owner-type", ownerTypeAccount)
	}
	parts = append(parts, "--region", s.answers.region)
	return strings.Join(parts, " ")
}

func (s *projectCreateState) summary() string {
	rows := [][2]string{
		{"Name", strings.TrimSpace(s.answers.name)},
		{"Owner", s.ownerDisplay()},
		{"Region", s.regionDisplay()},
	}
	// The row count must not change while the form runs (huh sizes pages up
	// front; see summaryGroup), so a missing command keeps its row.
	rows = append(rows, [2]string{"Command", cmp.Or(s.command(), "(none: this owner can only be picked here)")})
	var b strings.Builder
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%-8s %s", r[0], r[1])
	}
	return b.String()
}

// request is the create body the answers describe. The region is always sent:
// the wizard showed one, so that is the one the project gets.
func (s *projectCreateState) request() *coreapi.CreateProjectInputBody {
	o := s.owner()
	return &coreapi.CreateProjectInputBody{
		Name:      strings.TrimSpace(s.answers.name),
		OwnerId:   o.id,
		OwnerType: o.kind,
		Region:    coreapi.NewOptString(s.answers.region),
	}
}

// projectCreatePrompt is the seam the wizard's forms sit behind. It fills in
// s.answers and s.confirmed, returning (false, nil) when the user cancelled
// after being told so. Command-level tests swap it, because the forms are
// unreachable under `go test`.
var projectCreatePrompt = runProjectCreateForms

// runProjectCreateWizard is the prompting path of `project create`: load the
// choices, ask, then create what the summary showed.
func runProjectCreateWizard(cmd *cobra.Command, in projectCreateInput) error {
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
		s, err := newProjectCreateState(d, in, currentFolderName(ctx))
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
		// Named the way the direct path names it: the server's owner name
		// first, then the wizard's own (never the "you" stand-in).
		return printProjectCreated(cmd, &created.Response, created.Response.OwnerName.Or(s.owner().shownRef()))
	})
}

// wizardLoginNote is the "Using context 'x'." line a wizard shows on its first
// page in place of the notice printed above the form, which the wizard
// silences. Same rule as that notice: only when several logins are saved and
// none was picked for this invocation with --context; empty otherwise.
func wizardLoginNote() string {
	if contexts.Requested() {
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
// back through earlier answers and the region follows the owner. huh's
// accessible runner evaluates neither OptionsFunc nor DescriptionFunc, so there
// each stage is its own form, built once the answers it depends on are in.
func runProjectCreateForms(cmd *cobra.Command, s *projectCreateState) (bool, error) {
	s.confirmed = true
	if IsAccessibleMode() {
		// Each stage is built only when it runs: the summary's text and the
		// region's starting cursor are read at build time, so building them
		// up front showed the answers from before the owner was picked.
		for _, stage := range []func() []*huh.Group{
			func() []*huh.Group { return []*huh.Group{s.ownerGroup(true)} },
			func() []*huh.Group { return []*huh.Group{s.nameGroup(false), s.regionGroup(false)} },
			func() []*huh.Group { return []*huh.Group{s.summaryGroup(false)} },
		} {
			if ok, err := runProjectCreateForm(cmd, s, stage()...); !ok || err != nil {
				return ok, err
			}
			// Applied once the owner stage has run (a no-op after the
			// others), so the region default follows it.
			s.setOwner(s.pickedOwner)
		}
		return true, nil
	}
	s.nav = uiform.NewBackNav()
	return runProjectCreateForm(cmd, s, s.ownerGroup(false), s.nameGroup(true), s.regionGroup(true), s.summaryGroup(true))
}

// runProjectCreateForm runs one form and classifies how it ended: a cancelled
// context is an interruption and comes back as an error, a user abort prints
// the cancellation line where the prompt was, and a declined summary does too.
func runProjectCreateForm(cmd *cobra.Command, s *projectCreateState, groups ...*huh.Group) (bool, error) {
	ctx := cmd.Context()
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("project create: %w", err)
	}
	form := NewAccessibleForm(groups...)
	if s.nav != nil {
		// WithProgramOptions replaces huh's option list rather than adding
		// to it; the one default it drops (output) runPromptForm sets anyway.
		form = form.WithProgramOptions(s.nav.ProgramOption())
	}
	render, err := runPromptForm(cmd, form)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("project create: %w", ctxErr)
	}
	if err != nil {
		return false, handleFormCancellation(render, projectCreateCancelled, err)
	}
	return s.confirm(render), nil
}

// confirm turns a declined summary into the cancellation line, written where
// the prompt was drawn.
func (s *projectCreateState) confirm(render io.Writer) bool {
	if !s.confirmed {
		fmt.Fprintln(render, projectCreateCancelled+" cancelled.")
	}
	return s.confirmed
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
		s.pickedOwner = s.answers.ownerKey
		sel.Options(opts...).Value(&s.pickedOwner)
	} else {
		sel.Options(opts...).Accessor(projectOwnerAccessor{s: s})
	}
	var notes []string
	if s.ownerNote != "" {
		notes = append(notes, s.ownerNote)
	}
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
	lines := []string{"✓ Owner  " + s.ownerDisplay()}
	if stages >= projectStageName {
		lines = append(lines, "✓ Name   "+strings.TrimSpace(s.answers.name))
	}
	return strings.Join(lines, "\n")
}

// decidedDim sets the recap apart from the heading it sits above.
var decidedDim = lipgloss.NewStyle().Faint(true)

// pageTitle is a page's heading with the recap, dimmed, above it:
//
//	✓ Owner  acme (organization)
//	✓ Name   widgets
//	Region
func (s *projectCreateState) pageTitle(stages int, heading string) string {
	// Line by line: rendering the block at once pads every line to the
	// widest one.
	lines := strings.Split(s.decided(stages), "\n")
	for i, l := range lines {
		lines[i] = decidedDim.Render(l)
	}
	return strings.Join(append(lines, heading), "\n")
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
	s.nameGrp = huh.NewGroup(in.Accessor(projectNameAccessor{s: s}))
	s.refreshPageTitles()
	return s.nameGrp
}

// accessibleName adapts the name input to huh's accessible runner, which keeps
// the current value on an empty answer but validates the empty answer first,
// so a pre-filled name could not be accepted. It also never shows the value it
// would keep, so the question names it.
func (s *projectCreateState) accessibleName(in *huh.Input) *huh.Input {
	in.Value(&s.answers.name)
	current := strings.TrimSpace(s.answers.name)
	if current == "" {
		return in
	}
	return in.
		Title(fmt.Sprintf("Project name (press Enter for %q)", current)).
		Validate(func(v string) error {
			return s.validateName(cmp.Or(strings.TrimSpace(v), current))
		})
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
		Value(&s.answers.region)
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

// summaryGroup shows what will be created and asks to go ahead. dynamic keeps
// the summary current as earlier pages are revisited.
func (s *projectCreateState) summaryGroup(dynamic bool) *huh.Group {
	// The static text matters even when dynamic: huh sizes every page from
	// the first render, before a DescriptionFunc has run, so an empty
	// description left the page too short and scrolled the Name row out of
	// view. The row count never changes, so the initial text sizes it right.
	note := huh.NewNote().Description(s.summary())
	if dynamic {
		note.DescriptionFunc(s.summary, &s.answers)
	}
	return huh.NewGroup(
		note,
		huh.NewConfirm().
			Title("Create this project?").
			Affirmative("Create").
			Negative("Cancel").
			Value(&s.confirmed),
	).Title("Summary")
}

// projectOwnerAccessor routes the owner select through setOwner, so moving the
// cursor also moves the region default.
type projectOwnerAccessor struct{ s *projectCreateState }

func (a projectOwnerAccessor) Get() string  { return a.s.answers.ownerKey }
func (a projectOwnerAccessor) Set(v string) { a.s.setOwner(v) }

// projectNameAccessor keeps the region page's recap in step with the name.
type projectNameAccessor struct{ s *projectCreateState }

func (a projectNameAccessor) Get() string { return a.s.answers.name }
func (a projectNameAccessor) Set(v string) {
	if v == a.s.answers.name {
		return
	}
	a.s.answers.name = v
	a.s.refreshPageTitles()
}
