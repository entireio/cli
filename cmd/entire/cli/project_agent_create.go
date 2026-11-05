package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// The API's limits for a custom agent (entire-api AgentWriteFields); checked
// here so a bad value fails before anything is resolved or written.
const (
	projectAgentMaxName         = 128
	projectAgentMaxDescription  = 1024
	projectAgentMaxInstructions = 16384
	projectAgentMaxTimeout      = 30 * time.Minute
	projectAgentMaxGlobs        = 32
	projectAgentMaxGlobBytes    = 256
	projectAgentDefaultTimeout  = 10 * time.Minute
	// projectAgentWhenManual is --when's value for an agent with no trigger,
	// which runs only when asked to.
	projectAgentWhenManual = "manual"
	// projectAgentPolarityNeutral is a signal with no good direction, the
	// server's default.
	projectAgentPolarityNeutral = "neutral"
)

var (
	projectAgentHarnesses        = []string{"claude", "codex"}
	projectAgentSignalTypes      = []string{"boolean", "percent", "size"}
	projectAgentPolarities       = []string{"higher_is_better", "lower_is_better", projectAgentPolarityNeutral}
	projectAgentReasoningEfforts = []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	projectAgentWhens            = []string{"ready", "always", "not_ready", projectAgentWhenManual}
)

type projectAgentCreateOptions struct {
	name             string
	description      string
	harness          string
	model            string
	reasoningEffort  string
	timeout          time.Duration
	instructions     string
	instructionsFile string
	signal           string
	polarity         string
	when             string
	branches         []string
	paths            []string
	environment      string
	disabled         bool
}

