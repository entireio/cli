// Package trailers provides parsing and formatting for Entire commit message trailers.
// Trailers are key-value metadata appended to git commit messages following the
// git trailer convention (key: value format after a blank line).
package trailers

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Trailer key constants used in commit messages.
const (
	// MetadataTrailerKey points to the metadata directory within a commit tree.
	MetadataTrailerKey = "Entire-Metadata"

	// StrategyTrailerKey indicates which strategy created the commit.
	StrategyTrailerKey = "Entire-Strategy"

	// SessionTrailerKey identifies which session created a commit.
	SessionTrailerKey = "Entire-Session"

	// CheckpointTrailerKey links commits to their checkpoint metadata on entire/checkpoints/v1.
	// Format: a checkpoint ID — either a legacy 12-hex ID (e.g. "a3b2c4d5e6f7")
	// or a 26-char ULID (see checkpoint/id.CheckpointPattern).
	// This trailer survives git amend and rebase operations.
	CheckpointTrailerKey = "Entire-Checkpoint"

	// AgentTrailerKey identifies the agent that created a checkpoint.
	// Format: human-readable agent name e.g. "Claude Code", "Cursor"
	AgentTrailerKey = "Entire-Agent"

	// OPFAppliedTrailerKey marks an entire/checkpoints/v1 commit whose blobs
	// have been redacted by the OpenAI Privacy Filter (the opt-in 9th,
	// network-backed layer, applied on top of the 8 regex layers).
	// Format: literal "true"; the trailer is omitted entirely when OPF was
	// not applied. The pre-push rewrite path treats commits lacking this
	// trailer as candidates to OPF-redact before they reach the remote.
	OPFAppliedTrailerKey = "Entire-OPF-Applied"

	// OPFAppliedTrailerValue is the only value that means "OPF ran." Any
	// other value (or trailer absence) is treated as "not applied" so a
	// future "false" / "skipped" value never accidentally enables OPF.
	OPFAppliedTrailerValue = "true"
)

// Pre-compiled regexes for trailer parsing.
var (
	metadataTrailerRegex   = regexp.MustCompile(MetadataTrailerKey + `:\s*(.+)`)
	sessionTrailerRegex    = regexp.MustCompile(SessionTrailerKey + `:\s*(.+)`)
	checkpointTrailerRegex = regexp.MustCompile(CheckpointTrailerKey + `:\s*(` + checkpointID.CheckpointPattern + `)(?:\s|$)`)
)

