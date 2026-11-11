package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/spf13/cobra"
)

type projectAgentSetupOptions struct {
	serviceAccount, name string
	saveConnection       func(*projectAgentTarget) error
}

func newProjectAgentSetupCmd() *cobra.Command {
	opts := projectAgentSetupOptions{saveConnection: saveProjectAgentConnection}
	cmd := &cobra.Command{
		Use: "setup", Short: "Authorize a service account to execute self-hosted project Agents",
		Long: `Select an existing service account or create one with a machine-specific name.
Grants read access to the current repository and binds the account to the project's
managed self-hosted Environment. This authorizes ALL Agents in that Environment,
subject to the account's repository grants, not just one Agent.

Requires project admin access and an existing self-hosted Agent. No credentials
are printed or saved. Use service-account secret create separately, then supply
that secret as ENTIRE_TOKEN when starting project agent serve. Setup does not
change your active login or create an Agent.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if err := validateAgentSetupConsent(cmd); err != nil {
				return err
			}
			opts.serviceAccount = strings.TrimSpace(opts.serviceAccount)
			opts.name = strings.TrimSpace(opts.name)
			if (opts.serviceAccount == "") == (opts.name == "") {
				return errors.New("choose --service-account or --name")
			}
			t, err := resolveProjectAgentTarget(cmd)
			if err != nil {
				return err
			}
			repoID, label, err := t.currentRepoID(cmd)
			if err != nil {
				return err
			}
			return runCore(cmd, func(_ context.Context, c *coreapi.Client) error {
				return setupProjectAgent(cmd, c, t, repoID, label, opts)
			})
		},
	}
	cmd.Flags().StringVar(&opts.serviceAccount, "service-account", "", "Existing svc_ ID or project/name")
	cmd.Flags().StringVar(&opts.name, "name", "", "Create a service account with this machine-specific name")
	addYesFlag(cmd)
	addJSONFlag(cmd)
	cmd.MarkFlagsOneRequired("service-account", "name")
	cmd.MarkFlagsMutuallyExclusive("service-account", "name")
	return cmd
}

func validateAgentSetupConsent(cmd *cobra.Command) error {
	if !forceRequested(cmd) && !interactive.CanPromptInteractively() {
		return errors.New("setup requires confirmation; pass --yes without a terminal")
	}
	return nil
}

func setupProjectAgent(cmd *cobra.Command, c *coreapi.Client, t *projectAgentTarget, repoID, repoLabel string, opts projectAgentSetupOptions) error {
	if err := validateAgentSetupConsent(cmd); err != nil {
		return err
	}
	ctx := cmd.Context()
	envID, err := projectAgentSetupEnvironmentID(ctx, t)
	if err != nil {
		return err
	}
	var sa saRef
	if opts.serviceAccount != "" {
		sa, err = resolveSARef(ctx, c, opts.serviceAccount)
		if err != nil {
			return err
		}
		if sa.ProjectID != t.projectID {
			return errors.New("service account must belong to the target project")
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Project %s: grant repository read access to %s and bind to Environment %s.\nThis permits ALL self-hosted agents in that Environment, subject to repository grants.\n", t.label(), repoLabel, envID)
	if !forceRequested(cmd) {
		confirmed := false
		cancelled, promptErr := runProjectAgentPrompt(ctx, cmd.ErrOrStderr(), "Agent setup", NewAccessibleForm(huh.NewGroup(huh.NewConfirm().Title("Authorize this service account?").Value(&confirmed))))
		if promptErr != nil {
			return promptErr
		}
		if cancelled || !confirmed {
			return nil
		}
	}
	created := opts.serviceAccount == ""
	if created {
		record, err := c.CreateServiceAccount(ctx, &coreapi.CreateServiceAccountInputBody{ProjectID: t.projectID, Name: opts.name})
		if err != nil {
			return saCreateErr(err, t.label(), opts.name)
		}
		sa = saRef{ID: record.AccountId, ProjectID: t.projectID}
		fmt.Fprintf(cmd.ErrOrStderr(), "Created %s. Retry subsequent steps with --service-account %s.\n", sa.ID, sa.ID)
	}
	if err := ensureAgentRepoGrant(ctx, c, sa.ID, repoID, created); err != nil {
		return fmt.Errorf("grant repository access: retry with --service-account %s: %w", sa.ID, err)
	}
	path := t.environmentsPath() + "/" + url.PathEscape(envID) + "/runners/" + url.PathEscape(sa.ID)
	if err := t.client.ProjectConfigRequest(ctx, http.MethodPut, path, nil, nil, nil); err != nil {
		return fmt.Errorf("binding failed (repository grant retained); retry with --service-account %s: %w", sa.ID, err)
	}
	if opts.saveConnection != nil {
		if err := opts.saveConnection(t); err != nil {
			return fmt.Errorf("account bound but local connection could not be saved; retry with --service-account %s: %w", sa.ID, err)
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "No credentials were created. Next: entire service-account secret create %s\nThen set ENTIRE_TOKEN to the secret and run entire project agent serve --project %s.\n", sa.ID, t.label())
	if jsonRequested(cmd) {
		return printJSON(cmd.OutOrStdout(), map[string]string{"service_account_id": sa.ID, "environment_id": envID, "project_id": t.projectID})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Configured %s for %s.\n", sa.ID, t.label())
	return nil
}

func projectAgentSetupEnvironmentID(ctx context.Context, t *projectAgentTarget) (string, error) {
	envs, err := t.listEnvironments(ctx)
	if err != nil {
		return "", err
	}
	env, err := managedAgentEnvironment(envs)
	return env.ID, err
}

func managedAgentEnvironment(envs []api.ProjectEnvironment) (api.ProjectEnvironment, error) {
	var selected api.ProjectEnvironment
	for _, env := range envs {
		if env.Managed && env.RunsOn == "self_hosted" && env.ArchivedAt == nil {
			if selected.ID != "" {
				return selected, errors.New("multiple managed self-hosted Environments")
			}
			selected = env
		}
	}
	if selected.ID != "" {
		return selected, nil
	}
	return selected, errors.New("no managed self-hosted Environment; first create a custom agent with runs_on self_hosted")
}

// Do not downgrade existing grants when reusing an account.
func ensureAgentRepoGrant(ctx context.Context, c *coreapi.Client, account, repo string, created bool) error {
	if !created {
		grants, err := fetchAllPages(ctx, func(ctx context.Context, cursor string) ([]coreapi.ServiceAccountGrant, string, error) {
			p := coreapi.ListServiceAccountGrantsParams{AccountId: account}
			if cursor != "" {
				p.PageToken = coreapi.NewOptString(cursor)
			}
			out, err := c.ListServiceAccountGrants(ctx, p)
			if err != nil {
				return nil, "", err
			}
			return out.Grants, out.NextPageToken.Or(""), nil
		})
		if err != nil {
			return err
		}
		for _, g := range grants {
			if g.ResourceType == saResourceRepo && g.ResourceId == repo {
				return nil
			}
		}
	}
	_, err := c.GrantServiceAccountAccess(ctx, &coreapi.GrantServiceAccountAccessInputBody{ResourceType: saResourceRepo, ResourceId: repo, Role: "reader"}, coreapi.GrantServiceAccountAccessParams{AccountId: account})
	return err
}
