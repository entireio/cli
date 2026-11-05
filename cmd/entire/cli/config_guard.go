package cli

import (
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/internal/entireclient/tokenstore"

	"github.com/spf13/cobra"
)

// configCheckAnnotation sets how a command checks config.json.
// The nearest annotated ancestor wins. Unannotated commands check, unless
// ENTIRE_TOKEN is set. "skip" is for commands that must never fail or never
// touch the token store; "always" is for commands that write the store
// whatever ENTIRE_TOKEN says. configCheckLedger in the tests pins every
// command whose effective mode is not the default, with a reason.
const (
	configCheckAnnotation = "entire_config_check"
	configCheckSkip       = "skip"
	configCheckAlways     = "always"
)

// exemptFromConfigCheck marks cmd and its children exempt.
func exemptFromConfigCheck(cmd *cobra.Command) *cobra.Command {
	return annotateConfigCheck(cmd, configCheckSkip)
}

// alwaysCheckConfig ignores ENTIRE_TOKEN for cmd.
func alwaysCheckConfig(cmd *cobra.Command) *cobra.Command {
	return annotateConfigCheck(cmd, configCheckAlways)
}

func annotateConfigCheck(cmd *cobra.Command, mode string) *cobra.Command {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[configCheckAnnotation] = mode
	return cmd
}

// configCheckMode is the nearest annotated ancestor's mode.
func configCheckMode(cmd *cobra.Command) string {
	for c := cmd; c != nil; c = c.Parent() {
		if mode := c.Annotations[configCheckAnnotation]; mode != "" {
			return mode
		}
	}
	return ""
}

// checkConfigBeforeRun fails on an unusable config.json.
func checkConfigBeforeRun(cmd *cobra.Command) error {
	switch configCheckMode(cmd) {
	case configCheckSkip:
		return nil
	case configCheckAlways:
		return tokenstore.CheckConfig() //nolint:wrapcheck // already names the file
	}
	return auth.CheckTokenStoreConfig() //nolint:wrapcheck // already names the file
}
