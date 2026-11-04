package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// Core's published binding problem types.
const (
	saProblemBindingExists   = "https://entire.com/errors/service-account/binding-exists"
	saProblemNotSelfServe    = "https://entire.com/errors/service-account/binding-provider-not-self-serve"
	setupEntireActionVersion = "entireio/setup-entire@v1"
)

func newSABindCmd() *cobra.Command {
	var providerName string
	var pinArgs []string
	cmd := &cobra.Command{
		Use:   "bind <service-account>",
		Short: "Let a CI provider's tokens act as a service account",
		Long: "Binds tokens from an OIDC provider, such as GitHub Actions or Buildkite, " +
			"to the service account. Each --pin name=value narrows which tokens match. " +
			"Run 'entire service-account providers <name>' to see a provider's pins.\n\n" +
			"A terminal asks for missing required pins. A service account holds one " +
			"binding per provider. A pin matches a name, so unbind before a bound " +
			"repository or pipeline is renamed or deleted.",
		Example: "  entire service-account bind acme/ci --provider github-actions --pin repository=acme/web\n" +
			"  entire service-account bind acme/ci --provider buildkite --pin organization=acme --pin pipeline=deploy",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			pins, err := parsePins(pinArgs)
			if err != nil {
				return err
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				p, err := findSAProvider(ctx, c, providerName)
				if err != nil {
					return err
				}
				if err := completePins(cmd, p, pins); err != nil {
					return err
				}
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				filter, err := json.Marshal(pins)
				if err != nil {
					return fmt.Errorf("encode pins: %w", err)
				}
				b, err := c.CreateBinding(ctx, &coreapi.CreateBindingInputBody{ProviderId: p.wire.ID, AttributeFilter: filter},
					coreapi.CreateBindingParams{AccountId: sa.ID})
				if err != nil {
					return saBindErr(err, sa, p)
				}
				if jsonRequested(cmd) {
					return printJSON(cmd.OutOrStdout(), b)
				}
				printBindNext(cmd, c.CoreOrigin(), sa, p, pins, b.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&providerName, "provider", "", "Provider name, e.g. github-actions or buildkite")
	cmd.Flags().StringArrayVar(&pinArgs, "pin", nil, "Token attribute to pin as name=value (repeatable)")
	markRequired(cmd, "provider")
	addJSONFlag(cmd)
	return cmd
}

// saBindErr maps core's typed binding refusals.
func saBindErr(err error, sa saRef, p saProvider) error {
	switch coreapi.APIErrorType(err) {
	case saProblemBindingExists:
		return fmt.Errorf("%s already has a %s binding; unbind it first", sa.label(), p.name)
	case saProblemNotSelfServe:
		return fmt.Errorf("%s is operator-only", p.name)
	}
	return err
}

// parsePins reads repeated name=value flags.
func parsePins(args []string) (map[string]string, error) {
	pins := make(map[string]string, len(args))
	for _, a := range args {
		key, value, ok := strings.Cut(a, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("--pin %q must be name=value", a)
		}
		if _, dup := pins[key]; dup {
			return nil, fmt.Errorf("--pin %s is given twice", key)
		}
		pins[key] = value
	}
	return pins, nil
}

// errNoPins mirrors core: an empty filter matches everything.
var errNoPins = errors.New("pass at least one --pin name=value")

// completePins checks pins and asks for missing ones.
func completePins(cmd *cobra.Command, p saProvider, pins map[string]string) error {
	if err := describedPins(cmd, p, pins); err != nil {
		return err
	}
	if len(pins) == 0 {
		return errNoPins
	}
	return nil
}

// describedPins applies the descriptor's rules.
func describedPins(cmd *cobra.Command, p saProvider, pins map[string]string) error {
	// An old core describes nothing; core decides.
	if !p.described() {
		return nil
	}
	if err := checkPins(p, pins); err != nil {
		return err
	}
	missing := missingPins(p, pins)
	if len(missing) == 0 {
		return nil
	}
	if !interactive.CanPromptInteractively() {
		return fmt.Errorf("%s needs --pin for: %s", p.name, strings.Join(attributeNames(missing), ", "))
	}
	asked, err := askPins(cmd, p, missing)
	if err != nil {
		return err
	}
	maps.Copy(pins, asked)
	return checkPins(p, pins)
}

// checkPins mirrors core's descriptor rules.
func checkPins(p saProvider, pins map[string]string) error {
	byName := make(map[string]coreapi.OIDCProviderAttribute, len(p.wire.Attributes))
	for _, a := range p.wire.Attributes {
		byName[a.Name] = a
	}
	for _, key := range slices.Sorted(maps.Keys(pins)) {
		value := pins[key]
		a, ok := byName[key]
		if !ok {
			return fmt.Errorf("%s has no pin %q; see entire sa providers %s", p.name, key, p.name)
		}
		if a.Exact && strings.Contains(value, "*") {
			return fmt.Errorf("pin %s must be exact, not a glob", key)
		}
		if err := matchPattern(a, value); err != nil {
			return err
		}
	}
	return nil
}

func matchPattern(a coreapi.OIDCProviderAttribute, value string) error {
	pattern := a.Pattern.Or("")
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		// Core owns its pattern; it decides.
		return nil //nolint:nilerr // an unparsable pattern is core's to enforce
	}
	if !re.MatchString(value) {
		return fmt.Errorf("pin %s=%q must match %s", a.Name, value, pattern)
	}
	return nil
}

func missingPins(p saProvider, pins map[string]string) []coreapi.OIDCProviderAttribute {
	var out []coreapi.OIDCProviderAttribute
	for _, a := range p.wire.Attributes {
		if _, ok := pins[a.Name]; a.Required && !ok {
			out = append(out, a)
		}
	}
	return out
}

func attributeNames(attrs []coreapi.OIDCProviderAttribute) []string {
	out := make([]string, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, a.Name)
	}
	return out
}

