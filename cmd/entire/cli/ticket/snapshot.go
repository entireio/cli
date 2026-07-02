package ticket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// Snapshot is the last-seen state of a linked ticket, persisted next to
// the link so drift can be detected on the next fetch. It is the "working
// copy" — always refreshed to the latest, never the frozen history.
type Snapshot struct {
	Title     string `json:"title,omitempty"`
	State     string `json:"state,omitempty"`
	URL       string `json:"url,omitempty"`
	Digest    string `json:"digest,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"`
}

// snapshotOf captures task as a Snapshot, or nil for a nil task.
func snapshotOf(task *Task) *Snapshot {
	if task == nil {
		return nil
	}
	return &Snapshot{
		Title:     task.Title,
		State:     string(task.State),
		URL:       task.URL,
		Digest:    taskDigest(task),
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// taskDigest is a content hash over the fields that define the ticket's intent,
// used to detect any change cheaply.
func taskDigest(task *Task) string {
	data := strings.Join([]string{task.Title, string(task.State), task.Intent, task.Acceptance}, "\x00")
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// diffSnapshot lists human-readable changes from a prior snapshot to task.
// It returns nil when there is no prior snapshot or nothing changed.
func diffSnapshot(prior *Snapshot, task *Task) []string {
	if prior == nil || task == nil {
		return nil
	}
	next := snapshotOf(task)
	if prior.Digest == next.Digest {
		return nil
	}
	var changes []string
	if prior.State != next.State {
		changes = append(changes, fmt.Sprintf("state: %s → %s", displayState(prior.State), displayState(next.State)))
	}
	if prior.Title != next.Title {
		changes = append(changes, "title changed")
	}
	if len(changes) == 0 {
		changes = append(changes, "details updated")
	}
	return changes
}

// displayState renders a normalized state for humans.
func displayState(s string) string {
	if s == "" {
		return "unknown"
	}
	return strings.ReplaceAll(s, "_", " ")
}

// refreshSnapshot compares the linked branch's stored snapshot to task, updates
// it to the latest, and returns what changed. It returns nil changes when there
// is no prior snapshot, nothing changed, or the branch has no link.
func refreshSnapshot(ctx context.Context, branch string, task *Task) ([]string, error) {
	if task == nil {
		return nil, nil
	}
	store, err := newLinkStore(ctx)
	if err != nil {
		return nil, err
	}
	link, ok, err := store.get(branch)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	changes := diffSnapshot(link.Snapshot, task)
	if len(changes) > 0 {
		logging.Debug(ctx, "ticket drift detected",
			slog.String("branch", branch),
			slog.String("ticket", link.ID),
			slog.Int("changes", len(changes)))
	}
	link.Snapshot = snapshotOf(task)
	if err := store.set(branch, link); err != nil {
		return nil, err
	}
	return changes, nil
}
