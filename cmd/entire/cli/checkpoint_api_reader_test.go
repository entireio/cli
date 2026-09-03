package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

const (
	testAPIRepoID   = "01KVBJCWYA4YW6J5M9GP655HZ9"
	testAPIOwnerRep = "acme/widgets"
)

var testAPICheckpointID = id.CheckpointID("01KXGTTNGCEACC83QZEJ5YAF0D")

// checkpointEnvelopeJSON mirrors a real cell response for
// GET /repos/{repo_id}/checkpoints/{checkpoint_id}, trimmed to the fields the
// CLI reads. `branches` is deliberately present and different from the
// checkpoint's creation branch — see TestAPICheckpointReader_ReadLeavesBranchEmpty.
const checkpointEnvelopeJSON = `{
  "checkpoint_id": "01KXGTTNGCEACC83QZEJ5YAF0D",
  "branches": ["main"],
  "default_branch": "main",
  "suggested_branch": "main",
  "repo_full_name": "acme/widgets",
  "checkpoint": {
    "checkpointId": "01KXGTTNGCEACC83QZEJ5YAF0D",
    "commitSha": "13e379e4b0000000000000000000000000000000",
    "commitSubject": "docs(readme): document headless auth",
    "commitDate": "2026-07-14T17:30:13.000Z",
    "commitAuthor": "Peyton Montei",
    "commitAuthorUsername": "peyton-alt",
    "createdAt": "2026-07-14T17:30:22.661Z",
    "filesTouched": ["README.md", "cmd/entire/cli/auth.go"],
    "sessionCount": 2,
    "totalSteps": 4,
    "inputTokens": 468,
    "cacheCreationTokens": 1381547,
    "cacheReadTokens": 70839522,
    "outputTokens": 197680,
    "apiCallCount": 252,
    "sessions": [
      {
        "prompt": "first session prompt",
        "agent": "Claude Code",
        "model": "claude-fable-5",
        "steps": 1,
        "sessionId": "session-one",
        "createdAt": "2026-07-14T17:00:00.000Z",
        "checkpointTranscriptStart": 7,
        "tokenUsage": {"inputTokens": 100, "cacheCreationTokens": 2, "cacheReadTokens": 3, "outputTokens": 4, "apiCallCount": 5}
      },
      {
        "prompt": "second session prompt",
        "agent": "Claude Code",
        "steps": 3,
        "sessionId": "session-two",
        "createdAt": "2026-07-14T17:30:22.661Z"
      }
    ]
  }
}`

// newTestAPIReader wires an apiCheckpointReader at an httptest server. The
// returned recorder captures every requested path so tests can assert which
// endpoint a read actually hit.
func newTestAPIReader(t *testing.T, handler http.HandlerFunc) (*apiCheckpointReader, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	client := api.NewClientWithBaseURL("test-token", srv.URL)
	return newAPICheckpointReader(client, testAPIRepoID, testAPIOwnerRep), &paths
}

// defaultAPIHandler serves the envelope and a raw transcript per session index.
func defaultAPIHandler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/transcript/raw"):
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintf(w, `{"type":"user","session":%q}`+"\n", r.URL.Query().Get("session"))
	case strings.Contains(r.URL.Path, "/checkpoints/"):
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, checkpointEnvelopeJSON)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestAPICheckpointReader_ReadMapsEnvelope(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, defaultAPIHandler)
	summary, err := reader.Read(context.Background(), testAPICheckpointID)
	require.NoError(t, err)

	assert.Equal(t, testAPICheckpointID, summary.CheckpointID)
	assert.Equal(t, "13e379e4b0000000000000000000000000000000", summary.CommitSHA)
	assert.Equal(t, 4, summary.CheckpointsCount, "checkpoints_count comes from totalSteps")
	assert.Equal(t, []string{"README.md", "cmd/entire/cli/auth.go"}, summary.FilesTouched)
	assert.Len(t, summary.Sessions, 2, "one entry per session so index-based reads and len() work")
	require.NotNil(t, summary.TokenUsage)
	assert.Equal(t, 468, summary.TokenUsage.InputTokens)
	assert.Equal(t, 1381547, summary.TokenUsage.CacheCreationTokens)
	assert.Equal(t, 70839522, summary.TokenUsage.CacheReadTokens)
	assert.Equal(t, 197680, summary.TokenUsage.OutputTokens)
	assert.Equal(t, 252, summary.TokenUsage.APICallCount)
}

