package cli

import (
	"fmt"
	"io"
)

// This file renders the billing-class breakdown. It is shared by
// `checkpoint tokens` (committed metadata) and `session tokens` (live state):
// both compute a *tokenClassBreakdown and hand it here, so the two commands
// cannot drift into two different tables for the same four classes.

// writeTokenClasses renders the billing-class breakdown. The cost
// column is present only when the classes are priced: an empty or zeroed cost
// column would read as "this cost nothing" rather than "we cannot say".
//
// subagentTotal is passed in rather than read off the breakdown because it is
// not a billing class: subagent tokens are already folded into the four
// classes, and this line states what share of that same total they were.
func writeTokenClasses(w io.Writer, classes *tokenClassBreakdown, subagentTotal int) {
	if classes == nil {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "How it was billed")
	if classes.Priced {
		fmt.Fprintf(w, "  %-30s %10s %8s %7s\n", "", "tokens", "volume", "cost")
	} else {
		fmt.Fprintf(w, "  %-30s %10s %8s\n", "", "tokens", "volume")
	}

	rows := []struct {
		label string
		share tokenClassShare
		note  string
	}{
		{"Fresh input", classes.Input, ""},
		{"Cache write", classes.CacheWrite, subsetNote("1h TTL", classes.CacheWrite1h)},
		{"Cache read", classes.CacheRead, ""},
		{"Output", classes.Output, subsetNote("thinking", classes.Thinking)},
	}
	for _, row := range rows {
		if classes.Priced {
			fmt.Fprintf(w, "  %-30s %10s %8s %7s", row.label,
				formatTokenCount(row.share.Tokens), formatSharePercent(row.share.Tokens, row.share.VolumePercent),
				formatCostSharePercent(row.share))
		} else {
			fmt.Fprintf(w, "  %-30s %10s %8s", row.label,
				formatTokenCount(row.share.Tokens), formatSharePercent(row.share.Tokens, row.share.VolumePercent))
		}
		if row.note != "" {
			fmt.Fprintf(w, "  %s", row.note)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "  %-30s %10s\n", "Total", formatTokenCount(classes.Total))

	// Inside the block, immediately after Total, because it is a statement
	// about that total — and once, rather than also as a "Likely contributors"
	// entry: one figure under two labels is the duplication this block exists
	// to remove. Silent at zero, which cannot distinguish "none spawned" from
	// "spawned but not captured".
	if subagentTotal > 0 && classes.Total > 0 {
		fmt.Fprintf(w, "  %-30s %10s %8s\n", "Of the total, subagents used",
			formatTokenCount(subagentTotal),
			formatSharePercent(subagentTotal, roundedPercent(subagentTotal, classes.Total)))
	}

	if !classes.Priced {
		reason := classes.UnpricedReason
		if reason == "" {
			reason = unpricedNoModel
		}
		fmt.Fprintf(w, "  Cost share omitted: %s.\n", reason)
	}
}

// formatSharePercent renders a whole-percent share. A class with tokens in it
// but a share that rounds to zero prints "<1%" rather than "0%": a row showing
// 274.8k tokens beside "0%" reads as broken even though it is arithmetically
// right. A genuinely empty class still prints "0%".
func formatSharePercent(tokens, percent int) string {
	if percent == 0 && tokens > 0 {
		return "<1%"
	}
	return fmt.Sprintf("%d%%", percent)
}

// formatCostSharePercent renders a cost share. It differs from the volume
// column in one case: a class the provider does not bill at all prints "0%",
// not "<1%". "<1%" promises a small cost; several families bill no cache writes
// whatsoever, and claiming a fraction of a percent there is a number the user
// is never charged.
func formatCostSharePercent(share tokenClassShare) string {
	if share.CostZero {
		return "0%"
	}
	return formatSharePercent(share.Tokens, share.CostPercent)
}

// subsetNote renders a subset figure alongside its parent class, or "" when the
// agent recorded none. Subsets are part of their class, never added to the total.
func subsetNote(label string, tokens int) string {
	if tokens <= 0 {
		return ""
	}
	return fmt.Sprintf("(%s %s)", label, formatTokenCount(tokens))
}