// ParseMetadata extracts metadata dir from commit message.
// Returns the metadata directory and true if found, empty string and false otherwise.
func ParseMetadata(commitMessage string) (string, bool) {
	matches := metadataTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseSession extracts the session ID from a commit message.
// Returns the session ID and true if found, empty string and false otherwise.
// Note: If multiple Entire-Session trailers exist, this returns only the first one.
func ParseSession(commitMessage string) (string, bool) {
	matches := sessionTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseCheckpoint discovers the first checkpoint ID anywhere in a commit
// message. It supports squash history; callers must not treat the result as a
// trusted trailer when deciding whether to create, reuse, or mutate a checkpoint.
// Returns the CheckpointID and true if found, empty ID and false otherwise.
func ParseCheckpoint(commitMessage string) (checkpointID.CheckpointID, bool) {
	matches := checkpointTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		idStr := strings.TrimSpace(matches[1])
		// Validate it's a proper checkpoint ID
		if cpID, err := checkpointID.NewCheckpointID(idStr); err == nil {
			return cpID, true
		}
	}
	return checkpointID.EmptyCheckpointID, false
}

// ParseAllCheckpoints discovers all checkpoint IDs in a commit message.
// Returns a slice of CheckpointIDs (may be empty if none found).
// Duplicate IDs are deduplicated while preserving order.
// This is useful for squash merge commits that contain multiple Entire-Checkpoint trailers.
// The results are discovery candidates, not proof that the final trailer block
// authorized checkpoint mutation or reuse.
func ParseAllCheckpoints(commitMessage string) []checkpointID.CheckpointID {
	matches := checkpointTrailerRegex.FindAllStringSubmatch(commitMessage, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	ids := make([]checkpointID.CheckpointID, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			idStr := strings.TrimSpace(match[1])
			if !seen[idStr] {
				if cpID, err := checkpointID.NewCheckpointID(idStr); err == nil {
					seen[idStr] = true
					ids = append(ids, cpID)
				}
			}
		}
	}
	return ids
}

// ParseAllCheckpointsFromFinalTrailerBlock extracts checkpoint IDs from the
// final trailer block, deduplicating them while preserving order. Lines
// starting with commentPrefix are skipped, as git's trailer grammar does; an
// empty commentPrefix reads every line as written.
func ParseAllCheckpointsFromFinalTrailerBlock(commitMessage, commentPrefix string) []checkpointID.CheckpointID {
	lines := finalTrailerBlock(commitMessage, commentPrefix)
	if len(lines) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	ids := make([]checkpointID.CheckpointID, 0, len(lines))
	for _, line := range lines {
		cpID, ok := checkpointFromTrailerLine(line)
		if !ok || seen[cpID.String()] {
			continue
		}
		seen[cpID.String()] = true
		ids = append(ids, cpID)
	}
	return ids
}

func checkpointFromTrailerLine(line string) (checkpointID.CheckpointID, bool) {
	match := checkpointTrailerLineRegex.FindStringSubmatch(line)
	if match == nil {
		return checkpointID.EmptyCheckpointID, false
	}
	cpID, err := checkpointID.NewCheckpointID(match[1])
	return cpID, err == nil
}

// checkpointTrailerLineRegex matches one whole Entire-Checkpoint trailer line,
// however the separator is spaced ("Entire-Checkpoint:<id>", a tab).
var checkpointTrailerLineRegex = regexp.MustCompile(`^` + CheckpointTrailerKey + `:\s*(` + checkpointID.CheckpointPattern + `)\s*$`)

// scissorsMarker is the core of git's scissors line ("# ---- >8 ----"); with
// `commit -v` everything below it is the diff, which git discards.
const scissorsMarker = "------------------------ >8 ------------------------"

// DefaultCommentPrefix is git's comment prefix when core.commentChar is unset.
const DefaultCommentPrefix = "#"

// autoCommentCandidates are the prefixes git tries, in order, for
// core.commentChar=auto.
const autoCommentCandidates = "#;@!$%^&|:"

// isScissorsLine reports whether line is git's scissors line for commentPrefix.
// Git cuts only at that exact line, ended by "\n" alone: never at prose that
// mentions the marker, nor at the line an editor saved with CRLF.
func isScissorsLine(line, commentPrefix string) bool {
	return line == commentPrefix+" "+scissorsMarker
}

// StripEditorComments returns an editor-written commit message without the
// lines git's default cleanup drops before the commit is recorded: everything
// from the scissors line down, and every line starting with commentPrefix. An
// empty commentPrefix, for a -m or -F message whose lines git keeps as
// written, returns message unchanged.
func StripEditorComments(message, commentPrefix string) string {
	if commentPrefix == "" {
		return message
	}
	lines := strings.Split(CutAtScissors(message, commentPrefix), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, commentPrefix) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// EditorCommentBlockStart returns the index of the line where the comment
// block git appends to a message it opens in the editor starts. Git writes a
// blank line and then only comment lines, ending the message or, with
// `commit -v`, the part above the scissors line; under commit.cleanup=scissors
// the scissors line can start the block itself. So the block is the run of
// commentPrefix lines that ends the message, or ends it above the scissors
// line, with a blank line right above it, else the scissors line. It returns -1
// when there is neither, or commentPrefix is empty: comment lines joined to the
// paragraph above them, or making up the whole message, are the user's.
func EditorCommentBlockStart(lines []string, commentPrefix string) int {
	if commentPrefix == "" {
		return -1
	}
	scissors := ScissorsLineIndex(lines, commentPrefix)
	if start := commentRunStart(lines, commentPrefix, scissors); start > 0 && isBlankLine(lines[start-1]) {
		return start
	}
	return scissors
}

// commentRunStart returns the index of the first line of the run of
// commentPrefix lines that ends the message, ignoring trailing blank lines, or
// that ends right above the scissors line at index scissors (-1 for none). It
// returns -1 when no comment line ends there.
func commentRunStart(lines []string, commentPrefix string, scissors int) int {
	end := scissors
	if end < 0 {
		end = len(lines)
		for end > 0 && isBlankLine(lines[end-1]) {
			end--
		}
	}
	start := end
	for start > 0 && isCommentLine(lines[start-1], commentPrefix) {
		start--
	}
	if start == end {
		return -1
	}
	return start
}

// ScissorsLineIndex returns the index of git's scissors line for
// commentPrefix in lines, or -1.
func ScissorsLineIndex(lines []string, commentPrefix string) int {
	return slices.IndexFunc(lines, func(line string) bool { return isScissorsLine(line, commentPrefix) })
}

// CutAtScissors returns message without git's scissors line for commentPrefix
// and everything below it, which git discards from a `commit -v` message. An
// empty commentPrefix returns message unchanged.
func CutAtScissors(message, commentPrefix string) string {
	if commentPrefix == "" {
		return message
	}
	lines := strings.Split(message, "\n")
	if i := ScissorsLineIndex(lines, commentPrefix); i >= 0 {
		return strings.Join(lines[:i], "\n")
	}
	return message
}

// AutoRule is how a git version picks the character for core.commentChar=auto.
type AutoRule int

const (
	// AutoSkipsTrailingComments is git's rule since 2.51: the empty and "#"
	// lines that end the message, and an old "Conflicts:" block, do not
	// count, as when git places a sign-off (ignored_log_message_bytes).
	AutoSkipsTrailingComments AutoRule = iota
	// AutoCountsEveryLine is git's rule before 2.51.
	AutoCountsEveryLine
)

// AutoCommentPrefix returns the prefix git chose for core.commentChar=auto: the
// first of autoCommentCandidates that starts no line of the message, by rule,
// picked before git adds its comment block. When git may have written that block
// (mayHaveBlock: commit.status is on), its lines start with the choice, so it is
// read back from them: the scissors line if there is one, else the run of lines
// ending the message, below at least one other line, that start with the
// prefix git would pick for the lines above them. No blank line is needed
// above that run: git writes its block right below a message that lacks a
// final newline, and no line above the block can start with git's choice. A
// message without one was not opened in the editor.
func AutoCommentPrefix(message string, mayHaveBlock bool, rule AutoRule) string {
	if mayHaveBlock {
		lines := strings.Split(message, "\n")
		for _, line := range lines {
			for _, candidate := range autoCommentCandidates {
				if isScissorsLine(line, string(candidate)) {
					return string(candidate)
				}
			}
		}
		for i := range len(autoCommentCandidates) {
			candidate := autoCommentCandidates[i : i+1]
			if start := commentRunStart(lines, candidate, -1); start > 0 && autoCommentChoice(strings.Join(lines[:start], "\n"), rule) == candidate {
				return candidate
			}
		}
	}
	return autoCommentChoice(message, rule)
}

// autoCommentChoice is git's core.commentChar=auto rule for message: the
// first candidate no line starts with. The first line always counts; the
// lines rule skips do not.
func autoCommentChoice(message string, rule AutoRule) string {
	starts := make(map[byte]bool)
	if message != "" {
		starts[message[0]] = true
	}
	counted := message
	if rule == AutoSkipsTrailingComments {
		counted = message[:len(message)-trailingCommentBytes(message)]
	}
	// Git treats a CR as a line start too.
	for _, line := range strings.FieldsFunc(counted, func(r rune) bool { return r == '\n' || r == '\r' }) {
		starts[line[0]] = true
	}
	for i := range len(autoCommentCandidates) {
		if !starts[autoCommentCandidates[i]] {
			return autoCommentCandidates[i : i+1]
		}
	}
	return DefaultCommentPrefix
}

// trailingCommentBytes returns how many bytes end message as git's sign-off
// placement ignores them (ignored_log_message_bytes, with "#" as the comment
// prefix): everything from the "#" scissors line down, and the run of empty
// and "#" lines that ends the part above it, or an old "Conflicts:" block
// with its tab-indented paths.
func trailingCommentBytes(message string) int {
	scissors := DefaultCommentPrefix + " " + scissorsMarker + "\n"
	cutoff := len(message)
	if strings.HasPrefix(message, scissors) {
		cutoff = 0
	} else if i := strings.Index(message, "\n"+scissors); i >= 0 {
		cutoff = i + 1
	}
	boc, inConflicts := 0, false
	for bol := 0; bol < cutoff; {
		next := strings.IndexByte(message[bol:], '\n')
		if next < 0 {
			next = len(message)
		} else {
			next += bol + 1
		}
		switch {
		case strings.HasPrefix(message[bol:cutoff], DefaultCommentPrefix) || message[bol] == '\n':
			if boc == 0 {
				boc = bol
			}
		case strings.HasPrefix(message[bol:], "Conflicts:\n"):
			inConflicts = true
			if boc == 0 {
				boc = bol
			}
		case inConflicts && message[bol] == '\t':
		case boc != 0:
			boc, inConflicts = 0, false
		}
		bol = next
	}
	if boc != 0 {
		return len(message) - boc
	}
	return len(message) - cutoff
}

// RemoveCheckpointTrailers removes every whole Entire-Checkpoint trailer line
// whose ID drop selects, anywhere between the subject paragraph and a scissors
// line, and returns the new message with the IDs removed (nil when the
// message is unchanged). It does not try to reproduce git's trailer-block
// rules (mixed blocks, a "(cherry picked from ...)" note, custom comment
// characters): a line that is exactly a checkpoint trailer is one in every
// form git accepts, and leaving one behind would let an amend keep a deleted
// checkpoint alive. Indentation is ignored, since a squash message indents
// the squashed commits' messages. Prose that merely mentions an ID is not a
// whole trailer line and stays. Blank lines a removal leaves doubled are
// collapsed, and trailing blank lines are trimmed unless a scissors line
// follows; nothing at or below the scissors line is touched.
func RemoveCheckpointTrailers(message string, drop func(checkpointID.CheckpointID) bool) (string, []checkpointID.CheckpointID) {
	lines := strings.Split(message, "\n")
	bodyStart := subjectParagraphEnd(lines)
	end := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, scissorsMarker) })
	hasScissors := end >= 0
	if !hasScissors {
		end = len(lines)
	}
	removeLine := make([]bool, end)
	var removed []checkpointID.CheckpointID
	for i := bodyStart; i < end; i++ {
		match := checkpointTrailerLineRegex.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if match == nil {
			continue
		}
		cpID, err := checkpointID.NewCheckpointID(match[1])
		if err != nil || !drop(cpID) {
			continue
		}
		removeLine[i] = true
		if !slices.Contains(removed, cpID) {
			removed = append(removed, cpID)
		}
	}
	if len(removed) == 0 {
		return message, nil
	}
	if hasScissors {
		kept := keepLines(lines[:end], removeLine, false, false)
		return strings.Join(append(kept, lines[end:]...), "\n"), removed
	}
	return strings.Join(keepLines(lines, removeLine, true, strings.HasSuffix(message, "\n")), "\n"), removed
}

