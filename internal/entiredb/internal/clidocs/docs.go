// Package clidocs renders a cobra command tree to Markdown.
//
// Every CLI binary in this repo exposes a `docs` subcommand that dumps the
// full command tree as Markdown so agents can work from real command
// definitions instead of guessing flags.
package clidocs

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

// NewCmd returns a `docs` cobra command that walks the root command tree
// and writes Markdown to stdout.
func NewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "docs",
		Short: "Print full CLI reference as Markdown to stdout",
		Long: "Dumps every subcommand's description, flags, and examples in a " +
			"single Markdown stream, generated from the live command tree. " +
			"Pipe into an LLM context so agents work from real command " +
			"definitions instead of guessing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := cmd.Root()
			root.DisableAutoGenTag = true
			return Walk(root, cmd.OutOrStdout())
		},
	}
}

// Walk recursively renders cmd and its children as Markdown to w. The "help"
// and "completion" subcommands are skipped.
func Walk(cmd *cobra.Command, w io.Writer) error {
	if err := doc.GenMarkdown(cmd, w); err != nil {
		return fmt.Errorf("render docs for %q: %w", cmd.CommandPath(), err)
	}
	children := cmd.Commands()
	sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
	for _, c := range children {
		if !c.IsAvailableCommand() || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if err := Walk(c, w); err != nil {
			return err
		}
	}
	return nil
}
