package strategy

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

const maxDisplayedPushSessions = 5

type gitProgressPhase string

const (
	gitProgressPhaseCounting    gitProgressPhase = "counting"
	gitProgressPhaseCompressing gitProgressPhase = "compressing"
	gitProgressPhaseWriting     gitProgressPhase = "writing"
)

// gitProgressEvent is a parsed line from git push/fetch --progress stderr.
type gitProgressEvent struct {
	Phase   gitProgressPhase
	Percent int
	Current int
	Total   int
	Bytes   string
	Speed   string
	Done    bool
}

// sessionSummary aggregates unpushed checkpoint commits for one session.
type sessionSummary struct {
	SessionID       string
	CheckpointCount int
	CommitCount     int
	EarliestTime    time.Time
	LatestTime      time.Time
}

// formatSessionTreeOpts configures formatSessionTree output.
type formatSessionTreeOpts struct {
	TotalCommits int
	NoColor      bool
	IsNewBranch  bool
}

type pushProgressStyles struct {
	dim    func(string) string
	green  func(string) string
	yellow func(string) string
}

func pushProgressStylesFor(w io.Writer, noColor bool) pushProgressStyles {
	if noColor || !interactive.ShouldStyle(w) {
		return pushProgressStyles{
			dim:    func(s string) string { return s },
			green:  func(s string) string { return s },
			yellow: func(s string) string { return s },
		}
	}
	dimStyle := lipgloss.NewStyle().Faint(true)
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	return pushProgressStyles{
		dim:    func(s string) string { return dimStyle.Render(s) },
		green:  func(s string) string { return greenStyle.Render(s) },
		yellow: func(s string) string { return yellowStyle.Render(s) },
	}
}

var (
	checkpointMsgRE  = regexp.MustCompile(`^Checkpoint: ([0-9a-f]+)`)
	finalizeMsgRE    = regexp.MustCompile(`^Finalize transcript for Checkpoint: ([0-9a-f]+)`)
	updateMsgRE      = regexp.MustCompile(`^Update (?:summary|checkpoint summary) for (?:checkpoint )?([0-9a-f]+)`)
	sessionTrailerRE = regexp.MustCompile(regexp.QuoteMeta(trailers.SessionTrailerKey) + `: (.+)`)

	gitEnumeratingRE  = regexp.MustCompile(`^Enumerating objects:\s*(\d+)`)
	gitCountingRE     = regexp.MustCompile(`^Counting objects:\s*(\d+)%\s*\((\d+)/(\d+)\)`)
	gitCompressingRE  = regexp.MustCompile(`^Compressing objects:\s*(\d+)%\s*\((\d+)/(\d+)\)`)
	gitWritingRE      = regexp.MustCompile(`^Writing objects:\s*(\d+)%\s*\((\d+)/(\d+)\)`)
	gitWritingBytesRE = regexp.MustCompile(`(\d+(?:\.\d+)?\s*[KMG]iB)(?:\s*\|\s*(\d+(?:\.\d+)?\s*[KMG]iB/s))?`)
)

type pushSummaryAccumulator struct {
	checkpoints map[string]struct{}
	commitCount int
	earliest    time.Time
	latest      time.Time
}

