package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
)

// agentHelpAnnotation marks an otherwise-hidden command as worth advertising to
// coding agents through `entire agent-help`. Hidden commands (e.g. trail) opt in
// by setting Annotations[agentHelpAnnotation] = "true".
const agentHelpAnnotation = "entire_agent_help"

// agentHelpRequiresTrailsAnnotation marks a command whose surface should only be
// advertised to agents when trails are enabled for the repo. While the trails
// product may not be available to a user yet, agent-help must not point agents at
// commands they can't use — so trail-gated commands are hidden until the same
// "is trails enabled" signal the first-turn injection already gates on says yes.
const agentHelpRequiresTrailsAnnotation = "entire_agent_help_requires_trails"

// agentHelpAnnotationEnabled is the truthy value for the agent-help annotations.
const agentHelpAnnotationEnabled = "true"

// agentHelpOverview is the only hand-maintained prose in agent-help: a terse,
// high-level "what entire is for" plus the standing repo-inference rule. It names
// no flags or subcommands — those are rendered live from the installed command
// tree — so it changes only when a whole capability area lands, not when a flag
// is added.
const agentHelpOverview = `Entire's CLI is the source of truth for its own usage. Do not guess flags or
subcommands — read them from this command. You are already inside the repo:
entire auto-detects it from the git origin remote, so never ask the user for the
repo name. Pass --repo only to target a DIFFERENT repo.`

// agentHelpAudience answers the question an agent actually has when it reads this
// listing: may I run this without being asked? A flat alphabetical dump of every
// command cannot answer it, so an agent either runs nothing or runs something it
// should have left alone (`entire enable`, `entire review`, `entire org create`).
// Grouping by initiator puts that judgment in the CLI, where it is maintained
// once, rather than in each agent's guesswork or in the first-turn injection,
// which pays for it on every session.
type agentHelpAudience int

const (
	// agentHelpAudienceReadOnly: inspection only. Safe to run unprompted whenever
	// it would inform the work; cannot change repo, account, or Entire state.
	agentHelpAudienceReadOnly agentHelpAudience = iota
	// agentHelpAudienceTaskDriven: part of doing the work, but it writes data or
	// spends tokens. Run when the task calls for it, not speculatively.
	agentHelpAudienceTaskDriven
	// agentHelpAudienceUserOwned: setup, auth, account/admin, or destructive. The
	// agent may suggest these but must not run them on its own initiative.
	agentHelpAudienceUserOwned
)

// agentHelpFacts carries the two INDEPENDENT things agent-help knows about a
// command. Fusing them into one axis was a mistake worth naming: with only an
// audience, the listing had to spend a line per (command × audience) cell, so
// completeness and readability pulled against each other and the listing reached
// 45 lines — past the point an agent reads it at all. They are separate
// questions:
//
//	audience — may I run this unprompted?      (every command, at any depth)
//	listed   — is it worth showing by default? (top-level commands only)
type agentHelpFacts struct {
	audience agentHelpAudience
	// listed puts the command in the bare listing. Curation is the point: an
	// agent mid-task needs the few commands bearing on its work, not an index.
	// Unlisted commands stay reachable — `agent-help <command>` resolves them and
	// the footer names them — so this trades default visibility, not availability.
	listed bool
}

