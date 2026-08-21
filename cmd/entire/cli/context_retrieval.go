package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
	"github.com/entireio/cli/internal/coreapi"
	"github.com/entireio/cli/redact"
	"github.com/spf13/cobra"
)

const (
	maxContextQueryBytes      = 512
	maxContextReposPerRequest = 40
	maxContextScopeSources    = 640
	maxContextFanoutGroups    = 16
	localContextRecentWindow  = 2 * time.Hour
	maxLocalTranscriptBytes   = 4 << 20
	contextEvidenceRetention  = 30 * 24 * time.Hour
	maxContextEvidenceFiles   = 200
	maxContextEvidenceBytes   = 16 << 20
)

type contextEvidence struct {
	ID           string   `json:"id"`
	SourceType   string   `json:"sourceType"`
	RepoID       string   `json:"repoId"`
	RepoName     string   `json:"repoName"`
	CheckpointID string   `json:"checkpointId,omitempty"`
	SessionID    string   `json:"sessionId,omitempty"`
	Timestamp    string   `json:"timestamp,omitempty"`
	Summary      string   `json:"summary"`
	Excerpt      string   `json:"excerpt,omitempty"`
	Files        []string `json:"files,omitempty"`
	DrillDown    string   `json:"drillDown"`
	Score        float64  `json:"-"`
}

func redactContextQuery(query string) string {
	return truncateUTF8Bytes(sanitizeEvidenceText(redact.String(query)), maxContextQueryBytes)
}

func truncateUTF8Bytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

func stripEntireContextBlocks(value string) string {
	const start, end = "<entire-context>", "</entire-context>"
	for {
		lower := strings.ToLower(value)
		i := strings.Index(lower, start)
		if i < 0 {
			j := strings.Index(lower, end)
			if j < 0 {
				return value
			}
			value = value[:j] + value[j+len(end):]
			continue
		}
		j := strings.Index(lower[i+len(start):], end)
		if j < 0 {
			return value[:i]
		}
		value = value[:i] + value[i+len(start)+j+len(end):]
	}
}

func sanitizeEvidenceText(value string) string {
	value = stripEntireContextBlocks(value)
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (!unicode.IsControl(r) && r != utf8.RuneError) {
			return r
		}
		return -1
	}, value)
	return strings.TrimSpace(value)
}

func contextSourceGroups(sources []coreapi.ContextSharingSource) []cellGroup {
	if len(sources) > maxContextScopeSources {
		return nil
	}
	byCell := make(map[string]*cellGroup)
	for _, source := range sources {
		id := strings.TrimSpace(source.RepoId)
		if id == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(source.Cell)) + "\x00" + strings.ToLower(strings.TrimSpace(source.Jurisdiction)) + "\x00" + strings.ToLower(strings.TrimSpace(source.ClusterSlug))
		group := byCell[key]
		if group == nil {
			group = &cellGroup{cell: source.Cell, jurisdiction: source.Jurisdiction, clusterSlug: source.ClusterSlug}
			byCell[key] = group
		}
		group.repoIDs = append(group.repoIDs, id)
	}
	keys := make([]string, 0, len(byCell))
	for key := range byCell {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []cellGroup
	for _, key := range keys {
		group := byCell[key]
		sort.Strings(group.repoIDs)
		group.repoIDs = compactContextStrings(group.repoIDs)
		for start := 0; start < len(group.repoIDs); start += maxContextReposPerRequest {
			end := min(start+maxContextReposPerRequest, len(group.repoIDs))
			chunk := *group
			chunk.repoIDs = append([]string(nil), group.repoIDs[start:end]...)
			out = append(out, chunk)
			if len(out) > maxContextFanoutGroups {
				return nil
			}
		}
	}
	return out
}

func compactContextStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func contextEvidenceID(sourceType, repoID, resultID string) string {
	sum := sha256.Sum256([]byte(sourceType + "\x00" + repoID + "\x00" + resultID))
	return "ctx_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:10]))
}