// subjectParagraphEnd returns the index just past the subject paragraph: the
// first run of non-blank lines.
func subjectParagraphEnd(lines []string) int {
	i := 0
	for i < len(lines) && isBlankLine(lines[i]) {
		i++
	}
	for i < len(lines) && !isBlankLine(lines[i]) {
		i++
	}
	return i
}

// keepLines drops the removed lines and collapses a blank line that a
// removal would leave doubled. With trimTrailing it also trims trailing blank
// lines, keeping the message's final newline.
func keepLines(lines []string, removeLine []bool, trimTrailing, endsWithNewline bool) []string {
	kept := make([]string, 0, len(lines))
	afterRemoval := false
	for i, line := range lines {
		if removeLine[i] {
			afterRemoval = true
			continue
		}
		blank := isBlankLine(line)
		if blank && afterRemoval && len(kept) > 0 && isBlankLine(kept[len(kept)-1]) {
			continue
		}
		if !blank {
			afterRemoval = false
		}
		kept = append(kept, line)
	}
	if !trimTrailing {
		return kept
	}
	for len(kept) > 0 && isBlankLine(kept[len(kept)-1]) {
		kept = kept[:len(kept)-1]
	}
	if endsWithNewline {
		kept = append(kept, "")
	}
	return kept
}

