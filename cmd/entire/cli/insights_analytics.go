package cli

import (
	"context"
	"encoding/json"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript"

	"github.com/go-git/go-git/v5"
)

// InsightsQuery contains filtering criteria for insights.
type InsightsQuery struct {
	StartTime   time.Time
	EndTime     time.Time
	AgentFilter agent.AgentType
}

// InsightsReport contains aggregated analytics results.
type InsightsReport struct {
	// Summary stats
	TotalSessions    int
	TotalCheckpoints int
	TotalTime        time.Duration
	TotalTokens      int
	EstimatedCost    float64
	FilesModified    int
	CommitsCreated   int

	// Agent breakdown
	AgentStats []AgentStat

	// Activity patterns
	DailyActivity   []ActivityPoint
	WeeklyActivity  []ActivityPoint
	MonthlyActivity []ActivityPoint
	PeakHours       [24]int

	// Top-K lists
	TopRepos []RepoStat
	TopTools []ToolStat

	// Recent sessions
	RecentSessions []SessionSummary

	// Time range
	StartTime time.Time
	EndTime   time.Time
}

// AgentStat contains stats for a specific agent type.
type AgentStat struct {
	Agent    agent.AgentType
	Sessions int
	Tokens   int
	Hours    float64
}

// ActivityPoint represents activity at a specific time.
type ActivityPoint struct {
	Date     time.Time
	Sessions int
	Hours    float64
}

// RepoStat contains stats for a repository.
type RepoStat struct {
	Name  string
	Hours float64
}

// ToolStat contains stats for a tool.
type ToolStat struct {
	Name  string
	Count int
}

// SessionSummary contains summary info for a session.
type SessionSummary struct {
	ID          string
	Description string
	StartTime   time.Time
	Duration    time.Duration
	Tokens      int
}

// CacheStats contains cache performance metrics.
type CacheStats struct {
	TotalSessions  int
	CachedSessions int
	NewSessions    int
}

// computeInsights computes insights from sessions.
func computeInsights(repo *git.Repository, sessions []strategy.Session, query InsightsQuery, cache *InsightsCache, noCache bool) (*InsightsReport, CacheStats, error) {
	ctx := context.Background()
	store := checkpoint.NewGitStore(repo)

	// Filter sessions by quality (exclude <2 messages, <1 min duration, sub-tasks)
	filteredSessions := filterSessionQuality(ctx, sessions, store)

	// Filter by time range
	filteredSessions = filterSessionsByTime(filteredSessions, query.StartTime, query.EndTime)

	// Filter by agent if specified
	if query.AgentFilter != "" {
		filteredSessions = filterSessionsByAgent(ctx, filteredSessions, store, query.AgentFilter)
	}

	// Initialize cache if needed
	if cache == nil {
		cache = &InsightsCache{
			Facets:      make(map[string]SessionFacet),
			LastUpdated: time.Now(),
		}
	}

	// Identify new sessions (not in cache)
	var newSessions []strategy.Session
	cacheStats := CacheStats{
		TotalSessions: len(filteredSessions),
	}

	if noCache {
		// Force full re-analysis
		newSessions = filteredSessions
		cache.Facets = make(map[string]SessionFacet)
	} else {
		for _, s := range filteredSessions {
			if _, cached := cache.Facets[s.ID]; !cached {
				newSessions = append(newSessions, s)
			} else {
				cacheStats.CachedSessions++
			}
		}
	}
	cacheStats.NewSessions = len(newSessions)

	// Bound parallelism to max 50 new sessions per run (Claude Code pattern)
	if len(newSessions) > 50 {
		logging.Info(ctx, "analyzing subset of new sessions", "total_new", len(newSessions), "analyzing", 50)
		newSessions = newSessions[:50]
	}

	// Extract facets from new sessions in parallel
	if len(newSessions) > 0 {
		logging.Info(ctx, "extracting facets from new sessions", "count", len(newSessions))
		newFacets := extractFacetsParallel(ctx, newSessions, store)

		// Merge with cached facets
		for sessionID, facet := range newFacets {
			cache.Facets[sessionID] = facet
		}
		cache.LastUpdated = time.Now()
	}

	// Build session facet list for aggregation
	var facets []SessionFacet
	for _, s := range filteredSessions {
		if facet, ok := cache.Facets[s.ID]; ok {
			facets = append(facets, facet)
		}
	}

	// Aggregate across all facets
	report := aggregateFacets(facets, query)

	return report, cacheStats, nil
}