// parsePushSummaryFromLog groups git log lines by Entire-Session trailer.
// Input format: hash|subject|trailers|authorDate (one commit per line).
func parsePushSummaryFromLog(gitLogOutput string) []sessionSummary {
	gitLogOutput = strings.TrimSpace(gitLogOutput)
	if gitLogOutput == "" {
		return nil
	}

	checkpointToSession := make(map[string]string)
	sessionMap := make(map[string]*pushSummaryAccumulator)

	for _, line := range strings.Split(gitLogOutput, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 4 {
			continue
		}
		subject := parts[1]
		body := parts[2]
		when, err := time.Parse(time.RFC3339, parts[3])
		if err != nil {
			continue
		}

		var checkpointID string
		if m := checkpointMsgRE.FindStringSubmatch(subject); m != nil {
			checkpointID = m[1]
		} else if m := finalizeMsgRE.FindStringSubmatch(subject); m != nil {
			checkpointID = m[1]
		} else if m := updateMsgRE.FindStringSubmatch(subject); m != nil {
			checkpointID = m[1]
		}

		sessionID := ""
		if m := sessionTrailerRE.FindStringSubmatch(body); m != nil {
			sessionID = strings.TrimSpace(m[1])
			if checkpointID != "" {
				checkpointToSession[checkpointID] = sessionID
			}
		} else if checkpointID != "" {
			sessionID = checkpointToSession[checkpointID]
		}
		if sessionID == "" {
			sessionID = "unknown"
		}

		entry, ok := sessionMap[sessionID]
		if !ok {
			checkpoints := map[string]struct{}{}
			if checkpointID != "" {
				checkpoints[checkpointID] = struct{}{}
			}
			sessionMap[sessionID] = &pushSummaryAccumulator{
				checkpoints: checkpoints,
				commitCount: 1,
				earliest:    when,
				latest:      when,
			}
			continue
		}

		entry.commitCount++
		if checkpointID != "" {
			entry.checkpoints[checkpointID] = struct{}{}
		}
		if when.Before(entry.earliest) {
			entry.earliest = when
		}
		if when.After(entry.latest) {
			entry.latest = when
		}
	}

	results := make([]sessionSummary, 0, len(sessionMap))
	for sessionID, entry := range sessionMap {
		results = append(results, sessionSummary{
			SessionID:       sessionID,
			CheckpointCount: len(entry.checkpoints),
			CommitCount:     entry.commitCount,
			EarliestTime:    entry.earliest,
			LatestTime:      entry.latest,
		})
	}
	sortSessionSummariesByLatest(results)
	return results
}

func sortSessionSummariesByLatest(results []sessionSummary) {
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].LatestTime.After(results[i].LatestTime) {
				results[i], results[j] = results[j], results[i]
			}
		}
	}
}

// formatSessionTree renders session summaries as indented stderr lines.
func formatSessionTree(summaries []sessionSummary, opts formatSessionTreeOpts) []string {
	styles := pushProgressStylesFor(io.Discard, opts.NoColor)
	lines := make([]string, 0, len(summaries)+3)

	branchLabel := ""
	if opts.IsNewBranch {
		branchLabel = "new branch, "
	}
	header := fmt.Sprintf("%s Checkpoint push: %s%d commits, %d sessions",
		styles.dim("[entire]"), branchLabel, opts.TotalCommits, len(summaries))
	lines = append(lines, header)

	displayed := summaries
	remaining := 0
	if len(summaries) > maxDisplayedPushSessions {
		displayed = summaries[:maxDisplayedPushSessions]
		remaining = len(summaries) - maxDisplayedPushSessions
	}

	for i, s := range displayed {
		isLast := i == len(displayed)-1 && remaining == 0
		connector := "├─"
		if isLast {
			connector = "└─"
		}
		cpLabel := fmt.Sprintf("%d checkpoints", s.CheckpointCount)
		if s.CheckpointCount == 1 {
			cpLabel = "1 checkpoint"
		}
		timeLabel := formatPushTimeRange(s.EarliestTime, s.LatestTime)
		line := fmt.Sprintf("         %s %s  %s  %s",
			styles.dim(connector), s.SessionID, styles.dim(cpLabel), styles.dim("("+timeLabel+")"))
		lines = append(lines, line)
	}

	if remaining > 0 {
		lines = append(lines, fmt.Sprintf("         %s %s",
			styles.dim("├─"), styles.dim(fmt.Sprintf("... and %d more sessions", remaining))))
		oldest := summaries[len(summaries)-1]
		lines = append(lines, fmt.Sprintf("         %s %s",
			styles.dim("└─"), styles.dim("(oldest: "+formatPushDate(oldest.EarliestTime)+")")))
	}

	return lines
}

func parseGitProgressLine(line string) *gitProgressEvent {
	trimmed := strings.TrimSpace(line)

	if m := gitEnumeratingRE.FindStringSubmatch(trimmed); m != nil {
		total, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		return &gitProgressEvent{
			Phase: gitProgressPhaseCounting,
			Total: total,
			Done:  strings.Contains(trimmed, "done"),
		}
	}

	if m := gitCountingRE.FindStringSubmatch(trimmed); m != nil {
		return parsePercentGitProgressEvent(gitProgressPhaseCounting, m, trimmed)
	}
	if m := gitCompressingRE.FindStringSubmatch(trimmed); m != nil {
		return parsePercentGitProgressEvent(gitProgressPhaseCompressing, m, trimmed)
	}
	if m := gitWritingRE.FindStringSubmatch(trimmed); m != nil {
		event := parsePercentGitProgressEvent(gitProgressPhaseWriting, m, trimmed)
		if bm := gitWritingBytesRE.FindStringSubmatch(trimmed); bm != nil {
			event.Bytes = bm[1]
			if len(bm) > 2 {
				event.Speed = bm[2]
			}
		}
		return event
	}

	return nil
}

