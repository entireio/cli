package strategy

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// GitHookIntegrationMode identifies who owns the durable Git hook integration.
type GitHookIntegrationMode string

const (
	GitHookIntegrationNative   GitHookIntegrationMode = "native"
	GitHookIntegrationLefthook GitHookIntegrationMode = "lefthook"
)

// GitHookIntegrationState describes whether the selected integration currently
// delivers every Entire Git hook and can be inspected safely.
type GitHookIntegrationState string

const (
	GitHookIntegrationCurrent  GitHookIntegrationState = "current"
	GitHookIntegrationDegraded GitHookIntegrationState = "degraded"
	GitHookIntegrationOutdated GitHookIntegrationState = "outdated"
	GitHookIntegrationAbsent   GitHookIntegrationState = "absent"
	GitHookIntegrationError    GitHookIntegrationState = "error"
)

// GitHookIntegrationHealth is the shared, user-facing health contract for Git
// hook installation. ReasonCode is stable for machine comparisons; Reason is
// explanatory copy and may evolve independently.
type GitHookIntegrationHealth struct {
	Mode       GitHookIntegrationMode  `json:"mode"`
	State      GitHookIntegrationState `json:"state"`
	Manager    string                  `json:"manager,omitempty"`
	ReasonCode string                  `json:"reason_code,omitempty"`
	Reason     string                  `json:"reason,omitempty"`
}

const (
	nativeHooksAbsentReasonCode   = "native_hooks_absent"
	nativeHooksOutdatedReasonCode = "native_hooks_outdated"
	nativeHooksErrorReasonCode    = "native_hooks_inspection_error"
)

// CheckGitHookIntegration reports the effective Git hook integration health for
// the current repository.
func CheckGitHookIntegration(ctx context.Context) GitHookIntegrationHealth {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nativeHookInspectionError(err)
	}

	return checkGitHookIntegrationInDir(ctx, repoRoot)
}

