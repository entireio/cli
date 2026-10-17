package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

var protectionColumns = []string{colHeaderBranch, "LEVEL"}

const (
	protectionLevelProtected = "protected"
	protectionLevelMergeOnly = "server-side merge only"
	// protectionEmpty asserts that nothing protects this repository. The
	// verbs resolve only the native /et/ path, so every repo they reach is
	// one whose rules are Entire's own.
	protectionEmpty      = "Nothing is protected yet."
	protectionMirrorNote = "GitHub mirror: branch protection is governed by the upstream repository. " +
		"Its default branch is always protected on Entire; no rules can be added here."
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
	return requireSubcommand(cmd)
}

func newRepoProtectionListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdListRepo,
		Short: "Show a repository's branch-protection rules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mirror, ok, err := mirrorProtectionRef(args[0]); ok {
				if err != nil {
					return err
				}
				return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
					if err := requireVisibleMirror(ctx, c, mirror); err != nil {
						return err
					}
					return reportMirrorProtection(cmd)
				})
			}
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				resolved, err := resolveRepoPathResolved(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				out, err := c.GetBranchProtection(ctx, coreapi.GetBranchProtectionParams{RepoId: resolved.ID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// mirrorProtectionRef reports whether ref names a GitHub mirror, and parses it
// when it does. A mirror reads as empty from core, but "nothing is protected"
// would misstate it: its default branch is always protected and its rules are
// the upstream's, so no branch-protection read could say more than
// protectionMirrorNote does. A malformed /gh/ ref keeps the parser's reason.
func mirrorProtectionRef(ref string) (mirrorRepoRef, bool, error) {
	if !declaresForge(ref, mirrorCloneForge) {
		return mirrorRepoRef{}, false, nil
	}
	parsed, err := parseMirrorRepoRef(ref, mirrorCloneForge)
	return parsed, true, err
}

// requireVisibleMirror confirms the mirror exists before protectionMirrorNote
// speaks about it: the note says the repo's default branch is protected on
// Entire, which is false of a typo. It is the exact-match directory lookup
// `repo view` makes for a /gh/ ref, and a miss is an error, as an unknown repo
// always was here.
//
// The directory lists both forges under bare full names — an Entire repo
// /et/acme/widget is "acme/widget" there too — so a matching row counts only
// when it is a placed GitHub mirror: forgeOfEntry names the forge as
// buildRepoDir does, and a candidate (a GitHub repo not yet onboarded) is not
// on Entire at all. Not lookupRepoIndexEntry, which trusts the first row and
// so could answer with the native one.
func requireVisibleMirror(ctx context.Context, c *coreapi.Client, mirror mirrorRepoRef) error {
	fullName := mirror.owner + "/" + mirror.repo
	out, err := c.ListRepos(ctx, coreapi.ListReposParams{Filter: coreapi.NewOptString(fullName)})
	if err != nil {
		return err
	}
	if slices.ContainsFunc(out.Repos, func(e coreapi.RepoIndexEntry) bool {
		return strings.EqualFold(e.FullName, fullName) && isPlacedGitHubMirror(e)
	}) {
		return nil
	}
	return fmt.Errorf("no GitHub mirror %s visible from your login (`entire repo mirror list` shows the mirrors you can see)", mirror.qualified())
}

// isPlacedGitHubMirror reports whether a directory entry is a GitHub mirror
// with at least one placement on Entire.
func isPlacedGitHubMirror(e coreapi.RepoIndexEntry) bool {
	if _, candidate := e.Candidate.Get(); candidate {
		return false
	}
	if forge, _ := forgeOfEntry(e); forge != mirrorCloneForge {
		return false
	}
	return slices.ContainsFunc(e.Placements, func(p coreapi.RepoPlacement) bool {
		return placementServesForge(p, mirrorCloneForge)
	})
}

// reportMirrorProtection is `protection list` on a mirror. --json keeps the
// plain array on stdout so a script still parses it, and puts the caveat on
// stderr: a script concluding "no rules ⇒ nothing is protected" is wrong on a
// mirror, which is what the note exists to say.
func reportMirrorProtection(cmd *cobra.Command) error {
	if jsonRequested(cmd) {
		fmt.Fprintln(cmd.ErrOrStderr(), protectionMirrorNote)
		return printJSON(cmd.OutOrStdout(), []branchRule{})
	}
	fmt.Fprintln(cmd.OutOrStdout(), protectionMirrorNote)
	return nil
}

// refuseMirrorProtectionWrite is add and remove on a mirror: refused before any
// request, since its rules are the upstream's.
//
// The refusal is about the ref's forge, so it holds whether or not the mirror
// exists and costs no lookup — and says nothing about the repo it names.
func refuseMirrorProtectionWrite(cmd *cobra.Command, ref string) error {
	_, mirror, err := mirrorProtectionRef(ref)
	if !mirror {
		return nil
	}
	cmd.SilenceUsage = true
	if err != nil {
		return err
	}
	return fmt.Errorf("%q is a GitHub mirror ref: a mirror's branch protection is governed by its upstream repository, so no rules can be added or removed here", strings.TrimSpace(ref))
}

func newRepoProtectionAddCmd() *cobra.Command {
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
			if err := refuseMirrorProtectionWrite(cmd, args[0]); err != nil {
				return err
			}
			ref, err := expandBranchRef(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				resolved, err := resolveRepoPathResolved(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				// The level travels only when the flag was given. Absent, the
				// server keeps an existing rule's level and protects a new
				// branch, so an add that only names a branch can never lower
				// it; --server-side-merge-only=false is the explicit way down.
				rule := coreapi.BranchRuleUpdate{Ref: ref}
				if cmd.Flags().Changed(serverSideMergeOnlyFlag) {
					rule.ServerSideMergeOnly = coreapi.NewOptBool(mergeOnly)
				}
				body := &coreapi.UpdateBranchProtectionInputBody{AddRules: []coreapi.BranchRuleUpdate{rule}}
				out, err := c.UpdateBranchProtection(ctx, body, coreapi.UpdateBranchProtectionParams{RepoId: resolved.ID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	cmd.Flags().BoolVar(&mergeOnly, serverSideMergeOnlyFlag, false, "Refuse every direct push; the branch moves only through a merge Entire performs. Omit to keep an existing rule's level; =false lowers it")
	addJSONFlag(cmd)
	return cmd
}

func newRepoProtectionRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <repo> <branch>",
		Short: "Remove a branch-protection rule",
		Long: "Remove a branch-protection rule.\n\n" +
			"<branch> names the rule as it was added: \"HEAD\", a branch name, or a pattern. " +
			"Removing a branch that has no rule changes nothing. Prints the resulting rules. " +
			"Requires manage permission on the repo.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseMirrorProtectionWrite(cmd, args[0]); err != nil {
				return err
			}
			ref, err := expandBranchRef(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				resolved, err := resolveRepoPathResolved(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				body := &coreapi.UpdateBranchProtectionInputBody{RemoveRefs: []string{ref}}
				out, err := c.UpdateBranchProtection(ctx, body, coreapi.UpdateBranchProtectionParams{RepoId: resolved.ID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}
