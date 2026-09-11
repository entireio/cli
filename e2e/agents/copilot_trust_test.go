package agents

import (
	"path/filepath"
	"testing"
)

// trustDialogPane is a trimmed capture of Copilot v1.0.63's interactive
// startup dialog. Note the footer renders the navigation hint lowercase
// ("enter to select") and the selected option uses the same "❯" cursor as the
// input prompt — the exact shape that broke the old exact-case detection.
const trustDialogPane = `╭───────────────────────────────────────────────────────────╮
│ Confirm folder trust                                        │
│                                                             │
│ /tmp/e2e-repo-457367550                                     │
│                                                             │
│ Copilot can read files in this folder and, with your        │
│ permission, edit them or run code and shell commands.       │
│                                                             │
│ Do you trust the files in this folder?                      │
│                                                             │
│ ❯ 1. Yes                                                    │
│   2. Yes, and remember this folder for future sessions      │
│   3. No (Esc)                                               │
│                                                             │
│ ↑/↓ to navigate · enter to select · esc to cancel           │
╰───────────────────────────────────────────────────────────╯`

// interactivePromptPane is the real idle prompt: a bare "❯" with no dialog
// chrome. This must NOT be classified as a startup dialog.
const interactivePromptPane = ` Tip: /app
 /tmp/e2e-repo-457367550 [master%]

❯

 / commands · ? help                                  Claude Haiku 4.5`

// copilotV1081PromptPane is the idle prompt captured from CI after Copilot
// v1.0.81 replaced the bare "❯" marker with a bordered input area.
const copilotV1081PromptPane = `Current   Sessions   Issues   Pull requests   Gists

╭─╮╭─╮
╰─╯╰─╯  Copilot v1.0.81 uses AI.
█ ▘▝ █  Check for mistakes.
 ▔▔▔▔

/tmp/e2e-repo-3026103693 [⎇ master%]
╻▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄
┃
╹▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
← open sidebar · / commands · ? help · tab next tab           Claude Haiku 4.5`

// TestIsStartupDialog_DetectsLowercaseTrustFooter is the regression guard for
// the Copilot v1.0.63 break: the trust dialog must be recognized even though
// its footer is lowercase ("enter to select"), so the StartSession dismissal
// loop keeps sending Enter instead of mistaking the "❯ 1. Yes" cursor for the
// interactive prompt and swallowing the first real prompt.
func TestIsStartupDialog_DetectsLowercaseTrustFooter(t *testing.T) {
	t.Parallel()
	if !isStartupDialog(trustDialogPane) {
		t.Fatal("trust dialog with lowercase footer should be detected as a startup dialog")
	}
}

// TestIsStartupDialog_DetectsByTitle confirms detection keys off the dialog
// title too, so a footer-text change in a future Copilot release does not
// silently re-break the handshake.
func TestIsStartupDialog_DetectsByTitle(t *testing.T) {
	t.Parallel()
	const titleOnly = "│ Confirm folder trust │\n│ ❯ 1. Yes │"
	if !isStartupDialog(titleOnly) {
		t.Fatal("dialog title alone should be enough to detect a startup dialog")
	}
}

// TestIsStartupDialog_IgnoresInteractivePrompt ensures the real prompt is not
// classified as a dialog — otherwise StartSession would loop dismissing a
// dialog that isn't there and never hand back a usable session.
func TestIsStartupDialog_IgnoresInteractivePrompt(t *testing.T) {
	t.Parallel()
	if isStartupDialog(interactivePromptPane) {
		t.Fatal("bare interactive prompt should not be classified as a startup dialog")
	}
}

func TestCopilotPromptPattern_MatchesBareCursorPrompt(t *testing.T) {
	t.Parallel()

	if !copilotPromptReady(interactivePromptPane) {
		t.Fatal("a bare cursor alone on its line is still an idle prompt")
	}
}

func TestCopilotPromptPattern_IgnoresCursorWithTrailingText(t *testing.T) {
	t.Parallel()

	const listItem = `❯ 1. Yes, and remember this folder`
	if copilotPromptReady(listItem) {
		t.Fatal("a cursor followed by option text is a selection, not an idle prompt")
	}
}

func TestCopilotPromptPattern_MatchesV1081InputArea(t *testing.T) {
	t.Parallel()

	if !copilotPromptReady(copilotV1081PromptPane) {
		t.Fatal("Copilot v1.0.81 bordered input area should be recognized as ready")
	}
}

func TestCopilotPromptReady_RejectsSessionRestorePicker(t *testing.T) {
	t.Parallel()

	const restorePicker = `Choose which sessions to restore. Sessions that were open when Copilot stopped are preselected.
┃ ❯ • 1. create a markdown file  Idle  just now
Failed to restore interrupted sessions: Error: Session abc is already in use`
	if copilotPromptReady(restorePicker) {
		t.Fatal("session restore picker should not be recognized as an interactive prompt")
	}
}

func TestResolveGHConfigDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		goos      string
		home      string
		explicit  string
		xdgConfig string
		appData   string
		want      string
	}{
		{
			name:     "explicit override",
			goos:     "darwin",
			home:     "/Users/tester",
			explicit: "/tmp/gh-auth",
			want:     "/tmp/gh-auth",
		},
		{
			name:      "XDG config",
			goos:      "linux",
			home:      "/home/tester",
			xdgConfig: "/tmp/xdg",
			want:      filepath.Join("/tmp/xdg", "gh"),
		},
		{
			name:    "Windows AppData",
			goos:    "windows",
			home:    `C:\Users\tester`,
			appData: `C:\Users\tester\AppData\Roaming`,
			want:    filepath.Join(`C:\Users\tester\AppData\Roaming`, "GitHub CLI"),
		},
		{
			name: "macOS uses gh fallback instead of os.UserConfigDir",
			goos: "darwin",
			home: "/Users/tester",
			want: filepath.Join("/Users/tester", ".config", "gh"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveGHConfigDir(tt.goos, tt.home, tt.explicit, tt.xdgConfig, tt.appData)
			if got != tt.want {
				t.Fatalf("resolveGHConfigDir() = %q, want %q", got, tt.want)
			}
		})
	}
}
