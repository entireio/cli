package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// maxAPITranscriptBytes caps a single raw-transcript read from the cell. It
// matches the 50MB blob cap the checkpoint store writes under, so any
// transcript the store accepted is readable here; a larger response means the
// server changed its contract and we should fail loudly rather than silently
// truncate a transcript into the renderer.
const maxAPITranscriptBytes = 50 << 20

// apiCheckpointReader reads one repo's committed checkpoints over the
// entire-api cell HTTP surface instead of the local git object store. It
// implements checkpoint.CheckpointReader + checkpoint.SessionReader (plus the
// optional checkpoint.AuthorReader), which is the whole surface explain's
// render path needs — so cross-repo explain reuses the local renderers
// verbatim rather than carrying a second output implementation.
//
// Nothing is written to the local repository: the point of reading through the
// API is that another repo's checkpoint never enters this repo's object store,
// ref namespace, or `entire tokens profile`.
//
// It deliberately does NOT implement checkpoint.Writer. A foreign repo's
// checkpoint is read-only from here (`--generate` is rejected at the flag
// layer), and leaving Write off the type makes that structural instead of a
// convention this file has to remember.
type apiCheckpointReader struct {
	client *api.Client
	// repoID is the repo's Entire ULID; cell checkpoint routes key on it.
	repoID string
	// ownerRepo is the explicit, forge-qualified display coordinate used in
	// errors. repoFullName is the canonical identity entire-api returns: the
	// bare pair for both forges — "owner/name" for GitHub mirrors, and
	// "project/repo" for native rows (Core builds it without the et/ prefix).
	ownerRepo    string
	repoFullName string

	// detail caches the checkpoint envelope. Every read tier is derived from
	// this one response, and explain reads the summary and then each session,
	// so caching turns an N+1 into a single request.
	detail   *apiCheckpointInfo
	detailID id.CheckpointID
}

// newAPICheckpointReader returns a reader for repoID's checkpoints on the cell
// that client is already pointed at. Both coordinates are derived here, from
// the forge-qualified triple, so a caller cannot pair a forge-prefixed
// identity with the bare repo_full_name the server actually returns.
func newAPICheckpointReader(client *api.Client, repoID, forge, owner, repo string) *apiCheckpointReader {
	return &apiCheckpointReader{
		client:       client,
		repoID:       repoID,
		ownerRepo:    explainRepoRef(forge, owner, repo),
		repoFullName: owner + "/" + repo,
	}
}

// --- wire shapes ------------------------------------------------------
//
// Only the fields the CLI renders are declared. These mirror entire-api's
// httpapi.CheckpointInfo / CheckpointSessionInfo; unknown fields are ignored so
// a server-side addition can't break the read.

type apiCheckpointEnvelope struct {
	Checkpoint   *apiCheckpointInfo `json:"checkpoint"`
	RepoFullName string             `json:"repo_full_name"`
}

// apiCommitCheckpointsBody is GET /repos/{repo_id}/commits/{sha}/checkpoints:
// the checkpoints linked to one commit. Its repo_full_name is verified like
// any cell response; only the ID is decoded from each entry, because
// loadDetail then verifies that ID and the repo against an independent
// response before anything is rendered.
type apiCommitCheckpointsBody struct {
	Checkpoints []struct {
		CheckpointID string `json:"checkpointId"`
	} `json:"checkpoints"`
	RepoFullName string `json:"repo_full_name"`
}

type apiCheckpointInfo struct {
	CheckpointID         string                 `json:"checkpointId"`
	CommitSha            string                 `json:"commitSha"`
	CommitSubject        string                 `json:"commitSubject"`
	CommitDate           string                 `json:"commitDate"`
	CommitAuthor         string                 `json:"commitAuthor"`
	CommitAuthorUsername *string                `json:"commitAuthorUsername"`
	CreatedAt            string                 `json:"createdAt"`
	FilesTouched         []string               `json:"filesTouched"`
	Sessions             []apiCheckpointSession `json:"sessions"`
	SessionCount         int                    `json:"sessionCount"`
	TotalSteps           int                    `json:"totalSteps"`
	InputTokens          int64                  `json:"inputTokens"`
	CacheCreationTokens  int64                  `json:"cacheCreationTokens"`
	CacheReadTokens      int64                  `json:"cacheReadTokens"`
	OutputTokens         int64                  `json:"outputTokens"`
	APICallCount         int64                  `json:"apiCallCount"`
}

