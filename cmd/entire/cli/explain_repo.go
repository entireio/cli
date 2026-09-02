package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
)

// crossRepoReader is the read surface cross-repo explain needs: the two
// checkpoint reader tiers the renderers consume, plus the two cross-repo-only
// extras (author and anchoring commit) that normally come from local git.
// Narrowed to an interface so tests can drive the render paths without a cell.
type crossRepoReader interface {
	checkpoint.CheckpointReader
	checkpoint.SessionReader
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (checkpoint.Author, error)
	checkpointCommit(ctx context.Context, checkpointID id.CheckpointID) ([]associatedCommit, error)
}

// newCrossRepoReader builds the API-backed reader for a forge-qualified repo.
// Injectable so tests can substitute a fake cell (see explain_repo_test.go).
var newCrossRepoReader = func(ctx context.Context, insecureHTTP bool, forge, owner, repo string) (crossRepoReader, error) {
	repoRef := explainRepoRef(forge, owner, repo)
	// Resolve the repo_id and the cell together, from one placement: a mirror id
	// or native repo id is only resolvable by the cell holding that repository,
	// so a separately-chosen cell (the caller's home cell, for a multi-region
	// repo) would be asked about an id it has never seen and answer 404.
	placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, repo)
	if err != nil {
		// Not wrapped with repoRef: the placement resolver already names it,
		// and adding it here is what made this path print the repo twice.
		return nil, err
	}
	client, err := auth.NewEntireAPICellClient(ctx, insecureHTTP, placement.Target)
	if err != nil {
		// NewEntireAPICellClient already returns user-facing, context-rich
		// errors (login hint, discovery guidance); surface them verbatim.
		return nil, err //nolint:wrapcheck // pass through contextual auth errors
	}
	return newAPICheckpointReader(client, placement.RepoID, repoRef), nil
}

// crossRepoReadKey marks a context as rendering a checkpoint read from another
// repo. Context-scoped so the renderers can adjust their guidance without an
// extra positional parameter through every formatCheckpointOutput call site.
type crossRepoReadKey struct{}

// withCrossRepoRead marks ctx as reading ownerRepo's checkpoint from that
// repo's cell.
func withCrossRepoRead(ctx context.Context, ownerRepo string) context.Context {
	return context.WithValue(ctx, crossRepoReadKey{}, ownerRepo)
}

// crossRepoReadSource returns the repo a checkpoint is being read from when the
// current render is a cross-repo one.
func crossRepoReadSource(ctx context.Context) (string, bool) {
	ownerRepo, ok := ctx.Value(crossRepoReadKey{}).(string)
	return ownerRepo, ok && ownerRepo != ""
}

// crossRepoExplainOptions carries what `explain --repo` needs to render a
// foreign repo's checkpoint.
type crossRepoExplainOptions struct {
	repoFlag string
	// target is the checkpoint the user asked for, before ID validation.
	target string

	json          bool
	transcript    bool
	rawTranscript bool
	// sessionIndex is -1 for "latest session".
	sessionIndex int

	verbose bool
	full    bool
	noPager bool

	insecureHTTP bool
}

const explainRepoFlagShapes = "owner/name, gh/owner/name, et/project/repo, or an entire://<host>/et/<project>/<repo> clone URL"

