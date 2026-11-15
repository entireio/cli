package remote

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitdir"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// packDirName is the object store's pack directory, relative to the git common
// dir.
const packDirName = "objects/pack"

// receiveTempPrefixes are the names git gives the files it writes while it
// receives a pack: index-pack's tmp_pack_ for the pack itself, then tmp_idx_
// and tmp_rev_ for the index and reverse index it writes once the pack is in.
// Git renames them to pack-<hash>.* when the receive succeeds. When the
// receive dies (an early EOF from the remote, or the fetch killed at its
// deadline) it removes none of them, and the fetches here pass --no-auto-gc,
// so nothing else does either until a manual gc after git's two-week expiry.
// A checkpoint fetch that keeps failing the same way then leaves one partial
// pack, read-only and as large as what it received, per attempt: issue 2895
// measured 39 of 115 MB each in a day.
var receiveTempPrefixes = []string{"tmp_pack_", "tmp_idx_", "tmp_rev_"}

func isReceiveTempName(name string) bool {
	for _, prefix := range receiveTempPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// packTemps records the receive temp files already in a repository's pack
// directory before a fetch, so that when the fetch fails only the files that
// appeared during it are removed. A temp file that was already there belongs
// to an earlier fetch, or to another git process that is still receiving, and
// is left alone.
//
// What remains is a narrow race: another git process in the same repository
// that starts receiving a pack while this fetch runs, and is still receiving
// when this one fails, loses its temp file. Its receive then fails at the
// final rename without updating any ref, and can be retried. Git's own prune
// has no better test (it goes by age), and no portable one exists, because git
// takes no lock on these files.
type packTemps struct {
	commonDir string
	before    map[string]struct{}
}

// snapshotPackTemps records the temp files in the pack directory of the
// repository a fetch run in dir writes to (the current directory when dir is
// empty). It returns nil when that directory cannot be resolved, and nil when
// GIT_OBJECT_DIRECTORY points the fetch's writes somewhere else; a nil result
// makes removeLeftovers a no-op, so a fetch is never refused over its cleanup.
func snapshotPackTemps(ctx context.Context, dir string) *packTemps {
	if os.Getenv("GIT_OBJECT_DIRECTORY") != "" {
		return nil
	}
	var commonDir string
	var err error
	if dir != "" {
		commonDir, err = gitdir.CommonDirForWorktree(ctx, dir)
	} else {
		commonDir, err = gitdir.CommonDir(ctx)
	}
	if err != nil {
		logging.Debug(ctx, "fetch temp cleanup: could not resolve the git common dir",
			slog.String("error", err.Error()))
		return nil
	}
	before, err := readReceiveTemps(commonDir)
	if err != nil {
		logging.Debug(ctx, "fetch temp cleanup: could not list the pack directory",
			slog.String("error", err.Error()))
		return nil
	}
	return &packTemps{commonDir: commonDir, before: before}
}

// removeLeftovers removes the receive temp files that appeared in the pack
// directory since snapshotPackTemps. Call it only after the fetch failed and
// its process tree has exited: git renames every one of these files when a
// receive succeeds, so after a success there is nothing of its own to remove.
// Best-effort: failures only log.
func (p *packTemps) removeLeftovers(ctx context.Context) {
	if p == nil {
		return
	}
	pack, closePack, err := openPackDir(p.commonDir)
	if err != nil {
		logging.Debug(ctx, "fetch temp cleanup: could not open the pack directory",
			slog.String("error", err.Error()))
		return
	}
	defer closePack()

	temps, err := receiveTempEntries(pack)
	if err != nil {
		logging.Debug(ctx, "fetch temp cleanup: could not list the pack directory",
			slog.String("error", err.Error()))
		return
	}
	var removed int
	var removedBytes int64
	for _, entry := range temps {
		name := entry.Name()
		if _, existed := p.before[name]; existed {
			continue
		}
		var size int64
		if info, infoErr := entry.Info(); infoErr == nil {
			size = info.Size()
		}
		if err := osroot.Remove(pack, name); err != nil {
			logging.Warn(ctx, "fetch temp cleanup: could not remove a temporary pack file a failed fetch left",
				slog.String("file", name),
				slog.String("error", err.Error()))
			continue
		}
		removed++
		removedBytes += size
	}
	if removed > 0 {
		logging.Info(ctx, "removed temporary pack files a failed fetch left behind",
			slog.Int("files", removed),
			slog.Int64("bytes", removedBytes))
	}
}

// readReceiveTemps returns the names of the receive temp files in commonDir's
// pack directory. A repository whose pack directory does not exist yet has
// none.
func readReceiveTemps(commonDir string) (map[string]struct{}, error) {
	pack, closePack, err := openPackDir(commonDir)
	if os.IsNotExist(err) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer closePack()

	temps, err := receiveTempEntries(pack)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(temps))
	for _, entry := range temps {
		names[entry.Name()] = struct{}{}
	}
	return names, nil
}

// receiveTempEntries lists the regular files in pack named like the ones git
// writes while receiving a pack. Anything else, a symlink included, is not
// git's and is never removed.
func receiveTempEntries(pack *os.Root) ([]os.DirEntry, error) {
	entries, err := osroot.ReadDir(pack, ".")
	if err != nil {
		return nil, err //nolint:wrapcheck // callers only log it
	}
	var temps []os.DirEntry
	for _, entry := range entries {
		if entry.Type().IsRegular() && isReceiveTempName(entry.Name()) {
			temps = append(temps, entry)
		}
	}
	return temps, nil
}

// openPackDir opens the pack directory under the shared git-common-dir root,
// refusing a symlink at either component. The caller must call the returned
// close function.
func openPackDir(commonDir string) (*os.Root, func(), error) {
	root, err := gitdir.OpenAt(commonDir)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // gitdir names the directory and returns a missing one unwrapped for os.IsNotExist
	}
	return osroot.OpenDirNoSymlinks(root, packDirName) //nolint:wrapcheck // preserve os.IsNotExist for readReceiveTemps
}
