package agents

import "testing"

// droidTrustDialogPane is a trimmed capture of Droid v0.178.0's interactive
// "Trust this folder?" startup dialog. The pre-selected option renders as
// "> 1. Trust this folder", i.e. it uses the same ">" as the real input box —
// the exact shape that made the old bare-">" WaitFor mistake the dialog for the
// prompt and swallow the first real prompt.
const droidTrustDialogPane = `│ Trust this folder?                                                           │
│                                                                              │
│ /tmp/e2e-repo-4116086897                                                     │
│                                                                              │
│ Droid will read, edit, and run files in this folder, and load any project    │
│ configuration it defines, including hooks and MCP servers that can execute   │
│ commands on your machine.                                                    │
╰──────────────────────────────────────────────────────────────────────────────╯

> 1. Trust this folder
  2. Exit without trusting

Enter to confirm · Esc to exit`

// droidInteractivePromptPane is the real idle REPL after trust is granted: the
// input box with a bare ">" and no dialog chrome. This must NOT be classified
// as a startup dialog.
const droidInteractivePromptPane = ` Auto (High) · allow all commands           claude-haiku-custom (High) [custom]
╭──────────────────────────────────────────────────────────────────────────────╮
│ >                                                                            │
╰──────────────────────────────────────────────────────────────────────────────╯
? for help                                                                TMUX ⧉`

// TestIsDroidStartupDialog_DetectsTrustDialog is the regression guard for the
// Droid v0.178.0 break: the trust dialog must be recognized so StartSession
// keeps sending Enter to confirm it instead of mistaking "> 1. Trust this
// folder" for the interactive prompt and sending the first real prompt into a
// dialog that swallows it.
func TestIsDroidStartupDialog_DetectsTrustDialog(t *testing.T) {
	t.Parallel()
	if !isDroidStartupDialog(droidTrustDialogPane) {
		t.Fatal("trust dialog should be detected as a startup dialog")
	}
}

// TestIsDroidStartupDialog_DetectsByOptionLabel confirms detection keys off the
// "exit without trusting" option too, so a title-text change in a future Droid
// release does not silently re-break the handshake.
func TestIsDroidStartupDialog_DetectsByOptionLabel(t *testing.T) {
	t.Parallel()
	const optionOnly = "> 1. Trust folder\n  2. Exit without trusting"
	if !isDroidStartupDialog(optionOnly) {
		t.Fatal("the trust dialog's option labels alone should be enough to detect it")
	}
}

// TestIsDroidStartupDialog_IgnoresInteractivePrompt ensures the real prompt is
// not classified as a dialog — otherwise StartSession would loop dismissing a
// dialog that isn't there and never hand back a usable session.
func TestIsDroidStartupDialog_IgnoresInteractivePrompt(t *testing.T) {
	t.Parallel()
	if isDroidStartupDialog(droidInteractivePromptPane) {
		t.Fatal("bare interactive prompt should not be classified as a startup dialog")
	}
}
