package investigate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// slugRE matches one-or-more characters that are NOT (lowercase) ascii
// alphanumerics. Anything else is squashed to a single dash. Input is
// pre-lowercased before applying.
var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// SlugifyTopic converts an arbitrary topic string into a filesystem-safe slug.
// Result is lowercase, ASCII-alphanumeric with single dashes, no leading or
// trailing dash, and no longer than 60 characters. Empty/non-mappable input
// returns "investigation".
func SlugifyTopic(topic string) string {
	slug := slugRE.ReplaceAllString(strings.ToLower(topic), "-")
	slug = strings.Trim(slug, "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	if slug == "" {
		return "investigation"
	}
	return slug
}

// DeriveTopicFromSeed extracts a human-readable topic from a seed-doc body.
// Order of precedence:
//
//  1. The first `# Investigation: <topic>` line — the scaffold's own title
//     format. Round-trips a finished findings doc cleanly.
//  2. The first markdown H1 (`# anything`).
//  3. fallbackFilename without its extension.
func DeriveTopicFromSeed(body []byte, fallbackFilename string) string {
	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "# Investigation:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			return strings.TrimSpace(rest)
		}
	}
	base := filepath.Base(fallbackFilename)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// BootstrapInput carries the data needed to produce the initial findings
// doc on disk.
//
// Exactly one of SeedDoc / Topic / IssueLinkSeed must be set:
//   - SeedDoc:       the user passed a positional [seed-doc] path; render
//     the scaffold and embed the seed bytes under the
//     `## Question` section. Topic is derived from the
//     body (or filename).
//   - Topic only:    the user supplied the investigation prompt via the
//     spawn-time multipicker (no seed, no issue link); render
//     the scaffold with the topic printed under `## Question`.
//   - IssueLinkSeed: the user passed --issue-link; ResolveIssueLink
//     already produced a markdown body — render the
//     scaffold and embed those bytes under `## Question`,
//     using IssueLinkTopic as the topic.
type BootstrapInput struct {
	// SeedDoc is the absolute path to a user-provided seed file. Empty
	// when no seed was passed.
	SeedDoc string

	// Topic is the topic-only investigation prompt collected from the
	// spawn-time multipicker (set when neither SeedDoc nor IssueLinkSeed
	// is supplied). Empty otherwise.
	Topic string

	// IssueLinkSeed is the markdown bytes produced by ResolveIssueLink.
	// Empty when --issue-link was not used.
	IssueLinkSeed []byte

	// IssueLinkTopic is the topic derived from the resolved issue/PR
	// title. Used only when IssueLinkSeed is non-empty.
	IssueLinkTopic string

	// FindingsDoc is the absolute path the findings doc must be written
	// to.
	FindingsDoc string
}

// BootstrapResult reports what was produced.
type BootstrapResult struct {
	// Topic is the resolved topic — used downstream for slug derivation,
	// manifest entries, and prompt rendering.
	Topic string

	// FindingsDoc is the absolute path the findings doc was written to
	// (echoes BootstrapInput.FindingsDoc).
	FindingsDoc string
}

