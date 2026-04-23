package recap

import "time"

// HeatmapMetric selects what to count per day.
type HeatmapMetric int

const (
	MetricSessions HeatmapMetric = iota
	MetricCheckpoints
	MetricLinkedCommits
	// MetricTokens sums all billable token fields on agent.TokenUsage:
	// InputTokens + OutputTokens + CacheCreationTokens + CacheReadTokens.
	// APICallCount and SubagentTokens are not included here.
	MetricTokens
)

// HeatmapCell is one day's aggregate for the heatmap.
type HeatmapCell struct {
	Date  time.Time
	Value int
}

// BuildHeatmap produces daily cells between from and to (inclusive) in
// the provided timezone. Missing days are present with Value 0.
func BuildHeatmap(sessions []RecapSession, from, to time.Time, metric HeatmapMetric, tz *time.Location) []HeatmapCell {
	if tz == nil {
		tz = time.UTC
	}
	startDay := dayStart(from, tz)
	endDay := dayStart(to, tz)
	days := int(endDay.Sub(startDay).Hours()/24) + 1
	if days < 1 {
		return nil
	}
	cells := make([]HeatmapCell, days)
	for i := range days {
		cells[i] = HeatmapCell{Date: startDay.AddDate(0, 0, i)}
	}
	byDay := map[string]int{}
	for _, s := range sessions {
		switch metric {
		case MetricSessions:
			d := dayStart(s.StartedAt, tz).Format("2006-01-02")
			byDay[d]++
		case MetricCheckpoints:
			for _, cp := range s.Checkpoints {
				d := dayStart(cp.CreatedAt, tz).Format("2006-01-02")
				byDay[d]++
			}
		case MetricLinkedCommits:
			for _, cp := range s.Checkpoints {
				if cp.LinkedCommit == "" {
					continue
				}
				d := dayStart(cp.CreatedAt, tz).Format("2006-01-02")
				byDay[d]++
			}
		case MetricTokens:
			// Sum all billable token fields: input + output + cache creation + cache read.
			// Cache tokens are billed (creation at write rate, reads at discounted rate) and
			// reflect real model work, so they are included alongside fresh input/output.
			for _, cp := range s.Checkpoints {
				if cp.TokenUsage == nil {
					continue
				}
				d := dayStart(cp.CreatedAt, tz).Format("2006-01-02")
				byDay[d] += cp.TokenUsage.InputTokens +
					cp.TokenUsage.OutputTokens +
					cp.TokenUsage.CacheCreationTokens +
					cp.TokenUsage.CacheReadTokens
			}
		}
	}
	for i := range cells {
		cells[i].Value = byDay[cells[i].Date.Format("2006-01-02")]
	}
	return cells
}

func dayStart(t time.Time, tz *time.Location) time.Time {
	t = t.In(tz)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, tz)
}

// HeatmapIntensity returns a tier 0..4 for rendering the 5-tier color ramp.
// 0 = empty, 4 = peak.
func HeatmapIntensity(value, peak int) int {
	if value <= 0 || peak <= 0 {
		return 0
	}
	pct := float64(value) / float64(peak)
	switch {
	case pct >= 1.0:
		return 4
	case pct >= 0.66:
		return 3
	case pct >= 0.33:
		return 2
	default:
		return 1
	}
}
