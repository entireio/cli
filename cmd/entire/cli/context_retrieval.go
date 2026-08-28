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
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/search"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
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

func redactContextQuery(ctx context.Context, query string) (string, error) {
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return "", fmt.Errorf("configure redaction: %w", err)
	}
	return truncateUTF8Bytes(sanitizeEvidenceText(redact.String(query)), maxContextQueryBytes), nil
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}

const (
	contextPacketOpenTag  = "<entire-context>"
	contextPacketCloseTag = "</entire-context>"
)

// asciiFoldByte lowercases an ASCII letter and passes every other byte
// through. Used instead of unicode case folding wherever a match offset is
// used to slice the ORIGINAL string; see indexASCIIFold.
func asciiFoldByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// hasASCIIFoldPrefixAt reports whether s, starting at offset, begins with the
// ASCII substr, compared case-insensitively.
func hasASCIIFoldPrefixAt(s string, offset int, substr string) bool {
	if offset < 0 || offset+len(substr) > len(s) {
		return false
	}
	for k := range len(substr) {
		if asciiFoldByte(s[offset+k]) != asciiFoldByte(substr[k]) {
			return false
		}
	}
	return true
}

// indexASCIIFold returns the byte offset of the first ASCII-case-insensitive
// occurrence of substr in s, or -1.
//
// strings.ToLower is unusable for this: Unicode case folding changes byte
// length (U+023A lowercases to a rune one byte LONGER, U+212A to one two bytes
// SHORTER), so an offset found in the lowercased copy does not address the same
// byte in the original. Slicing the original at such an offset cut the wrong
// bytes out of untrusted evidence — leaving delimiter fragments behind, and on
// the growth cases panicking with a slice-bounds error inside the hook's
// evidence goroutine, which no recover() can catch.
func indexASCIIFold(s, substr string) int {
	if substr == "" {
		return 0
	}
	first := asciiFoldByte(substr[0])
	for i := 0; i+len(substr) <= len(s); i++ {
		if asciiFoldByte(s[i]) != first {
			continue
		}
		if hasASCIIFoldPrefixAt(s, i, substr) {
			return i
		}
	}
	return -1
}

// hasASCIIFoldSuffix reports whether b ends with the ASCII suffix, compared
// case-insensitively.
func hasASCIIFoldSuffix(b []byte, suffix string) bool {
	if len(b) < len(suffix) {
		return false
	}
	for k := range len(suffix) {
		if asciiFoldByte(b[len(b)-len(suffix)+k]) != asciiFoldByte(suffix[k]) {
			return false
		}
	}
	return true
}

// dropContextDelimiters guarantees the postcondition the packet depends on:
// the result contains neither packet delimiter, in any ASCII casing.
//
// It is the backstop for splices. Removing one delimiter joins the text on
// either side of it, and that join can spell a NEW delimiter
// ("<entire-cont" + "</entire-context>" + "ext>"), so a single left-to-right
// removal pass is not enough. Appending one byte at a time and collapsing the
// tail whenever it spells a delimiter catches every such case in linear time:
// any delimiter in the output would have been detected as its last byte landed.
func dropContextDelimiters(value string) string {
	out := make([]byte, 0, len(value))
	for i := range len(value) {
		out = append(out, value[i])
		for {
			if hasASCIIFoldSuffix(out, contextPacketCloseTag) {
				out = out[:len(out)-len(contextPacketCloseTag)]
				continue
			}
			if hasASCIIFoldSuffix(out, contextPacketOpenTag) {
				out = out[:len(out)-len(contextPacketOpenTag)]
				continue
			}
			break
		}
	}
	return string(out)
}