type apiCheckpointSession struct {
	Prompt                    *string               `json:"prompt"`
	Agent                     string                `json:"agent"`
	Model                     string                `json:"model"`
	Kind                      string                `json:"kind"`
	Steps                     int                   `json:"steps"`
	SessionID                 string                `json:"sessionId"`
	CreatedAt                 string                `json:"createdAt"`
	TokenUsage                *apiSessionTokenUsage `json:"tokenUsage"`
	CheckpointTranscriptStart *int                  `json:"checkpointTranscriptStart"`
	SkillEvents               []types.SkillEvent    `json:"skillEvents"`
}

type apiSessionTokenUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	APICallCount        int64 `json:"apiCallCount"`
}

// --- checkpoint tier --------------------------------------------------

// Read fetches the checkpoint envelope and maps it onto the local
// CheckpointSummary shape the renderers consume.
func (r *apiCheckpointReader) Read(ctx context.Context, checkpointID id.CheckpointID) (*checkpoint.CheckpointSummary, error) {
	info, err := r.loadDetail(ctx, checkpointID)
	if err != nil {
		return nil, err
	}

	// Sessions carries cardinality only: the renderers use len(Sessions) and
	// index into it, and read the actual per-session data through
	// SessionReader. The local shape's path fields name blobs in a git tree,
	// which has no meaning over HTTP, so they stay empty rather than carrying
	// invented paths a caller might try to resolve.
	sessions := make([]checkpoint.SessionFilePaths, len(info.Sessions))

	// Branch is deliberately left empty. The local shape's Branch is the branch
	// the checkpoint was CREATED on; the cell reports the branches that
	// currently CONTAIN the commit (a checkpoint made on a feature branch and
	// since merged reports "main"). Those are different facts, and putting the
	// second one behind the first one's JSON field would quietly mislead every
	// consumer of `explain --json`.
	return &checkpoint.CheckpointSummary{
		// The server's own checkpointId, not the requested param: loadDetail has
		// already verified the two agree, but sourcing this from info keeps a
		// future regression in that check from silently papering over a real
		// mismatch by relabeling foreign data with the id the caller asked for.
		CheckpointID:     id.CheckpointID(info.CheckpointID),
		Strategy:         "manual-commit",
		CommitSHA:        info.CommitSha,
		CheckpointsCount: info.TotalSteps,
		FilesTouched:     info.FilesTouched,
		Sessions:         sessions,
		TokenUsage: &types.TokenUsage{
			InputTokens:         int(info.InputTokens),
			CacheCreationTokens: int(info.CacheCreationTokens),
			CacheReadTokens:     int(info.CacheReadTokens),
			OutputTokens:        int(info.OutputTokens),
			APICallCount:        int(info.APICallCount),
		},
	}, nil
}

// List is not supported: enumerating a foreign repo's checkpoints is a
// different product surface (`entire search`), and cross-repo explain requires
// a full checkpoint ID precisely so no prefix resolution — the only thing that
// needs a listing — is involved.
func (r *apiCheckpointReader) List(context.Context) ([]checkpoint.CheckpointInfo, error) {
	return nil, errors.New("listing another repo's checkpoints is not supported; use `entire search`")
}

// GetCheckpointAuthor satisfies checkpoint.AuthorReader so the renderer can
// attribute a foreign checkpoint without any local commit to read it from.
func (r *apiCheckpointReader) GetCheckpointAuthor(ctx context.Context, checkpointID id.CheckpointID) (checkpoint.Author, error) {
	info, err := r.loadDetail(ctx, checkpointID)
	if err != nil {
		return checkpoint.Author{}, err
	}
	name := info.CommitAuthor
	if name == "" && info.CommitAuthorUsername != nil {
		name = *info.CommitAuthorUsername
	}
	// The cell exposes the commit author's display name and forge username,
	// never their email, so Email stays empty instead of being guessed.
	return checkpoint.Author{Name: name}, nil
}

