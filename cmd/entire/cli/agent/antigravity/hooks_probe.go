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

// ErrHooksProbeVersionUnknown is returned when `agy --version` printed
// something semver cannot parse (a build suffix, a banner line). It is
// deliberately distinct from ErrHooksProbeUnsupported: the agy may well be
// newer than the requirement, so "run `agy update`" would be wrong advice.
// The probe is skipped either way — a wrong guess is a real model turn.
var ErrHooksProbeVersionUnknown = errors.New("could not determine the agy version")

// DoctorProbeEnv opts `entire doctor` into running ProbeLoadedHooks. Off by
// default: the probe's zero-quota argument rested on the version gate alone,
// and on Windows 11 with agy 1.2.7 `agy -p "/hooks"` was observed to run a
// full model turn (~12k input tokens) instead of answering locally. Until the
// local-answer behaviour is verified per platform rather than assumed from a
// version number, a default doctor run must not risk the user's quota. The
// probe also only proves agy PARSED hooks.json, not that the command in it can
// run; HooksEntryMatchesHost is the check that catches the failure that
// actually occurs.
const DoctorProbeEnv = "ENTIRE_ANTIGRAVITY_DOCTOR_PROBE"

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
	// agy rejects a relative --add-dir but does not fail the run: it logs the
	// rejection, loads zero hooks and answers for its scratch workspace, which
	// would read here as "hooks not loaded". Refuse to ask the question wrong.
	if !filepath.IsAbs(repoRoot) {
		return probe, fmt.Errorf("agy --add-dir needs an absolute path, got %q", repoRoot)
	}
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
	if err := classifyProbeVersion(probe.Version); err != nil {
		return probe, err
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
	return classifyProbeVersion(version) == nil
}

// classifyProbeVersion returns nil for a version that answers /hooks locally,
// ErrHooksProbeVersionUnknown for one semver cannot parse, and
// ErrHooksProbeUnsupported for one that is too old.
func classifyProbeVersion(version string) error {
	v := strings.TrimSpace(version)
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return fmt.Errorf("%w: agy --version printed %q", ErrHooksProbeVersionUnknown, strings.TrimSpace(version))
	}
	if semver.Compare(v, "v"+MinHooksProbeVersion) < 0 {
		return fmt.Errorf("%w: have %s, need >= %s", ErrHooksProbeUnsupported, strings.TrimSpace(version), MinHooksProbeVersion)
	}
	return nil
}

// hooksProbeEnvelope is the subset of agy's --output-format json envelope the
// probe reads: command.data.hooks[] carries one entry per hooks.json "name"
// key with the file it came from.
// agyProbeStatusSuccess is the status agy reports for a completed command.
const agyProbeStatusSuccess = "SUCCESS"

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
	// A non-success envelope carries no hooks, which is indistinguishable from
	// a genuine empty list once the status is dropped — and the caller's
	// remediation for an empty list is "your hooks are NOT LOADED", the wrong
	// thing to tell someone whose probe failed. An absent status is left alone
	// rather than treated as failure: only a status agy actually reported is
	// evidence about the probe.
	if env.Status != "" && !strings.EqualFold(env.Status, agyProbeStatusSuccess) {
		return false, nil, fmt.Errorf("agy -p /hooks: reported status %q", env.Status)
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
