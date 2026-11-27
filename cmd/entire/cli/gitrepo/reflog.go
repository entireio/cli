package gitrepo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// RefPushedSince returns the commits ref was set to by `git push` (reflog
// entries "update by push") at or after since, newest first. A ref with no
// reflog returns none. It reads the reflog through the git CLI, so reftable
// repositories work, and reads only refs, so it is safe on agent hook paths.
func RefPushedSince(ctx context.Context, worktreeRoot, ref string, since time.Time) ([]string, error) {
	cmd := worktreeGitCommand(ctx, worktreeRoot, "reflog", "show", "--date=unix", "--format=%H%x09%gd%x09%gs", "-n", "50", ref, "--")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read reflog of %s: %w", ref, err)
	}
	return parsePushReflog(string(out), since), nil
}

// parsePushReflog keeps the "update by push" entries at or after since from
// `git reflog show --date=unix --format=%H%x09%gd%x09%gs` output, whose %gd
// reads "<ref>@{<unix seconds>}".
func parsePushReflog(out string, since time.Time) []string {
	var shas []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 || !strings.HasPrefix(fields[2], "update by push") {
			continue
		}
		open, closing := strings.LastIndex(fields[1], "@{"), strings.LastIndex(fields[1], "}")
		if open < 0 || closing < open {
			continue
		}
		secs, err := strconv.ParseInt(fields[1][open+2:closing], 10, 64)
		if err != nil || time.Unix(secs, 0).Before(since.Truncate(time.Second)) {
			continue
		}
		shas = append(shas, fields[0])
	}
	return shas
}
