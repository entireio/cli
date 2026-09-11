package cli

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
)

const (
	codexHookStateInactiveWorktreePath      = "inactive_in_worktree"
	codexHookStateUnavailableWorktree       = "unavailable_worktree"
	codexHookStateUnavailableDiscovered     = "unavailable_discovered"
	codexHookStateMalformedWorktree         = "malformed_worktree"
	codexHookStateMalformedDiscovered       = "malformed_discovered"
	codexHookStateProjectLayerMissing       = "project_layer_missing"
	codexHookStateDiscoveryUnresolved       = "discovery_unresolved"
	codexHookStateWorktreePathNotDiscovered = "worktree_path_not_discovered"
	codexHookStateOutdated                  = "out_of_date"
	codexHookStateTrustReview               = "trust_review_needed"
)

type codexHookIssue struct {
	State            string
	WorktreePath     string
	DiscoveredPath   string
	ProjectLayerPath string
	Error            string
	MissingHooks     []string
	MissingApprovals []string
}

func inspectCodexHookIssue(ctx context.Context) *codexHookIssue {
	return codexHookIssueFromDiagnostics(codex.InspectHookDiagnostics(ctx))
}

func inspectCodexSessionStartHookIssue(ctx context.Context) *codexHookIssue {
	return codexHookIssueFromDiagnostics(codex.InspectHookDiagnosticsLightweight(ctx))
}

func codexHookIssueFromDiagnostics(diagnostics codex.HookDiagnostics) *codexHookIssue {
	issue := &codexHookIssue{
		WorktreePath:   diagnostics.WorktreeHooks.Path(),
		DiscoveredPath: diagnostics.Discovery.DiscoveredHooks.Path(),
	}
	if issue.WorktreePath != "" {
		issue.ProjectLayerPath = filepath.Dir(issue.WorktreePath)
	}

	if diagnostics.Discovery.State != codex.HookDiscoveryResolved {
		if diagnostics.Worktree.State != codex.HookFileEntire &&
			diagnostics.Worktree.State != codex.HookFileMalformed &&
			diagnostics.Worktree.State != codex.HookFileUnavailable &&
			!diagnostics.Discovery.ProjectLayerExists() {
			return nil
		}
		issue.State = codexHookStateDiscoveryUnresolved
		if diagnostics.Discovery.Diagnostic != nil {
			issue.Error = diagnostics.Discovery.Diagnostic.Error()
		}
		return issue
	}

	if diagnostics.Discovered.State == codex.HookFileMalformed {
		issue.State = codexHookStateMalformedDiscovered
		if diagnostics.Discovered.Err != nil {
			issue.Error = diagnostics.Discovered.Err.Error()
		}
		return issue
	}
	if diagnostics.Discovered.State == codex.HookFileUnavailable {
		issue.State = codexHookStateUnavailableDiscovered
		if diagnostics.Discovered.Err != nil {
			issue.Error = diagnostics.Discovered.Err.Error()
		}
		return issue
	}

	if diagnostics.Discovered.State == codex.HookFileEntire && !diagnostics.Discovery.ProjectLayerExists() {
		issue.State = codexHookStateProjectLayerMissing
		return issue
	}

	if diagnostics.Discovered.State != codex.HookFileEntire {
		if diagnostics.PathsDiffer() && diagnostics.Worktree.State == codex.HookFileEntire {
			issue.State = codexHookStateInactiveWorktreePath
			return issue
		}
		return localCodexHookIssue(issue, diagnostics)
	}

	if !diagnostics.Discovered.Current {
		issue.State = codexHookStateOutdated
		issue.MissingHooks = diagnostics.Discovered.Missing
		return issue
	}
	if len(diagnostics.Trust.Gaps) > 0 {
		issue.State = codexHookStateTrustReview
		issue.MissingApprovals = diagnostics.Trust.Gaps
		return issue
	}
	if diagnostics.PathsDiffer() && diagnostics.Worktree.State == codex.HookFileEntire {
		issue.State = codexHookStateWorktreePathNotDiscovered
		return issue
	}

	return localCodexHookIssue(issue, diagnostics)
}

func localCodexHookIssue(issue *codexHookIssue, diagnostics codex.HookDiagnostics) *codexHookIssue {
	switch diagnostics.Worktree.State {
	case codex.HookFileMalformed:
		issue.State = codexHookStateMalformedWorktree
		if diagnostics.Worktree.Err != nil {
			issue.Error = diagnostics.Worktree.Err.Error()
		}
		return issue
	case codex.HookFileUnavailable:
		issue.State = codexHookStateUnavailableWorktree
		if diagnostics.Worktree.Err != nil {
			issue.Error = diagnostics.Worktree.Err.Error()
		}
		return issue
	case codex.HookFileAbsent, codex.HookFileUserOnly, codex.HookFileEntire:
		return nil
	}
	return nil
}

func codexStatusWarning(issue *codexHookIssue) string {
	if issue == nil {
		return ""
	}
	switch issue.State {
	case codexHookStateInactiveWorktreePath:
		return "Codex hooks are not active in this worktree · run 'entire doctor'"
	case codexHookStateMalformedWorktree:
		return "Current-worktree Codex hooks are malformed · run 'entire doctor'"
	case codexHookStateMalformedDiscovered:
		return "Codex-discovered hooks are malformed · run 'entire doctor'"
	case codexHookStateUnavailableWorktree:
		return "Current-worktree Codex hooks are unavailable · run 'entire doctor'"
	case codexHookStateUnavailableDiscovered:
		return "Codex-discovered hooks are unavailable · run 'entire doctor'"
	case codexHookStateProjectLayerMissing:
		return "Codex project layer missing in this worktree · run 'entire doctor'"
	case codexHookStateDiscoveryUnresolved:
		return "Codex hook discovery unresolved · run 'entire doctor'"
	case codexHookStateWorktreePathNotDiscovered:
		return ""
	case codexHookStateOutdated:
		return "Codex-discovered hooks are out of date · run 'entire doctor'"
	case codexHookStateTrustReview:
		return fmt.Sprintf("%d Codex hook(s) need approval · open /hooks", len(issue.MissingApprovals))
	default:
		return ""
	}
}

func codexSessionStartWarning(issue *codexHookIssue) string {
	if issue == nil {
		return ""
	}
	switch issue.State {
	case codexHookStateInactiveWorktreePath:
		return "Entire hooks in this worktree are not active; Codex discovers another checkout. Run 'entire doctor'."
	case codexHookStateMalformedWorktree:
		return "This worktree's Codex hooks configuration is malformed. Run 'entire doctor'."
	case codexHookStateMalformedDiscovered:
		return "Codex-discovered hooks are malformed. Run 'entire doctor'."
	case codexHookStateUnavailableWorktree:
		return "This worktree's Codex hooks could not be inspected. Run 'entire doctor'."
	case codexHookStateUnavailableDiscovered:
		return "Codex-discovered hooks could not be inspected. Run 'entire doctor'."
	case codexHookStateProjectLayerMissing:
		return "This worktree is missing the Codex project layer. Run 'entire doctor'."
	case codexHookStateDiscoveryUnresolved:
		return "Codex hook discovery is unresolved. Run 'entire doctor'."
	case codexHookStateWorktreePathNotDiscovered:
		return ""
	case codexHookStateTrustReview:
		return fmt.Sprintf("%d Codex hook(s) await approval. Open /hooks.", len(issue.MissingApprovals))
	default:
		return ""
	}
}