func currentContextTarget(ctx context.Context) (string, string, error) {
	_, owner, repo, err := gitremote.ResolveRemoteRepo(ctx, "origin")
	if err != nil {
		return "", "", fmt.Errorf("resolve current repository: %w", err)
	}
	placement, err := resolveRepoCellPlacement(ctx, owner, repo)
	if err != nil {
		return "", "", err
	}
	return placement.RepoID, owner + "/" + repo, nil
}

func remoteContextEvidence(result search.Result, sourceIDs map[string]string) (contextEvidence, bool) {
	if result.Type != search.TypeCheckpoint && result.Type != search.TypeSession {
		return contextEvidence{}, false
	}
	repoName := strings.Trim(strings.TrimSpace(result.ResultOrg())+"/"+strings.TrimSpace(result.ResultRepo()), "/")
	repoID := sourceIDs[strings.ToLower(repoName)]
	if repoID == "" {
		return contextEvidence{}, false
	}
	e := contextEvidence{
		SourceType: result.Type, RepoID: repoID, RepoName: repoName,
		Timestamp: result.ResultCreatedAt(), Summary: sanitizeEvidenceText(result.Meta.Summary),
		Excerpt: sanitizeEvidenceText(result.Meta.Snippet), Score: result.Meta.Score,
	}
	if result.Meta.RerankScore != nil {
		e.Score = *result.Meta.RerankScore
	}
	if e.Summary == "" {
		e.Summary = sanitizeEvidenceText(result.ResultTitle())
	}
	if result.Type == search.TypeCheckpoint && result.Checkpoint != nil {
		e.CheckpointID = result.Checkpoint.ID
		e.Files = append([]string(nil), result.Checkpoint.FilesTouched...)
		e.DrillDown = fmt.Sprintf("entire checkpoint explain %s --repo %s", e.CheckpointID, repoName)
		e.ID = contextEvidenceID(e.SourceType, repoID, e.CheckpointID)
	} else if result.Session != nil {
		e.SessionID = result.Session.SessionID
		e.CheckpointID = result.Session.MatchedCheckpointID
		resultID := e.SessionID
		if resultID == "" {
			resultID = e.CheckpointID
		}
		e.ID = contextEvidenceID(e.SourceType, repoID, resultID)
		if e.CheckpointID != "" {
			e.DrillDown = fmt.Sprintf("entire checkpoint explain %s --repo %s", e.CheckpointID, repoName)
		} else {
			e.DrillDown = "entire context inspect " + e.ID
		}
	}
	e = sanitizeContextEvidence(e)
	return e, e.ID != ""
}

func sanitizeContextEvidence(e contextEvidence) contextEvidence {
	e.SourceType = truncateUTF8Bytes(sanitizeEvidenceText(e.SourceType), 32)
	e.RepoID = truncateUTF8Bytes(sanitizeEvidenceText(e.RepoID), 64)
	e.RepoName = truncateUTF8Bytes(sanitizeEvidenceText(e.RepoName), 512)
	e.CheckpointID = truncateUTF8Bytes(sanitizeEvidenceText(e.CheckpointID), 128)
	e.SessionID = truncateUTF8Bytes(sanitizeEvidenceText(e.SessionID), 128)
	e.Timestamp = truncateUTF8Bytes(sanitizeEvidenceText(e.Timestamp), 64)
	e.Summary = truncateUTF8Bytes(sanitizeEvidenceText(e.Summary), 512)
	e.Excerpt = truncateUTF8Bytes(sanitizeEvidenceText(e.Excerpt), 4096)
	e.DrillDown = truncateUTF8Bytes(sanitizeEvidenceText(e.DrillDown), 1024)
	if len(e.Files) > 50 {
		e.Files = e.Files[:50]
	}
	files := make([]string, 0, len(e.Files))
	for _, file := range e.Files {
		if file = truncateUTF8Bytes(sanitizeEvidenceText(file), 512); file != "" {
			files = append(files, file)
		}
	}
	e.Files = files
	return e
}

