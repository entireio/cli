package recap

import (
	"sort"
	"time"
)

// AggregateRange collapses a slice of sessions into an RecapRangeSummary.
// The [from, to] range is INCLUSIVE on both ends — a checkpoint whose
// timestamp equals from or to is counted. Sessions outside the range still
// contribute to session-level totals (Sessions, LinkedCommits, AgentCounts,
// ModelCounts); only their checkpoint-level tallies gate on the range.
func AggregateRange(sessions []RecapSession, from, to time.Time) RecapRangeSummary {
	sum := RecapRangeSummary{
		From:        from,
		To:          to,
		LabelCounts: map[string]int{},
		AgentCounts: map[string]int{},
		ModelCounts: map[string]int{},
		ToolTimeMs:  map[string]int64{},
	}
	repos := map[string]bool{}
	activeDays := map[string]bool{}
	for _, s := range sessions {
		sum.Sessions++
		if s.Repo != "" {
			repos[s.Repo] = true
		}
		sum.LinkedCommits += len(s.LinkedCommits)
		for _, a := range s.AgentsUsed {
			sum.AgentCounts[a]++
		}
		for _, m := range s.ModelsUsed {
			sum.ModelCounts[m]++
		}
		for _, cp := range s.Checkpoints {
			if cp.CreatedAt.Before(from) || cp.CreatedAt.After(to) {
				continue
			}
			sum.Checkpoints++
			if cp.IsTask {
				sum.TaskCheckpoints++
			}
			activeDays[cp.CreatedAt.Format("2006-01-02")] = true
			for _, lbl := range cp.Labels {
				sum.LabelCounts[lbl]++
			}
		}
	}
	for r := range repos {
		sum.ReposTouched = append(sum.ReposTouched, r)
	}
	sort.Strings(sum.ReposTouched)
	sum.ActiveDays = len(activeDays)
	return sum
}

// LabelCounts returns a map of label -> occurrence count across all
// checkpoints in all given sessions.
func LabelCounts(sessions []RecapSession) map[string]int {
	out := map[string]int{}
	for _, s := range sessions {
		for _, cp := range s.Checkpoints {
			for _, lbl := range cp.Labels {
				out[lbl]++
			}
		}
	}
	return out
}

// DominantLabel returns the label whose share >= 0.55 AND leads the
// runner-up by >= 0.15. If no label qualifies, ok is false.
//
// Example:
//
//	counts = {feature_build: 6, bug_fix: 3, testing: 2} (total 11)
//	feature_build share 0.545 — below 0.55 threshold → ok=false
//
//	counts = {feature_build: 8, bug_fix: 2, testing: 1} (total 11)
//	feature_build share 0.727, lead 0.545 → ok=true, label="feature_build"
func DominantLabel(counts map[string]int) (string, bool) {
	total := 0
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return "", false
	}
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(counts))
	for k, v := range counts {
		all = append(all, kv{k, v})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	topShare := float64(all[0].v) / float64(total)
	if topShare < 0.55 {
		return "", false
	}
	if len(all) == 1 {
		return all[0].k, true
	}
	runnerShare := float64(all[1].v) / float64(total)
	if topShare-runnerShare < 0.15 {
		return "", false
	}
	return all[0].k, true
}

// AggregateByDay buckets checkpoints by day in the given tz, returning
// one RecapDay per calendar day in the range (zero-activity days
// included).
func AggregateByDay(sessions []RecapSession, from, to time.Time, tz *time.Location) []RecapDay {
	if tz == nil {
		tz = time.UTC
	}
	startDay := dayStart(from, tz)
	endDay := dayStart(to, tz)
	days := int(endDay.Sub(startDay).Hours()/24) + 1
	if days < 1 {
		return nil
	}
	out := make([]RecapDay, days)
	byDay := map[string]*RecapDay{}
	for i := range days {
		d := startDay.AddDate(0, 0, i)
		out[i] = RecapDay{
			Date:        d,
			LabelCounts: map[string]int{},
			ToolTimeMs:  map[string]int64{},
		}
		byDay[d.Format("2006-01-02")] = &out[i]
	}
	for _, s := range sessions {
		for _, cp := range s.Checkpoints {
			key := dayStart(cp.CreatedAt, tz).Format("2006-01-02")
			d, ok := byDay[key]
			if !ok {
				continue
			}
			d.Checkpoints++
			if cp.IsTask {
				d.TaskCheckpoints++
			}
			if cp.LinkedCommit != "" {
				d.LinkedCommits++
			}
			for _, lbl := range cp.Labels {
				d.LabelCounts[lbl]++
			}
		}
	}
	return out
}

// AggregateByAgent returns one summary per distinct agent, sorted by
// session count desc (ties broken by agent name asc).
//
// When a session lists multiple AgentsUsed entries, its checkpoints are
// attributed to EACH agent — the sum of per-agent checkpoint counts can
// exceed the global checkpoint count. This matches the "time spent by
// agent X" intuition: if both Claude and Codex worked on a session,
// both agents' panels should show its checkpoints. Consumers that need
// global-unique checkpoint counts should use AggregateRange instead.
func AggregateByAgent(sessions []RecapSession) []RecapAgentSummary {
	type bucket struct {
		summary RecapAgentSummary
		linked  int
	}
	m := map[string]*bucket{}
	for _, s := range sessions {
		for _, a := range s.AgentsUsed {
			b, ok := m[a]
			if !ok {
				b = &bucket{summary: RecapAgentSummary{
					Agent:       a,
					LabelCounts: map[string]int{},
					ToolTimeMs:  map[string]int64{},
				}}
				m[a] = b
			}
			b.summary.Sessions++
			for _, cp := range s.Checkpoints {
				b.summary.Checkpoints++
				if cp.LinkedCommit != "" {
					b.linked++
				}
				for _, lbl := range cp.Labels {
					b.summary.LabelCounts[lbl]++
				}
			}
		}
	}
	out := make([]RecapAgentSummary, 0, len(m))
	for _, b := range m {
		if b.summary.Checkpoints > 0 {
			b.summary.LinkedRate = float64(b.linked) / float64(b.summary.Checkpoints)
			b.summary.CheckpointDensity = float64(b.summary.Checkpoints) / float64(b.summary.Sessions)
		}
		out = append(out, b.summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// AggregateByRepo returns one summary per distinct repo, sorted by
// session count desc (ties broken by repo name asc).
func AggregateByRepo(sessions []RecapSession) []RecapRepoSummary {
	type bucket struct {
		summary RecapRepoSummary
		linked  int
	}
	m := map[string]*bucket{}
	for _, s := range sessions {
		if s.Repo == "" {
			continue
		}
		b, ok := m[s.Repo]
		if !ok {
			b = &bucket{summary: RecapRepoSummary{
				Repo:        s.Repo,
				LabelCounts: map[string]int{},
			}}
			m[s.Repo] = b
		}
		b.summary.Sessions++
		b.summary.LinkedCommits += len(s.LinkedCommits)
		for _, cp := range s.Checkpoints {
			b.summary.Checkpoints++
			if cp.LinkedCommit != "" {
				b.linked++
			}
			for _, lbl := range cp.Labels {
				b.summary.LabelCounts[lbl]++
			}
		}
	}
	out := make([]RecapRepoSummary, 0, len(m))
	for _, b := range m {
		if b.summary.Checkpoints > 0 {
			b.summary.LinkedRate = float64(b.linked) / float64(b.summary.Checkpoints)
		}
		out = append(out, b.summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sessions != out[j].Sessions {
			return out[i].Sessions > out[j].Sessions
		}
		return out[i].Repo < out[j].Repo
	})
	return out
}