// The cell reports branches CONTAINING the commit, which is a different fact
// from the local Branch field's "branch the checkpoint was created on" (a
// checkpoint made on a feature branch and since merged reports "main"). Putting
// the former behind the latter's JSON key would mislead every consumer of
// `explain --json`, so it stays empty.
func TestAPICheckpointReader_ReadLeavesBranchEmpty(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, defaultAPIHandler)
	summary, err := reader.Read(context.Background(), testAPICheckpointID)
	require.NoError(t, err)
	assert.Empty(t, summary.Branch, "must not report a containing branch as the creation branch")

	meta, err := reader.ReadSessionMetadata(context.Background(), testAPICheckpointID, 0)
	require.NoError(t, err)
	assert.Empty(t, meta.Branch)
}

func TestAPICheckpointReader_ReadSessionMetadataAndPrompts(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, defaultAPIHandler)

	meta, prompts, err := reader.ReadSessionMetadataAndPrompts(context.Background(), testAPICheckpointID, 0)
	require.NoError(t, err)
	assert.Equal(t, "session-one", meta.SessionID)
	assert.Equal(t, "Claude Code", string(meta.Agent))
	assert.Equal(t, "claude-fable-5", meta.Model)
	assert.Equal(t, 1, meta.CheckpointsCount, "per-session count comes from steps")
	assert.Equal(t, 7, meta.GetTranscriptStart(), "transcript scoping offset must survive the mapping")
	assert.Equal(t, "first session prompt", prompts)
	require.NotNil(t, meta.TokenUsage)
	assert.Equal(t, 100, meta.TokenUsage.InputTokens)
	assert.Equal(t, "2026-07-14T17:00:00Z", meta.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"))

	// A session with no recorded offset reads unscoped (offset 0), exactly like
	// a local checkpoint written before the field existed — not an invented one.
	meta, prompts, err = reader.ReadSessionMetadataAndPrompts(context.Background(), testAPICheckpointID, 1)
	require.NoError(t, err)
	assert.Equal(t, "session-two", meta.SessionID)
	assert.Equal(t, 0, meta.GetTranscriptStart())
	assert.Equal(t, "second session prompt", prompts)
	assert.Nil(t, meta.TokenUsage)
}

func TestAPICheckpointReader_SessionIndexOutOfRange(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, defaultAPIHandler)
	_, err := reader.ReadSessionMetadata(context.Background(), testAPICheckpointID, 5)
	require.ErrorContains(t, err, "session index 5 out of range")
	_, err = reader.ReadSessionMetadata(context.Background(), testAPICheckpointID, -1)
	require.ErrorContains(t, err, "out of range")
}

// The transcript must come from the raw endpoint, byte-for-byte. explain
// documents --transcript as the same bytes as --raw-transcript; sourcing it from
// the parsed message-tree endpoint would quietly break that promise.
func TestAPICheckpointReader_ReadSessionContentUsesRawEndpoint(t *testing.T) {
	t.Parallel()

	reader, paths := newTestAPIReader(t, defaultAPIHandler)
	content, err := reader.ReadSessionContent(context.Background(), testAPICheckpointID, 1)
	require.NoError(t, err)

	// Compared as bytes, not JSON: byte-exactness is the contract under test.
	assert.JSONEq(t, `{"type":"user","session":"1"}`+"\n", string(content.Transcript))
	assert.Equal(t, "second session prompt", content.Prompts)
	assert.Equal(t, "session-two", content.Metadata.SessionID)

	joined := strings.Join(*paths, " ")
	assert.Contains(t, joined, "/transcript/raw?session=1", "must request the raw endpoint for the asked-for session")
	assert.NotContains(t, joined, "/transcript?", "must not read transcript bytes from the parsed endpoint")
}

