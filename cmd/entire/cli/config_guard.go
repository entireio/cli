package cli

import (
	"github.com/entireio/cli/internal/entireclient/userconfig"

	"github.com/spf13/cobra"
)

// configCheckAnnotation sets how a command checks config.json.
// The nearest annotated ancestor wins. Unannotated commands check, even
// with ENTIRE_TOKEN set. "skip" is for commands that must never fail or
// never read config.json. configCheckLedger in the tests pins every
// skipped command, with a reason.
const (
	configCheckAnnotation = "entire_config_check"
	configCheckSkip       = "skip"
)

// exemptFromConfigCheck marks cmd and its children exempt.
func exemptFromConfigCheck(cmd *cobra.Command) *cobra.Command {
	return annotateConfigCheck(cmd, configCheckSkip)
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
	if configCheckMode(cmd) == configCheckSkip {
		return nil
	}
	return userconfig.Check() //nolint:wrapcheck // already names the file
}
