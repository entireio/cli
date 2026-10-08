package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/stretchr/testify/require"
)

// TestCodexCommandHookHash_MatchesCanonicalShape verifies the canonical
// JSON the hash is computed over, against a hand-built reference string.
// Pinning the byte-exact JSON catches drift in canonicalization (key
// sort, omit-when-nil rules, HTML-escape behavior, integer rendering)
// before it silently breaks pre-trust in e2e.
func TestCodexCommandHookHash_MatchesCanonicalShape(t *testing.T) {
	t.Parallel()
	// Reference canonical JSON for: post_tool_use, no matcher, single
	// command handler with timeout=30, async=false, no statusMessage.
	// Keys MUST be alphabetical; matcher and statusMessage MUST be absent.
	// shell-redirect characters (>) MUST NOT be HTML-escaped — Codex's
	// serde_json::to_vec passes them through unchanged.
	const wantCanonical = `{"event_name":"post_tool_use","hooks":[{"async":false,"command":"sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec entire hooks codex post-tool-use'","timeout":30,"type":"command"}]}`
	sum := sha256.Sum256([]byte(wantCanonical))
	wantHash := "sha256:" + hex.EncodeToString(sum[:])

	got := codexCommandHookHash(
		"post_tool_use",
		nil, // no matcher
		`sh -c 'if ! command -v entire >/dev/null 2>&1; then exit 0; fi; exec entire hooks codex post-tool-use'`,
		30,
		false,
		nil, // no statusMessage
	)
	require.Equal(t, wantHash, got)
}

// TestCodexCommandHookHash_OmitsNilOptions confirms that nil matcher and
// nil statusMessage are absent from the canonical JSON — Codex's TOML
// round-trip drops Option::None, and any divergence would produce a
// different hash than what discover_handlers computes at runtime.
func TestCodexCommandHookHash_OmitsNilOptions(t *testing.T) {
	t.Parallel()
	const wantCanonical = `{"event_name":"stop","hooks":[{"async":false,"command":"echo hi","timeout":600,"type":"command"}]}`
	sum := sha256.Sum256([]byte(wantCanonical))
	wantHash := "sha256:" + hex.EncodeToString(sum[:])

	got := codexCommandHookHash("stop", nil, "echo hi", 600, false, nil)
	require.Equal(t, wantHash, got)
}

// TestCodexHookTrustState_GeneratesEntriesForEveryDeclaredHandler
// exercises the full path: read .codex/hooks.json, walk every event in
// Codex's declared order, emit a [hooks.state."<path>:<event>:0:0"] block
// per command handler with the matching trusted_hash.
func TestCodexHookTrustState_GeneratesEntriesForEveryDeclaredHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	codexDir := filepath.Join(dir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))

	hooksJSON := `{
  "hooks": {
    "SessionStart": [
      {"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex session-start", "timeout": 30}]}
    ],
    "Stop": [
      {"matcher": null, "hooks": [{"type": "command", "command": "entire hooks codex stop", "timeout": 30}]}
    ]
  }
}
`
	hooksPath := filepath.Join(codexDir, "hooks.json")
	require.NoError(t, os.WriteFile(hooksPath, []byte(hooksJSON), 0o600))

	state, err := codexHookTrustState(dir)
	require.NoError(t, err)
	require.Contains(t, state, hooksPath+":session_start:0:0")
	require.Contains(t, state, hooksPath+":stop:0:0")
	require.Contains(t, state, "trusted_hash = \"sha256:")
	// Two declared handlers → two state entries.
	require.Equal(t, 2, strings.Count(state, "[hooks.state."))
	require.Equal(t, 2, strings.Count(state, "trusted_hash ="))
}

// TestCodexHookTrustState_CoversEveryInstalledEvent guards the hand-maintained
// event table against the CLI's install set. Codex silently skips hooks with no
// trusted_hash, so an event the table doesn't know is installed but inert for
// the entire e2e suite: the hook never fires, and no test fails to say so —
// they just quietly stop covering it. SessionEnd was exactly that gap.
func TestCodexHookTrustState_CoversEveryInstalledEvent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // InstallHooks falls back to cwd when there's no worktree root

	if _, err := (&codex.CodexAgent{}).InstallHooks(context.Background(), false); err != nil {
		t.Fatalf("InstallHooks() error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ".codex", "hooks.json"))
	require.NoError(t, err)

	// Read the events generically so the JSON shape remains the thing under test.
	var installed struct {
		Hooks map[string][]codexHookGroup `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(raw, &installed))

	state, err := codexHookTrustState(dir)
	require.NoError(t, err)

	known := make(map[string]string, len(codex.HookEventSpecs()))
	for _, ev := range codex.HookEventSpecs() {
		known[ev.Event] = ev.Label
	}

	for event, groups := range installed.Hooks {
		for _, group := range groups {
			for _, handler := range group.Hooks {
				if !strings.Contains(handler.Command, "entire hooks codex") {
					continue
				}
				label, ok := known[event]
				require.Truef(t, ok, "Codex event metadata has no entry for %s: the CLI installs it, so e2e leaves it untrusted and Codex never fires it", event)
				require.Containsf(t, state, label+":", "no trusted_hash generated for %s", event)

				// The hash is computed over the configured timeout, while Codex
				// caps SessionEnd handlers at SESSION_END_MAX_TIMEOUT_SEC. If it
				// applies that clamp before hashing, anything above the ceiling
				// would hash differently here and leave session_end untrusted —
				// silently, since an untrusted hook simply never fires. Installing
				// exactly the ceiling keeps both orderings identical, so pin it.
				if event == "SessionEnd" {
					require.NotNilf(t, handler.Timeout, "SessionEnd handler has no timeout; expected the %ds ceiling", codex.SessionEndTimeoutSec)
					require.EqualValuesf(t, codex.SessionEndTimeoutSec, *handler.Timeout,
						"SessionEnd installed above Codex's clamp ceiling: the pre-trust hash here may no longer match what Codex computes, which would leave the hook untrusted and silently dead in e2e")
				}
			}
		}
	}
}

// TestCodexHookTrustState_MissingFileIsNoop returns empty trust state
// (and no error) when there's no hooks.json yet — the e2e seed runs
// before some tests' enable step.
func TestCodexHookTrustState_MissingFileIsNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state, err := codexHookTrustState(dir)
	require.NoError(t, err)
	require.Empty(t, state)
}