// checkpointCommit returns the commit the checkpoint is anchored to, as
// reported by the cell. Cross-repo explain has no local commit to walk for
// this, so the renderer is fed the server's view instead of an empty list —
// which would otherwise render as "(none on this branch)" and read as if the
// foreign checkpoint were uncommitted.
func (r *apiCheckpointReader) checkpointCommit(ctx context.Context, checkpointID id.CheckpointID) ([]associatedCommit, error) {
	info, err := r.loadDetail(ctx, checkpointID)
	if err != nil {
		return nil, err
	}
	sha := strings.TrimSpace(info.CommitSha)
	if sha == "" {
		// Genuinely uncommitted (or not yet linked) — nil omits the row rather
		// than asserting either way.
		return nil, nil
	}
	return []associatedCommit{{
		SHA:      sha,
		ShortSHA: abbreviateSHA(sha),
		Message:  info.CommitSubject,
		Author:   info.CommitAuthor,
		Date:     parseAPITime(info.CommitDate),
	}}, nil
}

// --- task tier --------------------------------------------------------
//
// The cell's checkpoint routes serve the root summary, the sessions, and their
// raw transcripts, but not the tasks/<tool_use_id>/ records. Saying so beats
// reporting an empty list, which would claim the checkpoint had no subagents.

func (r *apiCheckpointReader) ListTasks(context.Context, id.CheckpointID) ([]checkpoint.TaskEntry, error) {
	return nil, r.tasksUnsupported()
}

func (r *apiCheckpointReader) ReadTaskTranscript(context.Context, id.CheckpointID, string) ([]byte, error) {
	return nil, r.tasksUnsupported()
}

func (r *apiCheckpointReader) tasksUnsupported() error {
	return fmt.Errorf("%w: --repo reads %s over the Entire API, which does not serve them; run explain in a clone of that repo", checkpoint.ErrTaskRecordsUnsupported, r.ownerRepo)
}

// --- session tier -----------------------------------------------------

func (r *apiCheckpointReader) ReadSessionMetadata(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, error) {
	meta, _, err := r.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
	return meta, err
}

func (r *apiCheckpointReader) ReadSessionPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (string, error) {
	_, prompts, err := r.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
	return prompts, err
}

// ReadSessionMetadataAndPrompts maps one entry of the checkpoint envelope's
// sessions[] onto the local session Metadata shape. Both narrower accessors
// delegate here so there is one mapping to keep correct.
func (r *apiCheckpointReader) ReadSessionMetadataAndPrompts(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.Metadata, string, error) {
	info, err := r.loadDetail(ctx, checkpointID)
	if err != nil {
		return nil, "", err
	}
	if sessionIndex < 0 || sessionIndex >= len(info.Sessions) {
		return nil, "", fmt.Errorf("session index %d out of range: checkpoint %s has %d session(s)", sessionIndex, checkpointID, len(info.Sessions))
	}
	s := info.Sessions[sessionIndex]

	meta := &checkpoint.Metadata{
		// The server's own checkpointId — see the matching comment in Read().
		CheckpointID:     id.CheckpointID(info.CheckpointID),
		SessionID:        s.SessionID,
		Strategy:         "manual-commit",
		CreatedAt:        parseAPITime(s.CreatedAt, info.CreatedAt),
		CommitSHA:        info.CommitSha,
		CheckpointsCount: s.Steps,
		FilesTouched:     info.FilesTouched,
		Agent:            types.AgentType(s.Agent),
		Model:            s.Model,
		Kind:             s.Kind,
		SkillEvents:      s.SkillEvents,
	}
	// Absent upstream means "not recorded", which is exactly how a local
	// checkpoint written before the field existed reads: GetTranscriptStart()
	// falls back to 0 and the transcript renders unscoped. Don't invent an
	// offset — a wrong one silently shows the wrong slice.
	if s.CheckpointTranscriptStart != nil {
		meta.CheckpointTranscriptStart = *s.CheckpointTranscriptStart
	}
	if s.TokenUsage != nil {
		meta.TokenUsage = &types.TokenUsage{
			InputTokens:         int(s.TokenUsage.InputTokens),
			CacheCreationTokens: int(s.TokenUsage.CacheCreationTokens),
			CacheReadTokens:     int(s.TokenUsage.CacheReadTokens),
			OutputTokens:        int(s.TokenUsage.OutputTokens),
			APICallCount:        int(s.TokenUsage.APICallCount),
		}
	}

	prompts := ""
	if s.Prompt != nil {
		prompts = *s.Prompt
	}
	return meta, prompts, nil
}