// One envelope fetch backs every read tier: explain reads the summary and then
// each session, so a per-call fetch would turn one view into an N+1.
func TestAPICheckpointReader_CachesEnvelope(t *testing.T) {
	t.Parallel()

	reader, paths := newTestAPIReader(t, defaultAPIHandler)
	ctx := context.Background()
	_, err := reader.Read(ctx, testAPICheckpointID)
	require.NoError(t, err)
	_, err = reader.ReadSessionMetadata(ctx, testAPICheckpointID, 0)
	require.NoError(t, err)
	_, err = reader.ReadSessionPrompts(ctx, testAPICheckpointID, 1)
	require.NoError(t, err)
	_, err = reader.GetCheckpointAuthor(ctx, testAPICheckpointID)
	require.NoError(t, err)

	detailFetches := 0
	for _, p := range *paths {
		if !strings.Contains(p, "/transcript") {
			detailFetches++
		}
	}
	assert.Equal(t, 1, detailFetches, "envelope should be fetched once across all read tiers")
}

func TestAPICheckpointReader_AuthorAndCommit(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, defaultAPIHandler)
	ctx := context.Background()

	author, err := reader.GetCheckpointAuthor(ctx, testAPICheckpointID)
	require.NoError(t, err)
	assert.Equal(t, "Peyton Montei", author.Name)
	assert.Empty(t, author.Email, "the cell never exposes the author's email; don't invent one")

	commits, err := reader.checkpointCommit(ctx, testAPICheckpointID)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	assert.Equal(t, "13e379e", commits[0].ShortSHA)
	assert.Equal(t, "docs(readme): document headless auth", commits[0].Message)
	assert.Equal(t, "Peyton Montei", commits[0].Author)
	assert.Equal(t, 2026, commits[0].Date.Year())
}

// A 404 means "not readable from here", and by far the most common reason is a
// checkpoint that was never pushed. The message must say so rather than blaming
// a storage backend, which sends the reader looking in the wrong place.
func TestAPICheckpointReader_NotFoundExplainsUnpushed(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":"checkpoint not found"}`)
	})
	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may not have been pushed")
	assert.Contains(t, err.Error(), testAPIOwnerRep)
	assert.NotContains(t, strings.ToLower(err.Error()), "branch-based")
}

func TestAPICheckpointReader_ForbiddenNamesAccess(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.ErrorContains(t, err, "cannot read checkpoints in acme/widgets")
}

