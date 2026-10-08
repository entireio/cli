package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/go-git/go-git/v6/plumbing"
)

// crossRepoReader is the read surface cross-repo explain needs: the two
// checkpoint reader tiers the renderers consume, plus the two cross-repo-only
// extras (author and anchoring commit) that normally come from local git.
// Narrowed to an interface so tests can drive the render paths without a cell.
type crossRepoReader interface {
	checkpoint.CheckpointReader
	checkpoint.SessionReader
	checkpoint.TaskReader
	GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (checkpoint.Author, error)
	checkpointCommit(ctx context.Context, checkpointID id.CheckpointID) ([]associatedCommit, error)
	resolveCommitCheckpoint(ctx context.Context, sha string) (id.CheckpointID, error)
}

// newCrossRepoReader builds the API-backed reader for a forge-qualified repo.
// Injectable so tests can substitute a fake cell (see explain_repo_test.go).
var newCrossRepoReader = func(ctx context.Context, insecureHTTP bool, forge, owner, repo string) (crossRepoReader, error) {
	// Resolve the repo_id and the cell together, from one placement: a mirror id
	// or native repo id is only resolvable by the cell holding that repository,
	// so a separately-chosen cell (the caller's home cell, for a multi-region
	// repo) would be asked about an id it has never seen and answer 404.
	placement, err := resolveForgeRepoCellPlacement(ctx, forge, owner, repo)
	if err != nil {
		// Not wrapped with the repo ref: the placement resolver already names
		// it, and adding it here is what made this path print the repo twice.
		return nil, err
	}
	client, err := auth.NewEntireAPICellClient(ctx, insecureHTTP, placement.Target)
	if err != nil {
		// NewEntireAPICellClient already returns user-facing, context-rich
		// errors (login hint, discovery guidance); surface them verbatim.
		return nil, err //nolint:wrapcheck // pass through contextual auth errors
	}
	return newAPICheckpointReader(client, placement.RepoID, forge, owner, repo), nil
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
	// target is the positional argument; checkpointID and commitSHA are the
	// explicit flags. Exactly one is set; see classifyCrossRepoTarget.
	target       string
	checkpointID string
	commitSHA    string

	json          bool
	transcript    bool
	rawTranscript bool
	// task selects a subagent task record's transcript (with transcript).
	task string
	// sessionIndex is -1 for "latest session".
	sessionIndex int

	verbose bool
	full    bool
	noPager bool

	insecureHTTP bool
}

const explainRepoFlagShapes = "gh/owner/name, et/project/repo, entire://<host>/gh/<owner>/<repo>, or entire://<host>/et/<project>/<repo>"

// explainRepoTargetShapes is shared by the help and the error so they cannot
// drift. Prefixes are excluded: resolving one means listing the foreign repo's
// checkpoints or commits, which is `entire search`'s job.
const explainRepoTargetShapes = "a full checkpoint ID (12-char hex or 26-char ULID) or a full commit SHA"