// Bootstrap writes the initial findings doc to disk.
//
// File-write semantics: creates parent directories as needed and writes
// the findings file unconditionally. Callers that want "skip if findings
// doc exists" semantics should stat the path themselves; Bootstrap is
// idempotent at the byte level (same input → same output) but does not
// protect existing files — protecting an existing investigation belongs
// to a layer above this one.
func Bootstrap(ctx context.Context, in BootstrapInput) (BootstrapResult, error) {
	_ = ctx // Reserved for future use (e.g. cancellation during long renders).

	if in.FindingsDoc == "" {
		return BootstrapResult{}, errors.New("FindingsDoc is required")
	}

	var (
		topic string
		body  []byte
	)

	switch {
	case in.SeedDoc != "":
		seedBytes, err := os.ReadFile(in.SeedDoc)
		if err != nil {
			return BootstrapResult{}, fmt.Errorf("read seed doc: %w", err)
		}
		topic = DeriveTopicFromSeed(seedBytes, in.SeedDoc)
		body = []byte(renderInvestigationScaffold(
			topic,
			time.Now().UTC().Format("2006-01-02"),
			string(seedBytes),
		))

	case len(in.IssueLinkSeed) > 0:
		topic = in.IssueLinkTopic
		if topic == "" {
			topic = DeriveTopicFromSeed(in.IssueLinkSeed, in.FindingsDoc)
		}
		body = []byte(renderInvestigationScaffold(
			topic,
			time.Now().UTC().Format("2006-01-02"),
			string(in.IssueLinkSeed),
		))

	case in.Topic != "":
		topic = in.Topic
		body = []byte(renderInvestigationScaffold(
			in.Topic,
			time.Now().UTC().Format("2006-01-02"),
			"",
		))

	default:
		return BootstrapResult{}, errors.New("Bootstrap: one of SeedDoc, Topic, or IssueLinkSeed is required")
	}

	if err := os.MkdirAll(filepath.Dir(in.FindingsDoc), 0o750); err != nil {
		return BootstrapResult{}, fmt.Errorf("create findings dir: %w", err)
	}

	if err := os.WriteFile(in.FindingsDoc, body, 0o600); err != nil {
		return BootstrapResult{}, fmt.Errorf("write findings doc: %w", err)
	}

	return BootstrapResult{
		Topic:       topic,
		FindingsDoc: in.FindingsDoc,
	}, nil
}

// renderInvestigationScaffold returns the investigation scaffold body.
//
// The doc is a richer multi-section investigation template — TLDR (current
// best answer), Question, Prior work, System under investigation, Approach,
// Findings, Unknowns / Assumptions, Conclusion. Agents append findings and
// evidence each turn until they converge on the Conclusion.
//
// When questionBody is non-empty (seed-doc or issue-link paths), it is
// printed verbatim under `## Question`. When empty (topic-only path), the
// topic itself is printed under `## Question`. Trailing whitespace on
// questionBody is trimmed to keep section spacing consistent.
func renderInvestigationScaffold(topic, createdISODate, questionBody string) string {
	question := strings.TrimRight(questionBody, " \t\r\n")
	if question == "" {
		question = topic
	}
	return fmt.Sprintf(`# Investigation: %s

**Status:** investigating
**Started:** %s

## TLDR

<!-- 2-4 sentences. The reader who only reads this section must understand:
the question, the answer (root cause / conclusion), and the single most
important piece of evidence. Updated every turn — until consensus, this
section reflects the current best hypothesis with confidence ("likely",
"confirmed"), not a final answer. -->

## Question

%s

## Prior work

<!-- What was searched, what was found, what was ruled out. If nothing
relevant, say "no prior work found; searched for: <queries>". When a
finding cites a commit hash, also note the Entire-Checkpoint trailer
(if any) and what `+"`entire explain --checkpoint <id> --no-pager`"+`
revealed. -->

## System under investigation

<!-- A small diagram of the path under investigation. For
producer/consumer or queue-shaped systems, show: who writes the input,
who reads it, where retries happen, and the per-attempt cost. ASCII or
mermaid both fine. Two boxes and an arrow beats a paragraph. -->

## Approach

<!-- A concise summary (3-5 sentences) of how the investigation was
conducted: the key queries, files read, hypotheses formed, and
hypotheses ruled out. Edit in place each turn — replace stale text,
keep the section tight. NO per-agent attribution; NO per-turn entries
("claude-code (round 1):" / "codex (round 2):"). The reasoning trail
lives in the agent session transcripts on entire/checkpoints/v1; run
`+"`entire checkpoint explain <id>`"+` to retrieve it. -->

## Findings

<!-- One numbered subsection per finding. Every claim needs concrete evidence:
file paths with line numbers (e.g. internal/cli/root.go:17), commands you ran,
test output, or direct quotes. -->

## Unknowns / Assumptions

<!-- Anything you could not confirm; assumptions that should be flagged. -->

## Conclusion

<!-- Filled in once consensus is reached. Stop here. Recommendations and
action items belong in a plan, not an investigation. -->
`, topic, createdISODate, question)
}
