package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// onDemandPluginInstall shares the normal install workflow, including index
// overrides, name/checksum validation and dependency confirmation. Tests replace
// it to exercise the prompt and dispatch without downloading real releases.
var onDemandPluginInstall = runRemoteInstall

func installMissingPlugin(ctx context.Context, rootCmd *cobra.Command, name string) (string, error) {
	// The plugin may already be in the managed directory and merely
	// unreachable through PATH — a managed bin dir that could not be
	// prepended at startup. Offering to install over it is a dead end:
	// installRepoAtTag refuses an existing install without --force, which the
	// on-demand path deliberately does not pass, so the user answers Yes,
	// waits for three network round-trips and gets "already installed; use
	// --force to replace". Execute the managed entry instead, which is what
	// this function's own return contract promises below.
	//
	// A listing error falls through to the install rather than failing here:
	// the install path reads the same directory and reports the problem in
	// terms of what it was trying to do.
	if installed, err := FindInstalledPlugin(name); err == nil && installed != nil {
		// The entry is not necessarily runnable. ListInstalledPlugins reports
		// what it finds with Lstat, so a local-dev symlink whose target moved
		// is listed like any other install — exec'ing it fails with a
		// fork/exec ENOENT that names a path the user never chose and offers
		// no way forward. Say what is wrong and how to fix it instead.
		//
		// Reinstalling automatically would be the other option, and it is
		// deliberately not taken: replacing a developer's deliberate symlink
		// with a released binary is their call to make, not ours.
		if reinstallFixes, cerr := checkManagedPluginRunnable(installed.Path); cerr != nil {
			broken := fmt.Errorf("the entire-%s plugin is installed at %s but cannot be run: %w", name, installed.Path, cerr)
			if !reinstallFixes {
				return "", broken
			}
			return "", fmt.Errorf("%w; reinstall it with 'entire plugin install %s --force'", broken, name)
		}
		return installed.Path, nil
	}
	if !interactive.CanPromptInteractively() {
		return "", fmt.Errorf("the entire-%s plugin is not installed; run 'entire plugin install %s' and retry", name, name)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}

	// Resolve the source before asking, for two reasons.
	//
	// The prompt is the only human checkpoint on this path — an index-listed
	// install never prompts inside runRemoteInstall, because the catalog is
	// the trust decision — so it has to say where the binary comes from.
	// confirmInstallOrCancel names redactURL(repoURL) for an unlisted
	// repository; a prompt that defaults to Yes and names nothing tells the
	// user less about a download-and-exec than the one that defaults to No.
	//
	// And a name the index does not carry cannot be installed at all, so
	// asking first and failing afterwards spends the user's Yes on a question
	// that never had an answer — the same prompt-then-dead-end shape as the
	// already-installed case above.
	//
	// This is not an extra round-trip: SyncPluginIndex touches a freshness
	// marker, and runRemoteInstall's own call moments later reads the clone
	// without fetching (pluginIndexTTL). Progress is reported and stopped
	// before the prompt, per the rule that a spinner never overlaps a
	// confirmation.
	indexURL := resolvePluginIndexURL("")
	stopIndex := startPluginStep(withPluginProgress(ctx, rootCmd.ErrOrStderr()), "Checking plugin index...")
	idx, err := SyncPluginIndex(ctx, indexURL, false)
	stopIndex()
	if err != nil {
		return "", fmt.Errorf("look up the entire-%s plugin in the plugin index %s: %w", name, redactURL(indexURL), err)
	}
	entry := idx.Find(name)
	if entry == nil {
		return "", fmt.Errorf("the entire-%s plugin is not listed in the plugin index %s, so it cannot be installed on demand; install it from its repository URL with 'entire plugin install <url>'", name, redactURL(indexURL))
	}

	confirmed, err := runPluginConfirm(ctx, rootCmd.ErrOrStderr(),
		fmt.Sprintf("Install the entire-%s plugin from %s?", name, redactURL(entry.RepoURL)), true)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", handleFormCancellation(rootCmd.ErrOrStderr(), "Install", err)
	}
	if !confirmed {
		fmt.Fprintln(rootCmd.ErrOrStderr(), "Install cancelled.")
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}

	// Keep install progress off stdout: the original command may emit JSON or
	// be piped to another tool. Do not parse any of the plugin's arguments.
	cmd := newPluginInstallCmd()
	cmd.SetOut(rootCmd.ErrOrStderr())
	cmd.SetErr(rootCmd.ErrOrStderr())
	// Resolved carries the entry the prompt named, so the install cannot
	// re-resolve into a different repository after the user has agreed to
	// this one — see installSource.Resolved.
	src := installSource{Kind: installFromIndex, Ref: name, Resolved: entry}
	if err := onDemandPluginInstall(ctx, cmd, src, remoteInstallFlags{}); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}
	installed, err := FindInstalledPlugin(name)
	if err != nil {
		return "", err
	}
	if installed == nil {
		return "", fmt.Errorf("the entire-%s plugin was not installed; run 'entire plugin install %s' and retry", name, name)
	}
	// Execute the managed entry directly, even if the managed directory could
	// not be prepended to PATH at startup.
	return installed.Path, nil
}

// checkManagedPluginRunnable reports why a managed plugin entry cannot be
// executed, or a nil error when it can. reinstallFixes is true only for a
// condition a reinstall actually repairs, so the caller does not attach that
// remedy to an error it would not resolve — a permission failure on the
// managed directory is not fixed by installing into it, and a remedy hung off
// an unmatched error is how advice comes to name the wrong cause (the mistake
// writeEntireDirRemedy documents for `.entire`). It is meaningless when the
// error is nil.
//
// os.Stat rather than Lstat, deliberately: the question is whether the thing
// at the end of the entry exists, and a dangling symlink is the case that
// brought this function into being. The executable bit is left to the exec —
// findInaccessiblePlugin draws the same line for PATH entries, and the mode
// does not mean the same thing on Windows.
func checkManagedPluginRunnable(path string) (reinstallFixes bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			// Worded rather than passed through because the errno itself ("no
			// such file or directory") describes the entry, which plainly
			// exists; what is missing is whatever it points at.
			return true, errors.New("it points at a file that no longer exists")
		}
		return false, statErr //nolint:wrapcheck // the caller adds the plugin name and path
	}
	if info.IsDir() {
		return true, errors.New("it is a directory")
	}
	return false, nil
}
