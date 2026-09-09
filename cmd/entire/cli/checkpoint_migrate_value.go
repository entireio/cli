package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// checkpointValue is what a repo's checkpoints add up to, computed locally for
// the closing block of `entire checkpoint migrate`. Every field is best-effort:
// the celebration must never fail the command, so missing data renders as an
// omitted row rather than an error.
type checkpointValue struct {
	Checkpoints int
	Sessions    int
	// Agents are display names, sorted.
	Agents []string
	// Tokens is the summed agent work across the sampled checkpoints; 0 when
	// unknown.
	Tokens int
	// TokensSampled is how many checkpoints contributed token data.
	TokensSampled int
	// TokensCapped reports that sampling stopped at checkpointValueTokenCap
	// (or the time budget) before covering every checkpoint.
	TokensCapped bool
	First, Last  time.Time
}

const (
	// checkpointValueTokenCap bounds how many checkpoints are read for token
	// totals: each read is a store lookup plus per-session metadata reads, and
	// a repo with thousands of checkpoints should not stall its own reward.
	checkpointValueTokenCap = 500
	// checkpointValueBudget is the wall-clock ceiling on the token pass.
	checkpointValueBudget = 4 * time.Second
)

// countLocalCheckpointRefs counts the local refs/entire/checkpoints/* refs that
// parse as checkpoint refs. Cheap: one reference walk, no store reads.
func countLocalCheckpointRefs(repo *git.Repository) (int, error) {
	refs, err := listLocalCheckpointRefs(repo)
	if err != nil {
		return 0, err
	}
	return len(refs), nil
}

// listLocalCheckpointRefs returns the local checkpoint refs in name order.
func listLocalCheckpointRefs(repo *git.Repository) ([]plumbing.ReferenceName, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, fmt.Errorf("list references: %w", err)
	}
	defer iter.Close()
	var names []plumbing.ReferenceName
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if _, ok := checkpoint.ParseRef(ref.Name()); ok {
			names = append(names, ref.Name())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk references: %w", err)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names, nil
}

// computeCheckpointValue gathers the metrics for the celebration block from
// local state only: the ref count, the checkpoint listing (sessions, agents,
// dates), and a bounded token pass over the most recent checkpoints. Errors
// from the listing are returned; per-checkpoint read errors are skipped.
func computeCheckpointValue(ctx context.Context, repo *git.Repository) (checkpointValue, error) {
	var v checkpointValue

	refCount, err := countLocalCheckpointRefs(repo)
	if err != nil {
		return v, err
	}

	// Local-only store: no blob or ref fetchers, so nothing here dials.
	stores, err := checkpoint.Open(ctx, repo, checkpoint.OpenOptions{ReadRemotes: strategy.CheckpointReadRemotes(ctx)})
	if err != nil {
		return v, fmt.Errorf("open checkpoint stores: %w", err)
	}
	store := stores.Persistent
	infos, err := store.List(ctx)
	if err != nil {
		return v, fmt.Errorf("list checkpoints: %w", err)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].CreatedAt.After(infos[j].CreatedAt) })

	// The ref walk is authoritative on git-refs; the listing is on git-branch,
	// where there are no refs to count. Take whichever knows more.
	v.Checkpoints = max(refCount, len(infos))

	sessions := make(map[string]bool)
	agents := make(map[string]bool)
	for _, info := range infos {
		ids := info.SessionIDs
		if len(ids) == 0 && info.SessionID != "" {
			ids = []string{info.SessionID}
		}
		for _, sid := range ids {
			sessions[sid] = true
		}
		if a := strings.TrimSpace(string(info.Agent)); a != "" {
			agents[a] = true
		}
		created := info.CreatedAt
		if created.IsZero() {
			created, _ = info.CheckpointID.Time()
		}
		if created.IsZero() {
			continue
		}
		if v.First.IsZero() || created.Before(v.First) {
			v.First = created
		}
		if created.After(v.Last) {
			v.Last = created
		}
	}
	v.Sessions = len(sessions)
	for a := range agents {
		v.Agents = append(v.Agents, a)
	}
	sort.Strings(v.Agents)

	sumCheckpointTokens(ctx, store, infos, &v)
	return v, nil
}

// sumCheckpointTokens fills Tokens/TokensSampled/TokensCapped from the most
// recent checkpoints, stopping at the cap or the time budget. Never fails.
func sumCheckpointTokens(ctx context.Context, store checkpoint.PersistentStore, infos []checkpoint.CheckpointInfo, v *checkpointValue) {
	sample := infos
	if len(sample) > checkpointValueTokenCap {
		sample = sample[:checkpointValueTokenCap]
		v.TokensCapped = true
	}
	budgetCtx, cancel := context.WithTimeout(ctx, checkpointValueBudget)
	defer cancel()

	for _, info := range sample {
		if budgetCtx.Err() != nil {
			v.TokensCapped = true
			return
		}
		summary, err := store.Read(budgetCtx, info.CheckpointID)
		if err != nil || summary == nil {
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				logging.Debug(ctx, "checkpoint value: skipping unreadable checkpoint",
					slog.String("checkpoint_id", info.CheckpointID.String()), slog.String("error", err.Error()))
			}
			continue
		}
		usage, _, err := tokensProfileCheckpointUsage(budgetCtx, store, info.CheckpointID, summary)
		if err != nil {
			continue
		}
		if n := totalTokens(usage); n > 0 {
			v.Tokens = saturatingIntAdd(v.Tokens, n)
			v.TokensSampled++
		}
	}
}

// tokensPhrase renders "18.4M tokens across your 142 checkpoints", naming the
// sample honestly when it did not cover everything.
func (v checkpointValue) tokensPhrase() string {
	scope := "across your " + countNoun(v.Checkpoints, "checkpoint", "checkpoints")
	switch {
	case v.TokensCapped && v.TokensSampled >= checkpointValueTokenCap:
		scope = fmt.Sprintf("across your %d most recent checkpoints", checkpointValueTokenCap)
	case v.TokensSampled < v.Checkpoints:
		scope = fmt.Sprintf("across %d of your checkpoints", v.TokensSampled)
	}
	return formatTokenCount(v.Tokens) + " tokens " + scope
}

// renderCheckpointValue prints the metric rows, omitting rows with nothing to
// say.
func renderCheckpointValue(w io.Writer, sty statusStyles, v checkpointValue) {
	rows := []explainRow{{Label: "Checkpoints", Value: strconv.Itoa(v.Checkpoints)}}
	if v.Sessions > 0 {
		rows = append(rows, explainRow{Label: "Sessions", Value: strconv.Itoa(v.Sessions)})
	}
	if len(v.Agents) > 0 {
		rows = append(rows, explainRow{Label: "Agents", Value: strings.Join(v.Agents, ", ")})
	}
	if v.Tokens > 0 {
		rows = append(rows, explainRow{Label: "Agent work", Value: v.tokensPhrase()})
	}
	if !v.First.IsZero() && !v.Last.IsZero() {
		const layout = "2 Jan 2006"
		span := v.First.Format(layout)
		if !sameDay(v.First, v.Last) {
			span += " to " + v.Last.Format(layout)
		}
		rows = append(rows, explainRow{Label: "From", Value: span})
	}
	fmt.Fprint(w, sty.metadataRows(rows))
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
