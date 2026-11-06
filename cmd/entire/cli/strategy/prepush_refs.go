package strategy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// PrePushRef is one line of the ref list git writes to a pre-push hook's
// stdin: the local ref and commit being pushed, and the remote ref it updates.
type PrePushRef struct {
	LocalRef  string
	LocalSHA  string
	RemoteRef string
	RemoteSHA string
	// Truncated is set when a field was longer than maxPrePushFieldLen and was
	// cut short. Its ref names are then not the real refs and must not be acted
	// on by name.
	Truncated bool
}

// sends reports whether the line sends content: not a deletion, and not a ref
// the remote already has at that commit.
func (r PrePushRef) sends() bool {
	local := plumbing.NewHash(r.LocalSHA)
	return !local.IsZero() && local != plumbing.NewHash(r.RemoteSHA)
}

// maxPrePushFieldLen bounds how much of one field of a ref line is kept. Only a
// ref name can be longer, and its first bytes already decide whether it is a
// checkpoint ref: longer than the v1 name, or under the per-checkpoint prefix.
const maxPrePushFieldLen = 4096

// ParsePrePushRefs reads the "<local ref> <local sha> <remote ref> <remote sha>"
// lines git passes to a pre-push hook. Every line is kept: any ref, branch or
// tag the push sends can point at a waiting checkpoint's commit. It reads to
// the end with no size cap: a `--mirror` of a repository with many tags can be
// large, and a truncated list could hide such a line. For the same reason an
// oversized ref name is cut short, and its line marked Truncated, rather than
// failing the whole list. Each field keeps at most maxPrePushFieldLen bytes.
// Malformed lines are skipped; only a failed read is an error.
func ParsePrePushRefs(r io.Reader) ([]PrePushRef, error) {
	refs := []PrePushRef{}
	br := bufio.NewReader(r)
	var fields [4]string
	nfields := 0
	var field []byte
	inField := false
	truncated := false
	endField := func() {
		if !inField {
			return
		}
		if nfields < len(fields) {
			fields[nfields] = string(field)
		}
		nfields++
		field = field[:0]
		inField = false
	}
	endLine := func() {
		endField()
		if nfields == len(fields) {
			ref := PrePushRef{LocalRef: fields[0], LocalSHA: fields[1], RemoteRef: fields[2], RemoteSHA: fields[3], Truncated: truncated}
			refs = append(refs, ref)
		}
		nfields = 0
		truncated = false
	}
	for {
		b, err := br.ReadByte()
		if errors.Is(err, io.EOF) {
			endLine()
			return refs, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read pre-push refs: %w", err)
		}
		switch b {
		case '\n':
			endLine()
		case ' ', '\t', '\r':
			endField()
		default:
			inField = true
			if len(field) < maxPrePushFieldLen {
				field = append(field, b)
			} else {
				truncated = true
			}
		}
	}
}

type prePushRefsKey struct{}

type prePushRefsUnsavedKey struct{}

// WithPrePushRefsUnsaved records that the hook line could not save git's ref
// list for this push, so what it sends cannot be checked.
func WithPrePushRefsUnsaved(ctx context.Context) context.Context {
	return context.WithValue(ctx, prePushRefsUnsavedKey{}, true)
}

func prePushRefsUnsaved(ctx context.Context) bool {
	unsaved, _ := ctx.Value(prePushRefsUnsavedKey{}).(bool) //nolint:errcheck // absent means saved or an old hook
	return unsaved
}

// prePushRefsKnown reports whether the hook passed the ref list, which an old
// hook script or an old hook-manager line does not.
func prePushRefsKnown(ctx context.Context) bool {
	_, ok := ctx.Value(prePushRefsKey{}).([]PrePushRef)
	return ok
}

// warnPrePushRefsUnknown tells a user whose pre-push line predates the ref list
// that a push sending checkpoint refs itself cannot be checked. Callers warn
// only when checkpoints are being held back, the one moment it matters.
func warnPrePushRefsUnknown(ctx context.Context, w io.Writer) {
	if prePushRefsKnown(ctx) || prePushRefsUnsaved(ctx) {
		return
	}
	fmt.Fprintln(w, "[entire] Warning: this pre-push hook does not pass the refs being pushed, so a push that "+
		"includes Entire's checkpoint refs itself (e.g. `git push --mirror`) cannot be checked. If you use a hook "+
		"manager such as Husky, replace its Entire pre-push line with the one `entire enable` prints; otherwise run "+
		"`entire enable` to update the hook.")
}

// checkpointRefName is whichever side of the line names a checkpoint ref.
func (r PrePushRef) checkpointRefName() string {
	if isCheckpointRefName(r.LocalRef) {
		return r.LocalRef
	}
	return r.RemoteRef
}

// sendsTag reports whether the line pushes a tag, the one kind of ref that can
// name an annotated tag object.
func (r PrePushRef) sendsTag() bool {
	return strings.HasPrefix(r.LocalRef, "refs/tags/") || strings.HasPrefix(r.RemoteRef, "refs/tags/")
}

// WithPrePushRefsFrom parses git's pre-push ref list from r and records it. A
// list that cannot be read leaves the refs unknown, as with an old hook, and
// the user's push is not blocked over it.
func WithPrePushRefsFrom(ctx context.Context, r io.Reader) context.Context {
	refs, err := ParsePrePushRefs(r)
	if err != nil {
		logging.Warn(ctx, "pre-push: could not read the ref list; treating it as unknown",
			slog.String("error", err.Error()))
		return ctx
	}
	return WithPrePushRefs(ctx, refs)
}

// WithPrePushRefs records the refs the user's own push is about to update.
// Without it (an old hook script, a hook manager line, or a caller that is not
// a hook) the refs are not known, which is not the same as an empty list.
func WithPrePushRefs(ctx context.Context, refs []PrePushRef) context.Context {
	if refs == nil {
		refs = []PrePushRef{} // known to be empty, unlike absent
	}
	return context.WithValue(ctx, prePushRefsKey{}, refs)
}

// outerPushSendingLines returns the lines of the user's own push that send
// content, as far as the hook passed them.
func outerPushSendingLines(ctx context.Context) []PrePushRef {
	refs, _ := ctx.Value(prePushRefsKey{}).([]PrePushRef) //nolint:errcheck // absent means "not known"
	var lines []PrePushRef
	for _, r := range refs {
		if r.sends() {
			lines = append(lines, r)
		}
	}
	return lines
}

// isCheckpointRefName reports whether name holds checkpoint content: the v1
// branch or a per-checkpoint ref.
func isCheckpointRefName(name string) bool {
	return name == plumbing.NewBranchReferenceName(paths.MetadataBranchName).String() ||
		strings.HasPrefix(name, checkpoint.CheckpointRefPrefix)
}