func TestAPICheckpointReader_EmptyEnvelopeIsNotFound(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"checkpoint": null}`)
	})
	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.ErrorContains(t, err, "not available")
}

// If the cell ever answers with a DIFFERENT repo's checkpoint data than the
// one requested (server bug, cache-key collision, authz bug), the reader must
// refuse it instead of caching and rendering it labeled as the requested
// repo. This is the reproduction for the cross-repo checkpoint identity gap.
func TestAPICheckpointReader_RepoFullNameMismatchIsRejected(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			fmt.Fprint(w, `{"type":"user"}`)
			return
		}
		// Same checkpoint payload, but the envelope claims a DIFFERENT repo than
		// the one the reader was constructed for (testAPIOwnerRep = acme/widgets).
		fmt.Fprint(w, strings.Replace(checkpointEnvelopeJSON, `"repo_full_name": "acme/widgets"`, `"repo_full_name": "totally-different/other-repo"`, 1))
	})

	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.Error(t, err, "a wrong-repo response must not be accepted as the requested repo's checkpoint")
	assert.Contains(t, err.Error(), "identity mismatch")
	assert.Contains(t, err.Error(), "other-repo")

	// The same guard applies to every read tier that flows through loadDetail,
	// not just Read().
	_, _, err = reader.ReadSessionMetadataAndPrompts(context.Background(), testAPICheckpointID, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity mismatch")
}

// The repo's own ULID is a legitimate stand-in for repo_full_name (entire-api
// falls back to it when the repo's display name hasn't resolved yet), so it
// must NOT be rejected as a mismatch.
func TestAPICheckpointReader_RepoFullNameAsRepoIDIsAccepted(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			fmt.Fprint(w, `{"type":"user"}`)
			return
		}
		fmt.Fprint(w, strings.Replace(checkpointEnvelopeJSON, `"repo_full_name": "acme/widgets"`, `"repo_full_name": "`+testAPIRepoID+`"`, 1))
	})

	summary, err := reader.Read(context.Background(), testAPICheckpointID)
	require.NoError(t, err)
	assert.Equal(t, testAPICheckpointID, summary.CheckpointID)
}

// If the cell ever answers with a DIFFERENT checkpoint than the one
// requested, the reader must refuse it rather than relabeling the mismatched
// content with the requested ID.
func TestAPICheckpointReader_CheckpointIDMismatchIsRejected(t *testing.T) {
	t.Parallel()

	const otherCheckpointID = "01KXGTTNGCEACC83QZEJ5YAFOTHER"
	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			fmt.Fprint(w, `{"type":"user"}`)
			return
		}
		fmt.Fprint(w, strings.Replace(checkpointEnvelopeJSON, `"checkpointId": "01KXGTTNGCEACC83QZEJ5YAF0D"`, `"checkpointId": "`+otherCheckpointID+`"`, 1))
	})

	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.Error(t, err, "a wrong-checkpoint response must not be accepted as the requested checkpoint")
	assert.Contains(t, err.Error(), "identity mismatch")
	assert.Contains(t, err.Error(), otherCheckpointID)
}

func TestAPICheckpointReader_NoSessionsIsAnError(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"repo_full_name":"acme/widgets","checkpoint": {"checkpointId":"01KXGTTNGCEACC83QZEJ5YAF0D","sessions":[]}}`)
	})
	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.ErrorContains(t, err, "no sessions to explain")
}

