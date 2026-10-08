package strategy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// PrePushRef is one line of the ref list git writes to a pre-push hook's
// stdin: the local ref and commit being pushed, and the remote ref it updates.
type PrePushRef struct {
	LocalRef  string
	LocalSHA  string
	RemoteRef string
	RemoteSHA string
}

// sends reports whether the line sends content: not a deletion, and not a ref
// the remote already has at that commit.
func (r PrePushRef) sends() bool {
	local := plumbing.NewHash(r.LocalSHA)
	return !local.IsZero() && local != plumbing.NewHash(r.RemoteSHA)
}

// maxPrePushRefsInput bounds how much of the hook's stdin is read. A ref line
// is under 300 bytes, so this covers tens of thousands of refs.
const maxPrePushRefsInput = 16 << 20

// ParsePrePushRefs reads the "<local ref> <local sha> <remote ref> <remote sha>"
// lines git passes to a pre-push hook. Malformed lines are skipped.
func ParsePrePushRefs(r io.Reader) ([]PrePushRef, error) {
	var refs []PrePushRef
	scanner := bufio.NewScanner(io.LimitReader(r, maxPrePushRefsInput))
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 4 {
			continue
		}
		refs = append(refs, PrePushRef{LocalRef: fields[0], LocalSHA: fields[1], RemoteRef: fields[2], RemoteSHA: fields[3]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read pre-push refs: %w", err)
	}
	return refs, nil
}

type prePushRefsKey struct{}

// prePushRefsKnown reports whether the hook passed the ref list, which an old
// hook script or an old hook-manager line does not.
func prePushRefsKnown(ctx context.Context) bool {
	_, ok := ctx.Value(prePushRefsKey{}).([]PrePushRef)
	return ok
}

// warnPrePushRefsUnknown tells a user whose pre-push line predates the ref list
// that a push sending entire/checkpoints/v1 itself cannot be checked, at the one
// moment it matters: v1 is holding checkpoints OPF has not verified.
func warnPrePushRefsUnknown(ctx context.Context, w io.Writer) {
	if prePushRefsKnown(ctx) {
		return
	}
	fmt.Fprintln(w, "[entire] Warning: this pre-push hook does not pass the refs being pushed, so a push that "+
		"includes entire/checkpoints/v1 itself (e.g. `git push --all`) would send it unscanned. If you use a hook "+
		"manager such as Husky, replace its Entire pre-push line with the one `entire enable` prints; otherwise run "+
		"`entire enable` to update the hook.")
}

// WithPrePushRefs records the refs the user's own push is about to update, so
// prePush can refuse to let it carry checkpoint content OPF has not verified.
// Without it (an old hook script, a hook manager line, or a caller that is not
// a hook) prePush cannot see the outer push and behaves as before.
func WithPrePushRefs(ctx context.Context, refs []PrePushRef) context.Context {
	if refs == nil {
		refs = []PrePushRef{} // known to be empty, unlike absent
	}
	return context.WithValue(ctx, prePushRefsKey{}, refs)
}

// outerPushV1SHA returns the commit the user's own push is sending to
// entire/checkpoints/v1, if it sends one (see PrePushRef.sends).
func outerPushV1SHA(ctx context.Context) (plumbing.Hash, bool) {
	refs, _ := ctx.Value(prePushRefsKey{}).([]PrePushRef) //nolint:errcheck // absent means "not known"
	v1 := plumbing.NewBranchReferenceName(paths.MetadataBranchName).String()
	for _, r := range refs {
		if (r.LocalRef == v1 || r.RemoteRef == v1) && r.sends() {
			return plumbing.NewHash(r.LocalSHA), true
		}
	}
	return plumbing.ZeroHash, false
}

// isCheckpointRefName reports whether name holds checkpoint content: the v1
// branch or a per-checkpoint ref.
func isCheckpointRefName(name string) bool {
	return name == plumbing.NewBranchReferenceName(paths.MetadataBranchName).String() ||
		strings.HasPrefix(name, checkpoint.CheckpointRefPrefix)
}

// checkOuterPushCheckpointRefs is the git-refs counterpart of checkOuterPushV1:
// it refuses the user's push when it sends checkpoint content (a per-checkpoint
// ref, or a v1 branch left from before a migration) whose commit does not carry
// the OPF trailer. The trailer is the same proof Entire's own delivery requires.
func checkOuterPushCheckpointRefs(ctx context.Context, repo *git.Repository) error {
	refs, _ := ctx.Value(prePushRefsKey{}).([]PrePushRef) //nolint:errcheck // absent means "not known"
	for _, r := range refs {
		if !r.sends() || (!isCheckpointRefName(r.LocalRef) && !isCheckpointRefName(r.RemoteRef)) {
			continue
		}
		commit, err := repo.CommitObject(plumbing.NewHash(r.LocalSHA))
		if err == nil && trailers.HasOPFApplied(commit.Message) {
			continue
		}
		return fmt.Errorf("%w: this push includes %s, which the OpenAI Privacy Filter has not verified. "+
			"Leave it out of this push (avoid --all/--mirror for checkpoint refs); Entire pushes checkpoints itself once they are scanned",
			ErrOuterPushCarriesUnverifiedCheckpoints, r.LocalRef)
	}
	return nil
}

// ErrOuterPushCarriesUnverifiedCheckpoints aborts a user push that sends
// checkpoint refs OPF has not verified, on the git-refs backend.
var ErrOuterPushCarriesUnverifiedCheckpoints = errors.New("refusing to push unverified checkpoint content")

// ErrOuterPushCarriesUnverifiedV1 aborts a user push that includes
// entire/checkpoints/v1 at a commit OPF has not verified. Entire's own push of
// v1 cannot stop it: git already chose what to send before the hook ran.
var ErrOuterPushCarriesUnverifiedV1 = errors.New("this push includes entire/checkpoints/v1 with checkpoints the OpenAI Privacy Filter has not verified")

// checkOuterPushV1 refuses the user's push when it sends entire/checkpoints/v1
// at anything other than verified, the tip OPF has just verified (zero when
// the checkpoints are still waiting for the background scan).
func checkOuterPushV1(ctx context.Context, verified plumbing.Hash) error {
	sha, ok := outerPushV1SHA(ctx)
	if !ok || (!verified.IsZero() && sha == verified) {
		return nil
	}
	if verified.IsZero() {
		return fmt.Errorf("%w; they are being scanned in the background. Push again once `entire status` shows none pending, or leave entire/checkpoints/v1 out of this push", ErrOuterPushCarriesUnverifiedV1)
	}
	return fmt.Errorf("%w; it was just rewritten with the filter applied. Run the push again", ErrOuterPushCarriesUnverifiedV1)
}
