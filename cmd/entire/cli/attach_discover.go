package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/stringutil"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// attachDiscoverPickerCancel is the sentinel option value for the native
// session picker's Cancel entry.
const attachDiscoverPickerCancel = "cancel"

// runAttachDiscoverSessionID resolves the session ID to attach when the user
// ran `entire session attach --agent <name>` with no session ID
// (entireio/cli#1992). It lists agentName's own untracked native sessions
// scoped to the current worktree and either returns the one the user picked
// (interactive terminal) or prints a plain list and returns "" (non-TTY, or
// nothing to attach): callers should treat a "", nil return as "already
// handled, stop without error" — the explanatory message has already been
// printed.
func runAttachDiscoverSessionID(cmd *cobra.Command, agentName types.AgentName) (string, error) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	errW := cmd.ErrOrStderr()

	ag, err := agent.Get(agentName)
	if err != nil {
		return "", fmt.Errorf("agent %q not available: %w", agentName, err)
	}
	lister, ok := agent.AsNativeSessionLister(ag)
	if !ok {
		fmt.Fprintf(errW, "Agent %q does not support session discovery; a session ID is required.\n\n", agentName)
		if helpErr := cmd.Help(); helpErr != nil {
			return "", fmt.Errorf("print attach usage: %w", helpErr)
		}
		return "", nil
	}

	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to resolve worktree root: %w", err)
	}

	entries, err := lister.ListNativeSessions(ctx, worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("failed to list %s sessions: %w", agentName, err)
	}

	entries = filterTrackedNativeSessions(ctx, entries)
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].UpdatedAt.After(entries[j].UpdatedAt)
	})

	if len(entries) == 0 {
		fmt.Fprintf(w, "No untracked %s sessions found for this worktree.\n", agentName)
		return "", nil
	}

	// Non-interactive callers (agents, CI, piped output) must still be able to
	// see the full list and act on it — per this repo's Agent-Safe CLI
	// Fallbacks convention, a picker can never be the only way to reach this
	// data. Auto-selecting when only one session is found is deliberately not
	// done here either: an unattended pick could attach unrelated work.
	if !interactive.CanPromptInteractively() {
		printNativeSessionList(w, agentName, entries)
		return "", nil
	}

	return pickNativeSession(ctx, w, agentName, entries)
}

// filterTrackedNativeSessions drops sessions Entire already has state for,
// leaving only ones genuinely untracked. A session state store that fails to
// open is not treated as "nothing is tracked" — showing every native session
// (including ones Entire already knows about) is the safer failure than
// hiding the whole picker, and re-attaching a tracked session is itself
// handled safely by runAttach (it reports the existing checkpoint rather than
// duplicating it).
func filterTrackedNativeSessions(ctx context.Context, entries []agent.NativeSessionInfo) []agent.NativeSessionInfo {
	store, err := session.NewStateStore(ctx)
	if err != nil {
		logging.Debug(ctx, "attach discover: could not open session state store; showing all native sessions", "error", err)
		return entries
	}
	untracked := make([]agent.NativeSessionInfo, 0, len(entries))
	for _, e := range entries {
		state, loadErr := store.Load(ctx, e.SessionID)
		if loadErr == nil && state != nil {
			continue
		}
		untracked = append(untracked, e)
	}
	return untracked
}

// printNativeSessionList is the non-interactive fallback: a plain list
// carrying every field needed to attach directly, per the Agent-Safe CLI
// Fallbacks convention (no TUI-only output).
func printNativeSessionList(w io.Writer, agentName types.AgentName, entries []agent.NativeSessionInfo) {
	fmt.Fprintf(w, "Untracked %s sessions for this worktree:\n\n", agentName)
	for _, e := range entries {
		fmt.Fprintf(w, "  %s  %s\n", e.SessionID, nativeSessionSummary(e))
	}
	fmt.Fprintf(w, "\nAttach one directly:\n  entire session attach <session-id> --agent %s\n", agentName)
}

// pickNativeSession shows an interactive picker of untracked native sessions,
// matching the shape of `session resume`'s no-arg picker (resume_picker.go):
// one huh.Select option per entry plus Cancel, keyed by index so the label can
// carry arbitrary display text.
func pickNativeSession(ctx context.Context, w io.Writer, agentName types.AgentName, entries []agent.NativeSessionInfo) (string, error) {
	options := make([]huh.Option[string], 0, len(entries)+1)
	for i, e := range entries {
		options = append(options, huh.NewOption(fmt.Sprintf("%s · %s", e.SessionID, nativeSessionSummary(e)), strconv.Itoa(i)))
	}
	options = append(options, huh.NewOption("Cancel", attachDiscoverPickerCancel))

	var selected string
	form := NewAccessibleForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(fmt.Sprintf("Attach an untracked %s session", agentName)).
				Description("Lists sessions from the agent's own store that Entire hasn't recorded yet, scoped to this worktree.").
				Options(options...).
				Value(&selected),
		),
	)
	if err := form.RunWithContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			return "", nil
		}
		return "", fmt.Errorf("selection failed: %w", err)
	}

	if selected == attachDiscoverPickerCancel || selected == "" {
		fmt.Fprintln(w, "Attach cancelled.")
		return "", nil
	}

	idx, convErr := strconv.Atoi(selected)
	if convErr != nil || idx < 0 || idx >= len(entries) {
		return "", fmt.Errorf("invalid selection %q", selected)
	}
	return entries[idx].SessionID, nil
}

// nativeSessionSummary renders the display line shared by both the
// non-interactive list and the interactive picker's option labels: title,
// last-updated time, and directory.
func nativeSessionSummary(e agent.NativeSessionInfo) string {
	title := strings.TrimSpace(e.Title)
	if title == "" {
		title = "(untitled)"
	} else {
		title = stringutil.TruncateRunes(stringutil.CollapseWhitespace(title), 50, "...")
	}
	when := "unknown time"
	if !e.UpdatedAt.IsZero() {
		when = timeAgo(e.UpdatedAt)
	}
	dir := e.Directory
	if dir == "" {
		dir = "(unknown directory)"
	}
	return fmt.Sprintf("\"%s\" · updated %s · %s", title, when, dir)
}
