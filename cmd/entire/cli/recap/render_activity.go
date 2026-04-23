package recap

import (
	"fmt"
	"strings"
	"time"
)

// renderActivityStrip renders the full activity panel: header with optional
// peak annotation, heatmap strip, and axis labels.
func renderActivityStrip(view View, styles Styles) string {
	if len(view.Activity) == 0 {
		return styles.muted.Render("  (no activity in range)")
	}

	// Header: "Activity · 90d" + optional "most active: <time>" right-aligned.
	header := styles.label.Render("Activity · " + rangeTag(view.Range))
	peak := formatPeak(view)
	if peak != "" {
		header += "    " + styles.label.Render(peak)
	}

	// Strip body.
	var strip string
	if view.Range == RangeDay && len(view.Activity) == 24 {
		var events [24]int
		copy(events[:], view.Activity)
		strip = renderIntraDayStrip(events, styles)
	} else {
		strip = renderHeatmapStripDense(view.Activity, styles)
	}

	labels := renderRangeAxisLabels(view.Range, len(view.Activity), styles)
	return header + "\n" + strip + "\n" + labels
}

// renderHeatmapStripDense renders every cell, including zero-activity days.
// Zero cells use the darkest shade so the strip fills its full width
// without looking broken.
func renderHeatmapStripDense(buckets []int, styles Styles) string {
	maxV := 0
	for _, v := range buckets {
		if v > maxV {
			maxV = v
		}
	}
	var b strings.Builder
	for _, v := range buckets {
		if maxV == 0 {
			// All-zero range: render every cell at the darkest visible shade.
			b.WriteRune(heatmapGlyphs[1])
		} else {
			b.WriteRune(heatmapGlyphRune(v, maxV))
		}
	}
	return styles.accent.Render(b.String())
}

// formatPeak returns "most active: <time>" where <time> is an hour string for
// --day and a date string for all other ranges. Returns "" when all activity
// is zero. Ties pick the rightmost (most recent) bucket.
func formatPeak(view View) string {
	peakIdx := -1
	peakV := 0
	for i, v := range view.Activity {
		if v >= peakV && v > 0 {
			peakIdx = i
			peakV = v
		}
	}
	if peakIdx < 0 {
		return ""
	}
	if view.Range == RangeDay {
		return fmt.Sprintf("most active: %02d:00", peakIdx)
	}
	start := time.Now().AddDate(0, 0, -(len(view.Activity) - 1))
	d := start.AddDate(0, 0, peakIdx)
	return "most active: " + d.Format("Jan 2")
}

// rangeTag returns the short label shown in the activity strip header.
func rangeTag(r RangeKey) string {
	switch r {
	case RangeDay:
		return "24h"
	case RangeWeek:
		return "7d"
	case RangeMonth:
		return "this month"
	case Range90d:
		return "90d"
	}
	return ""
}

// renderRangeAxisLabels produces a single row of date/time labels aligned
// under the heatmap cells. For RangeDay the labels are already part of
// renderIntraDayStrip; this function handles all other ranges.
//
// For non-day ranges the label row places a date marker every ~7 cells plus a
// "Today" anchor at the rightmost position. When the bucket count is small
// (≤7) only "Today" is emitted.
func renderRangeAxisLabels(r RangeKey, n int, styles Styles) string {
	if r == RangeDay {
		// Intra-day labels are embedded inside renderIntraDayStrip.
		return ""
	}
	if n == 0 {
		return ""
	}

	// Build a label row of exactly n rune-width cells. We compute label
	// positions, write them into a rune slice, then convert to a string.
	cells := make([]rune, n)
	for i := range cells {
		cells[i] = ' '
	}

	// Compute the label text for a given bucket index offset from range start.
	start := time.Now().AddDate(0, 0, -(n - 1))
	labelAt := func(idx int) string {
		if idx == n-1 {
			return "Today"
		}
		d := start.AddDate(0, 0, idx)
		return d.Format("Jan 2")
	}

	// Place labels approximately every 14 cells, starting at 0.
	stride := 14
	if n <= 7 {
		stride = n
	}

	positions := []int{}
	for pos := 0; pos < n; pos += stride {
		positions = append(positions, pos)
	}
	// Always include the rightmost cell as "Today".
	if len(positions) == 0 || positions[len(positions)-1] != n-1 {
		positions = append(positions, n-1)
	}

	for _, pos := range positions {
		text := []rune(labelAt(pos))
		for i, ch := range text {
			if pos+i < n {
				cells[pos+i] = ch
			}
		}
	}

	return styles.muted.Render(string(cells))
}
