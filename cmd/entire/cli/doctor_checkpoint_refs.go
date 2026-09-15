package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// checkCheckpointRefShardCase reports git-refs checkpoints whose ref is stored
// under the other spelling of its shard directory, and — the condition worth
// acting on — checkpoints named by BOTH spellings at once.
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
		return // the checkpoint checks below report an unreadable ref store
	}
	defer refs.Close()

	spellings := make(map[id.CheckpointID][]string)
	if err := refs.ForEach(func(ref *plumbing.Reference) error {
		cid, ok := checkpoint.ParseRef(ref.Name())
		if !ok {
			return nil
		}
		spellings[cid] = append(spellings[cid], ref.Name().String())
		return nil
	}); err != nil {
		return
	}

	var misfiled, split []string
	for cid, names := range spellings {
		canonical, err := checkpoint.RefName(cid)
		if err != nil {
			continue // an ID kind RefName rejects has no canonical spelling to compare
		}
		if len(names) > 1 {
			sort.Strings(names)
			split = append(split, fmt.Sprintf("%s: %s", cid, strings.Join(names, " and ")))
			continue
		}
		if names[0] != canonical.String() {
			misfiled = append(misfiled, names[0])
		}
	}
	sort.Strings(misfiled)
	sort.Strings(split)

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
		fmt.Fprintln(w, "  only by case. A CLI from before this was fixed could not read the first")
		fmt.Fprintln(w, "  ref once the refs were packed, so it started a second one — each holds")
		fmt.Fprintln(w, "  part of the checkpoint's history:")
		printCappedList(w, split, func(name string) string { return name })
		fmt.Fprintln(w, "  Entire serves the canonical spelling; the other ref is not read and its")
		fmt.Fprintln(w, "  content is not lost. Compare them and delete the one you do not want:")
		fmt.Fprintln(w, "    git log --oneline <ref>")
		fmt.Fprintln(w, "    git pack-refs --all && git update-ref -d <ref>")
		fmt.Fprintln(w, "  Pack first: while the refs are loose, a case-insensitive filesystem")
		fmt.Fprintln(w, "  resolves both names to one file and the delete takes the wrong one.")
	}
}