// parseExplainRepoFlag parses `--repo`. GitHub repositories accept owner/name
// and gh/owner/name; Entire-native repositories accept et/project/repo and a
// full entire://.../et/project/repo clone URL. Leading slashes are optional on
// path refs. Returns lowercased coordinates, as the control plane persists
// them.
//
// A bare repo ID is deliberately not accepted: the control plane and the search
// index expose different identifiers for a repository, and guessing which space
// an opaque ID belongs to would key the cell lookup off the wrong one.
func parseExplainRepoFlag(value string) (forge, owner, repo string, err error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", "", "", fmt.Errorf("--repo requires a value: %s", explainRepoFlagShapes)
	}

	if isEntireCloneURL(v) {
		info, parseErr := gitremote.ParseURL(v)
		if parseErr != nil || info.Protocol != gitremote.ProtocolEntire || info.Host == "" || info.Forge != nativeCloneForge {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected %s", gitremote.RedactURL(v), explainRepoFlagShapes)
		}
		project, repoName, ok := parseNativeCloneRef(nativeCloneForge + "/" + info.Owner + "/" + info.Repo)
		if !ok {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected %s", gitremote.RedactURL(v), explainRepoFlagShapes)
		}
		return nativeCloneForge, strings.ToLower(project), strings.ToLower(repoName), nil
	}

	ref := strings.TrimPrefix(v, "/")
	if strings.HasPrefix(ref, nativeCloneForge+"/") {
		project, repoName, ok := parseNativeCloneRef(ref)
		if !ok {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected %s", value, explainRepoFlagShapes)
		}
		return nativeCloneForge, strings.ToLower(project), strings.ToLower(repoName), nil
	}
	if !strings.Contains(ref, "/") {
		return "", "", "", fmt.Errorf("invalid --repo %q: expected %s", value, explainRepoFlagShapes)
	}
	// Normalize to the gh/owner/name clone-ref shape so the mirror parser's
	// GitHub charset validation applies to both accepted GitHub forms.
	if !strings.HasPrefix(ref, "gh/") {
		ref = "gh/" + ref
	}
	_, owner, repo, err = parseMirrorCloneRef(ref)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid --repo %q: expected %s: %w", value, explainRepoFlagShapes, err)
	}
	return "gh", owner, repo, nil
}

func explainRepoRef(forge, owner, repo string) string {
	if forge == nativeCloneForge {
		return forge + "/" + owner + "/" + repo
	}
	return owner + "/" + repo
}

// explainRepoTargetsCurrentRepo reports whether the --repo value names the repo
// checked out here. An unparseable value returns false so the cross-repo path
// runs and reports the parse error, rather than silently falling through to the
// local path.
func explainRepoTargetsCurrentRepo(ctx context.Context, repoFlag string) bool {
	forge, owner, repo, err := parseExplainRepoFlag(repoFlag)
	if err != nil {
		return false
	}
	return explainRepoIsCurrent(ctx, forge, owner, repo)
}

// explainRepoIsCurrent reports whether forge/owner/repo names the same
// repository as the cwd worktree's origin remote (which handles ssh, https,
// and entire:// URL forms). The forge comparison prevents same-named native
// and GitHub repositories from matching. Best-effort: any lookup or parse
// failure returns false and the cross-repo path runs.
//
// When it does match, explain falls through to the local path — which is both
// faster and strictly more capable, since it can read checkpoints that have not
// been pushed yet.
func explainRepoIsCurrent(ctx context.Context, forge, owner, repo string) bool {
	curForge, curOwner, curRepo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil || !strings.EqualFold(curForge, forge) {
		return false
	}
	return strings.EqualFold(curOwner, owner) && strings.EqualFold(curRepo, repo)
}

