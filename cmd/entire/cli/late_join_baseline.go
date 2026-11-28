package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// lateJoinAnchor returns when the previous turn of a session started, if this
// TurnStart is the first one this repository sees for a session that already
// ran turns elsewhere. Such a session may have written untracked files here
// during that turn; they belong to it, not to the baseline. Any failure
// returns ok=false, keeping the full baseline.
func lateJoinAnchor(ctx context.Context, ag agent.Agent, sessionID, sessionRef string) (time.Time, bool) {
	reader, ok := agent.AsPreviousTurnStartReader(ag)
	if !ok || sessionRef == "" {
		return time.Time{}, false
	}
	if state, err := strategy.LoadSessionState(ctx, sessionID); err != nil || state != nil {
		return time.Time{}, false
	}
	start, ok, err := reader.PreviousTurnStart(sessionRef)
	if err != nil || !ok {
		return time.Time{}, false
	}
	return start, true
}

// dropFilesSince removes from the baseline the untracked files modified at or
// after anchor, so turn end reports them as new. Files without stats are kept.
func dropFilesSince(files []string, stats map[string]UntrackedFileStat, anchor time.Time) ([]string, map[string]UntrackedFileStat, int) {
	cutoff := anchor.UnixNano()
	kept := make([]string, 0, len(files))
	dropped := 0
	for _, f := range files {
		if st, ok := stats[f]; ok && st.ModTime >= cutoff {
			delete(stats, f)
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	return kept, stats, dropped
}

// trimLateJoinBaseline applies dropFilesSince when lateJoinAnchor finds an
// anchor. A nil files slice (scan skipped) is returned unchanged.
func trimLateJoinBaseline(ctx context.Context, ag agent.Agent, sessionID, sessionRef string, files []string, stats map[string]UntrackedFileStat) ([]string, map[string]UntrackedFileStat) {
	if len(stats) == 0 {
		return files, stats
	}
	anchor, ok := lateJoinAnchor(ctx, ag, sessionID, sessionRef)
	if !ok {
		return files, stats
	}
	files, stats, dropped := dropFilesSince(files, stats, anchor)
	logging.Debug(logging.WithComponent(ctx, "state"), "late-joined session: excluded files written since its previous turn from baseline",
		slog.Int("files", dropped))
	return files, stats
}