// agentHelpClassification classifies commands by space-separated path relative
// to the root ("status", "checkpoint list"). One table rather than per-command
// Annotations so the whole policy is reviewable in one place — the
// classification is a judgment call and reads as one only when the commands sit
// side by side.
//
// Subcommands are classified wherever their audience differs from their
// parent's. That is what lets a mixed group render as "read-only except: policy"
// on ONE line: naming only the minority side keeps a group at one line however
// many subcommands it grows, where breaking each one out cost a line apiece.
//
// Unclassified commands fall back to agentHelpAudienceUserOwned, unlisted (see
// agentHelpFactsFor): the fail-safe direction is an agent declining to run
// something it could have, never running something it should not have.
// TestAgentHelpClassification_CoversEveryAdvertisedCommand fails CI when a new
// top-level command — or a new child of a listed group — lands unclassified, so
// the fallback is a backstop rather than the normal path.
var agentHelpClassification = map[string]agentHelpFacts{
	// ---- Listed: the commands that bear on an agent's work mid-task. -------
	"status": {agentHelpAudienceReadOnly, true},
	"why":    {agentHelpAudienceReadOnly, true},
	"search": {agentHelpAudienceReadOnly, true},

	"checkpoint":         {agentHelpAudienceTaskDriven, true},
	"checkpoint explain": {agentHelpAudienceReadOnly, false},
	"checkpoint list":    {agentHelpAudienceReadOnly, false},
	"checkpoint search":  {agentHelpAudienceReadOnly, false},
	"checkpoint tokens":  {agentHelpAudienceReadOnly, false},
	"checkpoint policy":  {agentHelpAudienceTaskDriven, false}, // "Inspect and update"

	"session":         {agentHelpAudienceTaskDriven, true},
	"session current": {agentHelpAudienceReadOnly, false},
	"session info":    {agentHelpAudienceReadOnly, false},
	"session list":    {agentHelpAudienceReadOnly, false},
	"session tokens":  {agentHelpAudienceReadOnly, false},
	"session adopt":   {agentHelpAudienceTaskDriven, false},
	"session attach":  {agentHelpAudienceTaskDriven, false},
	"session resume":  {agentHelpAudienceTaskDriven, false}, // switches branch
	"session stop":    {agentHelpAudienceTaskDriven, false},

	// trail is the highest-traffic family by a wide margin, so its read-only
	// subcommands must not disappear behind the group's write-capable label.
	"trail":                 {agentHelpAudienceTaskDriven, true},
	"trail approvals":       {agentHelpAudienceReadOnly, false},
	"trail list":            {agentHelpAudienceReadOnly, false},
	"trail show":            {agentHelpAudienceReadOnly, false},
	"trail watch":           {agentHelpAudienceReadOnly, false},
	"trail approve":         {agentHelpAudienceTaskDriven, false},
	"trail checkout":        {agentHelpAudienceTaskDriven, false},
	"trail comment":         {agentHelpAudienceTaskDriven, false},
	"trail create":          {agentHelpAudienceTaskDriven, false},
	"trail delete":          {agentHelpAudienceTaskDriven, false},
	"trail finding":         {agentHelpAudienceTaskDriven, false},
	"trail request-changes": {agentHelpAudienceTaskDriven, false},
	"trail resume":          {agentHelpAudienceTaskDriven, false},
	"trail update":          {agentHelpAudienceTaskDriven, false},

	// ---- Unlisted: real commands, just not the default view. ---------------
	"activity": {agentHelpAudienceReadOnly, false},
	"blame":    {agentHelpAudienceReadOnly, false},
	"experts":  {agentHelpAudienceReadOnly, false},
	"labs":     {agentHelpAudienceReadOnly, false},
	"recap":    {agentHelpAudienceReadOnly, false},
	"version":  {agentHelpAudienceReadOnly, false},
	"tokens":   {agentHelpAudienceReadOnly, false},
	// tokens is read-only, which is a claim about its children too — classified
	// rather than assumed, so a future mutating child cannot inherit the claim.
	"tokens profile": {agentHelpAudienceReadOnly, false},

	// api is deliberately UNLISTED. It is an escape hatch: the right tool when
	// you are developing against Entire's own APIs, or when no first-class
	// command covers what you need — not during ordinary work in a repo that has
	// Entire enabled, which is what the listing is for. It stays named in the
	// footer index and visible in `entire help`, so an agent that genuinely needs
	// raw access still finds it rather than hand-rolling curl with a token, which
	// is the failure this command exists to prevent.
	"api":      {agentHelpAudienceTaskDriven, false},
	"dispatch": {agentHelpAudienceTaskDriven, false},
	"doctor":   {agentHelpAudienceTaskDriven, false},
	"import":   {agentHelpAudienceTaskDriven, false},
	"runner":   {agentHelpAudienceTaskDriven, false},

	// The user's to start. review and investigate are not destructive but spawn
	// paid multi-agent runs, so an uninvited one spends the user's money.
	"agent":       {agentHelpAudienceUserOwned, false},
	"auth":        {agentHelpAudienceUserOwned, false},
	"clean":       {agentHelpAudienceUserOwned, false},
	"configure":   {agentHelpAudienceUserOwned, false},
	"disable":     {agentHelpAudienceUserOwned, false},
	"enable":      {agentHelpAudienceUserOwned, false},
	"grant":       {agentHelpAudienceUserOwned, false},
	"investigate": {agentHelpAudienceUserOwned, false},
	"login":       {agentHelpAudienceUserOwned, false},
	"logout":      {agentHelpAudienceUserOwned, false},
	"org":         {agentHelpAudienceUserOwned, false},
	"plugin":      {agentHelpAudienceUserOwned, false},
	"project":     {agentHelpAudienceUserOwned, false},
	"repo":        {agentHelpAudienceUserOwned, false},
	"review":      {agentHelpAudienceUserOwned, false},
}

