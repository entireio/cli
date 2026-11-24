package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// checkCheckpointRefShardCase reports git-refs checkpoints whose ref is stored
// under the other spelling of its shard directory, and — the condition worth
// acting on — checkpoints named by BOTH spellings at once. For those it says
// which ref is the earlier version, the only one it offers to remove.
//
// A shard bucket has two possible spellings because each ID format uses one
// case exclusively (legacy hex lowercase, ULID uppercase Crockford base32). On
// a case-insensitive-but-case-preserving filesystem the bucket is named after
// whichever format reached it first, so a migrated repo mixes the two and a
// checkpoint's ref can end up under the other format's spelling. See
// checkpoint.FoldedRefName.
//
// Where git recorded a case-insensitive filesystem, every ref belongs under its
// lowercase shard (checkpoint.LocalRefName), and the check offers to rename the
// others with checkpoint.NormalizeShardSpelling, which the next push also runs
// (see reportUppercaseShardRefs). It never prints rename commands to run by
// hand: there, `git update-ref` to one spelling and `git update-ref -d` on the
// other resolve to the SAME file while the refs are loose, so the pair of
// commands that looks like a rename is a delete.
//
// Elsewhere the check is read-only. A ref under the other spelling there came
// from another filesystem, and Entire reads and writes it where it is.
func checkCheckpointRefShardCase(cmd *cobra.Command, force bool) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return // no repository, or unreadable: other checks report that
	}
	defer repo.Close()

	if renames, divergedIDs, folds, pendingErr := checkpoint.PendingShardRenames(ctx, repo); folds {
		if pendingErr != nil {
			logging.Warn(ctx, "doctor: cannot list checkpoint refs; skipping the checkpoint shard spelling check",
				slog.String("error", pendingErr.Error()))
			return
		}
		reportUppercaseShardRefs(cmd, repo, renames, divergedIDs, force)
		return
	}

	refs, err := repo.References()
	if err != nil {
		logging.Warn(ctx, "doctor: cannot list refs; skipping the checkpoint shard spelling check",
			slog.String("error", err.Error()))
		return // the checkpoint checks below report an unreadable ref store
	}
	defer refs.Close()

	spellings := make(map[id.CheckpointID][]*plumbing.Reference)
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		cid, ok := checkpoint.ParseRef(ref.Name())
		if !ok {
			return nil
		}
		spellings[cid] = append(spellings[cid], ref)
		return nil
	}); err != nil {
		logging.Warn(ctx, "doctor: cannot read refs; skipping the checkpoint shard spelling check",
			slog.String("error", err.Error()))
		return
	}

	// A split checkpoint has one ref per spelling. earlier lists the refs a
	// later version replaced, each with the commit it holds; diverged lists the
	// pairs where neither did.
	var misfiled, split, earlier, diverged []string
	for cid, found := range spellings {
		canonical, err := checkpoint.RefName(cid)
		if err != nil {
			continue // an ID kind RefName rejects has no canonical spelling to compare
		}
		if len(found) == 1 {
			if found[0].Name() != canonical {
				misfiled = append(misfiled, found[0].Name().String())
			}
			continue
		}
		kept, other := found[0], found[1]
		if other.Name() == canonical {
			kept, other = other, kept
		}
		pair := fmt.Sprintf("%s: %s and %s", cid, kept.Name(), other.Name())
		split = append(split, pair)
		switch {
		case checkpoint.SupersededBy(repo, kept.Hash(), other.Hash()):
			earlier = append(earlier, kept.Name().String()+" "+kept.Hash().String())
		case kept.Hash().Equal(other.Hash()), checkpoint.SupersededBy(repo, other.Hash(), kept.Hash()):
			earlier = append(earlier, other.Name().String()+" "+other.Hash().String())
		default:
			diverged = append(diverged, pair)
		}
	}
	sort.Strings(misfiled)
	sort.Strings(split)
	sort.Strings(earlier)
	sort.Strings(diverged)

	if len(misfiled) > 0 {
		fmt.Fprintln(w, "Checkpoint refs: FOLDED SHARD DIRECTORY")
		fmt.Fprintf(w, "  %d checkpoint ref(s) are stored under the other spelling of their shard\n", len(misfiled))
		fmt.Fprintln(w, "  directory, which is what a case-insensitive filesystem does when legacy")
		fmt.Fprintln(w, "  and ULID checkpoints share a bucket:")
		printCappedList(w, misfiled, func(name string) string { return name })
		fmt.Fprintln(w, "  Nothing to do: Entire reads and writes them where they are. Renaming")
		fmt.Fprintln(w, "  them is not safe to suggest, because on this filesystem both spellings")
		fmt.Fprintln(w, "  name one file while the refs are loose.")
	}

	if len(split) > 0 {
		fmt.Fprintln(w, "Checkpoint refs: SPLIT ACROSS SHARD SPELLINGS")
		fmt.Fprintf(w, "  %d checkpoint(s) are named by two refs whose shard directories differ\n", len(split))
		fmt.Fprintln(w, "  only by case. On a case-insensitive filesystem git writes an update under")
		fmt.Fprintln(w, "  the spelling the directory has on disk, and a packed ref keeps the earlier")
		fmt.Fprintln(w, "  commit under the other:")
		printCappedList(w, split, func(name string) string { return name })
	}
	if len(earlier) > 0 {
		// The delete names the commit it expects. While the refs are loose a
		// case-insensitive filesystem resolves both names to one file, the
		// later version's, and git then refuses the delete.
		fmt.Fprintln(w, "  Entire reads the later version. These refs hold an earlier one and are")
		fmt.Fprintln(w, "  not read. To remove them, pack the refs, then delete each one by the")
		fmt.Fprintln(w, "  commit it holds:")
		fmt.Fprintln(w, "  git pack-refs --all")
		printCappedList(w, earlier, func(ref string) string { return "git update-ref -d " + ref })
		fmt.Fprintln(w, "  If git answers that a ref \"is at\" another commit, nothing was deleted:")
		fmt.Fprintln(w, "  pack the refs and run the delete again.")
	}
	if len(diverged) > 0 {
		fmt.Fprintln(w, "  These have histories that diverged, so each holds sessions the other")
		fmt.Fprintln(w, "  lacks. Entire reads the first of each pair. Compare them with")
		fmt.Fprintln(w, "  'git log --oneline <ref>' before removing either:")
		printCappedList(w, diverged, func(name string) string { return name })
	}
}