func newProjectAgentCreateCmd() *cobra.Command {
	var opts projectAgentCreateOptions
	cmd := &cobra.Command{
		Use:   "create --name <name> --harness <harness> --signal <type> (--instructions <text> | --instructions-file <path>)",
		Short: "Create a custom review agent for the project",
		Long: `Create a custom review agent: your own instructions, harness, model, and
signal, reviewing this repository. For an agent Entire maintains, use
` + "`entire project agent add`" + ` instead.

A custom agent runs only once the project's payer is enrolled in billing;
preset agents run on free credit.

The instructions say what the agent reviews and how it scores a change. They
may use {{branch}} and {{base_branch}}. --instructions-file - reads them from
standard input.

The signal is the one reading each review reports: boolean (true or false),
percent (0-100), or size (small, medium, large). --polarity says which readings
are good; without it the signal is neutral.

By default the agent runs on pushes to a branch with an open Change that is
ready for review (--when ready). --when always runs it on every push,
--when not_ready only before the Change is ready, and --when manual never on a
push. --branch and --path limit push runs to matching branches and changed
paths (globs such as release/* or frontend/**); manual runs ignore them.
--disabled saves the agent without running it on pushes.

--model and --reasoning-effort take the values the API lists for the harness;
without them the harness's defaults apply. Agents run in a project environment,
chosen as for ` + "`entire project agent add`" + `.`,
		Example: "  entire project agent create --name \"Migration Risk\" --harness claude \\\n" +
			"    --signal percent --polarity lower_is_better --instructions-file review.md\n" +
			"  entire project agent create --name Docs --harness codex --signal boolean \\\n" +
			"    --path 'docs/**' --instructions 'Flag documentation that contradicts the code.'",
		Args: cobra.NoArgs,
		// Every flag is checked before the project is resolved, so a bad value
		// fails without authenticating or reading anything.
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			return opts.validate(cmd.InOrStdin())
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return runProjectAgentCreate(cmd, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.name, "name", "", "Display name, unique among the project's agents (required)")
	f.StringVar(&opts.description, "description", "", "What the agent reviews")
	f.StringVar(&opts.harness, "harness", "", "Agent CLI that runs the instructions: "+strings.Join(projectAgentHarnesses, ", ")+" (required)")
	f.StringVar(&opts.model, "model", "", "Model for the harness (default: the harness's default)")
	f.StringVar(&opts.reasoningEffort, "reasoning-effort", "", "Reasoning effort: "+strings.Join(projectAgentReasoningEfforts, ", ")+" (default: the model's default)")
	f.DurationVar(&opts.timeout, "timeout", projectAgentDefaultTimeout, "Time limit for one review, at most 30m")
	f.StringVar(&opts.instructions, "instructions", "", "What to review and how to score it")
	f.StringVar(&opts.instructionsFile, "instructions-file", "", "Read the instructions from a file, or - for standard input")
	f.StringVar(&opts.signal, "signal", "", "Reading each review reports: "+strings.Join(projectAgentSignalTypes, ", ")+" (required)")
	f.StringVar(&opts.polarity, "polarity", "", "Which readings are good: "+strings.Join(projectAgentPolarities, ", ")+" (default: neutral)")
	f.StringVar(&opts.when, "when", "ready", "Which pushes run it: "+strings.Join(projectAgentWhens, ", "))
	f.StringArrayVar(&opts.branches, "branch", nil, "Run on pushes only to branches matching this glob (repeatable)")
	f.StringArrayVar(&opts.paths, "path", nil, "Run on pushes only when a changed path matches this glob (repeatable)")
	f.StringVar(&opts.environment, "environment", "", "Project environment (id or name) the agent runs in")
	f.BoolVar(&opts.disabled, "disabled", false, "Create the agent without running it on pushes")
	addJSONFlag(cmd)
	return cmd
}

// validate normalizes the options and refuses what the API would, reading
// the instructions file so a missing or oversized one fails early too.
func (o *projectAgentCreateOptions) validate(stdin io.Reader) error {
	o.name = strings.TrimSpace(o.name)
	o.description = strings.TrimSpace(o.description)
	o.harness = strings.ToLower(strings.TrimSpace(o.harness))
	o.model = strings.TrimSpace(o.model)
	o.reasoningEffort = strings.ToLower(strings.TrimSpace(o.reasoningEffort))
	o.signal = strings.ToLower(strings.TrimSpace(o.signal))
	o.polarity = strings.ToLower(strings.TrimSpace(o.polarity))
	o.when = strings.ToLower(strings.TrimSpace(o.when))

	switch {
	case o.name == "":
		return errors.New("--name is required")
	case len(o.name) > projectAgentMaxName:
		return fmt.Errorf("--name is limited to %d bytes", projectAgentMaxName)
	case len(o.description) > projectAgentMaxDescription:
		return fmt.Errorf("--description is limited to %d bytes", projectAgentMaxDescription)
	}
	if err := checkProjectAgentChoice("--harness", o.harness, projectAgentHarnesses, true); err != nil {
		return err
	}
	if err := checkProjectAgentChoice("--signal", o.signal, projectAgentSignalTypes, true); err != nil {
		return err
	}
	if err := checkProjectAgentChoice("--polarity", o.polarity, projectAgentPolarities, false); err != nil {
		return err
	}
	if err := checkProjectAgentChoice("--reasoning-effort", o.reasoningEffort, projectAgentReasoningEfforts, false); err != nil {
		return err
	}
	if err := checkProjectAgentChoice("--when", o.when, projectAgentWhens, true); err != nil {
		return err
	}
	var err error
	if o.branches, err = checkProjectAgentGlobs("--branch", o.branches); err != nil {
		return err
	}
	if o.paths, err = checkProjectAgentGlobs("--path", o.paths); err != nil {
		return err
	}
	if o.when == projectAgentWhenManual && (len(o.branches) > 0 || len(o.paths) > 0) {
		return errors.New("--branch and --path filter push runs; --when manual has none")
	}
	if o.timeout < time.Second || o.timeout > projectAgentMaxTimeout {
		return fmt.Errorf("--timeout must be between 1s and %s", projectAgentMaxTimeout)
	}
	return o.loadInstructions(stdin)
}

func (o *projectAgentCreateOptions) loadInstructions(stdin io.Reader) error {
	if (o.instructions == "") == (o.instructionsFile == "") {
		return errors.New("pass one of --instructions or --instructions-file")
	}
	if o.instructionsFile != "" {
		// The limit below applies after trimming, so the read leaves room for
		// surrounding whitespace.
		const limit = projectAgentMaxInstructions + 4096
		var raw []byte
		var err error
		if o.instructionsFile == "-" {
			raw, err = readWithinLimit(stdin, limit)
		} else {
			var f *os.File
			f, err = os.Open(o.instructionsFile)
			if err == nil {
				raw, err = readWithinLimit(f, limit)
				_ = f.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("read --instructions-file %s: %w", o.instructionsFile, err)
		}
		o.instructions = string(raw)
	}
	o.instructions = strings.TrimSpace(o.instructions)
	switch {
	case o.instructions == "":
		return errors.New("the instructions are empty")
	case !utf8.ValidString(o.instructions):
		return errors.New("the instructions are not valid UTF-8")
	case len(o.instructions) > projectAgentMaxInstructions:
		return fmt.Errorf("the instructions are limited to %d bytes", projectAgentMaxInstructions)
	}
	return nil
}

// checkProjectAgentGlobs trims each glob and refuses what the API would: an
// empty one, more than it accepts, or one anchored with a slash. The API checks
// the rest of the glob syntax.
func checkProjectAgentGlobs(flag string, globs []string) ([]string, error) {
	if len(globs) > projectAgentMaxGlobs {
		return nil, fmt.Errorf("%s is limited to %d patterns", flag, projectAgentMaxGlobs)
	}
	out := make([]string, 0, len(globs))
	for _, g := range globs {
		g = strings.TrimSpace(g)
		switch {
		case g == "":
			return nil, fmt.Errorf("%s needs a non-empty pattern", flag)
		case len(g) > projectAgentMaxGlobBytes:
			return nil, fmt.Errorf("%s patterns are limited to %d bytes", flag, projectAgentMaxGlobBytes)
		case strings.HasPrefix(g, "/") || strings.HasSuffix(g, "/"):
			return nil, fmt.Errorf("%s %q must not start or end with /", flag, g)
		}
		out = append(out, g)
	}
	return out, nil
}

// checkProjectAgentChoice refuses a value outside allowed; an empty one only
// when it is required.
func checkProjectAgentChoice(flag, value string, allowed []string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required: %s", flag, strings.Join(allowed, ", "))
		}
		return nil
	}
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("%s must be one of %s, not %q", flag, strings.Join(allowed, ", "), value)
	}
	return nil
}