// filterSessionQuality filters out low-quality sessions.
// Excludes sessions with <2 user messages or <1 minute duration.
func filterSessionQuality(ctx context.Context, sessions []strategy.Session, store *checkpoint.GitStore) []strategy.Session {
	var quality []strategy.Session

	for _, s := range sessions {
		if len(s.Checkpoints) == 0 {
			continue
		}

		// Load first checkpoint to check message count
		content, err := store.ReadLatestSessionContent(ctx, s.Checkpoints[0].CheckpointID)
		if err != nil {
			logging.Debug(ctx, "failed to read session content", "session", s.ID, "error", err)
			continue
		}

		// Count user messages
		lines, err := transcript.ParseFromBytes(content.Transcript)
		if err != nil {
			logging.Debug(ctx, "failed to parse transcript", "session", s.ID, "error", err)
			continue
		}

		userMessages := 0
		for _, line := range lines {
			if line.Type == transcript.TypeUser {
				userMessages++
			}
		}

		// Estimate duration (first to last checkpoint)
		var duration time.Duration
		if len(s.Checkpoints) > 0 {
			first := s.Checkpoints[len(s.Checkpoints)-1].Timestamp
			last := s.Checkpoints[0].Timestamp
			duration = last.Sub(first)
		}

		// Filter criteria (from Claude Code patterns)
		if userMessages >= 2 && duration >= 1*time.Minute {
			quality = append(quality, s)
		}
	}

	return quality
}

