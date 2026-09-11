package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// branchRule is the JSON/table view of one branch-protection rule: the
// pattern as the server stores it and its level.
type branchRule struct {
	Ref                 string `json:"ref"`
	ServerSideMergeOnly bool   `json:"serverSideMergeOnly"`
}

var protectionColumns = []string{"BRANCH", "LEVEL"}

const (
	protectionLevelProtected = "protected"
	protectionLevelMergeOnly = "server-side merge only"
	// protectionEmpty asserts that nothing protects this repository, so it
	// is printed only for a provider positively known to be Entire-native.
	protectionEmpty      = "Nothing is protected yet."
	protectionMirrorNote = "GitHub mirror: branch protection is governed by the upstream repository. " +
		"Its default branch is always protected on Entire; no rules can be added here."
	// protectionUnknownNote covers every empty list whose provider was not
	// established: absent (an older core), a value this build does not know
	// (a forge added later), or a lookup that failed. Each of those is
	// "we could not find out", which is not the same as "nothing is
	// protected" — see reportNoProtectionRules.
	protectionUnknownNote = "No branch-protection rules are set here on Entire. This repository's " +
		"provider could not be determined, so whether protection is governed elsewhere is unknown; " +
		"a mirror's rules are its upstream's."
	// headBranchPattern is the server's pattern for "whatever branch HEAD
	// points at"; it follows a default-branch rename.
	headBranchPattern       = "HEAD"
	serverSideMergeOnlyFlag = "server-side-merge-only"
)

func protectionRow(r branchRule) []string {
	level := protectionLevelProtected
	if r.ServerSideMergeOnly {
		level = protectionLevelMergeOnly
	}
	return []string{r.Ref, level}
}

func branchRulesFromWire(p *coreapi.BranchProtection) []branchRule {
	rules := make([]branchRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, branchRule{Ref: r.Ref, ServerSideMergeOnly: r.ServerSideMergeOnly.Or(false)})
	}
	return rules
}

// expandBranchRef maps the CLI argument onto the server's pattern syntax:
// "HEAD" and anything under refs/ pass through, a short name is a branch
// under refs/heads/. Wildcards are left to the server to validate.
func expandBranchRef(s string) (string, error) {
	switch {
	case s == "":
		return "", errors.New("branch must not be empty")
	case s == headBranchPattern, strings.HasPrefix(s, "refs/"):
		return s, nil
	default:
		return "refs/heads/" + s, nil
	}
}

// newRepoProtectionCmd groups the verbs for a repository's branch protection.
// Each rule names a branch pattern and one of two levels. "protected" refuses
// force pushes and deletion; fast-forward pushes stay allowed. "server-side
// merge only" also refuses every direct push: the branch moves only through a
// merge Entire performs, such as a trail merge, and whether that merge runs is
// decided by the repository's gates.
func newRepoProtectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "protection",
		Short: "List, add, or remove branch-protection rules",
		Long: "List, add, or remove a repository's branch-protection rules.\n\n" +
			"Each rule names a branch pattern and a level. \"protected\" refuses force pushes " +
			"and deletion; fast-forward pushes stay allowed. \"server-side merge only\" also " +
			"refuses every direct push: the branch moves only through a merge Entire performs, " +
			"such as a trail merge. A pattern is \"HEAD\" (the default branch), a branch name, " +
			"or a branch pattern with * and ? wildcards such as release/*. Entire-native " +
			"repositories only; a GitHub mirror's protection is the upstream's.",
	}
	cmd.AddCommand(newRepoProtectionListCmd())
	cmd.AddCommand(newRepoProtectionAddCmd())
	cmd.AddCommand(newRepoProtectionRemoveCmd())
	return cmd
}

func newRepoProtectionListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "Show a repository's branch-protection rules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return err
				}
				out, err := c.GetBranchProtection(ctx, coreapi.GetBranchProtectionParams{RepoId: repoID})
				if err != nil {
					return err
				}
				rules := branchRulesFromWire(out)
				if len(rules) > 0 {
					// Only the empty list needs the repo's provider, so the
					// common case costs one round trip rather than two.
					if jsonRequested(cmd) {
						return printJSON(cmd.OutOrStdout(), rules)
					}
					return printTable(cmd.OutOrStdout(), protectionColumns, rules, protectionRow)
				}
				return reportNoProtectionRules(ctx, cmd, c, repoID)
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addJSONFlag(cmd)
	return cmd
}