func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

// FormatCheckpoint creates a commit message with a checkpoint trailer.
// This links user commits to their checkpoint metadata on entire/checkpoints/v1 branch.
func FormatCheckpoint(message string, cpID checkpointID.CheckpointID) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, CheckpointTrailerKey, cpID.String())
}

// trailerLineRe matches git trailer format: a key of letters, digits and
// dashes at the start of the line, then optional spaces or tabs and the colon
// ("Reviewed-by : Bob"). Like git, it accepts any separator after the colon
// ("Key:value", a tab), which also makes a line starting with a URL
// ("https://...") trailer-shaped.
var trailerLineRe = regexp.MustCompile(`^[A-Za-z0-9-]+[ \t]*:`)

// IsTrailerLine reports whether a line matches git trailer format.
func IsTrailerLine(line string) bool {
	return trailerLineRe.MatchString(line)
}

// appendTrailerLine appends a single pre-formatted trailer line (e.g. "Key: value")
// to message in trailer-block-aware format: it is joined to a final paragraph
// that is a trailer block (see isTrailerBlock), and otherwise starts a new block
// after a blank line.
func appendTrailerLine(message, trailerLine string) string {
	trimmed := strings.TrimRight(message, "\n")

	// Collect the final paragraph. Joining also requires a blank line above
	// the paragraph so a trailer-shaped subject line (e.g. "fix: bug") never
	// has the trailer glued onto it.
	lines := strings.Split(trimmed, "\n")
	start := len(lines)
	separated := false
	for start > 0 {
		if strings.TrimSpace(lines[start-1]) == "" {
			separated = true
			break
		}
		start--
	}

	// Join only a block read as one without a comment prefix: every reader
	// accepts it whatever prefix it skips, and a new paragraph would push the
	// block's trailers out of the final block.
	if separated && isTrailerBlock(lines[start:], "") {
		return trimmed + "\n" + trailerLine + "\n"
	}
	return trimmed + "\n\n" + trailerLine + "\n"
}