func runContextQueryCommand(cmd *cobra.Command, query string, limit int) error {
	if limit < 1 || limit > 10 {
		return errors.New("--limit must be between 1 and 10")
	}
	ctx := cmd.Context()
	targetRepoID, _, err := currentContextTarget(ctx)
	if err != nil {
		return fmt.Errorf("resolve current context target: %w", err)
	}
	coreClient, err := coreapi.New()
	if err != nil {
		return err
	}
	evidence, _, err := retrieveContextEvidence(ctx, coreClient, targetRepoID, query, limit, "", insecureHTTPRequested(cmd))
	if err != nil {
		return contextSharingCommandError(err)
	}
	for i := range evidence {
		if err := persistContextEvidence(ctx, evidence[i]); err != nil {
			return fmt.Errorf("persist context evidence: %w", err)
		}
	}
	if jsonRequested(cmd) {
		data, err := jsonutil.MarshalIndentWithNewline(evidence, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal context evidence: %w", err)
		}
		_, err = cmd.OutOrStdout().Write(data)
		if err != nil {
			return fmt.Errorf("write context evidence: %w", err)
		}
		return nil
	}
	if len(evidence) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No authorized context evidence found.")
		return nil
	}
	return printTable(cmd.OutOrStdout(), []string{"ID", "SOURCE", "REPOSITORY", "SUMMARY", "WHEN"}, evidence, func(item contextEvidence) []string {
		return []string{item.ID, item.SourceType, item.RepoName, truncateUTF8Bytes(item.Summary, 100), item.Timestamp}
	})
}

func retrieveContextEvidence(ctx context.Context, coreClient *coreapi.Client, targetRepoID, query string, limit int, currentSessionID string, insecureHTTP bool) ([]contextEvidence, *coreapi.ResolveContextSharingScopeOutputBody, error) {
	// Scope is resolved before query sanitization or any cell call so a denied
	// policy can never leak prompt-derived text outside the current process.
	scope, err := coreClient.ResolveContextSharingScope(ctx, &coreapi.ResolveContextSharingScopeInputBody{TargetRepoId: targetRepoID})
	if err != nil {
		return nil, nil, err
	}
	if !scope.Enabled {
		return nil, scope, errors.New("cross-repository context sharing is disabled; run `entire context enable`")
	}
	safeQuery := redactContextQuery(query)
	if safeQuery == "" {
		return nil, scope, errors.New("query is empty after redaction")
	}

	if len(scope.Sources) > maxContextScopeSources {
		return nil, scope, fmt.Errorf("context scope has %d sources; maximum is %d", len(scope.Sources), maxContextScopeSources)
	}
	groups := contextSourceGroups(scope.Sources)
	if len(scope.Sources) > 0 && groups == nil {
		return nil, scope, fmt.Errorf("context scope requires more than %d cell requests", maxContextFanoutGroups)
	}
	resolveCellBaseURLs(ctx, coreClient, groups)
	allowedRepoIDs := make(map[string]struct{}, len(scope.Sources))
	for _, source := range scope.Sources {
		allowedRepoIDs[source.RepoId] = struct{}{}
	}
	localCh := make(chan []contextEvidence, 1)
	go func() {
		if !scope.IncludeLocalLive {
			localCh <- nil
			return
		}
		localCh <- loadLocalContextEvidence(ctx, safeQuery, targetRepoID, currentSessionID, allowedRepoIDs)
	}()

	cellResults, fanoutErr := fanOutCells(ctx, insecureHTTP, semanticSearchV4CellTimeout, groups, func(cellCtx context.Context, group cellGroup, client *api.Client) (*search.Response, error) {
		resp, err := search.CellV4(cellCtx, client, search.Config{Query: safeQuery, Limit: limit, Page: 1}, group.repoIDs)
		if resp != nil {
			filtered := resp.Results[:0]
			for _, result := range resp.Results {
				if result.Type == search.TypeCheckpoint || result.Type == search.TypeSession {
					filtered = append(filtered, result)
				}
			}
			resp.Results = filtered
		}
		if err != nil {
			return resp, fmt.Errorf("search context cell: %w", err)
		}
		return resp, nil
	})
	if fanoutErr != nil {
		return nil, scope, fanoutErr
	}
	merged, mergeErr := mergeSemanticV4Responses(ctx, limit, 1, cellResults)
	local := <-localCh
	if err := contextFanoutCompletenessError(merged, mergeErr); err != nil {
		return nil, scope, err
	}
	sourceIDs := make(map[string]string, len(scope.Sources))
	for _, source := range scope.Sources {
		sourceIDs[strings.ToLower(source.FullName)] = source.RepoId
	}
	evidence := make([]contextEvidence, 0, len(local))
	evidence = append(evidence, local...)
	if merged != nil {
		for _, result := range merged.Results {
			if item, ok := remoteContextEvidence(result, sourceIDs); ok {
				evidence = append(evidence, item)
			}
		}
	}
	sortContextEvidence(evidence)
	if len(evidence) > limit {
		evidence = evidence[:limit]
	}
	return evidence, scope, nil
}

