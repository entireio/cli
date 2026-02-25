// Package digest generates team activity digests from checkpoint data.
package digest

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/summarize"
)

// AuthorDigest contains digest data for a single author.
type AuthorDigest struct {
	Name          string
	Email         string
	Prompts       []Prompt
	Conversations []Conversation // conversation windows for AI curation
	Branches      map[string]bool
	FilesTouched  map[string]bool
	TokenUsage    agent.TokenUsage
	SessionCount  int
}

// Prompt represents a single user prompt with context.
type Prompt struct {
	Content   string
	Timestamp time.Time
	Branch    string
	SessionID string
	Score     int
}

// Conversation is a scored conversation window with surrounding context.
type Conversation struct {
	Author    string
	Branch    string
	Timestamp time.Time
	SessionID string
	Score     int
	Exchanges []Exchange
}

// Exchange is one turn in a conversation (user or assistant).
type Exchange struct {
	Role    string // "user" or "assistant"
	Content string
}

// TeamDigest contains the full team digest.
type TeamDigest struct {
	Period    string
	StartDate time.Time
	EndDate   time.Time
	Authors   map[string]*AuthorDigest
	Total     Stats
}

// Stats contains aggregate statistics.
type Stats struct {
	Checkpoints  int
	Sessions     int
	Prompts      int
	FilesTouched int
	TokenUsage   agent.TokenUsage
}

// DefaultLimit is the default number of prompts shown per author.
const DefaultLimit = 5

// Options configures digest generation.
type Options struct {
	Period       string
	AuthorFilter string
	Limit        int  // prompts per author (0 = use DefaultLimit)
	AllPrompts   bool // show all prompts, ignore limit
}

// periodPattern matches durations like "7d", "30d", "1d".
var periodPattern = regexp.MustCompile(`^(\d+)d$`)

// ParsePeriod converts a period string to a start time relative to now.
// Supports: "today", "7d", "30d", "all", or any "<N>d" format.
func ParsePeriod(period string, now time.Time) (time.Time, error) {
	switch period {
	case "all":
		return time.Time{}, nil
	case "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	default:
		matches := periodPattern.FindStringSubmatch(period)
		if matches == nil {
			return time.Time{}, fmt.Errorf("invalid period %q: use \"today\", \"7d\", \"30d\", or \"all\"", period)
		}
		days, _ := strconv.Atoi(matches[1])
		return now.AddDate(0, 0, -days), nil
	}
}