// ReadSessionContent pairs the session's metadata with its stored transcript
// bytes. The bytes come from the raw endpoint, not the parsed one: explain
// documents --transcript as the same bytes as --raw-transcript, and the parsed
// message-tree view is a derived shape that would quietly break that promise.
func (r *apiCheckpointReader) ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*checkpoint.SessionContent, error) {
	meta, prompts, err := r.ReadSessionMetadataAndPrompts(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	transcript, err := r.readRawTranscript(ctx, checkpointID, sessionIndex)
	if err != nil {
		return nil, err
	}
	return &checkpoint.SessionContent{
		Metadata:   *meta,
		Transcript: transcript,
		Prompts:    prompts,
	}, nil
}

// readRawTranscript streams one session's stored transcript bytes.
func (r *apiCheckpointReader) readRawTranscript(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) ([]byte, error) {
	path := fmt.Sprintf("/api/v1/repos/%s/checkpoints/%s/transcript/raw?%s",
		url.PathEscape(r.repoID), url.PathEscape(checkpointID.String()),
		url.Values{"session": []string{strconv.Itoa(sessionIndex)}}.Encode())

	resp, err := r.client.GetStream(ctx, path, nil)
	if err != nil {
		return nil, fmt.Errorf("read transcript for checkpoint %s from %s: %w", checkpointID, r.ownerRepo, err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		// A missing transcript blob is a real, reportable state (the
		// checkpoint exists but its bytes were never stored or have been
		// pruned) — distinct from the checkpoint itself being absent, which
		// Read already reported.
		if api.IsHTTPErrorStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("checkpoint %s in %s has no stored transcript", checkpointID, r.ownerRepo)
		}
		return nil, fmt.Errorf("read transcript for checkpoint %s from %s: %w", checkpointID, r.ownerRepo, err)
	}

	// LimitReader+1 so a transcript exactly at the cap is not mistaken for an
	// oversized one, and an oversized one is an error rather than a silent
	// truncation into the renderer.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPITranscriptBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read transcript for checkpoint %s from %s: %w", checkpointID, r.ownerRepo, err)
	}
	if len(body) > maxAPITranscriptBytes {
		return nil, fmt.Errorf("transcript for checkpoint %s in %s exceeds the %d MB read limit", checkpointID, r.ownerRepo, maxAPITranscriptBytes>>20)
	}
	return body, nil
}

// --- commit → checkpoint ------------------------------------------------

