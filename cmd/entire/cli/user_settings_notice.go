package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// userSettingsNoticeDir holds, in the user cache directory, one marker per
// agent session that has seen the unreadable-user-settings banner.
const userSettingsNoticeDir = "session-notices"

// unreadableUserSettingsText says that the user settings file cannot be read:
// everything in it, global_enable included, is then ignored, and nothing else
// would say so.
func unreadableUserSettingsText(err error) string {
	return "Entire could not read your user settings, so global_enable and repos entries there are ignored (" +
		tuiutil.SanitizeDisplayText(err.Error()) + "). Run 'entire doctor' for details."
}

// unreadableUserSettingsBannerSuffix is the line the session-start banner of a
// set-up repository ends with while the user settings file cannot be read.
func unreadableUserSettingsBannerSuffix(ctx context.Context, agentName types.AgentName) string {
	err := settings.UserSettingsReadError(ctx)
	if err == nil {
		return ""
	}
	return bannerSuffix(agentName, unreadableUserSettingsText(err))
}

// reportUnreadableUserSettings runs at session start, before the set-up gate.
// Agents with a session banner see the notice there, once per session: in a
// set-up repository the lifecycle banner carries it (see
// unreadableUserSettingsBannerSuffix); in one that now reads as not set up,
// where no lifecycle runs, it is the whole banner. Other agents get stderr.
func reportUnreadableUserSettings(cmd *cobra.Command, agentName types.AgentName, setUp bool) {
	ctx := cmd.Context()
	settingsErr := settings.UserSettingsReadError(ctx)
	if settingsErr == nil {
		return
	}
	var writer agent.HookResponseWriter
	if ag, err := agent.Get(agentName); err == nil {
		writer, _ = agent.AsHookResponseWriter(ag)
	}
	if writer == nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "[entire] %s\n", unreadableUserSettingsText(settingsErr))
		return
	}
	if setUp || !claimUnreadableUserSettingsNotice(cmd, agentName) {
		return
	}
	if err := writer.WriteHookResponse(unreadableUserSettingsText(settingsErr)); err != nil {
		logging.Debug(ctx, "could not write the unreadable user settings banner", slog.String("error", err.Error()))
	}
}

// claimUnreadableUserSettingsNotice reports whether this session has not had
// the standalone notice yet. The marker lives in the user cache directory:
// the repository is not set up, so nothing is written there. When the session
// cannot be identified or the marker cannot be made, the notice shows:
// repeating it beats never showing it.
func claimUnreadableUserSettingsNotice(cmd *cobra.Command, agentName types.AgentName) bool {
	ctx := cmd.Context()
	sessionID := sessionIDForNotice(cmd, agentName)
	if sessionID == "" {
		return true
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return true
	}
	if err := osroot.MkdirAllNoSymlink(root, userSettingsNoticeDir, 0o700); err != nil {
		return true
	}
	claimed, err := osroot.ClaimOnceMarker(root, userSettingsNoticeDir+"/user-settings-"+sessionID, "")
	if err != nil {
		logging.Debug(ctx, "could not create the user settings notice marker", slog.String("error", err.Error()))
		return true
	}
	return claimed
}

// sessionIDForNotice parses the hook payload for its session ID. The hook
// returns right after, so consuming stdin here costs nothing.
func sessionIDForNotice(cmd *cobra.Command, agentName types.AgentName) string {
	ag, err := agent.Get(agentName)
	if err != nil {
		return ""
	}
	handler, ok := agent.AsHookSupport(ag)
	if !ok {
		return ""
	}
	event, err := handler.ParseHookEvent(cmd.Context(), hookNameSessionStart, cmd.InOrStdin())
	if err != nil || event == nil || validation.ValidateSessionID(event.SessionID) != nil {
		return ""
	}
	return event.SessionID
}