// stripEntireContextBlocks removes every <entire-context> … </entire-context>
// block from untrusted text, along with any unpaired delimiter, so evidence can
// neither close the packet Entire wraps it in nor open a fake one. Matching is
// ASCII-case-insensitive; the result is guaranteed delimiter-free after a
// SINGLE call.
//
// That guarantee is the point. An earlier version returned early when an
// opening tag had no closing tag, without rescanning the prefix it kept — so
// "</entire-context> LEAK <entire-context>" sanitized to
// "</entire-context> LEAK", handing untrusted text a live closing delimiter.
// It only failed to be exploitable because every caller happened to sanitize
// more than once. Callers must be able to rely on one call.
func stripEntireContextBlocks(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for rest := value; rest != ""; {
		open := indexASCIIFold(rest, contextPacketOpenTag)
		closed := indexASCIIFold(rest, contextPacketCloseTag)
		switch {
		case open < 0 && closed < 0:
			b.WriteString(rest)
			rest = ""
		case open < 0 || (closed >= 0 && closed < open):
			// Unpaired closing tag: drop the tag, keep the text around it.
			b.WriteString(rest[:closed])
			rest = rest[closed+len(contextPacketCloseTag):]
		default:
			b.WriteString(rest[:open])
			after := rest[open+len(contextPacketOpenTag):]
			inner := indexASCIIFold(after, contextPacketCloseTag)
			if inner < 0 {
				// Unpaired opening tag: drop it and everything after it. The
				// prefix already written is rescanned by dropContextDelimiters.
				rest = ""
				break
			}
			rest = after[inner+len(contextPacketCloseTag):]
		}
	}
	return dropContextDelimiters(b.String())
}

// stripInvisibleRunes removes characters that carry no visible content but do
// carry attack surface: C0/C1 control codes (NUL, BEL, ANSI escapes) and the
// whole Unicode Cf "format" category — zero-width space/joiners, the bidi
// embedding, isolate and OVERRIDE controls (U+202A–U+202E, U+2066–U+2069), the
// word joiner and the BOM. unicode.IsControl covers only Cc, so Cf is named
// separately.
//
// Cf matters here for the same reason it matters in source code (CVE-2021-42574,
// "Trojan Source"): U+202E reorders how the packet renders wherever a human or a
// tool displays it, so an evidence item can make the untrusted-evidence preamble
// and the closing delimiter appear somewhere they are not, and zero-width
// characters split keywords so a payload survives review. The whole category is
// dropped rather than an allowlist of "the dangerous ones": evidence text is
// plain reference prose in a security packet, joiners carry nothing the model
// needs, and one category is auditable where a hand-picked list rots.
//
// Newline and tab are the only control characters kept — the packet is
// line-structured. Invalid UTF-8 arrives here as utf8.RuneError and is dropped.
func stripInvisibleRunes(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
}

