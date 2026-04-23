package recap

import (
	"context"
	"os/exec"
	"regexp"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// trailerRe matches a single Entire-Checkpoint trailer line. Compiled once
// at package load so every LookupLinkedCommits call reuses it.
var trailerRe = regexp.MustCompile(`(?m)^Entire-Checkpoint:\s*([a-f0-9]+)\s*$`)

// LookupLinkedCommits returns a map from checkpoint ID to the list of
// commit SHAs on the active branch whose message contains a matching
// Entire-Checkpoint trailer. Unknown checkpoint IDs yield empty slices.
//
// A missing/empty repo (no HEAD yet) is treated as "no linked commits"
// rather than an error — the caller still gets a populated map with
// empty slices for every requested ID.
//
// Uses `git log` to leverage its built-in formatting; go-git's commit
// iteration would also work but adds no value for this read-only op.
func LookupLinkedCommits(ctx context.Context, checkpointIDs []string) map[string][]string {
	out := map[string][]string{}
	for _, id := range checkpointIDs {
		out[id] = nil
	}
	cmd := exec.CommandContext(ctx, "git", "log", "--pretty=format:%H%n%B%n--ENTIRE-END--")
	data, err := cmd.Output()
	if err != nil {
		// Empty repo or no commits is acceptable — return the empty map.
		logging.Debug(ctx, "recap: git log failed while looking up linked commits",
			"error", err.Error())
		return out
	}
	entries := strings.Split(string(data), "--ENTIRE-END--\n")
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		lines := strings.SplitN(entry, "\n", 2)
		sha := strings.TrimSpace(lines[0])
		body := ""
		if len(lines) > 1 {
			body = lines[1]
		}
		matches := trailerRe.FindAllStringSubmatch(body, -1)
		for _, m := range matches {
			id := m[1]
			if _, ok := out[id]; ok {
				out[id] = append(out[id], sha)
			}
		}
	}
	return out
}