// runCrossRepoExplain explains a checkpoint owned by another repository by
// reading it from that repo's cell over HTTP. Nothing is written to the local
// repository: no ref, no objects, no session state — so a foreign checkpoint
// never shows up in this repo's own checkpoint history or token profile.
//
// The output modes reuse the same renderers as the local path, so a foreign
// checkpoint prints identically to a local one.
func runCrossRepoExplain(ctx context.Context, w, errW io.Writer, opts crossRepoExplainOptions) error {
	forge, owner, repoName, err := parseExplainRepoFlag(opts.repoFlag)
	if err != nil {
		return err
	}
	repoRef := explainRepoRef(forge, owner, repoName)

	// A prefix can't be resolved without listing the foreign repo's
	// checkpoints, which is `entire search`'s job, so cross-repo needs the
	// whole ID.
	cid, err := id.NewCheckpointID(opts.target)
	if err != nil {
		return fmt.Errorf("--repo requires a full checkpoint ID (12-char hex or 26-char ULID); a prefix cannot be resolved in another repo: %w", err)
	}

	reader, err := newCrossRepoReader(ctx, opts.insecureHTTP, forge, owner, repoName)
	if err != nil {
		if rendered := renderRepoNotOnboarded(errW, repoRef, err); rendered != nil {
			return rendered
		}
		return err
	}
	// Marked for the renderers: a foreign checkpoint cannot be written to, so
	// they must not offer actions that only work in the owning repo.
	ctx = withCrossRepoRead(ctx, repoRef)

	stop := startSpinner(errW, fmt.Sprintf("Reading checkpoint %s from %s", cid, repoRef))
	// Read directly rather than through checkpoint.ReadCheckpoint: the helper
	// prefixes "read persistent checkpoint:", which buries the reader's
	// already-user-facing message (e.g. the not-pushed-yet guidance) behind
	// storage vocabulary that means nothing for a repo read over HTTP.
	summary, err := reader.Read(ctx, cid)
	if err != nil {
		stop(false)
		return err //nolint:wrapcheck // the reader's errors already name the checkpoint and repo
	}
	if summary == nil {
		stop(false)
		return fmt.Errorf("checkpoint %s is not available for %s", cid, repoRef)
	}

	switch {
	case opts.transcript || opts.rawTranscript:
		content, contentErr := readCrossRepoSessionContent(ctx, reader, cid, summary, opts.sessionIndex)
		if contentErr != nil {
			stop(false)
			return contentErr
		}
		// Every failure has to land before stop(true): an empty transcript is a
		// failure, and reporting it after a ✓ line tells the reader the read
		// succeeded and then contradicts it.
		if len(content.Transcript) == 0 {
			stop(false)
			return fmt.Errorf("checkpoint %s in %s has no transcript", cid, repoRef)
		}
		stop(true)
		if _, err := w.Write(content.Transcript); err != nil {
			return fmt.Errorf("failed to write transcript: %w", err)
		}
		return nil

	case opts.json:
		envelope, failed := buildCheckpointJSONEnvelope(ctx, reader, summary, cid)
		stop(!envelope.Partial)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(envelope); err != nil {
			return fmt.Errorf("failed to encode checkpoint json: %w", err)
		}
		// Parity with the local --json path: fail hard so automation can't
		// mistake incomplete metadata for a clean export. The envelope, with
		// its per-session error fields, is already on stdout.
		if envelope.Partial {
			fmt.Fprintf(errW, "checkpoint %s: failed to read metadata for %d session(s) (indexes %v)\n", cid, len(failed), failed)
			return NewSilentError(fmt.Errorf("checkpoint %s export incomplete: %d session(s) unreadable", cid, len(failed)))
		}
		return nil

	default:
		content, contentErr := readCrossRepoSessionContent(ctx, reader, cid, summary, opts.sessionIndex)
		if contentErr != nil {
			stop(false)
			return contentErr
		}
		author, _ := reader.GetCheckpointAuthor(ctx, cid) //nolint:errcheck // author is optional
		commits, _ := reader.checkpointCommit(ctx, cid)   //nolint:errcheck // best-effort, already-cached read
		// Stop before the first write to w so stderr spinner frames never
		// interleave with stdout content.
		stop(false)
		output := formatCheckpointOutput(ctx, summary, content, cid, commits, author, opts.verbose, opts.full, w)
		outputExplainContent(w, output, opts.noPager)
		return nil
	}
}

// readCrossRepoSessionContent reads the requested session, or the latest when
// no explicit index was given.
func readCrossRepoSessionContent(ctx context.Context, reader crossRepoReader, cid id.CheckpointID, summary *checkpoint.CheckpointSummary, sessionIndex int) (*checkpoint.SessionContent, error) {
	if sessionIndex < 0 {
		content, err := checkpoint.ReadLatestSessionContent(ctx, reader, cid, summary)
		if err != nil {
			return nil, fmt.Errorf("failed to read checkpoint content for %s: %w", cid, err)
		}
		return content, nil
	}
	content, err := reader.ReadSessionContent(ctx, cid, sessionIndex)
	if err != nil {
		return nil, fmt.Errorf("failed to read checkpoint content for %s: %w", cid, err)
	}
	return content, nil
}

// crossRepoExplainSessionIndex maps the --session-index flag to the reader's
// convention (-1 = latest), so an unset flag reads the latest session.
func crossRepoExplainSessionIndex(changed bool, value int) int {
	if !changed {
		return -1
	}
	return value
}

// assert the API reader satisfies the read surface the render paths need.
var _ crossRepoReader = (*apiCheckpointReader)(nil)