func contextFanoutCompletenessError(resp *search.Response, mergeErr error) error {
	if mergeErr != nil {
		return mergeErr
	}
	if resp != nil && len(resp.Warnings) > 0 {
		return fmt.Errorf("context search incomplete: %s", strings.Join(resp.Warnings, "; "))
	}
	return nil
}

func sortContextEvidence(evidence []contextEvidence) {
	sort.SliceStable(evidence, func(i, j int) bool {
		if math.Abs(evidence[i].Score-evidence[j].Score) > 0.000001 {
			return evidence[i].Score > evidence[j].Score
		}
		ti, err := time.Parse(time.RFC3339, evidence[i].Timestamp)
		if err != nil {
			ti = time.Time{}
		}
		tj, err := time.Parse(time.RFC3339, evidence[j].Timestamp)
		if err != nil {
			tj = time.Time{}
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return evidence[i].ID < evidence[j].ID
	})
}

func lexicalScore(query, text string) float64 {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return 0
	}
	lower := strings.ToLower(text)
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		if len(term) > 1 && strings.Contains(lower, term) {
			seen[term] = struct{}{}
		}
	}
	return float64(len(seen)) / float64(len(terms))
}

func localContextSourceAllowed(repoID, targetRepoID, sessionID, currentSessionID string, allowedRepoIDs map[string]struct{}) bool {
	if repoID == targetRepoID || sessionID == currentSessionID {
		return false
	}
	_, ok := allowedRepoIDs[repoID]
	return ok
}

func loadLocalContextEvidence(ctx context.Context, query, targetRepoID, currentSessionID string, allowedRepoIDs map[string]struct{}) []contextEvidence {
	path, err := currentContextRegistryPath(ctx)
	if err != nil {
		return nil
	}
	registry, err := readContextRegistry(path)
	if err != nil {
		return nil
	}
	now := time.Now()
	var out []contextEvidence
	for _, session := range registry.Sessions {
		if !localContextSourceAllowed(session.RepoID, targetRepoID, session.SessionID, currentSessionID, allowedRepoIDs) || now.Sub(session.LastSeen) > localContextRecentWindow {
			continue
		}
		file, err := openTranscriptWithinSessionDir(session.TranscriptPath, session.SessionDir)
		if err != nil {
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, maxLocalTranscriptBytes+1))
		_ = file.Close()
		if readErr != nil || len(raw) > maxLocalTranscriptBytes {
			continue
		}
		redacted, err := redact.JSONLBytes(raw)
		if err != nil {
			continue
		}
		entries, err := summarize.BuildCondensedTranscriptFromBytes(redacted, types.AgentType(session.Agent))
		if err != nil {
			continue
		}
		var lines []string
		for _, entry := range entries {
			if entry.Type != summarize.EntryTypeUser && entry.Type != summarize.EntryTypeAssistant {
				continue
			}
			content := sanitizeEvidenceText(entry.Content)
			if content != "" {
				lines = append(lines, string(entry.Type)+": "+content)
			}
		}
		text := truncateUTF8Bytes(strings.Join(lines, "\n"), 4096)
		score := lexicalScore(query, text)
		if score == 0 || text == "" {
			continue
		}
		summary := text
		if len(lines) > 0 {
			summary = lines[0]
		}
		out = append(out, sanitizeContextEvidence(contextEvidence{
			ID: contextEvidenceID("local-live", session.RepoID, session.SessionID), SourceType: "local-live",
			RepoID: session.RepoID, RepoName: session.RepoName, SessionID: session.SessionID,
			Timestamp: session.LastSeen.UTC().Format(time.RFC3339), Summary: truncateUTF8Bytes(summary, 512), Excerpt: text,
			DrillDown: "local live session " + session.SessionID, Score: score,
		}))
	}
	return out
}