// Generate creates a team digest from checkpoint data.
func Generate(ctx context.Context, store checkpoint.Store, opts Options) (*TeamDigest, error) {
	now := time.Now()
	startDate, err := ParsePeriod(opts.Period, now)
	if err != nil {
		return nil, err
	}

	committed, err := store.ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list checkpoints: %w", err)
	}

	digest := &TeamDigest{
		Period:  opts.Period,
		EndDate: now,
		Authors: make(map[string]*AuthorDigest),
	}
	if !startDate.IsZero() {
		digest.StartDate = startDate
	}

	gitStore, ok := store.(*checkpoint.GitStore)
	if !ok {
		return nil, fmt.Errorf("digest requires a GitStore (got %T)", store)
	}

	for _, info := range committed {
		// Filter by period
		if !startDate.IsZero() && info.CreatedAt.Before(startDate) {
			continue
		}

		// Skip task checkpoints
		if info.IsTask {
			continue
		}

		// Get author
		author, authorErr := gitStore.GetCheckpointAuthor(ctx, info.CheckpointID)
		if authorErr != nil {
			continue
		}
		authorName := author.Name
		if authorName == "" {
			authorName = "Unknown"
		}

		// Filter by author
		if opts.AuthorFilter != "" &&
			!strings.EqualFold(authorName, opts.AuthorFilter) &&
			!strings.HasPrefix(strings.ToLower(authorName), strings.ToLower(opts.AuthorFilter)) {
			continue
		}

		// Get or create author digest
		ad, exists := digest.Authors[authorName]
		if !exists {
			ad = &AuthorDigest{
				Name:         authorName,
				Email:        author.Email,
				Branches:     make(map[string]bool),
				FilesTouched: make(map[string]bool),
			}
			digest.Authors[authorName] = ad
		}

		// Track files (from checkpoint summary, not per-session)
		for _, f := range info.FilesTouched {
			ad.FilesTouched[f] = true
		}

		// Read all sessions in this checkpoint (multi-session support)
		sessionCount := info.SessionCount
		if sessionCount <= 0 {
			sessionCount = 1
		}
		for si := 0; si < sessionCount; si++ {
			content, readErr := gitStore.ReadSessionContent(ctx, info.CheckpointID, si)
			if readErr != nil {
				continue
			}

			ad.SessionCount++

			// Track branch
			if content.Metadata.Branch != "" {
				ad.Branches[content.Metadata.Branch] = true
			}

			// Track tokens
			if content.Metadata.TokenUsage != nil {
				ad.TokenUsage.InputTokens += content.Metadata.TokenUsage.InputTokens
				ad.TokenUsage.OutputTokens += content.Metadata.TokenUsage.OutputTokens
				ad.TokenUsage.CacheCreationTokens += content.Metadata.TokenUsage.CacheCreationTokens
				ad.TokenUsage.CacheReadTokens += content.Metadata.TokenUsage.CacheReadTokens
			}

			// Extract user prompts from transcript
			prompts := extractPrompts(content, info)
			ad.Prompts = append(ad.Prompts, prompts...)

			// Extract conversation windows for AI curation
			convos := extractConversations(content, info)
			for ci := range convos {
				convos[ci].Author = authorName
			}
			ad.Conversations = append(ad.Conversations, convos...)
		}

		digest.Total.Checkpoints++
	}

	// Calculate totals (before limiting)
	allFiles := make(map[string]bool)
	sessionIDs := make(map[string]bool)
	for _, ad := range digest.Authors {
		digest.Total.Prompts += len(ad.Prompts)
		digest.Total.TokenUsage.InputTokens += ad.TokenUsage.InputTokens
		digest.Total.TokenUsage.OutputTokens += ad.TokenUsage.OutputTokens
		digest.Total.TokenUsage.CacheCreationTokens += ad.TokenUsage.CacheCreationTokens
		digest.Total.TokenUsage.CacheReadTokens += ad.TokenUsage.CacheReadTokens
		for f := range ad.FilesTouched {
			allFiles[f] = true
		}
		for _, p := range ad.Prompts {
			sessionIDs[p.SessionID] = true
		}
	}
	digest.Total.FilesTouched = len(allFiles)
	digest.Total.Sessions = len(sessionIDs)

	// Sort prompts by score (descending) and apply limit
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	for _, ad := range digest.Authors {
		sort.Slice(ad.Prompts, func(i, j int) bool {
			return ad.Prompts[i].Score > ad.Prompts[j].Score
		})
		if !opts.AllPrompts && len(ad.Prompts) > limit {
			ad.Prompts = ad.Prompts[:limit]
		}

		// Sort, deduplicate, and limit conversations for AI curation
		sort.Slice(ad.Conversations, func(i, j int) bool {
			return ad.Conversations[i].Score > ad.Conversations[j].Score
		})
		ad.Conversations = deduplicateConversations(ad.Conversations)
		if len(ad.Conversations) > maxConversationsPerAuthor {
			ad.Conversations = ad.Conversations[:maxConversationsPerAuthor]
		}
	}

	return digest, nil
}

// extractPrompts pulls user prompts from checkpoint content, filtering noise.
func extractPrompts(content *checkpoint.SessionContent, info checkpoint.CommittedInfo) []Prompt {
	if len(content.Transcript) == 0 {
		return nil
	}

	agentType := content.Metadata.Agent
	condensed, err := summarize.BuildCondensedTranscriptFromBytes(content.Transcript, agentType)
	if err != nil {
		return nil
	}

	var prompts []Prompt
	seen := make(map[string]bool)

	for _, entry := range condensed {
		if entry.Type != summarize.EntryTypeUser {
			continue
		}
		text := strings.TrimSpace(entry.Content)
		if text == "" {
			continue
		}

		// Filter noise
		if isNoisePrompt(text) {
			continue
		}

		// Deduplicate
		if seen[text] {
			continue
		}
		seen[text] = true

		prompts = append(prompts, Prompt{
			Content:   text,
			Timestamp: info.CreatedAt,
			Branch:    content.Metadata.Branch,
			SessionID: content.Metadata.SessionID,
			Score:     scorePrompt(text),
		})
	}

	return prompts
}

// maxConversationsPerAuthor is how many conversation windows to keep per author for AI curation.
const maxConversationsPerAuthor = 20

