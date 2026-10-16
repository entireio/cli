package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Why an enabled-by-default codex feature is left on for summary generation.
// generateTextDisabledFeatures turns off what is known to give the model a
// tool that reaches files or spawns work; everything else codex enables by
// default must be classified here, so a codex release that adds a feature
// cannot slip into summary runs without someone deciding about it.
//
// The classification is by feature name and codex's own description, not by
// a guarantee from codex: codex exec has no "no tools" mode. What the summary
// run can actually reach is measured live by TestTextGeneration_LiveHasNoToolReach.
const (
	// The model gets a tool from it, and it stays on. These are the residual
	// tools generateTextDisabledFeatures documents; none read the canary in
	// live probes (codex-cli 0.156.1), which is observed behaviour only.
	residualTool = "residual tool, accepted"
	// A sub-feature of one generateTextDisabledFeatures turns off, so it has
	// nothing to attach to.
	parentDisabled = "inert: its parent feature is disabled"
	// Configures something loaded from ~/.codex/config.toml (hooks, MCP
	// servers, plugins, skills), which --ignore-user-config skips.
	configIgnored = "inert: --ignore-user-config skips the configuration it reads"
	// Client, transport, or UI behaviour; gives the model no tool.
	notATool = "not a tool"
)

var generateTextAcceptedFeatures = map[string]string{
	"goals":                   residualTool,
	"sleep_tool":              residualTool,
	"tool_suggest":            residualTool,
	"skill_search":            residualTool,
	"worktrees":               residualTool,
	"workspace_dependencies":  residualTool,
	"in_app_local_automation": residualTool,

	"browser_use_full_cdp_access": parentDisabled, // browser_use
	"shell_snapshot":              parentDisabled, // shell_tool
	"unified_exec_tty":            parentDisabled, // unified_exec
	"plugin_sharing":              parentDisabled, // plugins
	"remote_plugin":               parentDisabled, // plugins

	"hooks":                        configIgnored,
	"skill_mcp_dependency_install": configIgnored,
	"tool_call_mcp_elicitation":    configIgnored,
	"mentions_v2":                  configIgnored,

	"auth_elicitation":                 notATool,
	"compaction_image_budget":          notATool,
	"content_item_kinds":               notATool,
	"enable_request_compression":       notATool,
	"fast_mode":                        notATool,
	"guardian_approval":                notATool,
	"guardian_reuse_parent_compaction": notATool,
	"in_app_chat":                      notATool,
	"in_app_dictation":                 notATool,
	"in_app_updates":                   notATool,
	"realtime_conversation":            notATool,
	"system_proxy_fallback":            notATool,
	"unbounded_connection_retries":     notATool,
}

// pinnedFeaturesFile is `codex features list` from the codex release the
// classification was last reviewed against. `features list` prints EFFECTIVE
// state, so a developer's ~/.codex/config.toml would leak into it, while
// summary runs pass --ignore-user-config. Refresh it from an empty config
// home and an empty working directory when upgrading:
//
//	d=$(mktemp -d) && (cd "$d" && CODEX_HOME="$d" codex features list) > testdata/<name>
//
// then classify whatever TestGenerateTextFeatures_EveryEnabledFeatureIsClassified
// reports. `features list` needs no sign-in.
const pinnedFeaturesFile = "codex-features-0.156.1.txt"

// enabledFeatures parses `codex features list` rows ("<name> <stage>
// <enabled>") into the features codex turns on by default. Removed features
// are skipped: codex keeps listing them, and some still print "true", but
// they no longer do anything.
func enabledFeatures(t *testing.T, list string) []string {
	t.Helper()
	var enabled []string
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			t.Fatalf("unrecognized `codex features list` row %q", line)
		}
		name, enabledByDefault := fields[0], fields[len(fields)-1]
		stage := strings.Join(fields[1:len(fields)-1], " ")
		if stage != "removed" && enabledByDefault == "true" {
			enabled = append(enabled, name)
		}
	}
	if !slices.Contains(enabled, probeAnchorFeature) {
		t.Fatalf("parsed no %s from `codex features list`; the format changed", probeAnchorFeature)
	}
	return enabled
}

// unclassified returns the enabled features neither disabled for summary
// generation nor accepted with a reason.
func unclassified(enabled []string) []string {
	var out []string
	for _, f := range enabled {
		if !slices.Contains(generateTextDisabledFeatures, f) && generateTextAcceptedFeatures[f] == "" {
			out = append(out, f)
		}
	}
	return out
}

func TestGenerateTextFeatures_EveryEnabledFeatureIsClassified(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", pinnedFeaturesFile))
	if err != nil {
		t.Fatal(err)
	}
	enabled := enabledFeatures(t, string(data))
	if missing := unclassified(enabled); len(missing) > 0 {
		t.Errorf("codex enables %q by default and summary generation neither disables nor accepts it: "+
			"add each to generateTextDisabledFeatures if it gives the model a tool, else to generateTextAcceptedFeatures with the reason", missing)
	}

	// Stale entries: a classification for a feature codex no longer enables
	// by default says something about codex that is no longer true.
	for f := range generateTextAcceptedFeatures {
		if !slices.Contains(enabled, f) {
			t.Errorf("generateTextAcceptedFeatures lists %q, which %s does not enable by default; remove it", f, pinnedFeaturesFile)
		}
		if slices.Contains(generateTextDisabledFeatures, f) {
			t.Errorf("%q is both disabled and accepted", f)
		}
	}
}

// TestGenerateTextFeatures_InstalledCodexIsClassified is the same check
// against the codex on this machine, which is what catches a new release. It
// makes no API call (`codex features list` is local) but depends on what is
// installed, so it runs only with the other real-agent tests.
func TestGenerateTextFeatures_InstalledCodexIsClassified(t *testing.T) {
	t.Parallel()
	if os.Getenv("ENTIRE_TEST_REAL_AGENTS") == "" {
		t.Skip("set ENTIRE_TEST_REAL_AGENTS=1 to check the installed codex")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not on PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Codex's defaults, not this developer's: summary runs ignore user config.
	cmd := exec.CommandContext(ctx, "codex", "features", "list")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "CODEX_HOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("codex features list: %v", err)
	}
	if missing := unclassified(enabledFeatures(t, string(out))); len(missing) > 0 {
		t.Errorf("the installed codex enables %q by default and summary generation neither disables nor accepts it; "+
			"refresh testdata/%s and classify them", missing, pinnedFeaturesFile)
	}
}