func evidencePath(registryPath, id string) (string, error) {
	if !strings.HasPrefix(id, "ctx_") || strings.ContainsAny(id, "/\\.") {
		return "", errors.New("invalid evidence id")
	}
	return filepath.Join(filepath.Dir(registryPath), "evidence", id+".json"), nil
}

func persistContextEvidence(ctx context.Context, evidence contextEvidence) error {
	registryPath, err := currentContextRegistryPath(ctx)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(contextNamespaceLockPath(registryPath)), 0o700); err != nil {
		return fmt.Errorf("create context namespace root: %w", err)
	}
	release, err := flock.AcquireContext(ctx, contextNamespaceLockPath(registryPath))
	if err != nil {
		return fmt.Errorf("lock context namespace: %w", err)
	}
	defer release()
	path, err := evidencePath(registryPath, evidence.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create context evidence directory: %w", err)
	}
	evidence = sanitizeContextEvidence(evidence)
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal context evidence: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".evidence.tmp.*")
	if err != nil {
		return fmt.Errorf("create context evidence temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure context evidence temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write context evidence temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close context evidence temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace context evidence: %w", err)
	}
	if err := pruneContextEvidenceFiles(filepath.Dir(path), time.Now()); err != nil {
		return fmt.Errorf("prune context evidence: %w", err)
	}
	return nil
}

func pruneContextEvidenceFiles(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read context evidence directory: %w", err)
	}
	type evidenceFile struct {
		path    string
		modTime time.Time
		size    int64
	}
	files := make([]evidenceFile, 0, len(entries))
	cutoff := now.Add(-contextEvidenceRetention)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat context evidence %q: %w", entry.Name(), err)
		}
		path := filepath.Join(dir, entry.Name())
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove expired context evidence %q: %w", entry.Name(), err)
			}
			continue
		}
		files = append(files, evidenceFile{path: path, modTime: info.ModTime(), size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	var keptBytes int64
	for i, file := range files {
		keep := i < maxContextEvidenceFiles && keptBytes+file.size <= maxContextEvidenceBytes
		if keep {
			keptBytes += file.size
			continue
		}
		if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove excess context evidence %q: %w", filepath.Base(file.path), err)
		}
	}
	return nil
}

func inspectContextEvidence(cmd *cobra.Command, id string) error {
	registryPath, err := currentContextRegistryPath(cmd.Context())
	if err != nil {
		return fmt.Errorf("read context evidence: %w", err)
	}
	path, err := evidencePath(registryPath, id)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path) //nolint:gosec // evidencePath validates the id and fixes the namespace.
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("context evidence %q not found; run `entire context query` again", id)
	}
	if err != nil {
		return fmt.Errorf("read context evidence: %w", err)
	}
	var evidence contextEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		return fmt.Errorf("parse context evidence: %w", err)
	}
	if jsonRequested(cmd) {
		_, err = cmd.OutOrStdout().Write(append(data, '\n'))
		if err != nil {
			return fmt.Errorf("write context evidence: %w", err)
		}
		return nil
	}
	return printFields(cmd.OutOrStdout(), []string{"ID", "SOURCE", "REPOSITORY", "CHECKPOINT", "SESSION", "TIMESTAMP", "SUMMARY", "EXCERPT", "FILES", "DRILL DOWN"}, []string{
		evidence.ID, evidence.SourceType, evidence.RepoName, evidence.CheckpointID, evidence.SessionID,
		evidence.Timestamp, evidence.Summary, evidence.Excerpt, strings.Join(evidence.Files, ", "), evidence.DrillDown,
	})
}