// extractConversations pulls conversation windows (user + assistant + follow-ups) from transcripts.
// Each window is centered on a high-scoring user prompt and includes surrounding context.
func extractConversations(content *checkpoint.SessionContent, info checkpoint.CommittedInfo) []Conversation {
	if len(content.Transcript) == 0 {
		return nil
	}

	agentType := content.Metadata.Agent
	condensed, err := summarize.BuildCondensedTranscriptFromBytes(content.Transcript, agentType)
	if err != nil {
		return nil
	}

	// Build list of non-tool entries with their original indices
	type indexedEntry struct {
		entry summarize.Entry
		index int
	}
	var entries []indexedEntry
	for i, e := range condensed {
		if e.Type == summarize.EntryTypeTool {
			continue
		}
		entries = append(entries, indexedEntry{entry: e, index: i})
	}

	var conversations []Conversation
	seen := make(map[string]bool)

	for i, ie := range entries {
		if ie.entry.Type != summarize.EntryTypeUser {
			continue
		}
		text := strings.TrimSpace(ie.entry.Content)
		if text == "" || isNoisePrompt(text) || seen[text] {
			continue
		}
		seen[text] = true

		score := scorePrompt(text)

		// Build conversation window around this user prompt
		var exchanges []Exchange

		// Look back: if previous entry is assistant, include it
		if i > 0 && entries[i-1].entry.Type == summarize.EntryTypeAssistant {
			prev := strings.TrimSpace(entries[i-1].entry.Content)
			if prev != "" {
				exchanges = append(exchanges, Exchange{Role: "assistant", Content: prev})
			}
		}

		// The user prompt itself
		exchanges = append(exchanges, Exchange{Role: "user", Content: text})

		// Look forward: assistant response + optional short follow-up exchanges
		for j := i + 1; j < len(entries) && len(exchanges) < 6; j++ {
			next := entries[j]
			nextText := strings.TrimSpace(next.entry.Content)
			if nextText == "" || isNoisePrompt(nextText) {
				continue
			}
			if next.entry.Type == summarize.EntryTypeAssistant {
				exchanges = append(exchanges, Exchange{Role: "assistant", Content: nextText})
			} else if next.entry.Type == summarize.EntryTypeUser {
				// Include short follow-up prompts (the "Really?" moments)
				if len(strings.Fields(nextText)) <= 20 {
					exchanges = append(exchanges, Exchange{Role: "user", Content: nextText})
				} else {
					break
				}
			}
		}

		// Score follow-up user messages (the arc makes conversations funny, not just the anchor)
		for _, ex := range exchanges {
			if ex.Role == "user" && ex.Content != text {
				score += scorePrompt(ex.Content) / 2
			}
		}

		// Bonus for rapid-fire back-and-forth (4+ exchanges are almost always entertaining)
		if len(exchanges) >= 4 {
			score += 2
		}

		conversations = append(conversations, Conversation{
			Branch:    content.Metadata.Branch,
			Timestamp: info.CreatedAt,
			SessionID: content.Metadata.SessionID,
			Score:     score,
			Exchanges: exchanges,
		})
	}

	return conversations
}

// isNoisePrompt returns true for prompts that should be filtered out.
func isNoisePrompt(text string) bool {
	// Skip task notifications
	if strings.HasPrefix(text, "<task-notification>") {
		return true
	}
	// Skip request interrupted markers
	if strings.Contains(text, "[Request interrupted by user]") {
		return true
	}
	// Skip system reminders
	if strings.HasPrefix(text, "<system-reminder>") {
		return true
	}
	return false
}

// SortedAuthors returns authors sorted by prompt count (descending).
func (d *TeamDigest) SortedAuthors() []*AuthorDigest {
	authors := make([]*AuthorDigest, 0, len(d.Authors))
	for _, a := range d.Authors {
		authors = append(authors, a)
	}
	sort.Slice(authors, func(i, j int) bool {
		return len(authors[i].Prompts) > len(authors[j].Prompts)
	})
	return authors
}

// deduplicateConversations removes cross-session duplicates by normalized prompt text.
// Must be called after sorting by score so the highest-scoring duplicate wins.
func deduplicateConversations(convos []Conversation) []Conversation {
	seen := make(map[string]bool)
	result := make([]Conversation, 0, len(convos))
	for _, c := range convos {
		key := normalizePromptKey(c.Exchanges)
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, c)
	}
	return result
}

// normalizePromptKey extracts the first user message from exchanges and normalizes it for dedup.
func normalizePromptKey(exchanges []Exchange) string {
	for _, ex := range exchanges {
		if ex.Role == "user" {
			return strings.ToLower(strings.TrimSpace(ex.Content))
		}
	}
	return ""
}

// SortedBranches returns branch names sorted alphabetically.
func (a *AuthorDigest) SortedBranches() []string {
	branches := make([]string, 0, len(a.Branches))
	for b := range a.Branches {
		branches = append(branches, b)
	}
	sort.Strings(branches)
	return branches
}
