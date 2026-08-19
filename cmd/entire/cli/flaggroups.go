package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// flagGroupAnnotation marks which help group a flag renders under; see
// useGroupedFlagHelp.
const flagGroupAnnotation = "entire_flag_group"

// Shared flag-group names, so every list command presents the same taxonomy:
// how much is fetched and which page (navigation), what the client narrows or
// orders after the fetch (filtering & sorting), and how output is rendered
// (formatting).
const (
	flagGroupNavigation = "Navigation"
	flagGroupFiltering  = "Filtering & Sorting"
	flagGroupFormatting = "Formatting"
)

// setFlagGroup assigns the named local flags to a help group. Naming a flag
// that does not exist is a programming error and panics at wiring time.
func setFlagGroup(cmd *cobra.Command, group string, names ...string) {
	for _, n := range names {
		if err := cmd.Flags().SetAnnotation(n, flagGroupAnnotation, []string{group}); err != nil {
			panic(fmt.Sprintf("flag group %q: %v", group, err))
		}
	}
}

// flagGroup is one help section: its name renders as "<Name> Flags:", and an
// optional note renders once under the header — for a fact shared by every
// flag in the group (e.g. "these run client-side"), instead of repeating it
// in each flag's description.
type flagGroup struct {
	name string
	note string
}

// useGroupedFlagHelp replaces the command's flat "Flags:" usage section with
// one "<Group> Flags:" section per group, in the given order, each with its
// optional group-level note. Ungrouped visible local flags (e.g. help) render
// under a plain "Flags:" section after the groups; inherited flags keep their
// usual "Global Flags:" section.
func useGroupedFlagHelp(cmd *cobra.Command, groups ...flagGroup) {
	cmd.SetUsageFunc(func(c *cobra.Command) error {
		w := c.OutOrStderr()
		fmt.Fprintf(w, "Usage:\n  %s\n", c.UseLine())
		sets := make(map[string]*pflag.FlagSet, len(groups)+1)
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			group := ""
			if a := f.Annotations[flagGroupAnnotation]; len(a) > 0 {
				group = a[0]
			}
			fs, ok := sets[group]
			if !ok {
				fs = pflag.NewFlagSet(group, pflag.ContinueOnError)
				sets[group] = fs
			}
			fs.AddFlag(f)
		})
		for _, g := range groups {
			fs, ok := sets[g.name]
			if !ok {
				continue
			}
			fmt.Fprintf(w, "\n%s Flags:\n", g.name)
			if g.note != "" {
				fmt.Fprintf(w, "  %s\n", g.note)
			}
			fmt.Fprint(w, fs.FlagUsages())
		}
		if fs, ok := sets[""]; ok {
			fmt.Fprintf(w, "\nFlags:\n%s", fs.FlagUsages())
		}
		if c.HasAvailableInheritedFlags() {
			fmt.Fprintf(w, "\nGlobal Flags:\n%s", c.InheritedFlags().FlagUsages())
		}
		return nil
	})
}