// parseExplainRepoFlag parses `--repo`. Every accepted form states its forge:
// gh/owner/repo or et/project/repo, including as the path of a full entire://
// clone URL. Leading slashes are optional on path refs. Returns lowercased
// coordinates, as the control plane persists them.
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
		if parseErr != nil || info.Protocol != gitremote.ProtocolEntire || info.Host == "" {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected %s", gitremote.RedactURL(v), explainRepoFlagShapes)
		}
		ref := info.Forge + "/" + info.Owner + "/" + info.Repo
		switch info.Forge {
		case nativeCloneForge:
			project, repoName, nativeErr := parseNativeCloneRef(ref)
			if nativeErr != nil {
				return "", "", "", fmt.Errorf("invalid --repo %q: expected %s: %w", gitremote.RedactURL(v), explainRepoFlagShapes, nativeErr)
			}
			return nativeCloneForge, strings.ToLower(project), strings.ToLower(repoName), nil
		case mirrorCloneForge:
			_, owner, repoName, mirrorErr := parseMirrorCloneRef(ref)
			if mirrorErr != nil {
				return "", "", "", fmt.Errorf("invalid --repo %q: expected %s: %w", gitremote.RedactURL(v), explainRepoFlagShapes, mirrorErr)
			}
			return mirrorCloneForge, owner, repoName, nil
		default:
			return "", "", "", fmt.Errorf("invalid --repo %q: unsupported forge %q; expected %s", gitremote.RedactURL(v), info.Forge, explainRepoFlagShapes)
		}
	}

	ref := strings.TrimPrefix(v, "/")
	switch {
	case strings.HasPrefix(ref, nativeCloneForge+"/"):
		project, repoName, nativeErr := parseNativeCloneRef(ref)
		if nativeErr != nil {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected %s: %w", value, explainRepoFlagShapes, nativeErr)
		}
		return nativeCloneForge, strings.ToLower(project), strings.ToLower(repoName), nil
	case strings.HasPrefix(ref, mirrorCloneForge+"/"):
		_, owner, repo, mirrorErr := parseMirrorCloneRef(ref)
		if mirrorErr != nil {
			return "", "", "", fmt.Errorf("invalid --repo %q: expected %s: %w", value, explainRepoFlagShapes, mirrorErr)
		}
		return mirrorCloneForge, owner, repo, nil
	default:
		return "", "", "", fmt.Errorf("invalid --repo %q: forge prefix is required; expected %s", value, explainRepoFlagShapes)
	}
}

func explainRepoRef(forge, owner, repo string) string {
	return forge + "/" + owner + "/" + repo
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

	// Shape is checked before any network call so a typo fails instantly.
	cid, sha, err := classifyCrossRepoTarget(opts)
	if err != nil {
		return err
	}

	reader, err := newCrossRepoReader(ctx, opts.insecureHTTP, forge, owner, repoName)
	if err != nil {
		if rendered := renderRepoNotOnboarded(errW, repoRef, err); rendered != nil {
			return rendered
		}
		return err
	}
	if sha != "" {
		stop := startSpinner(errW, fmt.Sprintf("Resolving commit %s in %s", abbreviateSHA(sha), repoRef))
		cid, err = reader.resolveCommitCheckpoint(ctx, sha)
		if err != nil {
			stop(false)
			return err
		}
		stop(true)
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
	case opts.task != "":
		// The task records are not served over the API; the reader says so.
		stop(false)
		return streamTaskTranscript(ctx, w, reader, cid, opts.task)

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
		// Parity with the local --json path, including failing hard on a
		// partial envelope.
		return writeCheckpointJSONEnvelope(w, errW, cid, envelope, failed)

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

// classifyCrossRepoTarget yields either a checkpoint ID or a commit SHA. The
// explicit flags are strict; the positional is classified by shape, which is
// safe because the full forms are disjoint (12 or 26 chars vs 40 or 64).
func classifyCrossRepoTarget(opts crossRepoExplainOptions) (id.CheckpointID, string, error) {
	switch {
	case opts.commitSHA != "" && opts.checkpointID != "":
		return id.EmptyCheckpointID, "", errors.New("cannot combine --commit with --checkpoint")
	case opts.commitSHA != "":
		if !plumbing.IsHash(opts.commitSHA) {
			return id.EmptyCheckpointID, "", fmt.Errorf("--commit with --repo requires a full commit SHA; %q cannot be resolved in another repo", opts.commitSHA)
		}
		return id.EmptyCheckpointID, opts.commitSHA, nil
	case opts.checkpointID != "":
		cid, err := id.NewCheckpointID(opts.checkpointID)
		if err != nil {
			return id.EmptyCheckpointID, "", fmt.Errorf("--checkpoint with --repo requires a full checkpoint ID; a prefix cannot be resolved in another repo: %w", err)
		}
		return cid, "", nil
	default:
		if plumbing.IsHash(opts.target) {
			return id.EmptyCheckpointID, opts.target, nil
		}
		cid, err := id.NewCheckpointID(opts.target)
		if err != nil {
			return id.EmptyCheckpointID, "", fmt.Errorf("--repo requires %s; a prefix cannot be resolved in another repo: %w", explainRepoTargetShapes, err)
		}
		return cid, "", nil
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
