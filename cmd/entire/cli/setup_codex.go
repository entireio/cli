package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	codexagent "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/review"

	"charm.land/huh/v2"
)

// canOfferCodexDiscoveredInstall reports whether setup may ask to install
// Codex hooks into another checkout. --yes, scripts, CI, and agent
// subprocesses never get that write; they get the remedy text instead.
//
// An agent child that holds a TTY can pass CanPromptInteractively, so this
// offer, which writes outside the checkout, also refuses any detected agent
// session.
func canOfferCodexDiscoveredInstall(opts EnableOptions) bool {
	return !opts.Yes && detectCodexOfferAgentSession() == "" && interactive.CanPromptInteractively()
}

// detectCodexOfferAgentSession is a var so tests can run without inheriting
// the developer's agent identity.
var detectCodexOfferAgentSession = review.DetectAgentSession

// promptInstallCodexDiscoveredHooks asks for consent to write the hooks file
// Codex discovers. It is a package-level var so tests can substitute it.
//
// The answer comes from the controlling terminal, not stdin, and the question
// renders there when w is not a terminal: piped input must not be able to
// approve a write to another checkout, and a redirected stream must not hide
// the question. ENTIRE_TEST_TTY=1 is the explicit test path, where
// integration tests answer through stdin.
var promptInstallCodexDiscoveredHooks = func(ctx context.Context, w io.Writer, target codexagent.DiscoveredHooksPath) (bool, error) {
	// Everything goes in the title: accessible mode drops descriptions.
	title := fmt.Sprintf("Codex reads hooks from %s, not from this worktree. Install Entire's Codex hooks there too? %s",
		target.Path(), codexDiscoveredPromptNote(target))
	if os.Getenv(interactive.EnvTestTTY) != "1" {
		return runPluginConfirm(ctx, w, title, false) // default No: this writes outside the checkout
	}
	install := false
	form := NewAccessibleForm(huh.NewGroup(huh.NewConfirm().Title(title).Value(&install)))
	if err := form.RunWithContext(ctx); err != nil {
		return false, fmt.Errorf("codex hooks prompt: %w", err)
	}
	return install, nil
}

// finishCodexHooksSetup runs after Codex hooks were installed in the current
// checkout. When Codex reads a different checkout's hooks file that lacks
// Entire's hooks, it offers to install them there; only an explicit yes, and
// only when mayPrompt, writes outside the current checkout. The offer is
// optional, so an aborted prompt or a failed write warns and prints the
// remedy instead of failing setup; only a cancelled command returns an error.
func finishCodexHooksSetup(ctx context.Context, w io.Writer, ag agent.Agent, mayPrompt bool) error {
	if ag.Name() != agent.AgentNameCodex {
		return nil
	}

	diagnostics := codexagent.InspectHookDiagnostics(ctx)
	if !diagnostics.PathsDiffer() {
		return nil
	}
	fmt.Fprintf(w, "  Codex reads hooks from: %s\n", diagnostics.Discovery.DiscoveredHooks.Path())
	fmt.Fprintf(w, "  Entire configured this worktree at: %s\n", diagnostics.WorktreeHooks.Path())

	target, offer := diagnostics.DiscoveredInstallCandidate()
	if !offer && diagnostics.Discovered.State == codexagent.HookFileEntire && diagnostics.Discovered.Current {
		fmt.Fprintln(w, "  Entire's Codex hooks are already installed there.")
		return nil
	}
	install := false
	if offer && mayPrompt {
		var err error
		if install, err = promptInstallCodexDiscoveredHooks(ctx, w, target); err != nil {
			// A cancelled command stops setup; anything else about this
			// optional prompt (an aborted or broken form) counts as No.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("codex hooks prompt: %w", ctxErr)
			}
			fmt.Fprintf(w, "  Skipped installing Codex hooks in %s: %v\n", target.Path(), err)
			install = false
		}
	}
	if !install {
		fmt.Fprintln(w, "  Hooks in the current-worktree file will not run here.")
		if offer {
			writeCodexPrimaryCheckoutRemedy(w, target.Path(), codexNonWorktreeRoot(target))
		} else {
			fmt.Fprintln(w, "  Run `entire doctor` for details.")
		}
		return nil
	}
	count, err := codexagent.InstallDiscoveredHooks(ctx, target)
	if err != nil {
		fmt.Fprintf(w, "  Could not install Codex hooks in %s: %v\n", target.Path(), err)
		writeCodexPrimaryCheckoutRemedy(w, target.Path(), codexNonWorktreeRoot(target))
		return nil
	}
	if count > 0 {
		fmt.Fprintf(w, "  Installed Entire's Codex hooks in %s. %s Open /hooks in Codex to approve them.\n",
			target.Path(), codexDiscoveredChangeNote(target))
	}
	return nil
}

func codexDiscoveredPromptNote(target codexagent.DiscoveredHooksPath) string {
	if target.RootIsCheckout() {
		return "This leaves an uncommitted change in " + target.Root() + "."
	}
	return "This writes a file in " + target.Root() + ", outside this worktree."
}

func codexDiscoveredChangeNote(target codexagent.DiscoveredHooksPath) string {
	if target.RootIsCheckout() {
		return "This is an uncommitted change in that checkout."
	}
	return "That project root is not a worktree, so Git does not track this file."
}
