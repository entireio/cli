package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// Turn and task baselines live in the .entire/tmp of the worktree whose hook
// captured them. The hook follows the agent, so an end hook can run in another
// worktree of the same repository than the start hook that wrote its baseline.

// baselineSearch lists the worktrees whose .entire/tmp may hold a baseline, in
// lookup order; "" stands for the hook's own tree.
type baselineSearch []string

// turnBaselineSearch looks first where the session's turn started: a file left
// in the hook's own tree by an earlier turn there would be stale.
func turnBaselineSearch(ctx context.Context, sessionID string) baselineSearch {
	others := otherBaselineWorktrees(ctx, sessionID)
	if len(others) > 0 && others[0].isTurn {
		return baselineSearch{others[0].root, ""}
	}
	return baselineSearch{""}
}

// taskBaselineSearch looks in the hook's own tree first — the task may have
// launched here after the agent moved — then where the turn started and the
// session's home. Task baselines are keyed by a unique tool use, so none is stale.
func taskBaselineSearch(ctx context.Context, sessionID string) baselineSearch {
	search := baselineSearch{""}
	for _, o := range otherBaselineWorktrees(ctx, sessionID) {
		search = append(search, o.root)
	}
	return search
}

type baselineWorktree struct {
	root   string
	isTurn bool
}

// otherBaselineWorktrees returns the session's turn and home worktrees that
// differ from the hook's own tree and belong to the same repository. The paths
// come from session state; each is checked against this repository's common
// directory before its .entire is opened.
func otherBaselineWorktrees(ctx context.Context, sessionID string) []baselineWorktree {
	if sessionID == "" {
		return nil
	}
	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil || state == nil {
		return nil
	}
	current, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil
	}
	var candidates []baselineWorktree
	for _, c := range []baselineWorktree{{root: state.TurnWorktreePath, isTurn: true}, {root: state.WorktreePath}} {
		if c.root == "" || sameDir(c.root, current) {
			continue
		}
		if len(candidates) > 0 && sameDir(candidates[0].root, c.root) {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil // the common case: the agent never left this tree
	}
	currentMeta, err := gitrepo.ResolveWorktreeMetadata(current)
	if err != nil {
		return nil
	}
	var out []baselineWorktree
	for _, c := range candidates {
		meta, err := gitrepo.ResolveWorktreeMetadata(c.root)
		if err != nil || !sameDir(meta.CommonDir, currentMeta.CommonDir) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// read returns the first copy of the baseline file found and the worktree it
// came from ("" for the hook's own tree). A missing file is (nil, "", nil).
func (s baselineSearch) read(ctx context.Context, name string) ([]byte, string, error) {
	for _, root := range s {
		data, err := readBaselineIn(ctx, root, name)
		if err == nil {
			return data, root, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, "", err
		}
	}
	return nil, "", nil
}

func readBaselineIn(ctx context.Context, worktree, name string) ([]byte, error) {
	var root *os.Root
	var err error
	if worktree == "" {
		root, err = entiredir.OpenForRead(ctx)
	} else {
		root, err = entiredir.OpenAtForRead(worktree)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", paths.EntireDir, err)
	}
	data, err := entiredir.ReadFile(root, tmpFile("%s", name))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return data, nil
}

// remove deletes every copy of the baseline file, best-effort beyond the first
// error, so a moved agent leaves nothing behind in the tree it started in.
func (s baselineSearch) remove(ctx context.Context, name string) error {
	var firstErr error
	for _, worktree := range s {
		var err error
		if worktree == "" {
			err = cleanupTmpStateFile(ctx, name)
		} else {
			err = removeBaselineIn(worktree, name)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			logging.Debug(logging.WithComponent(ctx, "state"), "failed to remove baseline",
				slog.String("worktree", worktree), slog.String("error", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func removeBaselineIn(worktree, name string) error {
	root, err := entiredir.OpenAtForRead(worktree)
	if err != nil {
		return fmt.Errorf("open %s: %w", paths.EntireDir, err)
	}
	if err := osroot.RemoveNoSymlinks(root, tmpFile("%s", name)); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// carryTurnPrompt moves this turn's prompts, which the turn-start hook appended
// past offset in the session's prompt.txt in the worktree the agent has since
// left, into this tree's copy, which the end hook checkpoints. Earlier prompts
// stay where they were: they belong to steps already saved in that tree.
func carryTurnPrompt(ctx context.Context, from, sessionID string, offset int) error {
	name := sessionMetadataName(sessionID) + "/" + paths.PromptFileName
	src, err := entiredir.OpenAtForRead(from)
	if err != nil {
		return fmt.Errorf("open %s in %s: %w", paths.EntireDir, from, err)
	}
	content, err := entiredir.ReadFile(src, name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read carried prompt: %w", err)
	}
	if offset < 0 || offset > len(content) {
		offset = 0 // the file was reset since the turn began: all of it is this turn's
	}
	kept, carried := content[:offset], bytes.TrimPrefix(content[offset:], []byte(promptSeparator))
	if len(carried) == 0 {
		return nil
	}
	dst, err := entiredir.Open(ctx)
	if err != nil {
		return fmt.Errorf("open %s: %w", paths.EntireDir, err)
	}
	if err := osroot.MkdirAllNoSymlink(dst, sessionMetadataName(sessionID), 0o750); err != nil {
		return fmt.Errorf("create session metadata dir: %w", err)
	}
	merged := carried
	if existing, readErr := entiredir.ReadFile(dst, name); readErr == nil && len(existing) > 0 {
		merged = append(append(existing, []byte(promptSeparator)...), carried...)
	}
	if err := entiredir.WriteFile(dst, name, merged, 0o600); err != nil {
		return fmt.Errorf("write carried prompt: %w", err)
	}
	if len(kept) > 0 {
		err = entiredir.WriteFile(src, name, kept, 0o600)
	} else {
		err = osroot.RemoveNoSymlinks(src, name)
	}
	if err != nil {
		return fmt.Errorf("trim carried prompt: %w", err)
	}
	return nil
}

// promptSeparator joins the prompts of successive turns in prompt.txt.
const promptSeparator = "\n\n---\n\n"
