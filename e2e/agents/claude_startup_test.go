package agents

import "testing"

const claudeWorkspaceTrustPane = `Accessing workspace:
/tmp/e2e-repo-1996556193

Quick safety check: Is this a project you created or one you trust?

❯ No, exit
  Yes, I trust this folder

Enter to confirm · Esc to cancel`

func TestClaudeStartupSelectionIsYes_RejectsWorkspaceTrustDefault(t *testing.T) {
	t.Parallel()

	if claudeStartupSelectionIsYes(claudeWorkspaceTrustPane) {
		t.Fatal("workspace trust dialog defaults to No, so startup must move to Yes before confirming")
	}
}

func TestClaudeStartupSelectionIsYes_AcceptsSelectedYes(t *testing.T) {
	t.Parallel()

	const selectedYes = `  No, exit
❯ Yes, I trust this folder
Enter to confirm · Esc to cancel`
	if !claudeStartupSelectionIsYes(selectedYes) {
		t.Fatal("startup must not move down when a Yes option is already selected")
	}
}

func TestClaudeStartupSelectionIsYes_RejectsRewordedNegative(t *testing.T) {
	t.Parallel()

	const rewordedNegative = `Do you trust the files in this folder?

❯ No, cancel
  Yes, proceed

Enter to confirm · Esc to cancel`
	if claudeStartupSelectionIsYes(rewordedNegative) {
		t.Fatal("a negative option worded other than \"No, exit\" must still not be confirmed")
	}
}

func TestClaudeStartupSelectionIsYes_IgnoresCursorAbovePane(t *testing.T) {
	t.Parallel()

	const cursorAbove = `❯ some earlier prompt line

❯ Yes, I trust this folder
Enter to confirm · Esc to cancel`
	if !claudeStartupSelectionIsYes(cursorAbove) {
		t.Fatal("the dialog's own selection is the last cursor line, not the first")
	}
}

func TestClaudeStartupOffersYes_IgnoresFirstRunDialogWithoutChoice(t *testing.T) {
	t.Parallel()

	const themeSelection = `Choose the text style that looks best with your terminal:

❯ Dark mode
  Light mode

Enter to confirm · Esc to cancel`
	if claudeStartupOffersYes(themeSelection) {
		t.Fatal("a first-run dialog with no affirmative option must be answered with its default")
	}
	if !claudeStartupOffersYes(claudeWorkspaceTrustPane) {
		t.Fatal("the workspace trust dialog does offer an affirmative option")
	}
}