// write is the create body for a custom agent reviewing repoID in env.
func (o *projectAgentCreateOptions) write(repoID string, env projectAgentEnv) api.ProjectAgentWrite {
	body := api.ProjectAgentWrite{
		Name:          o.name,
		Description:   o.description,
		IsEnabled:     !o.disabled,
		Harness:       o.harness,
		TimeoutMS:     int(o.timeout.Milliseconds()),
		Instructions:  o.instructions,
		Signal:        &api.ProjectAgentSignal{ValueType: o.signal, Polarity: o.polarity},
		RepositoryIDs: []string{repoID},
	}
	env.apply(&body)
	if o.model != "" {
		body.Model = &o.model
	}
	if o.reasoningEffort != "" {
		body.ReasoningEffort = &o.reasoningEffort
	}
	if o.when != projectAgentWhenManual {
		body.Triggers = []api.ProjectAgentTrigger{{Kind: "push", When: o.when, Branches: o.branches, Paths: o.paths}}
	}
	return body
}

func runProjectAgentCreate(cmd *cobra.Command, opts projectAgentCreateOptions) error {
	ctx := cmd.Context()
	w, errW := cmd.OutOrStdout(), cmd.ErrOrStderr()

	t, err := resolveProjectAgentTarget(cmd)
	if err != nil {
		return err
	}
	repoID, repoLabel, err := t.currentRepoID(cmd)
	if err != nil {
		return err
	}
	env, err := resolveProjectAgentEnvironment(ctx, errW, t, opts.environment)
	if err != nil || !env.chosen() {
		return err // nothing chosen without an error is a cancelled picker
	}
	created, err := t.createAgent(ctx, opts.write(repoID, env))
	if err != nil {
		return fmt.Errorf("create agent %q in %s: %w", opts.name, t.label(), err)
	}
	env.saved(errW, created)
	if jsonRequested(cmd) {
		return printJSON(w, created)
	}
	state := ""
	if !created.IsEnabled {
		state = ", disabled"
	}
	fmt.Fprintf(w, "created %s (%s)%s, reviewing %s\n", tuiutil.SanitizeTerminalLabel(created.Name), created.ID, state, repoLabel)
	fmt.Fprintln(errW, "A custom agent runs only once the project's payer is enrolled in billing.")
	return nil
}