// reportNoProtectionRules renders an empty rule list, naming which kind of
// empty it is. A GitHub mirror always reads as empty — its rules are the
// upstream's, and the data plane protects its default branch regardless — so
// "nothing is protected" would misstate it.
//
// All three outcomes are identified positively, and "native" is not the else
// of "mirror". `provider` is optional (an older core omits it) and open
// (`readModelEnumFields` drops its enum, so a forge added later decodes
// verbatim), and in JSON mode the lookup may fail outright. Every one of
// those is "we could not find out", which is not evidence that nothing is
// protected — treating it as such is how an unqualified "Nothing is
// protected yet." would come to hide a mirror's upstream rules. Only
// repoProviderEntire earns that sentence; everything else gets
// protectionUnknownNote.
//
// The caveat reaches --json callers too, on stderr: stdout stays the bare
// array a script parses, but a script concluding "no rules ⇒ nothing is
// protected" is wrong on a mirror, which is the misreading the note exists
// to prevent. That makes the note advisory for --json and load-bearing for
// the human rendering, so a failed provider lookup is fatal only to the
// latter — the branch-protection answer is already in hand, and a script
// must not lose its array because a secondary lookup flaked. It is still
// told, with the reason, rather than handed a silent [].
func reportNoProtectionRules(ctx context.Context, cmd *cobra.Command, c *coreapi.Client, repoID string) error {
	note := protectionUnknownNote
	repo, err := c.GetRepo(ctx, coreapi.GetRepoParams{RepoId: repoID})
	switch {
	case err != nil:
		if !jsonRequested(cmd) {
			return err
		}
		note = fmt.Sprintf("%s (looking it up failed: %v)", protectionUnknownNote, err)
	case repo.Provider.Or("") == repoProviderGitHub:
		note = protectionMirrorNote
	case repo.Provider.Or("") == repoProviderEntire:
		note = "" // the one case that positively establishes "native".
	}
	if jsonRequested(cmd) {
		if note != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), note)
		}
		return printJSON(cmd.OutOrStdout(), []branchRule{})
	}
	if note == "" {
		note = protectionEmpty
	}
	fmt.Fprintln(cmd.OutOrStdout(), note)
	return nil
}

func newRepoProtectionAddCmd() *cobra.Command {
	var project string
	var mergeOnly bool
	cmd := &cobra.Command{
		Use:   "add <repo> <branch>",
		Short: "Protect a branch, or change the level of an existing rule",
		Long: "Protect a branch, or change the level of an existing rule.\n\n" +
			"<branch> is \"HEAD\", a branch name such as main, or a pattern such as release/*. " +
			"A new rule protects the branch from force pushes and deletion. With " +
			"--server-side-merge-only, every direct push is refused and the branch moves only " +
			"through a merge Entire performs. Re-adding a branch without the flag keeps its " +
			"current level; pass --server-side-merge-only=false to lower it. Prints the " +
			"resulting rules. Requires manage permission on the repo.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := expandBranchRef(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			// protectionEmpty asserts the repo is native, which list has to
			// establish but this verb gets for free: add and remove share one
			// PATCH, the server refuses it on a mirror, so reaching the render
			// at all means the write landed on a repo that accepts rules.
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				// The level travels only when the flag was given. Absent, the
				// server keeps an existing rule's level and protects a new
				// branch, so an add that only names a branch can never lower
				// it; --server-side-merge-only=false is the explicit way down.
				rule := coreapi.BranchRule{Ref: ref}
				if cmd.Flags().Changed(serverSideMergeOnlyFlag) {
					rule.ServerSideMergeOnly = coreapi.NewOptBool(mergeOnly)
				}
				body := &coreapi.UpdateBranchProtectionInputBody{AddRules: []coreapi.BranchRule{rule}}
				out, err := c.UpdateBranchProtection(ctx, body, coreapi.UpdateBranchProtectionParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	cmd.Flags().BoolVar(&mergeOnly, serverSideMergeOnlyFlag, false, "Refuse every direct push; the branch moves only through a merge Entire performs. Omit to keep an existing rule's level; =false lowers it")
	addJSONFlag(cmd)
	return cmd
}

func newRepoProtectionRemoveCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "remove <repo> <branch>",
		Short: "Remove a branch-protection rule",
		Long: "Remove a branch-protection rule.\n\n" +
			"<branch> names the rule as it was added: \"HEAD\", a branch name, or a pattern. " +
			"Removing a branch that has no rule changes nothing. Prints the resulting rules. " +
			"Requires manage permission on the repo.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := expandBranchRef(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			// protectionEmpty asserts the repo is native, which list has to
			// establish but this verb gets for free: add and remove share one
			// PATCH, the server refuses it on a mirror, so reaching the render
			// at all means the write landed on a repo that accepts rules.
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				body := &coreapi.UpdateBranchProtectionInputBody{RemoveRefs: []string{ref}}
				out, err := c.UpdateBranchProtection(ctx, body, coreapi.UpdateBranchProtectionParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addJSONFlag(cmd)
	return cmd
}