func parsePercentGitProgressEvent(phase gitProgressPhase, m []string, trimmed string) *gitProgressEvent {
	percent, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	current, err := strconv.Atoi(m[2])
	if err != nil {
		return nil
	}
	total, err := strconv.Atoi(m[3])
	if err != nil {
		return nil
	}
	return &gitProgressEvent{
		Phase:   phase,
		Percent: percent,
		Current: current,
		Total:   total,
		Done:    strings.Contains(trimmed, "done"),
	}
}

// displayGitProgress writes human-friendly git transfer progress lines to w.
func displayGitProgress(w io.Writer, stderr string) {
	styles := pushProgressStylesFor(w, false)
	lastPhase := gitProgressPhase("")
	for _, line := range strings.FieldsFunc(stderr, func(r rune) bool { return r == '\n' || r == '\r' }) {
		event := parseGitProgressLine(line)
		if event == nil {
			continue
		}
		if !event.Done && event.Phase == lastPhase {
			continue
		}
		lastPhase = event.Phase

		switch event.Phase {
		case gitProgressPhaseCounting:
			if event.Done {
				fmt.Fprintf(w, "         %s\n", styles.dim(fmt.Sprintf("counting objects: %d", event.Total)))
			}
		case gitProgressPhaseCompressing:
			if event.Done {
				fmt.Fprintf(w, "         %s\n", styles.dim(fmt.Sprintf("compressing: %d/%d", event.Current, event.Total)))
			}
		case gitProgressPhaseWriting:
			if event.Done {
				parts := []string{fmt.Sprintf("writing: %d objects", event.Total)}
				if event.Bytes != "" {
					parts = append(parts, event.Bytes)
				}
				if event.Speed != "" {
					parts = append(parts, event.Speed)
				}
				fmt.Fprintf(w, "         %s %s\n",
					styles.dim(strings.Join(parts, ", ")+"..."),
					styles.green("done"))
			}
		}
	}
}

func formatPushTimeRange(earliest, latest time.Time) string {
	if earliest.Equal(latest) {
		return formatPushTime(earliest)
	}
	if formatPushDate(earliest) == formatPushDate(latest) {
		return formatPushHM(earliest) + " ~ " + formatPushHM(latest)
	}
	return formatPushDate(earliest) + " ~ " + formatPushDate(latest)
}

func formatPushTime(d time.Time) string {
	now := time.Now()
	diffDays := int(now.Sub(d).Hours() / 24)
	if diffDays <= 0 && now.Before(d.Add(24*time.Hour)) {
		return formatPushHM(d)
	}
	if diffDays == 1 {
		return "yesterday"
	}
	if diffDays > 1 {
		return fmt.Sprintf("%d days ago", diffDays)
	}
	return formatPushHM(d)
}

func formatPushHM(d time.Time) string {
	return fmt.Sprintf("%02d:%02d", d.Hour(), d.Minute())
}

func formatPushDate(d time.Time) string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year(), int(d.Month()), d.Day())
}

func elapsedPushSec(start time.Time) string {
	sec := int(time.Since(start).Round(time.Second) / time.Second)
	return fmt.Sprintf("%ds", sec)
}

func writePushFinishLine(ctx context.Context, w io.Writer, result pushResult, start time.Time, target string) {
	styles := pushProgressStylesFor(w, false)
	if result.upToDate {
		fmt.Fprintf(w, "         %s\n", styles.dim("already up-to-date"))
		return
	}
	fmt.Fprintf(w, "         %s %s\n", styles.green("done"), styles.dim("("+elapsedPushSec(start)+")"))
	printSettingsCommitHint(ctx, target)
}

func writePushConflictLine(w io.Writer, start time.Time) {
	styles := pushProgressStylesFor(w, false)
	fmt.Fprintf(w, "         %s %s\n", styles.yellow("conflict"), styles.dim("("+elapsedPushSec(start)+")"))
}
