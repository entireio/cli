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
	// ownerRepo is the display coordinate ("owner/name") used in errors.
	ownerRepo string

	// detail caches the checkpoint envelope. Every read tier is derived from
	// this one response, and explain reads the summary and then each session,
	// so caching turns an N+1 into a single request.
	detail   *apiCheckpointInfo
	detailID id.CheckpointID
}

// newAPICheckpointReader returns a reader for repoID's checkpoints on the cell
// that client is already pointed at.
func newAPICheckpointReader(client *api.Client, repoID, ownerRepo string) *apiCheckpointReader {
	return &apiCheckpointReader{client: client, repoID: repoID, ownerRepo: ownerRepo}
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
		CheckpointID:     checkpointID,
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
	short := sha
	if len(short) > 7 {
		short = short[:7]
	}
	return []associatedCommit{{
		SHA:      sha,
		ShortSHA: short,
		Message:  info.CommitSubject,
		Author:   info.CommitAuthor,
		Date:     parseAPITime(info.CommitDate),
	}}, nil
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
		CheckpointID:     checkpointID,
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

// ReadTaskRecords reports no task records: the API exposes no per-subagent
// task record endpoint, so a checkpoint read this way has no attributable
// subagent work.
func (r *apiCheckpointReader) ReadTaskRecords(context.Context, id.CheckpointID) ([]checkpoint.StoredTaskRecord, error) {
	return nil, nil
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
			return nil, fmt.Errorf("checkpoint %s is not available for %s yet: it may not have been pushed, or Entire may not have finished ingesting it. Checkpoints you have not pushed are only readable in the repo that created them", checkpointID, r.ownerRepo)
		}
		if api.IsHTTPErrorStatus(err, http.StatusForbidden) {
			return nil, fmt.Errorf("your login cannot read checkpoints in %s", r.ownerRepo)
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
	if len(env.Checkpoint.Sessions) == 0 {
		return nil, fmt.Errorf("checkpoint %s in %s has no sessions to explain", checkpointID, r.ownerRepo)
	}

	r.detail = env.Checkpoint
	r.detailID = checkpointID
	return r.detail, nil
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
