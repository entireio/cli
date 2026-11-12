package cli

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
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
// Read-only, and deliberately without a fix command for the misfiled case. On
// the filesystem that produces the condition, `git update-ref` to the canonical
// name and `git update-ref -d` on the folded one resolve to the SAME file while
// the refs are loose, so the pair of commands that looks like a rename is a
// delete. Entire reads and writes the ref where it actually is, so there is
// nothing for the user to do.
func checkCheckpointRefShardCase(cmd *cobra.Command) {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		return // no repository, or unreadable: other checks report that
	}
	defer repo.Close()

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
	// later version replaced; diverged lists the pairs where neither did.
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
			earlier = append(earlier, kept.Name().String())
		case kept.Hash().Equal(other.Hash()), checkpoint.SupersededBy(repo, other.Hash(), kept.Hash()):
			earlier = append(earlier, other.Name().String())
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
		fmt.Fprintln(w, "  Entire reads the later version. These refs hold an earlier one, are not")
		fmt.Fprintln(w, "  read, and can be removed:")
		printCappedList(w, earlier, func(name string) string { return "git update-ref -d " + name })
		fmt.Fprintln(w, "  Run 'git pack-refs --all' first: while the refs are loose, a")
		fmt.Fprintln(w, "  case-insensitive filesystem resolves both names to one file and the")
		fmt.Fprintln(w, "  delete takes the wrong one.")
	}
	if len(diverged) > 0 {
		fmt.Fprintln(w, "  These have histories that diverged, so each holds sessions the other")
		fmt.Fprintln(w, "  lacks. Entire reads the first of each pair. Compare them with")
		fmt.Fprintln(w, "  'git log --oneline <ref>' before removing either:")
		printCappedList(w, diverged, func(name string) string { return name })
	}
}
