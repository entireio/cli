// Command onboarding is a throwaway prototype of Entire's onboarding UX,
// modelled on the "one-keypress" proposal: a single Bubble Tea review screen
// where every value is pre-decided and editable, and Enter alone builds the
// folder. Every side effect (login, mirror, hooks, git) is mocked so we can
// iterate on the flow fast. Nothing here imports entire-cli internals — it only
// shares cobra/bubbletea/lipgloss so the look and feel transfers.
//
//	go run ./prototypes/onboarding                  # auto-detect, review screen
//	go run ./prototypes/onboarding --state empty     # simulate an empty folder
//	go run ./prototypes/onboarding --state repo-gh --yes --fast   # non-interactive
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		stateFlag string
		github    bool
		assumeYes bool
		noTelem   bool
		agents    []string
		region    string
		fast      bool
	)
	cmd := &cobra.Command{
		Use:           "enable",
		Short:         "Prototype: one-keypress onboarding review screen",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if fast {
				mockDelay = 0
			}
			if len(agents) == 0 {
				agents = []string{"Claude Code"}
			}
			state, slug, err := resolveState(stateFlag)
			if err != nil {
				return err
			}
			cfg := defaultConfig(state, slug, region, agents, !noTelem)
			if github && state == StateEmpty {
				cfg.RepoMode = "github"
				cfg.Connect = cfg.mirrorable()
			}

			w := cmd.OutOrStdout()

			// Non-interactive (--yes / no review): take the defaults, exactly
			// like today's --yes. Otherwise show the one-keypress review.
			if !assumeYes {
				reviewed, ok, rerr := runReview(cmd.Context(), cfg)
				if rerr != nil {
					return rerr
				}
				if !ok {
					fmt.Fprintln(w, styDim.Render("Cancelled — folder untouched."))
					return nil
				}
				cfg = reviewed
			}

			runPlan(w, planFromConfig(cfg))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&stateFlag, "state", "auto", "folder state: auto | repo-gh | repo-no-origin | empty")
	f.BoolVar(&github, "github", false, "empty folder: create + push a private GitHub repo (enables mirroring)")
	f.BoolVarP(&assumeYes, "yes", "y", false, "accept the defaults without the review screen")
	f.BoolVar(&noTelem, "no-telemetry", false, "opt out of anonymous usage data")
	f.StringArrayVar(&agents, "agent", nil, "agent(s) to install hooks for (default: Claude Code)")
	f.StringVar(&region, "region", "aws-us-east-2", "mirror data-residency region")
	f.BoolVar(&fast, "fast", false, "skip the simulated delays")
	return cmd
}

// resolveState maps --state to a FolderState. "auto" really inspects the
// current directory; explicit values let us demo any branch.
func resolveState(flag string) (FolderState, string, error) {
	switch flag {
	case "repo-gh":
		return StateRepoGitHub, "github.com/acme/api", nil
	case "repo-no-origin":
		// Slug is the would-be published name; only used in the mirror URL, not
		// the "no GitHub origin" summary.
		return StateRepoNoOrigin, "github.com/acme/my-repo", nil
	case "empty":
		return StateEmpty, "github.com/acme/new-project", nil
	case "auto":
		return detectState()
	default:
		return 0, "", fmt.Errorf("unknown --state %q (want auto|repo-gh|repo-no-origin|empty)", flag)
	}
}

func detectState() (FolderState, string, error) {
	if err := exec.Command("git", "rev-parse", "--git-dir").Run(); err != nil {
		return StateEmpty, "github.com/acme/new-project", nil
	}
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return StateRepoNoOrigin, "", nil
	}
	if slug := githubSlug(strings.TrimSpace(string(out))); slug != "" {
		return StateRepoGitHub, slug, nil
	}
	return StateRepoNoOrigin, "", nil
}

func githubSlug(url string) string {
	url = strings.TrimSuffix(url, ".git")
	switch {
	case strings.HasPrefix(url, "git@github.com:"):
		return "github.com/" + strings.TrimPrefix(url, "git@github.com:")
	case strings.HasPrefix(url, "https://github.com/"):
		return "github.com/" + strings.TrimPrefix(url, "https://github.com/")
	}
	return ""
}