// filterSessionsByTime filters sessions by time range.
func filterSessionsByTime(sessions []strategy.Session, startTime, endTime time.Time) []strategy.Session {
	if startTime.IsZero() && endTime.IsZero() {
		return sessions
	}

	var filtered []strategy.Session
	for _, s := range sessions {
		if !startTime.IsZero() && s.StartTime.Before(startTime) {
			continue
		}
		if !endTime.IsZero() && s.StartTime.After(endTime) {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

// filterSessionsByAgent filters sessions by agent type.
func filterSessionsByAgent(ctx context.Context, sessions []strategy.Session, store *checkpoint.GitStore, agentFilter agent.AgentType) []strategy.Session {
	var filtered []strategy.Session

	for _, s := range sessions {
		if len(s.Checkpoints) == 0 {
			continue
		}

		// Check first checkpoint's agent type
		summary, err := store.ReadCommitted(ctx, s.Checkpoints[0].CheckpointID)
		if err != nil {
			logging.Debug(ctx, "failed to read checkpoint", "session", s.ID, "error", err)
			continue
		}

		// Read session content to get agent from metadata
		content, err := store.ReadLatestSessionContent(ctx, s.Checkpoints[0].CheckpointID)
		if err != nil {
			logging.Debug(ctx, "failed to read session content", "session", s.ID, "error", err)
			continue
		}

		sessionAgent := content.Metadata.Agent
		if sessionAgent == "" && summary != nil {
			// Fallback to summary if metadata doesn't have agent
			// (shouldn't happen, but for robustness)
			sessionAgent = agent.AgentTypeUnknown
		}

		if sessionAgent == agentFilter {
			filtered = append(filtered, s)
		}
	}

	return filtered
}

// extractFacetsParallel extracts facets from sessions in parallel.
func extractFacetsParallel(ctx context.Context, sessions []strategy.Session, store *checkpoint.GitStore) map[string]SessionFacet {
	numWorkers := runtime.NumCPU() / 2
	if numWorkers < 1 {
		numWorkers = 1
	}

	semaphore := make(chan struct{}, numWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	facets := make(map[string]SessionFacet)

	for _, session := range sessions {
		wg.Add(1)
		go func(s strategy.Session) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			facet := extractSessionFacet(ctx, s, store)
			if facet != nil {
				mu.Lock()
				facets[s.ID] = *facet
				mu.Unlock()
			}
		}(session)
	}

	wg.Wait()
	return facets
}

// extractSessionFacet extracts a facet from a session.
func extractSessionFacet(ctx context.Context, s strategy.Session, store *checkpoint.GitStore) *SessionFacet {
	if len(s.Checkpoints) == 0 {
		return nil
	}

	facet := SessionFacet{
		SessionID:   s.ID,
		StartTime:   s.StartTime,
		Description: s.Description,
		ToolCounts:  make(map[string]int),
	}

	// Load session content
	content, err := store.ReadLatestSessionContent(ctx, s.Checkpoints[0].CheckpointID)
	if err != nil {
		logging.Debug(ctx, "failed to read session content", "session", s.ID, "error", err)
		return nil
	}

	// Extract basic metrics from metadata
	facet.Agent = content.Metadata.Agent
	if content.Metadata.TokenUsage != nil {
		facet.Tokens = content.Metadata.TokenUsage.InputTokens +
			content.Metadata.TokenUsage.CacheCreationTokens +
			content.Metadata.TokenUsage.CacheReadTokens +
			content.Metadata.TokenUsage.OutputTokens
	}
	facet.Messages = len(s.Checkpoints)
	facet.FilesModified = len(content.Metadata.FilesTouched)

	// Estimate duration (first to last checkpoint)
	if len(s.Checkpoints) > 0 {
		first := s.Checkpoints[len(s.Checkpoints)-1].Timestamp
		last := s.Checkpoints[0].Timestamp
		facet.Duration = last.Sub(first)
	}

	// Parse transcript for tool usage and hourly activity
	transcriptBytes := content.Transcript

	// Chunk large transcripts (>30K chars) into 25K segments
	if len(transcriptBytes) > 30000 {
		transcriptBytes = chunkTranscript(transcriptBytes, 25000)
	}

	// Extract tool usage
	toolCounts, err := extractToolUsage(transcriptBytes)
	if err == nil {
		facet.ToolCounts = toolCounts
	}

	// Extract hourly activity
	hourlyActivity, err := extractHourlyActivity(transcriptBytes)
	if err == nil {
		facet.HourlyActivity = hourlyActivity
	}

	return &facet
}

// chunkTranscript chunks a transcript if it exceeds maxSize.
// Returns the first chunk for analysis.
func chunkTranscript(transcriptBytes []byte, maxSize int) []byte {
	if len(transcriptBytes) <= maxSize {
		return transcriptBytes
	}

	// Find the last newline before maxSize
	for i := maxSize; i > 0; i-- {
		if transcriptBytes[i] == '\n' {
			return transcriptBytes[:i+1]
		}
	}

	// Fallback: hard cutoff
	return transcriptBytes[:maxSize]
}

// extractToolUsage extracts tool usage counts from a transcript.
func extractToolUsage(transcriptBytes []byte) (map[string]int, error) {
	lines, err := transcript.ParseFromBytes(transcriptBytes)
	if err != nil {
		return nil, err
	}

	toolCounts := make(map[string]int)

	for _, line := range lines {
		if line.Type != transcript.TypeAssistant {
			continue
		}

		var msg transcript.AssistantMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		for _, block := range msg.Content {
			if block.Type == transcript.ContentTypeToolUse {
				toolCounts[block.Name]++
			}
		}
	}

	return toolCounts, nil
}

// extractHourlyActivity extracts hourly activity from a transcript.
func extractHourlyActivity(transcriptBytes []byte) ([24]int, error) {
	lines, err := transcript.ParseFromBytes(transcriptBytes)
	if err != nil {
		return [24]int{}, err
	}

	var hourly [24]int

	for _, line := range lines {
		if line.Type != transcript.TypeUser {
			continue
		}

		// Parse timestamp from line
		var msg struct {
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			continue
		}

		// Parse timestamp
		t, err := time.Parse(time.RFC3339, msg.Timestamp)
		if err != nil {
			continue
		}

		hourly[t.Hour()]++
	}

	return hourly, nil
}

// aggregateFacets aggregates facets into an insights report.
func aggregateFacets(facets []SessionFacet, query InsightsQuery) *InsightsReport {
	report := &InsightsReport{
		StartTime: query.StartTime,
		EndTime:   query.EndTime,
	}

	// Aggregate basic stats
	agentMap := make(map[agent.AgentType]*AgentStat)
	toolMap := make(map[string]int)
	dailyMap := make(map[string]*ActivityPoint)

	for _, facet := range facets {
		report.TotalSessions++
		report.TotalTokens += facet.Tokens
		report.TotalTime += facet.Duration
		report.FilesModified += facet.FilesModified

		// Agent stats
		if _, ok := agentMap[facet.Agent]; !ok {
			agentMap[facet.Agent] = &AgentStat{Agent: facet.Agent}
		}
		agentMap[facet.Agent].Sessions++
		agentMap[facet.Agent].Tokens += facet.Tokens
		agentMap[facet.Agent].Hours += facet.Duration.Hours()

		// Tool usage
		for tool, count := range facet.ToolCounts {
			toolMap[tool] += count
		}

		// Peak hours
		for hour, count := range facet.HourlyActivity {
			report.PeakHours[hour] += count
		}

		// Daily activity
		dateKey := facet.StartTime.Format("2006-01-02")
		if _, ok := dailyMap[dateKey]; !ok {
			dailyMap[dateKey] = &ActivityPoint{
				Date: facet.StartTime.Truncate(24 * time.Hour),
			}
		}
		dailyMap[dateKey].Sessions++
		dailyMap[dateKey].Hours += facet.Duration.Hours()
	}

	// Convert maps to slices and sort
	for _, stat := range agentMap {
		report.AgentStats = append(report.AgentStats, *stat)
	}
	sort.Slice(report.AgentStats, func(i, j int) bool {
		return report.AgentStats[i].Sessions > report.AgentStats[j].Sessions
	})

	for tool, count := range toolMap {
		report.TopTools = append(report.TopTools, ToolStat{Name: tool, Count: count})
	}
	sort.Slice(report.TopTools, func(i, j int) bool {
		return report.TopTools[i].Count > report.TopTools[j].Count
	})
	if len(report.TopTools) > 5 {
		report.TopTools = report.TopTools[:5]
	}

	for _, point := range dailyMap {
		report.DailyActivity = append(report.DailyActivity, *point)
	}
	sort.Slice(report.DailyActivity, func(i, j int) bool {
		return report.DailyActivity[i].Date.Before(report.DailyActivity[j].Date)
	})

	// Estimate cost ($15/1M tokens - Claude 3.5 Sonnet average)
	report.EstimatedCost = float64(report.TotalTokens) * 15.0 / 1_000_000.0

	// Build recent sessions (last 5)
	if len(facets) > 0 {
		// Sort facets by start time (most recent first)
		sortedFacets := make([]SessionFacet, len(facets))
		copy(sortedFacets, facets)
		sort.Slice(sortedFacets, func(i, j int) bool {
			return sortedFacets[i].StartTime.After(sortedFacets[j].StartTime)
		})

		count := 5
		if len(sortedFacets) < count {
			count = len(sortedFacets)
		}

		for i := range count {
			f := sortedFacets[i]
			report.RecentSessions = append(report.RecentSessions, SessionSummary{
				ID:          f.SessionID,
				Description: f.Description,
				StartTime:   f.StartTime,
				Duration:    f.Duration,
				Tokens:      f.Tokens,
			})
		}
	}

	return report
}
