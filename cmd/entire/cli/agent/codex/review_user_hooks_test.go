package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
)

const userHooksFixture = `{"hooks":{
  "Stop":[
    {"matcher":"x","hooks":[{"type":"command","command":"echo first"}]},
    {"hooks":[{"type":"command","command":"echo mine"},{"type":"command","command":"entire hooks codex stop"}]}
  ],
  "SessionStart":[{"hooks":[{"type":"command","command":"entire hooks codex session-start"}]}],
  "PreToolUse":[{"hooks":[{"type":"command","command":"echo pre"}]}]
}}`

const userConfigTOMLFixture = `model = "x"

[[hooks.Stop]]
hooks = [{ type = "command", command = "echo toml-mine" }, { type = "command", command = "entire hooks codex stop" }]

[hooks.state."/elsewhere/hooks.json:stop:0:0"]
enabled = false
`

// Only Entire's handlers are keyed, at their exact group and handler
// positions; a drifted index would disable the user's own hook instead.
func TestUserEntireHookKeys_PositionsInHooksJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), HooksFileName)
	if err := os.WriteFile(path, []byte(userHooksFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := userEntireHookKeys(path, parseHooksJSONGroups)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{path + ":session_start:0:0", path + ":stop:1:1"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestUserEntireHookKeys_ConfigTOML(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(userConfigTOMLFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := userEntireHookKeys(path, parseConfigTOMLGroups)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{path + ":stop:0:1"}; !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestUserEntireHookKeys_MissingFile(t *testing.T) {
	t.Parallel()
	got, err := userEntireHookKeys(filepath.Join(t.TempDir(), HooksFileName), parseHooksJSONGroups)
	if err != nil || got != nil {
		t.Errorf("got %v, %v; want nil, nil", got, err)
	}
}

func TestHookStateDisableOverride(t *testing.T) {
	t.Parallel()
	if got := hookStateDisableOverride(nil); got != nil {
		t.Errorf("no keys: got %v", got)
	}
	got := hookStateDisableOverride([]string{"/h/hooks.json:stop:0:1", "/h/config.toml:stop:0:0"})
	want := []string{"-c", `hooks.state={"/h/hooks.json:stop:0:1"={enabled=false},"/h/config.toml:stop:0:0"={enabled=false}}`}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Codex keys a symlinked CODEX_HOME by its resolved path, but the default
// ~/.codex as joined from $HOME.
func TestCodexHomeForHookKeys(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := codexHomeForHookKeys(link, true)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(target) //nolint:errcheck // a temp dir always resolves
	if got != want {
		t.Errorf("override home = %q, want %q", got, want)
	}
	if got, err := codexHomeForHookKeys(link, false); err != nil || got != link {
		t.Errorf("default home = %q, %v; want %q as given", got, err, link)
	}
}

// An isolated run disables the user's own Entire hooks and nothing else.
func TestPrepareCodexReviewConfig_DisablesUserEntireHooks(t *testing.T) {
	home := codexReviewRepo(t)
	if err := os.WriteFile(filepath.Join(home, HooksFileName), []byte(userHooksFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _, err := prepareCodexReviewConfig(t.Context(), reviewtypes.RunConfig{AgentConfig: &reviewtypes.AgentConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(home) //nolint:errcheck // a temp dir always resolves
	hooksPath := filepath.Join(resolved, HooksFileName)
	var state string
	for _, arg := range got.ExtraArgs {
		if strings.HasPrefix(arg, "hooks.state=") {
			if state != "" {
				t.Fatalf("more than one hooks.state override: %v", got.ExtraArgs)
			}
			state = arg
		}
	}
	for _, key := range []string{hooksPath + ":stop:1:1", hooksPath + ":session_start:0:0"} {
		if !strings.Contains(state, `"`+key+`"={enabled=false}`) {
			t.Errorf("override %q missing %s", state, key)
		}
	}
	if strings.Contains(state, ":stop:1:0") || strings.Contains(state, ":stop:0:0") {
		t.Errorf("override disables a user hook: %q", state)
	}
}