func checkGitHookIntegrationInDir(ctx context.Context, repoRoot string) GitHookIntegrationHealth {
	managers, detectionErr := detectHookManagersForIntegration(repoRoot)
	if detectionErr != nil {
		var managerErr *hookManagerDetectionError
		if errors.As(detectionErr, &managerErr) {
			mode := GitHookIntegrationNative
			if managerErr.manager.IntegrationKind == hookManagerIntegrationLefthook {
				mode = GitHookIntegrationLefthook
			}
			return GitHookIntegrationHealth{
				Mode:       mode,
				State:      GitHookIntegrationError,
				Manager:    managerErr.manager.Name,
				ReasonCode: "hook_manager_detection_error",
				Reason:     fmt.Sprintf("Could not detect hook-manager ownership safely: %v", detectionErr),
			}
		}
		return nativeHookInspectionError(detectionErr)
	}
	manager, selected, selectionErr := selectLefthookIntegrationManager(managers)
	if selectionErr != nil {
		nativeState, nativeErr := inspectNativeHookIntegration(ctx, repoRoot)
		if nativeErr == nil && nativeState == GitHooksCurrent {
			return GitHookIntegrationHealth{
				Mode:       GitHookIntegrationNative,
				State:      GitHookIntegrationDegraded,
				ReasonCode: "hook_manager_ambiguous",
				Reason:     fmt.Sprintf("Entire's native Git hooks work, but hook-manager ownership is ambiguous: %v", selectionErr),
			}
		}
		return GitHookIntegrationHealth{
			Mode:       GitHookIntegrationNative,
			State:      GitHookIntegrationError,
			ReasonCode: "hook_manager_ambiguous_no_native",
			Reason:     fmt.Sprintf("Hook-manager ownership is ambiguous and no working native integration is available: %v", selectionErr),
		}
	}
	if selected {
		root, err := worktreedir.OpenAt(repoRoot)
		if err != nil {
			return lefthookInspectionError(manager, err)
		}
		if err := validateLefthookMainConfig(root, manager); err != nil {
			return lefthookInspectionError(manager, err)
		}
		configCtx := settings.WithWorktreeRoot(ctx, repoRoot)
		cmdPrefix, err := hookCmdPrefix(hookSettingsFromConfig(configCtx))
		if err != nil {
			return lefthookInspectionError(manager, err)
		}
		current, err := inspectLefthookArtifacts(root, cmdPrefix)
		if err != nil {
			return lefthookInspectionError(manager, err)
		}
		if current {
			activeCurrent, activeErr := inspectActiveHookDelivery(ctx, repoRoot)
			if activeErr != nil {
				return lefthookInspectionError(manager, activeErr)
			}
			if !activeCurrent {
				return GitHookIntegrationHealth{
					Mode:       GitHookIntegrationLefthook,
					State:      GitHookIntegrationOutdated,
					Manager:    manager.Name,
					ReasonCode: "lefthook_active_hooks_outdated",
					Reason:     "Entire's Lefthook artifacts are current, but one or more active Git hooks do not invoke Entire.",
				}
			}
			return GitHookIntegrationHealth{
				Mode:    GitHookIntegrationLefthook,
				State:   GitHookIntegrationCurrent,
				Manager: manager.Name,
			}
		}
		nativeState, nativeErr := inspectNativeHookIntegration(ctx, repoRoot)
		if nativeErr == nil && nativeState == GitHooksCurrent {
			return GitHookIntegrationHealth{
				Mode:       GitHookIntegrationLefthook,
				State:      GitHookIntegrationDegraded,
				Manager:    manager.Name,
				ReasonCode: "lefthook_native_bridge",
				Reason:     "Entire's native Git hooks are working while the Lefthook integration awaits migration.",
			}
		}
		return GitHookIntegrationHealth{
			Mode:       GitHookIntegrationLefthook,
			State:      GitHookIntegrationOutdated,
			Manager:    manager.Name,
			ReasonCode: "lefthook_artifacts_outdated",
			Reason:     "Entire's Lefthook artifacts are missing or outdated.",
		}
	}

	nativeState, err := inspectNativeHookIntegration(ctx, repoRoot)
	if err != nil {
		return nativeHookInspectionError(err)
	}

	switch nativeState {
	case GitHooksCurrent:
		return GitHookIntegrationHealth{
			Mode:  GitHookIntegrationNative,
			State: GitHookIntegrationCurrent,
		}
	case GitHooksOutdated:
		return GitHookIntegrationHealth{
			Mode:       GitHookIntegrationNative,
			State:      GitHookIntegrationOutdated,
			ReasonCode: nativeHooksOutdatedReasonCode,
			Reason:     "Entire Git hooks were installed by an older CLI version.",
		}
	case GitHooksAbsent:
		return GitHookIntegrationHealth{
			Mode:       GitHookIntegrationNative,
			State:      GitHookIntegrationAbsent,
			ReasonCode: nativeHooksAbsentReasonCode,
			Reason:     "Entire Git hooks are not installed.",
		}
	}
	return nativeHookInspectionError(fmt.Errorf("unknown native Git hook state %d", nativeState))
}

func inspectNativeHookIntegration(ctx context.Context, repoRoot string) (GitHookState, error) {
	hooksDir, err := getHooksDirInPath(ctx, repoRoot)
	if err != nil {
		return GitHooksAbsent, err
	}
	return inspectGitHookStateInHooksDir(hooksDir)
}

func lefthookInspectionError(manager hookManager, err error) GitHookIntegrationHealth {
	return GitHookIntegrationHealth{
		Mode:       GitHookIntegrationLefthook,
		State:      GitHookIntegrationError,
		Manager:    manager.Name,
		ReasonCode: "lefthook_inspection_error",
		Reason:     fmt.Sprintf("Could not inspect Entire's Lefthook integration: %v", err),
	}
}

func nativeHookInspectionError(err error) GitHookIntegrationHealth {
	return GitHookIntegrationHealth{
		Mode:       GitHookIntegrationNative,
		State:      GitHookIntegrationError,
		ReasonCode: nativeHooksErrorReasonCode,
		Reason:     fmt.Sprintf("Could not inspect Entire Git hooks: %v", err),
	}
}
