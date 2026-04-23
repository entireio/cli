package recap

import "strings"

const (
	barMinWidth = 12
	barMaxWidth = 40
)

// renderComparisonBar renders a single horizontal bar comparing `you` vs
// `team` values. Rules (spec §Agents panel spec):
//   - Both zero: returns "" so the caller drops the row entirely.
//   - you > team: amber █ fills width; magenta ▒ overlays the first
//     team_ratio × width cells.
//   - team > you: flipped — magenta █ fills width; amber █ fills the
//     first you_ratio × width cells (no ▒ overlay).
//   - you == team (both non-zero): striped — magenta ▒ on even columns,
//     amber █ on odd columns.
//   - Non-zero values always render at least 1 cell to stay visible.
//   - width clamped to [barMinWidth, barMaxWidth]. Below min returns "".
func renderComparisonBar(you, team, width int, styles Styles) string {
	if you == 0 && team == 0 {
		return ""
	}
	if width < barMinWidth {
		return ""
	}
	if width > barMaxWidth {
		width = barMaxWidth
	}

	if you == team && you > 0 {
		var b strings.Builder
		for i := range width {
			if i%2 == 0 {
				b.WriteString(styles.team.Render("▒"))
			} else {
				b.WriteString(styles.accent.Render("█"))
			}
		}
		return b.String()
	}

	if you > team {
		teamCells := cellsFor(team, you, width)
		youCells := width - teamCells
		if team > 0 && teamCells == 0 {
			teamCells = 1
			youCells = width - 1
		}
		return strings.Repeat(styles.accent.Render("█"), youCells) +
			strings.Repeat(styles.team.Render("▒"), teamCells)
	}

	// team-dominant: magenta fill width, amber fills start.
	youCells := cellsFor(you, team, width)
	if you > 0 && youCells == 0 {
		youCells = 1
	}
	teamCells := width - youCells
	return strings.Repeat(styles.accent.Render("█"), youCells) +
		strings.Repeat(styles.team.Render("█"), teamCells)
}

// cellsFor returns the number of cells that should represent `value` when
// the total bar is `width` and the larger side is `dominant`. Rounds to nearest
// integer; returns at least 1 for non-zero values (minimum-1-cell rule).
func cellsFor(value, dominant, width int) int {
	if dominant == 0 {
		return 0
	}
	n := (value*width + dominant/2) / dominant
	if value > 0 && n == 0 {
		return 1
	}
	if n > width {
		return width
	}
	return n
}
