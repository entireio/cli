package agents

import "testing"

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
