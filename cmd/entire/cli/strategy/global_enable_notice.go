package strategy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
)

// globalEnablePushNoticeName marks, in the git common dir, that this clone has
// shown the global-enable push notice. The common dir, so it is once per clone
// rather than once per worktree.
const globalEnablePushNoticeName = "entire-global-enable-push-notice"

// noticeGlobalEnablePush tells the developer, the first time a push from this
// clone carries session checkpoints, that the repository is on only through
// global enable and how to stop it. Callers invoke it only after delivery.
//
// It blocks nothing and never fails the push: global enable was agreed to at
// `entire enable --global`, and this is the reminder of where that applies.
func noticeGlobalEnablePush(ctx context.Context, ps pushSettings) {
	if !ps.enabledOnlyByGlobal || !claimGlobalEnablePushNotice(ctx) {
		return
	}
	fmt.Fprintf(stderrWriter,
		"[entire] Entire pushed session checkpoints to %s. This repository is enabled through global enable, not `entire enable`; run 'entire disable' here to stop.\n",
		globalEnablePushDestination(ps))
	logging.Info(ctx, "global enable: first checkpoint push from this clone")
}

// claimGlobalEnablePushNotice creates the marker, reporting whether this call
// created it. A marker it cannot create for any reason but "already there"
// still lets the notice print: repeating it is better than never showing it.
func claimGlobalEnablePushNotice(ctx context.Context) bool {
	root, err := gitdir.Open(ctx)
	if err != nil {
		logging.Debug(ctx, "global enable: cannot open git dir for the push notice marker",
			slog.String("error", err.Error()))
		return true
	}
	f, err := osroot.OpenFileNoFollow(root, globalEnablePushNoticeName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false
	}
	if err != nil {
		logging.Debug(ctx, "global enable: cannot create the push notice marker",
			slog.String("error", err.Error()))
		return true
	}
	_ = f.Close()
	return true
}

// globalEnablePushDestination names where checkpoints went without ever
// printing a URL, which can carry credentials: a remote name is shown, a URL
// or path pushed to directly is not.
func globalEnablePushDestination(ps pushSettings) string {
	if ps.hasCheckpointURL() {
		return "your configured checkpoint remote"
	}
	if remote.IsURL(ps.remote) || strings.ContainsAny(ps.remote, `:/\`) {
		return "this push's remote"
	}
	return strconv.Quote(tuiutil.SanitizeDisplayText(ps.remote))
}