// agentHelpGuidance is agent-only advice about WHEN to reach for a command,
// keyed by the same command path as agentHelpClassification.
//
// It is deliberately not in the command's Long. Cobra's Long is human help, and
// a human who typed `entire api --help` chose that command on purpose — telling
// them it is a last resort is noise at best and condescending at worst. An agent
// is in the opposite position: it is picking from a surface it does not know, so
// "prefer the purpose-built command" is the single most useful thing to say.
// Same fact, opposite value, so it ships on the agent channel only. Anything
// true for both audiences (e.g. "these endpoints are internal and can change")
// belongs in Long instead, where both see it.
var agentHelpGuidance = map[string]string{
	"api": "LAST RESORT. Right in two cases: you are developing against Entire's own\n" +
		"APIs and want a raw response, or no first-class command covers your need.\n" +
		"Otherwise prefer the command built for the job (checkpoint, session, trail,\n" +
		"status, repo, …) — its output is stable, raw endpoints are not. If you are\n" +
		"reaching for this during ordinary work in a repo with Entire enabled, run\n" +
		"`entire agent-help` first; there is probably a command for it. When you do\n" +
		"need it, use this rather than hand-rolling curl — it attaches the right\n" +
		"bearer and dials the right host for you.",
}

// agentHelpFactsFor classifies one command path, defaulting the unclassified
// case to user-owned and unlisted so an unclassified addition is under- rather
// than over-advertised.
func agentHelpFactsFor(path string) agentHelpFacts {
	if f, ok := agentHelpClassification[path]; ok {
		return f
	}
	return agentHelpFacts{audience: agentHelpAudienceUserOwned}
}

// agentHelpClassified reports an explicit classification only. The renderers use
// it so an unclassified subcommand is omitted rather than asserting a
// user-owned default the table never actually made.
func agentHelpClassified(path string) (agentHelpFacts, bool) {
	f, ok := agentHelpClassification[path]
	return f, ok
}

