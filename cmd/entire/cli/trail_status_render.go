package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ANSI SGR codes used by the status-line renderer. Claude Code's status line
// renders ANSI colors and OSC 8 hyperlinks; other terminals that lack support
// degrade gracefully (colors drop, the link text still shows).
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "2"
	ansiCyan   = "36"
	ansiRed    = "31"
	ansiYellow = "33"
)

// writeTrailStatus renders a snapshot in the requested format. For the
// statusline and plain formats an empty render prints nothing at all (no
// trailing newline) so an agent status line stays blank rather than showing an
// empty row.
func writeTrailStatus(w io.Writer, snap trailStatusSnapshot, format string) error {
	switch format {
	case trailStatusFormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(snap); err != nil {
			return fmt.Errorf("encode trail status: %w", err)
		}
		return nil
	case trailStatusFormatPlain:
		if line := renderTrailStatusPlain(snap); line != "" {
			fmt.Fprintln(w, line)
		}
		return nil
	default: // statusline
		if line := renderTrailStatusLine(snap); line != "" {
			fmt.Fprintln(w, line)
		}
		return nil
	}
}

// renderTrailStatusLine renders the compact, colorized, hyperlinked status-line
// form. Non-actionable states (disabled, unauth, no repo, error) render empty
// so they never clutter a status line.
func renderTrailStatusLine(snap trailStatusSnapshot) string {
	switch snap.State {
	case trailStatusStateTrail:
		label := osc8(snap.URL, ansiColor(trailStatusTrailLabel(snap), ansiCyan))
		if badge := trailStatusFindingsBadge(snap); badge != "" {
			return label + ansiColor("  ", ansiDim) + badge
		}
		return label
	case trailStatusStateNoTrail:
		return ansiColor("◇ no trail", ansiDim)
	default:
		return ""
	}
}

// trailBannerLine renders the one-line banner form (no status-line glyphs) used
// by the session-start hook banner for agents without a persistent status line.
// Only the trail state produces text; everything else returns empty.
func trailBannerLine(snap trailStatusSnapshot) string {
	if snap.State != trailStatusStateTrail {
		return ""
	}
	parts := []string{"Trail " + trailStatusNumberOrTitle(snap)}
	if snap.URL != "" {
		parts = append(parts, snap.URL)
	}
	if snap.FindingsKnown && snap.OpenFindings > 0 {
		parts = append(parts, trailStatusFindingsText(snap))
	}
	return strings.Join(parts, " · ")
}

// renderTrailStatusPlain renders an uncolored, human-readable line for manual
// invocation (`entire trail status --format plain`). Unlike the status-line
// form it is informative for every state.
func renderTrailStatusPlain(snap trailStatusSnapshot) string {
	switch snap.State {
	case trailStatusStateTrail:
		line := "Trail " + trailStatusNumberOrTitle(snap)
		if snap.FindingsKnown && snap.OpenFindings > 0 {
			line += " — " + trailStatusFindingsText(snap)
		}
		if snap.URL != "" {
			line += " (" + snap.URL + ")"
		}
		return line
	case trailStatusStateNoTrail:
		if snap.Branch != "" {
			return "No trail for branch " + snap.Branch
		}
		return "No trail for the current branch"
	case trailStatusStateDisabled:
		return "Trails are not enabled for this repository"
	case trailStatusStateUnauth:
		return "Not logged in (run 'entire login')"
	case trailStatusStateNoRepo:
		return "Not an Entire trails-supported repository"
	default:
		if snap.Message != "" {
			return "Trail status unavailable: " + snap.Message
		}
		return "Trail status unavailable"
	}
}

// trailStatusTrailLabel is the status-line label: a diamond glyph plus the
// trail number and a truncated title.
func trailStatusTrailLabel(snap trailStatusSnapshot) string {
	return "◆ " + trailStatusNumberOrTitle(snap)
}

func trailStatusNumberOrTitle(snap trailStatusSnapshot) string {
	title := truncateOneLine(snap.Title, trailStatusMaxTitleRunes)
	switch {
	case snap.Number > 0 && title != "":
		return "#" + strconv.Itoa(snap.Number) + " " + title
	case snap.Number > 0:
		return "#" + strconv.Itoa(snap.Number)
	case title != "":
		return title
	default:
		return snap.Branch
	}
}

// trailStatusFindingsBadge is the colorized open-findings badge for the status
// line: red when any are high severity, yellow otherwise, empty when none.
func trailStatusFindingsBadge(snap trailStatusSnapshot) string {
	if !snap.FindingsKnown || snap.OpenFindings <= 0 {
		return ""
	}
	color := ansiYellow
	if snap.HighFindings > 0 {
		color = ansiRed
	}
	return ansiColor("⚑ "+trailStatusFindingsText(snap), color)
}

// trailStatusFindingsText is the uncolored findings phrase shared by the badge,
// banner, and plain renderers, e.g. "3 open findings (1 high)" or "100+ open
// findings" when the single-page scan was capped.
func trailStatusFindingsText(snap trailStatusSnapshot) string {
	count := strconv.Itoa(snap.OpenFindings)
	if snap.OpenFindings >= trailStatusFindingsScanLimit {
		count = strconv.Itoa(trailStatusFindingsScanLimit) + "+"
	}
	text := count + " open " + pluralize("finding", snap.OpenFindings)
	if snap.HighFindings > 0 {
		text += fmt.Sprintf(" (%d high)", snap.HighFindings)
	}
	return text
}

func ansiColor(s, code string) string {
	if s == "" || noColorEnabled() {
		return s
	}
	return "\x1b[" + code + "m" + s + ansiReset
}

// osc8 wraps text in an OSC 8 hyperlink to url. Terminals without OSC 8 support
// ignore the escape and show the text unchanged, so this is always safe to emit.
func osc8(url, text string) string {
	if strings.TrimSpace(url) == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x07" + text + "\x1b]8;;\x07"
}

// noColorEnabled reports whether ANSI color should be suppressed, honoring the
// NO_COLOR convention (https://no-color.org).
func noColorEnabled() bool {
	return strings.TrimSpace(os.Getenv("NO_COLOR")) != ""
}
