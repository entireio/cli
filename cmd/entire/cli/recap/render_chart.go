package recap

import (
	"fmt"
	"strings"
)

// Heatmap intensity glyphs, low to high. Five tiers match the 5-tier ramp
// called out in the visual-language section of the plan.
var heatmapGlyphs = []rune{' ', '░', '▒', '▓', '█'}

// renderHeatmapStrip turns a slice of bucketed counts into a single-line
// heatmap. Intensity is scaled to maxIntensity; buckets with zero activity
// render as a blank (space) glyph, preserving the strip's width.
//
// Empty input returns "". maxIntensity <= 0 is treated as "no scale yet" and
// the strip renders all zeros. Buckets exceeding maxIntensity are clamped to
// the top tier rather than panicking.
func renderHeatmapStrip(bucketed []int, maxIntensity int, styles Styles) string {
	if len(bucketed) == 0 {
		return ""
	}
	var b strings.Builder
	for _, v := range bucketed {
		b.WriteRune(heatmapGlyphRune(v, maxIntensity))
	}
	return styles.accent.Render(b.String())
}

// heatmapGlyphRune maps a single bucket count to its ramp glyph.
// Linear bucketing across the ramp (5 tiers) keeps the code trivial; we can
// swap for quantile bucketing later if real data proves uneven.
func heatmapGlyphRune(v, maxIntensity int) rune {
	if v <= 0 || maxIntensity <= 0 {
		return heatmapGlyphs[0]
	}
	tier := (v * (len(heatmapGlyphs) - 1)) / maxIntensity
	if tier >= len(heatmapGlyphs) {
		tier = len(heatmapGlyphs) - 1
	}
	if tier < 1 {
		tier = 1 // any non-zero v gets at least the lowest visible tier
	}
	return heatmapGlyphs[tier]
}

// gradientBarWidth is the fixed bar cell count. Kept as a named constant so
// the renderer doesn't take a parameter that's only ever set to one value —
// a nice visual width is stable UX, not a runtime choice.
const gradientBarWidth = 8

// renderGradientBar draws a horizontal bar `▰▰▰▰▱▱▱▱  45%` where the filled
// portion is proportional to value/max. Returns "" when max <= 0.
//
// The bar is followed by two spaces and the percentage rounded to whole
// numbers — callers can trim the suffix when they render their own label.
func renderGradientBar(value, maxVal int, styles Styles) string {
	width := gradientBarWidth
	if maxVal <= 0 {
		return ""
	}
	if value < 0 {
		value = 0
	}
	if value > maxVal {
		value = maxVal
	}
	filled := (value * width) / maxVal
	if value > 0 && filled == 0 {
		filled = 1 // any non-zero value shows at least one block
	}
	pct := (value * 100) / maxVal
	bar := strings.Repeat("▰", filled) + strings.Repeat("▱", width-filled)
	return fmt.Sprintf("%s  %d%%", styles.accent.Render(bar), pct)
}

// renderIntraDayStrip draws a 24-cell strip (one per hour) showing event
// intensity across a day, with hour labels below. Used on the Day tab's
// Activity panel. Events is indexed by hour 0–23.
//
// Return is a two-line string: the strip on line 1, hour labels on line 2.
// Labels fall on hours 0, 3, 6, 9, 12, 15, 18, 21 — 8 anchors, each
// offset 3 cells — which matches the mockup in the plan.
func renderIntraDayStrip(events [24]int, styles Styles) string {
	maxV := 0
	for _, v := range events {
		if v > maxV {
			maxV = v
		}
	}
	// Row 1: the strip itself.
	var row strings.Builder
	for _, v := range events {
		row.WriteRune(heatmapGlyphRune(v, maxV))
	}

	// Row 2: hour labels at the 8 anchor hours, padded to line up with each
	// 3-hour tick on the strip above.
	var labels strings.Builder
	for h := 0; h < 24; h++ {
		if h%3 == 0 {
			fmt.Fprintf(&labels, "%02d", h)
			// Already wrote 2 chars; skip the next cell-position because a
			// 2-char label occupies two strip cells.
			h++
		} else {
			labels.WriteRune(' ')
		}
	}
	return styles.accent.Render(row.String()) + "\n" + styles.muted.Render(labels.String())
}