// agentHelpPath is a command's path relative to the root, the key shape used by
// agentHelpClassification ("status", "checkpoint list").
func agentHelpPath(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// agentHelpUserOwnedSlug is both the user-owned slug and the value an unknown
// audience degrades to, so a future enum member can never silently render as an
// agent-runnable one.
const agentHelpUserOwnedSlug = "user-owned"

// agentHelpAudienceSlug is the stable machine-readable form emitted in --json,
// so a structured consumer gets the same answer as a text reader.
func agentHelpAudienceSlug(a agentHelpAudience) string {
	switch a {
	case agentHelpAudienceReadOnly:
		return "read-only"
	case agentHelpAudienceTaskDriven:
		return "task-driven"
	case agentHelpAudienceUserOwned:
		return agentHelpUserOwnedSlug
	default:
		return agentHelpUserOwnedSlug
	}
}

// agentHelpAudienceNote describes a command's audience in one clause.
//
// For a group whose classified subcommands disagree, it names only the SHORTER
// side and leaves the rest implicit ("read-only except: policy", or
// "read-only: show, list, … — others write"). Naming the minority is what keeps
// a mixed group to a single line however many subcommands it gains; naming both
// sides is what made an earlier revision of this listing grow a line per
// subcommand until it stopped being readable.
func agentHelpAudienceNote(cmd *cobra.Command, facts agentHelpFacts, trailsEnabled bool) string {
	var readOnly, writes []string
	for _, child := range agentHelpCommands(cmd, trailsEnabled) {
		cf, ok := agentHelpClassified(agentHelpPath(child))
		if !ok {
			continue
		}
		switch cf.audience {
		case agentHelpAudienceReadOnly:
			readOnly = append(readOnly, child.Name())
		case agentHelpAudienceTaskDriven:
			writes = append(writes, child.Name())
		case agentHelpAudienceUserOwned:
			// A user-owned child inside a listed group would need its own phrasing.
			// None exists today; the completeness guard surfaces one if it lands.
		}
	}
	switch {
	case len(readOnly) == 0 || len(writes) == 0:
		// Leaf, or every classified child agrees with the group.
		return agentHelpAudienceSlug(facts.audience)
	case len(writes) <= len(readOnly):
		return "read-only except: " + strings.Join(writes, ", ")
	default:
		return "read-only: " + strings.Join(readOnly, ", ") + " — others write"
	}
}

// wrapIndented soft-wraps a " · "-joined list at width, prefixing each line with
// indent, so the footer index stays compact as the command set grows.
func wrapIndented(s, indent string, width int) string {
	var b strings.Builder
	line := indent
	for i, part := range strings.Split(s, " · ") {
		candidate := part
		if i > 0 {
			candidate = " · " + part
		}
		if len(line)+len(candidate) > width && line != indent {
			b.WriteString(line + "\n")
			line = indent + part
			continue
		}
		line += candidate
	}
	return b.String() + line + "\n"
}

// newAgentHelpCmd builds the `entire agent-help` command. It is visible in
// `entire help` (so agents on transports without context injection can still
// find it) and renders agent-facing usage live from rootCmd's command tree.
func newAgentHelpCmd(rootCmd *cobra.Command) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "agent-help [command...]",
		Short: "Machine-readable usage for coding agents (always matches the installed CLI)",
		Long: `Prints agent-facing usage for the Entire CLI, generated live from the installed
command tree so it always matches this binary. With no arguments it prints a
high-level map of when to use entire and which subcommand; pass a command path
(e.g. "agent-help checkpoint") to see that command's exact, current flags.`,
		RunE: func(c *cobra.Command, args []string) error {
			// Resolve the origin remote once and derive both the repo line and the
			// trails-enablement check from it (avoids two git subprocesses per run).
			repoLine, trailsEnabled := agentHelpRepoContext(c.Context())
			out, err := runAgentHelp(rootCmd, args, repoLine, asJSON, trailsEnabled)
			if err != nil {
				return err
			}
			fmt.Fprint(c.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured JSON instead of text")
	return cmd
}

// agentHelpRepoContext resolves the origin remote ONCE and derives both the repo
// line (forge/owner/repo, or "" when it can't be determined — no origin /
// detached HEAD — so the renderer degrades gracefully) and whether trails are
// enabled for that scope. Unlike the prompt-path gate, agent-help is an explicit
// command and can afford to refresh an absent or stale enablement decision rather
// than incorrectly treating an unknown cache entry as "trails unavailable".
func agentHelpRepoContext(ctx context.Context) (repoLine string, trailsEnabled bool) {
	return agentHelpRepoContextWithRefresh(ctx, refreshAgentHelpTrailsEnabledCacheIfStaleForScope)
}

// refreshAgentHelpTrailsEnabledCacheIfStaleForScope refreshes synchronously
// because agent-help is an explicit command whose output must reflect the
// current availability decision. SessionStart uses the detached
// refreshTrailsEnabledCacheIfStaleForScope path instead to avoid hook latency.
func refreshAgentHelpTrailsEnabledCacheIfStaleForScope(ctx context.Context, scope trailEnablementScope) error {
	if cachedTrailsEnablementForScope(ctx, scope, time.Now()) != trailEnablementCacheUnknown {
		return nil
	}
	if !scope.Supported {
		return saveTrailsEnabledForScope(ctx, scope, false, time.Now())
	}
	client, err := NewAuthenticatedAPIClient(ctx, false)
	if err != nil {
		return err
	}
	_, err = refreshTrailsEnabledCacheForScope(ctx, client, scope)
	return err
}

// agentHelpRepoContextWithRefresh keeps the refresh dependency explicit so the
// cache-miss behavior can be tested without authenticating against a real API.
func agentHelpRepoContextWithRefresh(
	ctx context.Context,
	refresh func(context.Context, trailEnablementScope) error,
) (repoLine string, trailsEnabled bool) {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		return "", false
	}
	if scope.Forge != "" && scope.Owner != "" && scope.Repo != "" {
		repoLine = scope.RepoKey
	}

	now := time.Now()
	if decision := cachedTrailsEnablementForScope(ctx, scope, now); decision != trailEnablementCacheUnknown {
		return repoLine, decision == trailEnablementCacheEnabled
	}

	// ResolveDataAPIToken performs data-host discovery before it can reject a
	// missing login. The scope already carries the locally resolved auth identity,
	// so avoid making an unauthenticated first run wait on a network request that
	// cannot produce an enabled decision.
	if scope.AuthKey == "" {
		return repoLine, false
	}
	if recentAgentHelpTrailsRefreshFailure(ctx, scope, now) {
		return repoLine, false
	}

	refreshCtx, cancel := context.WithTimeout(ctx, trailEnablementRefreshTimeout)
	defer cancel()
	if err := refresh(refreshCtx, scope); err != nil {
		// A separate short backoff keeps an offline authenticated user from paying
		// this timeout on every agent-help invocation. It must not alter the shared
		// enablement decision, which SessionStart uses for context injection.
		if cacheErr := saveAgentHelpTrailsRefreshFailure(ctx, scope, time.Now()); cacheErr != nil {
			logging.Debug(ctx, "failed to save agent-help trails refresh backoff", "error", cacheErr)
		}
		return repoLine, false
	}
	return repoLine, cachedTrailsEnablementForScope(ctx, scope, time.Now()) == trailEnablementCacheEnabled
}

// runAgentHelp resolves args to a command node and renders it. It is pure (no
// git / IO): the caller passes the already-resolved repoLine and trailsEnabled.
func runAgentHelp(rootCmd *cobra.Command, args []string, repoLine string, asJSON, trailsEnabled bool) (string, error) {
	target := rootCmd
	for _, name := range args {
		child := agentHelpFindChild(target, name)
		if child == nil {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		// Keep the specific, actionable message for the trail-gated case.
		if !trailsEnabled && child.Annotations[agentHelpRequiresTrailsAnnotation] == agentHelpAnnotationEnabled {
			return "", fmt.Errorf("`%s` is unavailable: trails are not enabled for this repo", child.Name())
		}
		// The drillable surface must match the advertised surface: a name an agent
		// guesses for a command the listing intentionally hides (help, deprecated,
		// or plain-hidden infra like `hooks`) reads as nonexistent here too.
		if !isAgentHelpAdvertised(child, trailsEnabled) {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		target = child
	}
	if asJSON {
		return renderAgentHelpJSON(rootCmd, target, repoLine, trailsEnabled)
	}
	if target == rootCmd {
		return renderAgentHelpTop(rootCmd, repoLine, trailsEnabled), nil
	}
	return renderAgentHelpCommand(target, repoLine, trailsEnabled), nil
}

// agentHelpFindChild finds a direct child of parent by name or alias. It
// includes hidden commands so an annotated one like trail resolves; the caller
// (runAgentHelp) then enforces isAgentHelpAdvertised, so the drillable surface
// matches the advertised one.
func agentHelpFindChild(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

type agentHelpFlagJSON struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

type agentHelpSubcommandJSON struct {
	Name  string `json:"name"`
	Short string `json:"short"`
	// Audience mirrors the text renderer's grouping so a --json consumer gets the
	// same "may I run this unprompted?" answer as a text reader. Omitted when the
	// table makes no explicit claim, rather than asserting the user-owned default.
	Audience string `json:"audience,omitempty"`
}

type agentHelpJSON struct {
	Command string `json:"command"`
	Short   string `json:"short,omitempty"`
	Long    string `json:"long,omitempty"`
	// Guidance is agent-only advice on WHEN to use the command, absent from
	// cobra's human help. Structured consumers get it alongside text readers.
	Guidance    string                    `json:"guidance,omitempty"`
	Example     string                    `json:"example,omitempty"`
	Repo        string                    `json:"repo,omitempty"`
	Flags       []agentHelpFlagJSON       `json:"flags,omitempty"`
	Subcommands []agentHelpSubcommandJSON `json:"subcommands,omitempty"`
}

// renderAgentHelpJSON renders the structured form of a command node.
func renderAgentHelpJSON(rootCmd, target *cobra.Command, repoLine string, trailsEnabled bool) (string, error) {
	doc := agentHelpJSON{
		Command:  target.CommandPath(),
		Short:    target.Short,
		Long:     strings.TrimSpace(target.Long),
		Guidance: agentHelpGuidance[agentHelpPath(target)],
		Example:  strings.TrimSpace(target.Example),
		Repo:     repoLine,
	}
	if target != rootCmd {
		collect := func(fs *flag.FlagSet) {
			fs.VisitAll(func(f *flag.Flag) {
				if f.Hidden {
					return
				}
				doc.Flags = append(doc.Flags, agentHelpFlagJSON{
					Name:      f.Name,
					Shorthand: f.Shorthand,
					Type:      f.Value.Type(),
					Default:   f.DefValue,
					Usage:     f.Usage,
				})
			})
		}
		collect(target.LocalFlags())
		collect(target.InheritedFlags())
	}
	for _, sub := range agentHelpCommands(target, trailsEnabled) {
		entry := agentHelpSubcommandJSON{Name: sub.Name(), Short: sub.Short}
		if f, ok := agentHelpClassified(agentHelpPath(sub)); ok {
			entry.Audience = agentHelpAudienceSlug(f.audience)
		}
		doc.Subcommands = append(doc.Subcommands, entry)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal agent-help json: %w", err)
	}
	return string(b) + "\n", nil
}

// isAgentHelpAdvertised reports whether sub should be exposed to agents through
// agent-help. The listing AND the drill-down resolver share this predicate so
// the drillable surface always matches the advertised surface: visible commands
// plus hidden commands that opt in via agentHelpAnnotation, minus the help
// command, deprecated commands, and (when trails are disabled) trail-gated ones.
func isAgentHelpAdvertised(sub *cobra.Command, trailsEnabled bool) bool {
	if sub.Name() == "help" || sub.Name() == "agent-help" || sub.Deprecated != "" {
		return false
	}
	if sub.Hidden && sub.Annotations[agentHelpAnnotation] != agentHelpAnnotationEnabled {
		return false
	}
	if !trailsEnabled && sub.Annotations[agentHelpRequiresTrailsAnnotation] == agentHelpAnnotationEnabled {
		return false
	}
	return true
}

// agentHelpCommands returns the child commands to advertise to agents.
func agentHelpCommands(parent *cobra.Command, trailsEnabled bool) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range parent.Commands() {
		if isAgentHelpAdvertised(sub, trailsEnabled) {
			out = append(out, sub)
		}
	}
	return out
}

// agentHelpRepoBlock formats the auto-detected repo line, degrading gracefully
// when the repo can't be resolved (no origin / detached HEAD) rather than
// implying a repo that isn't there.
func agentHelpRepoBlock(repoLine string) string {
	// Defense-in-depth: this line is emitted as plain text into agent context and
	// the user's terminal. A crafted origin URL's control characters (newline,
	// ANSI escapes) are rejected upstream in gitremote, but never let one reach
	// this plain-text sink — degrade to the not-detectable message instead.
	if strings.TrimSpace(repoLine) == "" || strings.IndexFunc(repoLine, unicode.IsControl) >= 0 {
		return "Current repo: not auto-detectable here (no origin remote / detached HEAD); pass --repo explicitly.\n"
	}
	return "Current repo: " + repoLine + "  (auto-detected from origin; pass --repo only for a DIFFERENT repo)\n"
}

// renderAgentHelpCommand renders one resolved command node for an agent: its
// path + Short, its Long description, the auto-detected repo line, its live flag
// usages (hidden flags are skipped by cobra), and its advertised subcommands.
func renderAgentHelpCommand(cmd *cobra.Command, repoLine string, trailsEnabled bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", cmd.CommandPath(), cmd.Short)
	if long := strings.TrimSpace(cmd.Long); long != "" && long != strings.TrimSpace(cmd.Short) {
		b.WriteString(long)
		b.WriteString("\n")
	}
	// Before the flags and examples: whether to use the command at all outranks
	// how to call it.
	if guidance := agentHelpGuidance[agentHelpPath(cmd)]; guidance != "" {
		b.WriteString("\nWhen to use this:\n")
		b.WriteString(guidance)
		b.WriteString("\n")
	}
	if example := strings.TrimSpace(cmd.Example); example != "" {
		b.WriteString("\nExamples:\n")
		b.WriteString(example)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(agentHelpRepoBlock(repoLine))

	// LocalFlags()/InheritedFlags() trigger cobra's persistent-flag merge (plain
	// Flags() does not without Execute) and skip hidden flags in FlagUsages.
	if usages := strings.TrimRight(cmd.LocalFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nFlags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}
	if usages := strings.TrimRight(cmd.InheritedFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nInherited flags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}

	if subs := agentHelpCommands(cmd, trailsEnabled); len(subs) > 0 {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		fmt.Fprintf(&b, "\nSubcommands: %s\n", strings.Join(names, " · "))
		// The text drill-down must carry the same "may I run this?" answer as
		// --json does (the repo's agent-safe-fallback rule); a bare subcommand list
		// hides which of them write.
		if facts, ok := agentHelpClassified(agentHelpPath(cmd)); ok {
			fmt.Fprintf(&b, "             %s\n", agentHelpAudienceNote(cmd, facts, trailsEnabled))
		}
		fmt.Fprintf(&b, "Next:  entire agent-help %s <subcommand>\n", strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "))
	}
	return b.String()
}

// renderAgentHelpTop renders the top-level agent-facing overview: the curated
// intro + rule, the auto-detected repo line, the LISTED commands with their
// audience, a compact index of everything else, and the drill-down pointer.
//
// It shows a curated subset rather than the whole tree. An exhaustive listing
// answers "what exists?", which is not the question an agent mid-task has, and
// past a certain length it stops being read at all. Everything omitted stays
// resolvable through `agent-help <command>` and is named in the footer, so this
// trades default visibility, never availability.
func renderAgentHelpTop(rootCmd *cobra.Command, repoLine string, trailsEnabled bool) string {
	var b strings.Builder
	b.WriteString(agentHelpOverview)
	b.WriteString("\n\n")
	b.WriteString(agentHelpRepoBlock(repoLine))

	var listed []*cobra.Command
	rest := map[agentHelpAudience][]string{}
	width := 10
	for _, sub := range agentHelpCommands(rootCmd, trailsEnabled) {
		facts := agentHelpFactsFor(agentHelpPath(sub))
		if !facts.listed {
			rest[facts.audience] = append(rest[facts.audience], sub.Name())
			continue
		}
		listed = append(listed, sub)
		if len(sub.Name()) > width {
			width = len(sub.Name())
		}
	}

	if len(listed) > 0 {
		b.WriteString("\nWhen to use entire — the commands that bear on the work:\n")
		for _, sub := range listed {
			facts := agentHelpFactsFor(agentHelpPath(sub))
			fmt.Fprintf(&b, "  %-*s  %s\n", width, sub.Name(), sub.Short)
			fmt.Fprintf(&b, "  %-*s    %s\n", width, "", agentHelpAudienceNote(sub, facts, trailsEnabled))
		}
	}
	// Names only, no Short help: enough for an agent to connect a user's request
	// ("run a review") to a command, at a fraction of the lines full entries
	// cost. Still split by audience — a single bucket would have to caption
	// itself with the most restrictive rule, which would tell an agent not to run
	// `activity` or `blame` uninvited when the table says they are safe.
	footer := []struct {
		audience agentHelpAudience
		label    string
	}{
		{agentHelpAudienceReadOnly, "read-only, safe to run:"},
		{agentHelpAudienceTaskDriven, "when the task needs them:"},
		{agentHelpAudienceUserOwned, "the user's — suggest, don't run:"},
	}
	if len(rest) > 0 {
		b.WriteString("\nAlso available (entire agent-help <command> for any of these):\n")
		for _, section := range footer {
			names := rest[section.audience]
			if len(names) == 0 {
				continue
			}
			fmt.Fprintf(&b, "  %s\n", section.label)
			b.WriteString(wrapIndented(strings.Join(names, " · "), "    ", 76))
		}
	}
	// Use an example command that is actually advertised here (trail is gated on
	// trails being enabled), so we never point at a command the agent can't use.
	example := "checkpoint"
	if trailsEnabled {
		example = "trail"
	}
	fmt.Fprintf(&b, "\nDrill in for exact, currently-installed flags:  entire agent-help <command>  (e.g. entire agent-help %s)\n", example)
	b.WriteString("Add --json for structured output.\n")
	return b.String()
}
