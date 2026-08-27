package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// MinHooksProbeVersion is the first agy release whose print mode answers the
// read-only `/hooks` slash command locally — one tab-separated record per hook
// (or a structured payload under --output-format json) — "without starting an
// agent turn, spending quota or leaving a conversation behind" (agy 1.1.12
// release notes). On older releases `-p "/hooks"` is sent to the model as
// literal prompt text, so the probe must never run there.
const MinHooksProbeVersion = "1.1.12"

// antigravityBinaryName is the agy CLI binary looked up on PATH.
const antigravityBinaryName = "agy"

// hooksProbeTimeout bounds the probe: agy still boots its language server for
// print mode, which takes a few seconds, but a hang (e.g. a stuck keyring
// prompt) must not stall `entire doctor`.
const hooksProbeTimeout = 30 * time.Second

// HooksProbe is the outcome of asking agy which hooks it actually loads for a
// workspace. Loaded distinguishes "agy sees Entire's entry" from "the file is
// on disk but agy ignores it" — the latter is the untrusted-workspace trap
// (agy resolves an untrusted cwd to its scratch workspace, so no hooks fire).
type HooksProbe struct {
	// Version is the agy version string reported by `agy --version`.
	Version string
	// Loaded is true when agy lists an Entire hook entry sourced from the
	// workspace's .agents/hooks.json.
	Loaded bool
	// Sources are the hooks.json paths agy reported, for diagnostics.
	Sources []string
}

// ErrHooksProbeUnsupported is returned when the installed agy predates
// MinHooksProbeVersion; callers should report "cannot verify" rather than
// "not loaded".
var ErrHooksProbeUnsupported = errors.New("agy too old to answer /hooks in print mode")

// ProbeLoadedHooks asks the agy binary on PATH which hooks it loads for
// repoRoot, via `agy -p /hooks --add-dir <repoRoot> --output-format json`.
// --add-dir is what makes agy treat repoRoot as the workspace (the same flag
// the e2e harness relies on); without it the probe would answer for agy's
// scratch workspace and always report nothing loaded.
//
// Zero-quota by construction: the version gate guarantees agy answers /hooks
// locally. Returns ErrHooksProbeUnsupported for older agy, and any spawn or
// parse failure otherwise (an unauthenticated agy fails print mode with
// "authentication required", which surfaces here as an error).
func ProbeLoadedHooks(ctx context.Context, repoRoot string) (HooksProbe, error) {
	probe := HooksProbe{}
	agyPath, err := exec.LookPath(antigravityBinaryName)
	if err != nil {
		return probe, fmt.Errorf("agy not on PATH: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, hooksProbeTimeout)
	defer cancel()

	versionOut, err := exec.CommandContext(ctx, agyPath, "--version").Output()
	if err != nil {
		return probe, fmt.Errorf("agy --version: %w", err)
	}
	probe.Version = strings.TrimSpace(string(versionOut))
	if !HooksProbeSupported(probe.Version) {
		return probe, fmt.Errorf("%w: have %s, need >= %s", ErrHooksProbeUnsupported, probe.Version, MinHooksProbeVersion)
	}

	cmd := exec.CommandContext(ctx, agyPath, "-p", "/hooks", "--add-dir", repoRoot, "--output-format", "json")
	cmd.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return probe, fmt.Errorf("agy -p /hooks: %w: %s", err, firstLine(msg))
	}

	loaded, sources, err := parseHooksProbeOutput(stdout.Bytes(), filepath.Join(repoRoot, ".agents", AgentsHooksFileName))
	if err != nil {
		return probe, err
	}
	probe.Loaded = loaded
	probe.Sources = sources
	return probe, nil
}

// HooksProbeSupported reports whether an agy version string answers /hooks
// locally in print mode. Unparseable versions are treated as unsupported —
// the failure mode of a wrong guess is a real model turn on the user's quota.
func HooksProbeSupported(version string) bool {
	v := strings.TrimSpace(version)
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return semver.IsValid(v) && semver.Compare(v, "v"+MinHooksProbeVersion) >= 0
}

// hooksProbeEnvelope is the subset of agy's --output-format json envelope the
// probe reads: command.data.hooks[] carries one entry per hooks.json "name"
// key with the file it came from.
type hooksProbeEnvelope struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Hooks []struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
				Source  string `json:"source"`
			} `json:"hooks"`
		} `json:"data"`
	} `json:"command"`
}

// parseHooksProbeOutput reports whether the envelope lists an enabled "entire"
// entry whose source is hooksPath (compared after symlink resolution on both
// sides, since agy reports the path it resolved). Sources of every listed
// entry are returned for diagnostics.
func parseHooksProbeOutput(out []byte, hooksPath string) (loaded bool, sources []string, err error) {
	var env hooksProbeEnvelope
	if err := json.Unmarshal(out, &env); err != nil {
		return false, nil, fmt.Errorf("agy -p /hooks: unexpected output: %w", err)
	}
	wantPath := resolveAgySymlinks(hooksPath)
	for _, h := range env.Command.Data.Hooks {
		sources = append(sources, h.Source)
		if h.Name != "entire" || !h.Enabled {
			continue
		}
		if resolveAgySymlinks(h.Source) == wantPath {
			loaded = true
		}
	}
	return loaded, sources, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