// askPins is a seam: tests have no terminal.
var askPins = func(cmd *cobra.Command, p saProvider, missing []coreapi.OIDCProviderAttribute) (map[string]string, error) {
	values := make([]string, len(missing))
	fields := make([]huh.Field, 0, len(missing))
	for i, a := range missing {
		in := huh.NewInput().Title(a.Label + " (" + a.Name + ")").Value(&values[i]).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("enter a value")
				}
				return nil
			})
		if d := a.Description.Or(""); d != "" {
			in = in.Description(d)
		}
		fields = append(fields, in)
	}
	if _, err := runPromptForm(cmd, NewAccessibleForm(huh.NewGroup(fields...).Title(p.label+" pins"))); err != nil {
		return nil, fmt.Errorf("pin prompt: %w", err)
	}
	out := make(map[string]string, len(missing))
	for i, a := range missing {
		out[a.Name] = strings.TrimSpace(values[i])
	}
	return out, nil
}

// printBindNext names the token exchange inputs.
func printBindNext(cmd *cobra.Command, origin string, sa saRef, p saProvider, pins map[string]string, bindingID string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "✓ Bound %s to %s (%s)\n", sa.label(), p.label, bindingID)
	for _, key := range slices.Sorted(maps.Keys(pins)) {
		fmt.Fprintf(out, "  pin       %s=%s\n", key, pins[key])
	}
	fmt.Fprintln(out, "Exchange the CI job's OIDC token for this account:")
	fmt.Fprintf(out, "  audience  %s/api/authz/sts\n", origin)
	fmt.Fprintf(out, "  endpoint  %s/oauth/token\n", origin)
	fmt.Fprintf(out, "  resource  %s\n", sa.ID)
	if docs := p.wire.DocsURL.Or(""); docs != "" {
		fmt.Fprintf(out, "  docs      %s\n", docs)
	}
	if p.name != githubActionsName {
		return
	}
	fmt.Fprintln(out, "In a GitHub Actions workflow:")
	fmt.Fprintf(out, "  permissions:\n    id-token: write\n  steps:\n"+
		"    - uses: %s\n      with:\n        service-account: %s\n        auth-url: %s\n",
		setupEntireActionVersion, sa.ID, origin)
}

var saBindingColumns = []string{"ID", "PROVIDER", "FILTER"}

// saBinding is a binding plus its provider name.
type saBinding struct {
	coreapi.Binding

	provider string
}

func saBindingRow(b saBinding) []string {
	var filter bytes.Buffer
	if err := json.Compact(&filter, b.AttributeFilter); err != nil {
		return []string{b.ID, b.provider, string(b.AttributeFilter)}
	}
	return []string{b.ID, b.provider, filter.String()}
}

func newSABindingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bindings <service-account>",
		Short: "List a service account's bindings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			view := listView[saBinding]{
				table: func([]saBinding) ([]string, func(saBinding) []string) { return saBindingColumns, saBindingRow },
				toJSON: func(items []saBinding) (any, error) {
					wire := make([]coreapi.Binding, 0, len(items))
					for _, b := range items {
						wire = append(wire, b.Binding)
					}
					return wire, nil
				},
			}
			return runCoreListShaped(cmd, "No bindings found.", view, func(ctx context.Context, c *coreapi.Client) ([]saBinding, error) {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				bindings, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.Binding, string, error) {
					params := coreapi.ListBindingsParams{AccountId: sa.ID}
					if cursor != "" {
						params.PageToken = coreapi.NewOptString(cursor)
					}
					out, err := c.ListBindings(ctx, params)
					if err != nil {
						return nil, "", err
					}
					return out.Bindings, out.NextPageToken.Or(""), nil
				})
				if err != nil || len(bindings) == 0 {
					return nil, err
				}
				return nameBindingProviders(ctx, c, bindings)
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// nameBindingProviders swaps provider IDs for names.
func nameBindingProviders(ctx context.Context, c *coreapi.Client, bindings []coreapi.Binding) ([]saBinding, error) {
	providers, err := listSAProviders(ctx, c)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(providers))
	for _, p := range providers {
		names[p.wire.ID] = p.name
	}
	out := make([]saBinding, 0, len(bindings))
	for _, b := range bindings {
		name := names[b.ProviderId]
		if name == "" {
			name = b.ProviderId
		}
		out = append(out, saBinding{Binding: b, provider: name})
	}
	return out, nil
}

func newSAUnbindCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "unbind <service-account> <binding-id>",
		Short:   "Remove a binding from a service account",
		Long:    "Removes the binding, so its CI jobs can no longer act as the account. Removing a missing binding succeeds.",
		Example: "  entire service-account unbind acme/ci eib_01HZX7QABCDEFGHJKMNPQRSTVW",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				sa, err := resolveSARef(ctx, c, args[0])
				if err != nil {
					return err
				}
				if err := c.DeleteBinding(ctx, coreapi.DeleteBindingParams{AccountId: sa.ID, BindingId: args[1]}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Removed binding %s from %s\n", args[1], sa.label())
				return nil
			})
		},
	}
}