// sanitizeEvidenceText makes untrusted text safe to place inside the
// <entire-context> packet.
//
// The order is load-bearing and must not be swapped: invisible characters are
// removed BEFORE the delimiter scan, because a delimiter split by one of them
// is invisible to the scan and reassembles into a live delimiter once the
// character is dropped — "a</entire-\x00context>EVIL" sanitized to
// "a</entire-context>EVIL" under the old order, which is a complete packet
// escape from a single NUL byte.
func sanitizeEvidenceText(value string) string {
	value = stripInvisibleRunes(value)
	value = stripEntireContextBlocks(value)
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
	if insecureHTTPRequested(cmd) {
		auth.EnableInsecureHTTP()
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

type localContextEvidenceResult struct {
	evidence []contextEvidence
	err      error
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
	safeQuery, err := redactContextQuery(ctx, query)
	if err != nil {
		return nil, scope, err
	}
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
	localCh := make(chan localContextEvidenceResult, 1)
	if scope.IncludeLocalLive {
		go func() {
			evidence, err := loadLocalContextEvidence(ctx, safeQuery, targetRepoID, currentSessionID, allowedRepoIDs)
			localCh <- localContextEvidenceResult{evidence: evidence, err: err}
		}()
	} else {
		localCh <- localContextEvidenceResult{}
	}

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
	localResult := <-localCh
	if localResult.err != nil {
		return nil, scope, fmt.Errorf("local-live context unavailable: %w", localResult.err)
	}
	if err := contextFanoutCompletenessError(merged, mergeErr); err != nil {
		return nil, scope, err
	}
	sourceIDs := make(map[string]string, len(scope.Sources))
	for _, source := range scope.Sources {
		sourceIDs[strings.ToLower(source.FullName)] = source.RepoId
	}
	evidence := make([]contextEvidence, 0, len(localResult.evidence)+limit)
	evidence = append(evidence, localResult.evidence...)
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
	if resp != nil {
		if resp.Error != "" {
			return fmt.Errorf("context search incomplete: %s", resp.Error)
		}
		if len(resp.Warnings) > 0 {
			return fmt.Errorf("context search incomplete: %s", strings.Join(resp.Warnings, "; "))
		}
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

func loadLocalContextEvidence(ctx context.Context, query, targetRepoID, currentSessionID string, allowedRepoIDs map[string]struct{}) ([]contextEvidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load local context evidence: %w", err)
	}
	if err := strategy.EnsureRedactionConfigured(ctx); err != nil {
		return nil, fmt.Errorf("configure redaction: %w", err)
	}
	path, err := currentContextRegistryPath(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve context registry: %w", err)
	}
	registry, err := readContextRegistryContext(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read context registry: %w", err)
	}
	now := time.Now()
	var out []contextEvidence
	for _, session := range registry.Sessions {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load local context evidence: %w", err)
		}
		if !localContextSourceAllowed(session.RepoID, targetRepoID, session.SessionID, currentSessionID, allowedRepoIDs) || now.Sub(session.LastSeen) > localContextRecentWindow {
			continue
		}
		if !localContextSessionLive(session) {
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
		type scoredLine struct {
			text  string
			score float64
		}
		var lines []scoredLine
		for _, entry := range entries {
			if entry.Type != summarize.EntryTypeUser && entry.Type != summarize.EntryTypeAssistant {
				continue
			}
			content := sanitizeEvidenceText(entry.Content)
			if content == "" {
				continue
			}
			line := string(entry.Type) + ": " + content
			score := lexicalScore(query, line)
			if score == 0 {
				continue
			}
			lines = append(lines, scoredLine{text: line, score: score})
		}
		if len(lines) == 0 {
			continue
		}
		sort.SliceStable(lines, func(i, j int) bool {
			if lines[i].score != lines[j].score {
				return lines[i].score > lines[j].score
			}
			return lines[i].text < lines[j].text
		})
		textLines := make([]string, len(lines))
		for i, line := range lines {
			textLines[i] = line.text
		}
		text := truncateUTF8Bytes(strings.Join(textLines, "\n"), 4096)
		score := lines[0].score
		summary := lines[0].text
		out = append(out, sanitizeContextEvidence(contextEvidence{
			ID: contextEvidenceID("local-live", session.RepoID, session.SessionID), SourceType: "local-live",
			RepoID: session.RepoID, RepoName: session.RepoName, SessionID: session.SessionID,
			Timestamp: session.LastSeen.UTC().Format(time.RFC3339), Summary: truncateUTF8Bytes(summary, 512), Excerpt: text,
			DrillDown: "local live session " + session.SessionID, Score: score,
		}))
	}
	return out, nil
}

func readContextRegistryContext(ctx context.Context, path string) (contextRegistry, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return contextRegistry{}, fmt.Errorf("create context registry directory: %w", err)
	}
	release, err := flock.AcquireContext(ctx, contextNamespaceLockPath(path))
	if err != nil {
		return contextRegistry{}, fmt.Errorf("lock context registry: %w", err)
	}
	defer release()
	if contextNamespaceDisabled(path) {
		return contextRegistry{Sessions: []localContextSession{}}, nil
	}
	registry, err := readContextRegistryNoLock(path)
	if err != nil {
		return contextRegistry{}, err
	}
	pruneContextRegistry(&registry, time.Now())
	return registry, nil
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync context evidence temp file: %w", err)
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
	if insecureHTTPRequested(cmd) {
		auth.EnableInsecureHTTP()
	}
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