// AppendCheckpointTrailer appends Entire-Checkpoint in trailer-aware format
// (see appendTrailerLine).
func AppendCheckpointTrailer(message, checkpointID string) string {
	trailer := fmt.Sprintf("%s: %s", CheckpointTrailerKey, checkpointID)
	return appendTrailerLine(message, trailer)
}

// HasOPFApplied reports whether the commit message carries an
// `Entire-OPF-Applied: true` trailer. Any other value (or absence) is
// treated as "OPF not applied" so the pre-push rewrite considers the
// commit a candidate for OPF redaction. Pinning the value to literal
// "true" — rather than just trailer presence — prevents a future
// "Entire-OPF-Applied: false" or "skipped" from accidentally meaning
// "yes, applied."
//
// The trailer marks commits Entire rewrites, not user commits, so a message
// that is only a trailer block counts too.
func HasOPFApplied(commitMessage string) bool {
	for _, line := range lastTrailerBlock(commitMessage, "", true) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != OPFAppliedTrailerKey {
			continue
		}
		if strings.TrimSpace(value) == OPFAppliedTrailerValue {
			return true
		}
	}
	return false
}

// finalTrailerBlock returns the trailer and continuation lines of message's
// final trailer block, skipping lines that start with commentPrefix as git's
// trailer grammar does, or nil when the final paragraph is not a trailer block.
func finalTrailerBlock(message, commentPrefix string) []string {
	return lastTrailerBlock(message, commentPrefix, false)
}