// reportUppercaseShardRefs is the shard check where git recorded a
// case-insensitive filesystem. There every checkpoint ref belongs under its
// lowercase shard (checkpoint.LocalRefName), and doctor can rename the others
// safely, which the next push would also do.
func reportUppercaseShardRefs(cmd *cobra.Command, repo *git.Repository, renames []plumbing.ReferenceName, diverged []id.CheckpointID, force bool) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	if len(renames) > 0 {
		names := make([]string, 0, len(renames))
		for _, name := range renames {
			names = append(names, name.String())
		}
		fmt.Fprintln(w, "Checkpoint refs: UPPERCASE SHARD NAMES")
		fmt.Fprintf(w, "  %d checkpoint ref(s) are stored under an uppercase shard directory. On this\n", len(renames))
		fmt.Fprintln(w, "  filesystem a shard spelled two ways is one directory, which can split a")
		fmt.Fprintln(w, "  checkpoint across two refs. Entire stores refs under lowercase shards and")
		fmt.Fprintln(w, "  renames these after the next push:")
		printCappedList(w, names, func(name string) string { return name })
		if renameNow(ctx, w, force) {
			result, err := checkpoint.NormalizeShardSpelling(ctx, repo)
			if err != nil {
				fmt.Fprintf(w, "  Renamed %d checkpoint ref(s); the rest keep their names: %v\n", result.Renamed, err)
			} else {
				fmt.Fprintf(w, "  Renamed %d checkpoint ref(s).\n", result.Renamed)
			}
		}
	}

	if len(diverged) > 0 {
		fmt.Fprintln(w, "Checkpoint refs: SPLIT ACROSS SHARD SPELLINGS")
		fmt.Fprintf(w, "  %d checkpoint(s) have a ref under each spelling of their shard, and the two\n", len(diverged))
		fmt.Fprintln(w, "  histories diverged, so each holds sessions the other lacks. Compare them")
		fmt.Fprintln(w, "  with 'git log --oneline <ref>' before removing either:")
		pairs := make([]string, 0, len(diverged))
		for _, cid := range diverged {
			local, err := checkpoint.LocalRefName(cid)
			if err != nil {
				continue
			}
			if other, ok := checkpoint.OtherShardSpelling(local); ok {
				pairs = append(pairs, fmt.Sprintf("%s: %s and %s", cid, other, local))
			}
		}
		printCappedList(w, pairs, func(pair string) string { return pair })
	}
}

// renameNow reports whether doctor should rename the refs now: with --force, or
// when the user agrees at the prompt. Without a terminal it says how instead.
func renameNow(ctx context.Context, w io.Writer, force bool) bool {
	if force {
		return true
	}
	if !interactive.CanPromptInteractively() {
		fmt.Fprintln(w, "  Run `entire doctor --force` to rename them now.")
		return false
	}
	proceed, err := confirmDoctorFix(ctx, w, "Rename these checkpoint refs now?")
	if err != nil {
		logging.Warn(ctx, "doctor: shard rename prompt failed", slog.String("error", err.Error()))
		return false
	}
	return proceed
}
