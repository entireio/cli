package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/gitexec"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// AgentConfigRun is a private per-run directory under the user's cache for
// the files a reviewer's profile config needs (settings, MCP config, a skills
// plugin). It is removed when the reviewer exits.
type AgentConfigRun struct {
	root *os.Root
	name string
	dir  string
	// CheckoutRoot is the reviewed checkout, canonicalized.
	CheckoutRoot string
	// ForbiddenRoots are the checkouts no profile command may run from.
	ForbiddenRoots []string
}

// NewAgentConfigRun creates the run directory and resolves the checkout.
func NewAgentConfigRun(ctx context.Context) (*AgentConfigRun, error) {
	checkout, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve the review checkout: %w", err)
	}
	canonical, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s: %w", checkout, err)
	}
	forbidden := []string{checkout, canonical}
	if caller := strings.TrimSpace(os.Getenv(envReviewFindingsWorktree)); caller != "" {
		forbidden = append(forbidden, caller)
		if resolved, err := filepath.EvalSymlinks(caller); err == nil {
			forbidden = append(forbidden, resolved)
		}
	}

	root, err := userdirs.CacheRoot()
	if err != nil {
		return nil, fmt.Errorf("resolve cache dir: %w", err)
	}
	cacheDir, err := userdirs.CacheDirChecked()
	if err != nil {
		return nil, fmt.Errorf("resolve cache dir: %w", err)
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, fmt.Errorf("name review run dir: %w", err)
	}
	name := path.Join("review-runs", hex.EncodeToString(suffix[:]))
	if err := osroot.MkdirAllNoSymlink(root, name, 0o700); err != nil {
		return nil, fmt.Errorf("create review run dir: %w", err)
	}
	return &AgentConfigRun{
		root:           root,
		name:           name,
		dir:            filepath.Join(cacheDir, filepath.FromSlash(name)),
		CheckoutRoot:   canonical,
		ForbiddenRoots: forbidden,
	}, nil
}

// WriteFile writes rel (slash-separated) inside the run dir with mode 0600 and
// returns its absolute path.
func (r *AgentConfigRun) WriteFile(rel string, data []byte) (string, error) {
	name := path.Join(r.name, rel)
	if dir := path.Dir(name); dir != r.name {
		if err := osroot.MkdirAllNoSymlink(r.root, dir, 0o700); err != nil {
			return "", fmt.Errorf("create %s: %w", rel, err)
		}
	}
	if err := osroot.WriteFile(r.root, name, data, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	return filepath.Join(r.dir, filepath.FromSlash(rel)), nil
}

// Cleanup removes the run dir.
func (r *AgentConfigRun) Cleanup() {
	_ = osroot.RemoveAllNoSymlinks(r.root, r.name) //nolint:errcheck // best effort; a leftover dir in the cache is harmless
}

// CopyCheckoutTree copies the regular files under each dir of the checkout's
// HEAD commit into the run dir at dest/<path relative to dir>. It reads the
// committed tree, so the copy matches what the trust gate inspected, and it
// refuses symlinks and submodules rather than following them. It reports how
// many files it copied.
func (r *AgentConfigRun) CopyCheckoutTree(ctx context.Context, dirs map[string]string) (int, error) {
	copied := 0
	for src, dest := range dirs {
		out, err := gitexec.Run(ctx, r.CheckoutRoot, "ls-tree", "-r", "-z", "--full-tree", "--end-of-options", "HEAD", "--", src)
		if err != nil {
			return copied, fmt.Errorf("list %s: %w", src, err)
		}
		for _, record := range strings.Split(out, "\x00") {
			if record == "" {
				continue
			}
			meta, name, found := strings.Cut(record, "\t")
			fields := strings.Fields(meta)
			if !found || len(fields) != 3 {
				return copied, fmt.Errorf("unexpected git ls-tree output %q", record)
			}
			if fields[0] != "100644" && fields[0] != "100755" {
				return copied, fmt.Errorf("%s is a symlink or submodule; the review will not load it", name)
			}
			rel := strings.TrimPrefix(name, src+"/")
			if rel == name || strings.Contains(rel, "..") {
				return copied, fmt.Errorf("unexpected path %q under %s", name, src)
			}
			blob, err := gitexec.Run(ctx, r.CheckoutRoot, "cat-file", "blob", fields[2])
			if err != nil {
				return copied, fmt.Errorf("read %s: %w", name, err)
			}
			if _, err := r.WriteFile(path.Join(dest, rel), []byte(blob)); err != nil {
				return copied, err
			}
			copied++
		}
	}
	return copied, nil
}

// MainRepoRoot returns the main checkout of the reviewed repository (the
// parent of the git common dir), canonicalized; for a linked worktree it
// differs from CheckoutRoot.
func (r *AgentConfigRun) MainRepoRoot(_ context.Context) (string, error) {
	meta, err := gitrepo.ResolveWorktreeMetadata(r.CheckoutRoot)
	if err != nil {
		return "", fmt.Errorf("resolve git common dir: %w", err)
	}
	root := filepath.Dir(meta.CommonDir)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root, nil
}

// TreeEntryNames lists the names directly under dir in the checkout's HEAD
// tree, or nil when it has none.
func (r *AgentConfigRun) TreeEntryNames(ctx context.Context, dir string) []string {
	out, err := gitexec.Run(ctx, r.CheckoutRoot, "ls-tree", "-z", "--name-only", "--full-tree", "--end-of-options", "HEAD:"+dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}
