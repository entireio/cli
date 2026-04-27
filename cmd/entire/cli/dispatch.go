package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var runDispatch = dispatchpkg.Run
var renderDispatchMarkdown = dispatchpkg.RenderMarkdown
var dispatchTerminalMode = interactive.IsTerminalWriter
var runInteractiveDispatch = defaultRunInteractiveDispatch
var renderTerminalMarkdown = defaultRenderTerminalMarkdown

func newDispatchCmd() *cobra.Command {
	var (
		flagLocal            bool
		flagSince            string
		flagUntil            string
		flagAllBranches      bool
		flagRepos            []string
		flagVoice            string
		flagInsecureHTTPAuth bool
	)

	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Generate a dispatch summarizing recent agent work",
		Long: `Generate a dispatch summarizing recent agent work.

Examples:
  entire dispatch
  entire dispatch --local --all-branches
  entire dispatch --repos entireio/cli
  entire dispatch --voice neutral`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var (
				opts dispatchpkg.Options
				err  error
			)

			if shouldRunDispatchWizard(cmd.Flags().NFlag(), isTerminalStdin(os.Stdin), interactive.IsTerminalWriter(cmd.OutOrStdout())) {
				opts, err = runDispatchWizard(cmd)
			} else {
				opts, err = parseDispatchFlags(cmd, flagLocal, flagSince, flagUntil, flagAllBranches, flagRepos, flagVoice, flagInsecureHTTPAuth)
			}
			if err != nil {
				if errors.Is(err, errDispatchCancelled) {
					return nil
				}
				return err
			}

			if err := runDispatchCommand(cmd.Context(), cmd.OutOrStdout(), opts); err != nil {
				if errors.Is(err, errDispatchCancelled) {
					return nil
				}
				return err
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&flagLocal, "local", false, "generate via the locally-installed agent CLI instead of the Entire server")
	cmd.Flags().StringVar(&flagSince, "since", "7d", "time window (Go duration, relative time, or ISO date)")
	cmd.Flags().StringVar(&flagUntil, "until", "", "window end time (defaults to now)")
	cmd.Flags().BoolVar(&flagAllBranches, "all-branches", false, "include every existing local branch (--local only; renamed or deleted branches are skipped)")
	cmd.Flags().StringSliceVar(&flagRepos, "repos", nil, fmt.Sprintf("cloud repo slugs, up to %d (for example entireio/cli)", dispatchpkg.CloudRepoLimit))
	cmd.Flags().StringVar(&flagVoice, "voice", "", "voice preset name or literal description")
	cmd.Flags().BoolVar(&flagInsecureHTTPAuth, "insecure-http-auth", false, "Allow authentication over plain HTTP (insecure, for local development only)")
	if err := cmd.Flags().MarkHidden("insecure-http-auth"); err != nil {
		panic(fmt.Sprintf("hide insecure-http-auth flag: %v", err))
	}

	return cmd
}

func runDispatchCommand(ctx context.Context, outW io.Writer, opts dispatchpkg.Options) error {
	if dispatchTerminalMode(outW) && !IsAccessibleMode() {
		markdown, err := runInteractiveDispatch(ctx, outW, opts)
		if err != nil {
			return err
		}
		rendered, err := renderTerminalMarkdown(outW, markdown)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprint(outW, rendered); err != nil {
			return fmt.Errorf("write dispatch output: %w", err)
		}
		return nil
	}

	result, err := runDispatch(ctx, opts)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(outW, renderDispatchMarkdown(result)); err != nil {
		return fmt.Errorf("write dispatch output: %w", err)
	}
	return nil
}

func isTerminalStdin(file *os.File) bool {
	return term.IsTerminal(int(file.Fd())) //nolint:gosec // G115: uintptr->int is safe for fd
}

func shouldRunDispatchWizard(flagCount int, stdinIsTerminal bool, stdoutIsTerminal bool) bool {
	return flagCount == 0 && stdinIsTerminal && stdoutIsTerminal
}

func parseDispatchFlags(
	cmd *cobra.Command,
	flagLocal bool,
	flagSince string,
	flagUntil string,
	flagAllBranches bool,
	flagRepos []string,
	flagVoice string,
	flagInsecureHTTPAuth bool,
) (dispatchpkg.Options, error) {
	return resolveDispatchOptions(
		flagLocal,
		flagSince,
		flagUntil,
		flagAllBranches,
		flagRepos,
		flagVoice,
		flagInsecureHTTPAuth,
		func() (string, error) {
			return GetCurrentBranch(cmd.Context())
		},
	)
}

//nolint:wrapcheck // passthrough glue to keep CLI error text unchanged while option logic lives in dispatch package
func resolveDispatchOptions(
	flagLocal bool,
	flagSince string,
	flagUntil string,
	flagAllBranches bool,
	flagRepos []string,
	flagVoice string,
	flagInsecureHTTPAuth bool,
	currentBranch func() (string, error),
) (dispatchpkg.Options, error) {
	return dispatchpkg.ResolveOptions(
		flagLocal,
		flagSince,
		flagUntil,
		flagAllBranches,
		flagRepos,
		flagVoice,
		flagInsecureHTTPAuth,
		currentBranch,
	)
}