// resolveCommitCheckpoint maps a full commit SHA to the checkpoint that
// produced it, through the owning repo's cell. Exactly one linked checkpoint
// is required: the listing's order is not a contract, so several cannot be
// picked from and are named instead (the local path reads the first trailer,
// so a squash commit explains locally but not from here). Only the listing's
// repo identity is verified; loadDetail verifies repo and checkpoint ID
// against an independent response before anything is rendered.
func (r *apiCheckpointReader) resolveCommitCheckpoint(ctx context.Context, sha string) (id.CheckpointID, error) {
	// Lowercase: the server canonicalizes SHAs and the route is not case-blind.
	sha = strings.ToLower(sha)
	short := abbreviateSHA(sha)

	path := fmt.Sprintf("/api/v1/repos/%s/commits/%s/checkpoints",
		url.PathEscape(r.repoID), url.PathEscape(sha))
	resp, err := r.client.Get(ctx, path)
	if err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("resolve commit %s in %s: %w", short, r.ownerRepo, err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		if api.IsHTTPErrorStatus(err, http.StatusNotFound) {
			return id.EmptyCheckpointID, r.notIngestedError("commit " + short)
		}
		if api.IsHTTPErrorStatus(err, http.StatusForbidden) {
			return id.EmptyCheckpointID, r.forbiddenError()
		}
		return id.EmptyCheckpointID, fmt.Errorf("resolve commit %s in %s: %w", short, r.ownerRepo, err)
	}

	var body apiCommitCheckpointsBody
	if err := api.DecodeJSON(resp, &body); err != nil {
		return id.EmptyCheckpointID, fmt.Errorf("resolve commit %s in %s: %w", short, r.ownerRepo, err)
	}
	if err := r.verifyRepoIdentity(body.RepoFullName, "commit "+short); err != nil {
		return id.EmptyCheckpointID, err
	}

	switch len(body.Checkpoints) {
	case 0:
		return id.EmptyCheckpointID, fmt.Errorf("commit %s in %s has no linked Entire checkpoint: it may have been made without Entire, or Entire may not have finished ingesting its checkpoint", short, r.ownerRepo)
	case 1:
		cid, err := id.NewCheckpointID(body.Checkpoints[0].CheckpointID)
		if err != nil {
			return id.EmptyCheckpointID, fmt.Errorf("commit %s in %s links a checkpoint the server named unusably: %w (this looks like a server-side bug, please report it)", short, r.ownerRepo, err)
		}
		return cid, nil
	default:
		ids := make([]string, 0, len(body.Checkpoints))
		for i := range body.Checkpoints {
			ids = append(ids, body.Checkpoints[i].CheckpointID)
		}
		return id.EmptyCheckpointID, fmt.Errorf("commit %s in %s links %d checkpoints; rerun with one of their IDs instead of the commit: %s", short, r.ownerRepo, len(ids), strings.Join(ids, ", "))
	}
}

func abbreviateSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (r *apiCheckpointReader) forbiddenError() error {
	return fmt.Errorf("your login cannot read checkpoints in %s", r.ownerRepo)
}

// notIngestedError leads with "not pushed": in an active repo that is by far
// the common cause of a 404, and blaming storage sends readers the wrong way.
func (r *apiCheckpointReader) notIngestedError(what string) error {
	return fmt.Errorf("%s is not available for %s yet: it may not have been pushed, or Entire may not have finished ingesting it", what, r.ownerRepo)
}

// --- envelope fetch ---------------------------------------------------

// loadDetail fetches (and memoizes) the checkpoint envelope.
func (r *apiCheckpointReader) loadDetail(ctx context.Context, checkpointID id.CheckpointID) (*apiCheckpointInfo, error) {
	if r.detail != nil && r.detailID == checkpointID {
		return r.detail, nil
	}

	path := fmt.Sprintf("/api/v1/repos/%s/checkpoints/%s",
		url.PathEscape(r.repoID), url.PathEscape(checkpointID.String()))
	resp, err := r.client.Get(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read checkpoint %s from %s: %w", checkpointID, r.ownerRepo, err)
	}
	defer resp.Body.Close()

	if err := api.CheckResponse(resp); err != nil {
		if api.IsHTTPErrorStatus(err, http.StatusNotFound) {
			// Lead with the cause that is overwhelmingly the common one. A
			// checkpoint only becomes visible here once it has been pushed
			// and ingested, and most local checkpoints in an active repo are
			// not pushed yet.
			return nil, fmt.Errorf("%w. Checkpoints you have not pushed are only readable in the repo that created them", r.notIngestedError("checkpoint "+checkpointID.String()))
		}
		if api.IsHTTPErrorStatus(err, http.StatusForbidden) {
			return nil, r.forbiddenError()
		}
		return nil, fmt.Errorf("read checkpoint %s from %s: %w", checkpointID, r.ownerRepo, err)
	}

	var env apiCheckpointEnvelope
	if err := api.DecodeJSON(resp, &env); err != nil {
		return nil, fmt.Errorf("read checkpoint %s from %s: %w", checkpointID, r.ownerRepo, err)
	}
	if env.Checkpoint == nil {
		return nil, fmt.Errorf("checkpoint %s is not available for %s (the server returned no checkpoint)", checkpointID, r.ownerRepo)
	}
	// Identity first, ahead of every content-based check below: a
	// wrong-repo or wrong-checkpoint response that also happens to trip one
	// of those (zero sessions, say) would otherwise be reported as a fact
	// about the checkpoint the caller asked for -- "checkpoint X in Y has no
	// sessions" when the server never answered about X in Y at all.
	if err := r.verifyResponseIdentity(checkpointID, env); err != nil {
		return nil, err
	}
	if len(env.Checkpoint.Sessions) == 0 {
		return nil, fmt.Errorf("checkpoint %s in %s has no sessions to explain", checkpointID, r.ownerRepo)
	}

	r.detail = env.Checkpoint
	r.detailID = checkpointID
	return r.detail, nil
}

