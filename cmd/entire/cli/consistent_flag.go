package cli

import (
	"context"

	"github.com/spf13/cobra"
)

// consistentFlagName is the root's persistent read-your-writes switch.
const consistentFlagName = "consistent"

// consistentReadsKey marks a context whose repo name resolves skip the
// follower read.
type consistentReadsKey struct{}

// withConsistentReads asks for fresh name resolves.
func withConsistentReads(ctx context.Context) context.Context {
	return context.WithValue(ctx, consistentReadsKey{}, true)
}

// consistentReads reports whether ctx asks for fresh resolves.
func consistentReads(ctx context.Context) bool {
	on, ok := ctx.Value(consistentReadsKey{}).(bool)
	return ok && on
}

// addConsistentFlag registers --consistent on the root command.
//
// Core resolves a repo name from a follower that lags a few seconds, so
// `entire repo create x && entire repo clone x` can miss the new repo. The
// flag asks core for a present-time read instead. It is global because name
// resolves happen under `repo`, `trail`, `explain` and the grant commands, and
// a new resolving command must not silently ignore it.
func addConsistentFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().Bool(consistentFlagName, false, "Read your own recent writes; slower, for scripts")
}

// applyConsistentFlag carries --consistent into the command context.
//
// The context, not a package variable, carries it: every resolve already
// takes the command's context, and a test can set it per call.
func applyConsistentFlag(cmd *cobra.Command) {
	// GetBool errors only on a tree without the flag.
	on, err := cmd.Flags().GetBool(consistentFlagName)
	if err != nil || !on {
		return
	}
	cmd.SetContext(withConsistentReads(cmd.Context()))
}