// A wrong-checkpoint response that ALSO trips a content-based check must be
// reported as the identity mismatch it is, not as a fact about the checkpoint
// the caller asked for. With the identity check ordered after the
// zero-sessions guard, this payload produced "checkpoint <requested> in
// acme/widgets has no sessions to explain" -- a statement about a checkpoint
// the server never answered about, and the misleading error Copilot flagged
// on the PR.
func TestAPICheckpointReader_IdentityCheckedBeforeContentGuards(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"repo_full_name":"totally-different/other-repo","checkpoint": {"checkpointId":"01KXGTTNGCEACC83QZEJ5YAFOTHER","sessions":[]}}`)
	})
	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "identity mismatch")
	assert.NotContains(t, err.Error(), "no sessions to explain",
		"a foreign response must not be described as a property of the requested checkpoint")
}

// repo_full_name is required, not best-effort. A response that simply omits
// it used to skip the repo check entirely, leaving only checkpointId -- which
// any wrong-repo response satisfies by echoing the ID it was handed. That made
// the guard something the verified party could opt out of: this exact payload
// rendered as acme/widgets data before the fix.
func TestAPICheckpointReader_MissingRepoFullNameIsRejected(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			fmt.Fprint(w, `{"type":"user"}`)
			return
		}
		fmt.Fprint(w, strings.Replace(checkpointEnvelopeJSON,
			`"repo_full_name": "acme/widgets"`, `"repo_full_name": ""`, 1))
	})

	_, err := reader.Read(context.Background(), testAPICheckpointID)
	require.Error(t, err, "a response that does not say which repo it answered for must not render as the requested repo's data")
	assert.Contains(t, err.Error(), "identity unverifiable")
}

// The checkpoint-ID comparison is byte equality, deliberately. The two ID
// kinds have opposite canonical spellings -- a legacy ID is 12 lowercase hex
// (id.Pattern), a ULID is canonical uppercase (isULID requires
// ParseStrict(s).String() == s) -- so folding case would accept a
// non-canonical spelling and, because CheckpointID is sourced from the
// server's value, mint an id.CheckpointID that id.Validate itself rejects.
// This pins the legacy-hex case, the one a fold actually breaks.
func TestAPICheckpointReader_LegacyHexIDIsCaseSensitive(t *testing.T) {
	t.Parallel()

	const legacyID = "abc123def456"
	require.NoError(t, id.Validate(legacyID), "fixture must be a valid legacy ID")
	upper := strings.ToUpper(legacyID)
	require.Error(t, id.Validate(upper), "the uppercased form must itself be invalid, which is the point")

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			fmt.Fprint(w, `{"type":"user"}`)
			return
		}
		fmt.Fprint(w, strings.Replace(checkpointEnvelopeJSON,
			`"checkpointId": "01KXGTTNGCEACC83QZEJ5YAF0D"`, `"checkpointId": "`+upper+`"`, 1))
	})

	_, err := reader.Read(context.Background(), id.CheckpointID(legacyID))
	require.Error(t, err, "an uppercased legacy ID is not the ID that was requested")
	assert.Contains(t, err.Error(), "identity mismatch")
}

// A transcript larger than the read cap must fail loudly. Truncating it would
// hand the renderer a silently-incomplete transcript, which reads as a real one.
func TestAPICheckpointReader_OversizeTranscriptFailsLoudly(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			chunk := strings.Repeat("x", 1<<20)
			for range (maxAPITranscriptBytes >> 20) + 1 {
				if _, err := w.Write([]byte(chunk)); err != nil {
					return
				}
			}
			return
		}
		fmt.Fprint(w, checkpointEnvelopeJSON)
	})
	_, err := reader.ReadSessionContent(context.Background(), testAPICheckpointID, 0)
	require.ErrorContains(t, err, "exceeds the")
}

func TestAPICheckpointReader_MissingTranscriptBlob(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/transcript/raw") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, checkpointEnvelopeJSON)
	})
	_, err := reader.ReadSessionContent(context.Background(), testAPICheckpointID, 0)
	require.ErrorContains(t, err, "has no stored transcript")
}

// List would mean enumerating another repo's checkpoints, which is `entire
// search`'s job. Cross-repo explain requires a full ID so nothing needs it.
func TestAPICheckpointReader_ListUnsupported(t *testing.T) {
	t.Parallel()

	reader, _ := newTestAPIReader(t, defaultAPIHandler)
	_, err := reader.List(context.Background())
	require.ErrorContains(t, err, "not supported")
}

// The reader must not be a Writer: a foreign repo's checkpoint is read-only
// from here, and keeping Write off the type makes that structural rather than a
// rule the flag layer has to remember.
func TestAPICheckpointReader_IsNotAWriter(t *testing.T) {
	t.Parallel()

	var anyReader any = newAPICheckpointReader(nil, testAPIRepoID, testAPIOwnerRep)
	_, isWriter := anyReader.(checkpoint.Writer)
	assert.False(t, isWriter, "apiCheckpointReader must not implement checkpoint.Writer")
	_, isStore := anyReader.(checkpoint.PersistentStore)
	assert.False(t, isStore, "apiCheckpointReader must not satisfy PersistentStore")
}

func TestParseAPITime(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 2026, parseAPITime("2026-07-14T17:30:22.661Z").Year())
	assert.Equal(t, 2026, parseAPITime("", "not-a-time", "2026-07-14T17:30:22Z").Year(),
		"falls through to the first parseable candidate")
	assert.True(t, parseAPITime("", "nonsense").IsZero(),
		"an unparseable timestamp must stay zero rather than becoming a wrong date")
}