// verifyResponseIdentity checks the envelope's own identity fields
// (repo_full_name, checkpointId) against what was actually requested, before
// the response is cached or rendered. This is the content-layer equivalent of
// cell_target.go's resolveProcessingPlacement self-check at the routing
// layer: a cell that answers with the wrong repo's or wrong checkpoint's data
// — a server bug, a cache-key collision, an authz bug scoping by checkpoint ID
// without cross-checking repo ownership — must produce an error here, not a
// "successful" read of foreign private transcript/session data silently
// labeled as belonging to the repo the caller asked about.
func (r *apiCheckpointReader) verifyResponseIdentity(checkpointID id.CheckpointID, env apiCheckpointEnvelope) error {
	// Byte equality, NOT EqualFold. Case is load-bearing for a checkpoint
	// ID because the two kinds have opposite canonical spellings: a legacy
	// ID is 12 LOWERCASE hex (id.Pattern) and a ULID is canonical
	// UPPERCASE (isULID requires ulid.ParseStrict(s).String() == s). So a
	// fold would accept a non-canonical spelling of a real ID and then --
	// because CheckpointID below is sourced from the server's value -- mint
	// an id.CheckpointID that id.Validate itself rejects. An honest server
	// echoes the ID it was asked for, byte for byte; anything else is the
	// mismatch this function exists to catch.
	if got := env.Checkpoint.CheckpointID; got != checkpointID.String() {
		return fmt.Errorf("checkpoint identity mismatch: requested checkpoint %s from %s, but the server returned checkpoint %q; refusing to display possibly-mismatched data (this looks like a server-side bug, please report it)",
			checkpointID, r.ownerRepo, got)
	}
	return r.verifyRepoIdentity(env.RepoFullName, "checkpoint "+checkpointID.String())
}

// verifyRepoIdentity checks a response's repo_full_name against the repo this
// reader was built for. what names the thing that was requested ("checkpoint
// <id>", "commit <sha>") for the message.
//
// repo_full_name is REQUIRED, not best-effort. It is the only field that ties
// a response to the repo that was asked about: an echoed checkpoint ID or
// commit SHA alone proves nothing, since any response -- including one
// carrying another repo's private transcripts -- satisfies it by echoing what
// it was handed. Tolerating an empty value would let the verified party opt
// out of its own verification, which is not verification. Every 200 from a
// real cell carries it (checked against aws-us-east-2: "entireio/cli").
func (r *apiCheckpointReader) verifyRepoIdentity(got, what string) error {
	if got == "" {
		return fmt.Errorf("response identity unverifiable: requested %s from %s, but the server returned no repo_full_name to confirm which repo answered; refusing to display possibly-mismatched data (this looks like a server-side bug, please report it)",
			what, r.ownerRepo)
	}
	// entire-api (repoFullNameOr) legitimately falls back to echoing the bare
	// repo ULID when the repo's metadata has not resolved a display name yet,
	// so either form is an honest claim to be the requested repo. EqualFold is
	// right for repoFullName -- forge owner/repo names are case-insensitive -- and
	// the repo ULID is compared byte-for-byte, canonical uppercase like any
	// other ULID.
	if !strings.EqualFold(got, r.repoFullName) && got != r.repoID {
		return fmt.Errorf("response identity mismatch: requested %s from %s, but the server returned data for repo %q; refusing to display possibly-mismatched data (this looks like a server-side bug, please report it)",
			what, r.ownerRepo, got)
	}
	return nil
}

// parseAPITime parses the first parseable RFC3339 timestamp from the given
// candidates. Returns the zero time when none parse, which renders as "unknown"
// rather than as a wrong date.
func parseAPITime(candidates ...string) time.Time {
	for _, c := range candidates {
		if c = strings.TrimSpace(c); c != "" {
			if t, err := time.Parse(time.RFC3339, c); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}
