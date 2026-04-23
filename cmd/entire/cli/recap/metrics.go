package recap

import (
	"sort"
)

// LabelCounts tallies label occurrences across every checkpoint in every
// session. Used by the summary panel's top-label signal and by the
// "Labels" row on the bottom panel. Returns an empty map when nothing
// matches — callers should gate on len > 0 before rendering.
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