// lastTrailerBlock is finalTrailerBlock that, with subjectMayBeTrailers, also
// reads a message of one paragraph as a trailer block.
func lastTrailerBlock(message, commentPrefix string, subjectMayBeTrailers bool) []string {
	lines := strings.Split(message, "\n")
	// Git ignores the blank and comment lines that end a message.
	end := len(lines)
	for end > 0 && (isBlankLine(lines[end-1]) || isCommentLine(lines[end-1], commentPrefix)) {
		end--
	}
	start := end
	for start > 0 && !isBlankLine(lines[start-1]) {
		start--
	}
	// Git reads the first paragraph as the subject, never as trailers.
	if start == 0 && !subjectMayBeTrailers || !isTrailerBlock(lines[start:end], commentPrefix) {
		return nil
	}
	return slices.DeleteFunc(lines[start:end], func(line string) bool { return isCommentLine(line, commentPrefix) })
}

// gitGeneratedPrefixes start the lines git itself adds to a trailer block
// (`commit -s`, `cherry-pick -x`).
var gitGeneratedPrefixes = []string{"Signed-off-by: ", "(cherry picked from commit "}

// isTrailerBlock reports whether lines form a trailer block by git's rule: at
// least one trailer line, with every other line an indented continuation of a
// trailer's value or a line starting with commentPrefix, which git skips. A
// block holding a line git generates may also hold other lines, as long as
// they are at most three times its trailer lines.
func isTrailerBlock(lines []string, commentPrefix string) bool {
	trailerLines, otherLines := 0, 0
	generated, continuable := false, false
	for _, line := range lines {
		switch {
		case isCommentLine(line, commentPrefix):
			// Git skips the line, and an indented line after it continues
			// nothing.
			continuable = false
		case slices.ContainsFunc(gitGeneratedPrefixes, func(prefix string) bool { return strings.HasPrefix(line, prefix) }):
			trailerLines++
			generated, continuable = true, true
		case IsTrailerLine(line):
			trailerLines++
			continuable = true
		case continuable && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			// Git treats an indented line after a trailer as a continuation
			// of that trailer's value, never as a new trailer.
		default:
			otherLines++
			continuable = false
		}
	}
	return trailerLines > 0 && (otherLines == 0 || generated && trailerLines*3 >= otherLines)
}

func isCommentLine(line, commentPrefix string) bool {
	return commentPrefix != "" && strings.HasPrefix(line, commentPrefix)
}

// AppendOPFAppliedTrailer appends `Entire-OPF-Applied: true` in
// trailer-aware format. Idempotent: if the message already carries
// the trailer with value "true", the original message is returned
// unchanged so re-parenting an already-applied commit doesn't
// duplicate the trailer.
func AppendOPFAppliedTrailer(message string) string {
	if HasOPFApplied(message) {
		return message
	}
	trailer := fmt.Sprintf("%s: %s", OPFAppliedTrailerKey, OPFAppliedTrailerValue)
	return appendTrailerLine(message, trailer)
}
