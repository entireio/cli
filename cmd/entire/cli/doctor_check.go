package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// doctorCheckStatus is the severity of one doctor diagnostic line.
type doctorCheckStatus int

const (
	doctorCheckOK doctorCheckStatus = iota
	doctorCheckWarn
	doctorCheckError
)

// doctorPrint writes one flutter-doctor-style status line: a [✓]/[!]/[✗]
// marker followed by the description. Plain text (no ANSI) so the output
// stays script-grep-friendly.
func doctorPrint(w io.Writer, status doctorCheckStatus, text string) {
	var marker string
	switch status {
	case doctorCheckOK:
		marker = "[✓]"
	case doctorCheckWarn:
		marker = "[!]"
	case doctorCheckError:
		marker = "[✗]"
	}
	fmt.Fprintf(w, "%s %s\n", marker, text)
}

// checkForUpdate is the version-fetch seam for the CLI-version doctor check.
// Production wires versioncheck.CheckForUpdate; tests stub it to avoid
// network access (and versioninfo.Version is "dev" in tests anyway, which
// CheckForUpdate short-circuits).
var checkForUpdate = versioncheck.CheckForUpdate

// runDoctorChecks runs the environment / installation / configuration
// diagnostics of `entire doctor` — the flutter-doctor-style sweep. It is
// read-only and non-interactive: it reports issues and points at the command
// that fixes them, and never prompts. Returns the number of issues found
// (warnings + errors) so callers can fold them into the doctor summary.
func runDoctorChecks(cmd *cobra.Command) int {
	w := cmd.OutOrStdout()
	issues := 0

	fmt.Fprintln(w, "Entire Doctor — environment, installation, and configuration checks")
	fmt.Fprintln(w)

	// 1. CLI version and update availability.
	version := versioninfo.Version
	if versioncheck.IsDevBuild(version) {
		doctorPrint(w, doctorCheckOK, fmt.Sprintf("Entire CLI: %s (dev build — update check skipped)", version))
	} else {
		latest, outdated, err := checkForUpdate(cmd.Context(), version)
		switch {
		case err != nil:
			issues++
			doctorPrint(w, doctorCheckWarn, fmt.Sprintf("Entire CLI: %s (could not check for updates: %v)", version, err))
		case outdated:
			issues++
			doctorPrint(w, doctorCheckWarn, fmt.Sprintf("Entire CLI: %s (update available: %s — run '%s')",
				versioncheck.DisplayVersion(version),
				versioncheck.DisplayVersion(latest),
				versioncheck.UpdateCommandForCurrentBinary(version)))
		default:
			doctorPrint(w, doctorCheckOK, fmt.Sprintf("Entire CLI: %s (up to date)", versioncheck.DisplayVersion(version)))
		}
	}

	// 2. Git, the core runtime dependency.
	gitBin, gitErr := exec.LookPath("git")
	if gitErr != nil {
		issues++
		doctorPrint(w, doctorCheckError, "Git: not found on PATH (Entire requires git)")
	} else if out, err := exec.CommandContext(cmd.Context(), gitBin, "--version").Output(); err != nil {
		issues++
		doctorPrint(w, doctorCheckError, fmt.Sprintf("Git: found at %s but could not run it: %v", gitBin, err))
	} else {
		doctorPrint(w, doctorCheckOK, "Git: "+strings.TrimSpace(string(out)))
	}

	// 3. Repository and enablement. Settings are loaded once here and reused
	// by the Configuration check below.
	s, settingsErr := LoadEntireSettings(cmd.Context())
	root, rootErr := paths.WorktreeRoot(cmd.Context())
	switch {
	case rootErr != nil:
		issues++
		doctorPrint(w, doctorCheckWarn, "Repository: not inside a git repository (run 'entire enable' from a repo)")
	case settingsErr != nil:
		issues++
		doctorPrint(w, doctorCheckError, fmt.Sprintf("Repository: %s (could not load settings: %v)", root, settingsErr))
	case !s.Enabled:
		issues++
		doctorPrint(w, doctorCheckWarn, fmt.Sprintf("Repository: %s (Entire is disabled — run 'entire enable')", root))
	default:
		doctorPrint(w, doctorCheckOK, fmt.Sprintf("Repository: %s (Entire enabled)", root))
	}

	// 4. Agent hooks and the CLIs behind them.
	installed := GetAgentsWithHooksInstalled(cmd.Context())
	if len(installed) == 0 {
		issues++
		doctorPrint(w, doctorCheckWarn, "Agent hooks: none installed (run 'entire enable' or 'entire agent add <name>')")
	} else {
		ok := agent.AvailableCLIBinaries(installed)
		missing := agent.MissingCLIBinaries(installed)
		if len(missing) > 0 {
			issues++
			parts := make([]string, 0, len(missing))
			for _, name := range missing {
				parts = append(parts, fmt.Sprintf("%s (CLI '%s' not found on PATH)", name, agent.CLIBinaryName(name)))
			}
			doctorPrint(w, doctorCheckWarn, "Agent hooks installed but agent CLI missing: "+strings.Join(parts, ", "))
		}
		if len(ok) > 0 {
			doctorPrint(w, doctorCheckOK, fmt.Sprintf("Agent hooks: %s (CLI found)", strings.Join(ok, ", ")))
		}
	}

	// 5. Configuration files.
	switch {
	case settingsErr != nil:
		issues++
		doctorPrint(w, doctorCheckError, "Configuration: invalid — "+settingsErr.Error())
		doctorPrint(w, doctorCheckError, "  Fix the JSON in .entire/settings.json or .entire/settings.local.json, then re-run 'entire doctor'.")
	case s.LogLevel != "" && !validLogLevel(s.LogLevel):
		issues++
		doctorPrint(w, doctorCheckWarn, fmt.Sprintf("Configuration: log_level %q is not one of debug/info/warn/error", s.LogLevel))
	default:
		doctorPrint(w, doctorCheckOK, "Configuration: valid")
	}

	// 6. Authentication.
	if _, ok := os.LookupEnv(auth.EnvTokenVar); ok {
		doctorPrint(w, doctorCheckOK, "Authentication: "+auth.EnvTokenVar+" environment variable set")
	} else {
		contexts, _, err := auth.Contexts()
		switch {
		case err != nil:
			issues++
			doctorPrint(w, doctorCheckError, fmt.Sprintf("Authentication: could not read login contexts: %v", err))
		case len(contexts) == 0:
			issues++
			doctorPrint(w, doctorCheckWarn, "Authentication: not logged in (run 'entire login')")
		default:
			doctorPrint(w, doctorCheckOK, fmt.Sprintf("Authentication: logged in (%d context(s) saved)", len(contexts)))
		}
	}

	fmt.Fprintln(w)
	if issues == 0 {
		fmt.Fprintln(w, "No issues found.")
	} else {
		fmt.Fprintf(w, "Doctor found %d issue(s) — see the [!] and [✗] items above.\n", issues)
	}
	fmt.Fprintln(w)

	return issues
}

// validLogLevel reports whether level is one of the supported log levels.
func validLogLevel(level string) bool {
	switch level {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}
