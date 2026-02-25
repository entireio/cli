package digest

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// maxPromptDisplayLen is the maximum characters to show for a prompt in terminal mode.
const maxPromptDisplayLen = 200

// FormatOptions controls output verbosity.
type FormatOptions struct {
	Short bool // stats only, no prompts
}

// FormatTerminal writes a terminal-friendly digest to the writer.
func FormatTerminal(w io.Writer, d *TeamDigest, highlights *Highlights, opts FormatOptions) {
	// Header
	fmt.Fprintf(w, "Team Digest")
	if d.Period != "all" {
		fmt.Fprintf(w, " (last %s)", d.Period)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, strings.Repeat("-", 60))
	fmt.Fprintln(w)

	// Summary stats
	fmt.Fprintf(w, "Checkpoints: %d\n", d.Total.Checkpoints)
	fmt.Fprintf(w, "Sessions:    %d\n", d.Total.Sessions)
	fmt.Fprintf(w, "Prompts:     %d\n", d.Total.Prompts)
	fmt.Fprintf(w, "Files:       %d\n", d.Total.FilesTouched)
	totalTokens := d.Total.TokenUsage.InputTokens + d.Total.TokenUsage.OutputTokens
	if totalTokens > 0 {
		fmt.Fprintf(w, "Tokens:      %s\n", formatTokenCount(totalTokens))
	}
	fmt.Fprintln(w)

	if len(d.Authors) == 0 {
		fmt.Fprintln(w, "No activity found.")
		return
	}

	if opts.Short {
		// Short mode: stats per author, no prompts
		for _, author := range d.SortedAuthors() {
			authorTokens := author.TokenUsage.InputTokens + author.TokenUsage.OutputTokens
			fmt.Fprintf(w, "  %-20s %d prompts | %d sessions | %d files",
				author.Name, len(author.Prompts), author.SessionCount, len(author.FilesTouched))
			if authorTokens > 0 {
				fmt.Fprintf(w, " | %s tokens", formatTokenCount(authorTokens))
			}
			fmt.Fprintln(w)
		}
		return
	}

	// AI-curated highlights
	if highlights != nil {
		formatHighlights(w, highlights)
	}

	// Per-author sections
	for _, author := range d.SortedAuthors() {
		formatAuthorSection(w, author)
	}
}

// FormatMarkdown writes a markdown-formatted digest to the writer.
func FormatMarkdown(w io.Writer, d *TeamDigest, highlights *Highlights, opts FormatOptions) {
	fmt.Fprintf(w, "# Team Digest")
	if d.Period != "all" {
		fmt.Fprintf(w, " (last %s)", d.Period)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	// Summary table
	fmt.Fprintln(w, "| Metric | Value |")
	fmt.Fprintln(w, "|--------|-------|")
	fmt.Fprintf(w, "| Checkpoints | %d |\n", d.Total.Checkpoints)
	fmt.Fprintf(w, "| Sessions | %d |\n", d.Total.Sessions)
	fmt.Fprintf(w, "| Prompts | %d |\n", d.Total.Prompts)
	fmt.Fprintf(w, "| Files | %d |\n", d.Total.FilesTouched)
	totalTokens := d.Total.TokenUsage.InputTokens + d.Total.TokenUsage.OutputTokens
	if totalTokens > 0 {
		fmt.Fprintf(w, "| Tokens | %s |\n", formatTokenCount(totalTokens))
	}
	fmt.Fprintln(w)

	if len(d.Authors) == 0 {
		fmt.Fprintln(w, "No activity found.")
		return
	}

	if opts.Short {
		return
	}

	// AI-curated highlights
	if highlights != nil {
		formatHighlightsMarkdown(w, highlights)
	}

	// Per-author sections
	for _, author := range d.SortedAuthors() {
		formatAuthorSectionMarkdown(w, author)
	}
}

func formatAuthorSection(w io.Writer, author *AuthorDigest) {
	fmt.Fprintf(w, "## %s\n", author.Name)

	// Stats line
	authorTokens := author.TokenUsage.InputTokens + author.TokenUsage.OutputTokens
	fmt.Fprintf(w, "   %d prompts | %d sessions | %d files",
		len(author.Prompts), author.SessionCount, len(author.FilesTouched))
	if authorTokens > 0 {
		fmt.Fprintf(w, " | %s tokens", formatTokenCount(authorTokens))
	}
	fmt.Fprintln(w)

	// Branches
	branches := author.SortedBranches()
	if len(branches) > 0 {
		fmt.Fprintf(w, "   Branches: %s\n", strings.Join(branches, ", "))
	}
	fmt.Fprintln(w)

	// Prompts (already sorted by score and limited)
	for _, p := range author.Prompts {
		text := truncate(p.Content, maxPromptDisplayLen)
		fmt.Fprintf(w, "   > %s\n", text)
	}
	fmt.Fprintln(w)
}

func formatAuthorSectionMarkdown(w io.Writer, author *AuthorDigest) {
	fmt.Fprintf(w, "## %s\n\n", author.Name)

	authorTokens := author.TokenUsage.InputTokens + author.TokenUsage.OutputTokens
	fmt.Fprintf(w, "**%d prompts** | %d sessions | %d files",
		len(author.Prompts), author.SessionCount, len(author.FilesTouched))
	if authorTokens > 0 {
		fmt.Fprintf(w, " | %s tokens", formatTokenCount(authorTokens))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)

	branches := author.SortedBranches()
	if len(branches) > 0 {
		fmt.Fprintf(w, "**Branches:** %s\n\n", strings.Join(branches, ", "))
	}

	if len(author.Prompts) > 0 {
		fmt.Fprintln(w, "### Prompts")
		fmt.Fprintln(w)
		for _, p := range author.Prompts {
			fmt.Fprintf(w, "> %s\n\n", p.Content)
		}
	}
}

func formatHighlights(w io.Writer, h *Highlights) {
	// New curated format
	if len(h.CuratedItems) > 0 {
		fmt.Fprintln(w, "HIGHLIGHTS")
		fmt.Fprintln(w, strings.Repeat("=", 60))
		fmt.Fprintln(w)
		for i, item := range h.CuratedItems {
			fmt.Fprintf(w, "%d. \"%s\" - %s\n", i+1, item.Title, item.Author)
			if item.DateCtx != "" {
				fmt.Fprintf(w, "   (%s)\n", item.DateCtx)
			}
			fmt.Fprintln(w)
			if item.Exchange != "" {
				for _, line := range strings.Split(item.Exchange, "\n") {
					fmt.Fprintf(w, "   %s\n", line)
				}
				fmt.Fprintln(w)
			}
			if item.Commentary != "" {
				for _, line := range wrapText(item.Commentary, 72) {
					fmt.Fprintf(w, "   %s\n", line)
				}
			}
			fmt.Fprintln(w)
		}
		return
	}

	// Legacy category format
	if len(h.Fun) == 0 && len(h.Clever) == 0 && len(h.Notable) == 0 {
		return
	}

	fmt.Fprintln(w, "HIGHLIGHTS")
	fmt.Fprintln(w, strings.Repeat("=", 60))
	fmt.Fprintln(w)

	if len(h.Fun) > 0 {
		fmt.Fprintln(w, "Fun / Frustrated:")
		for _, item := range h.Fun {
			fmt.Fprintf(w, "  [%s] \"%s\"\n", item.Author, item.Quote)
			if item.Context != "" {
				fmt.Fprintf(w, "    %s\n", item.Context)
			}
		}
		fmt.Fprintln(w)
	}

	if len(h.Clever) > 0 {
		fmt.Fprintln(w, "Clever:")
		for _, item := range h.Clever {
			fmt.Fprintf(w, "  [%s] \"%s\"\n", item.Author, item.Quote)
			if item.Context != "" {
				fmt.Fprintf(w, "    %s\n", item.Context)
			}
		}
		fmt.Fprintln(w)
	}

	if len(h.Notable) > 0 {
		fmt.Fprintln(w, "Notable:")
		for _, item := range h.Notable {
			fmt.Fprintf(w, "  [%s] \"%s\"\n", item.Author, item.Quote)
			if item.Context != "" {
				fmt.Fprintf(w, "    %s\n", item.Context)
			}
		}
		fmt.Fprintln(w)
	}
}

func formatHighlightsMarkdown(w io.Writer, h *Highlights) {
	// New curated format
	if len(h.CuratedItems) > 0 {
		fmt.Fprintln(w, "## Highlights")
		fmt.Fprintln(w)
		for i, item := range h.CuratedItems {
			fmt.Fprintf(w, "### %d. \"%s\" - %s\n", i+1, item.Title, item.Author)
			if item.DateCtx != "" {
				fmt.Fprintf(w, "*%s*\n", item.DateCtx)
			}
			fmt.Fprintln(w)
			if item.Exchange != "" {
				fmt.Fprintf(w, "> %s\n", strings.ReplaceAll(item.Exchange, "\n", "\n> "))
			}
			fmt.Fprintln(w)
			if item.Commentary != "" {
				fmt.Fprintln(w, item.Commentary)
			}
			fmt.Fprintln(w)
		}
		return
	}

	// Legacy category format
	if len(h.Fun) == 0 && len(h.Clever) == 0 && len(h.Notable) == 0 {
		return
	}

	fmt.Fprintln(w, "## Highlights")
	fmt.Fprintln(w)

	if len(h.Fun) > 0 {
		fmt.Fprintln(w, "### Fun / Frustrated")
		fmt.Fprintln(w)
		for _, item := range h.Fun {
			fmt.Fprintf(w, "- **%s**: \"%s\"", item.Author, item.Quote)
			if item.Context != "" {
				fmt.Fprintf(w, " - _%s_", item.Context)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}

	if len(h.Clever) > 0 {
		fmt.Fprintln(w, "### Clever")
		fmt.Fprintln(w)
		for _, item := range h.Clever {
			fmt.Fprintf(w, "- **%s**: \"%s\"", item.Author, item.Quote)
			if item.Context != "" {
				fmt.Fprintf(w, " - _%s_", item.Context)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}

	if len(h.Notable) > 0 {
		fmt.Fprintln(w, "### Notable")
		fmt.Fprintln(w)
		for _, item := range h.Notable {
			fmt.Fprintf(w, "- **%s**: \"%s\"", item.Author, item.Quote)
			if item.Context != "" {
				fmt.Fprintf(w, " - _%s_", item.Context)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}
}

// wrapText wraps text to the given width at word boundaries.
func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}

	var lines []string
	line := words[0]
	for _, word := range words[1:] {
		if len(line)+1+len(word) > width {
			lines = append(lines, line)
			line = word
		} else {
			line += " " + word
		}
	}
	lines = append(lines, line)
	return lines
}

func truncate(s string, maxLen int) string {
	// Replace newlines with spaces for single-line display
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// diverseTopN selects the top N conversations by score while filtering out
// near-duplicates. Conversations that share >60% word overlap with an
// already-selected conversation are skipped in favor of more diverse results.
func diverseTopN(convos []JSONConversation, n int) []JSONConversation {
	sort.Slice(convos, func(i, j int) bool {
		return convos[i].Score > convos[j].Score
	})

	selected := make([]JSONConversation, 0, n)
	for _, c := range convos {
		if len(selected) >= n {
			break
		}
		if isTooSimilar(c, selected) {
			continue
		}
		selected = append(selected, c)
	}
	return selected
}

// isTooSimilar checks if a candidate conversation's user prompt overlaps >60%
// with any already-selected conversation.
func isTooSimilar(candidate JSONConversation, selected []JSONConversation) bool {
	candidateWords := wordSet(firstUserContent(candidate))
	if len(candidateWords) == 0 {
		return false
	}
	for _, s := range selected {
		selectedWords := wordSet(firstUserContent(s))
		if len(selectedWords) == 0 {
			continue
		}
		overlap := 0
		for w := range candidateWords {
			if selectedWords[w] {
				overlap++
			}
		}
		smaller := len(candidateWords)
		if len(selectedWords) < smaller {
			smaller = len(selectedWords)
		}
		if smaller > 0 && float64(overlap)/float64(smaller) > 0.6 {
			return true
		}
	}
	return false
}

// firstUserContent returns the first user message from a JSON conversation.
func firstUserContent(c JSONConversation) string {
	for _, ex := range c.Exchanges {
		if ex.Role == "user" {
			return ex.Content
		}
	}
	return ""
}

// wordSet splits text into a set of lowercase words.
func wordSet(text string) map[string]bool {
	words := strings.Fields(strings.ToLower(text))
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}

func formatTokenCount(tokens int) string {
	if tokens >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	}
	if tokens >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(tokens)/1_000)
	}
	return fmt.Sprintf("%d", tokens)
}

// maxJSONExchangeLen is the max characters per exchange turn in JSON output.
const maxJSONExchangeLen = 800

// maxGlobalConversations is the max total conversations in JSON output across all authors.
const maxGlobalConversations = 30

// JSONOutput is the top-level structure for --format json.
type JSONOutput struct {
	Period           string             `json:"period"`
	Stats            JSONStats          `json:"stats"`
	ScoringGuide     string             `json:"scoring_guide"`
	TopConversations []JSONConversation `json:"top_conversations"`
	Authors          []JSONAuthor       `json:"authors"`
}

// JSONStats contains aggregate metrics.
type JSONStats struct {
	Checkpoints int `json:"checkpoints"`
	Sessions    int `json:"sessions"`
	Prompts     int `json:"prompts"`
	Files       int `json:"files"`
	Tokens      int `json:"tokens"`
}

// JSONConversation is a scored conversation with exchanges.
type JSONConversation struct {
	Rank      int            `json:"rank"`
	Score     int            `json:"score"`
	Author    string         `json:"author"`
	Branch    string         `json:"branch"`
	Timestamp string         `json:"timestamp"`
	Exchanges []JSONExchange `json:"exchanges"`
}

// JSONExchange is one turn in a conversation.
type JSONExchange struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// JSONAuthor contains per-author summary stats.
type JSONAuthor struct {
	Name     string   `json:"name"`
	Prompts  int      `json:"prompts"`
	Sessions int      `json:"sessions"`
	Files    int      `json:"files"`
	Tokens   int      `json:"tokens"`
	Branches []string `json:"branches"`
}

// FormatJSON writes a JSON digest to the writer with conversation windows for AI consumption.
func FormatJSON(w io.Writer, d *TeamDigest) {
	output := JSONOutput{
		Period: d.Period,
		Stats: JSONStats{
			Checkpoints: d.Total.Checkpoints,
			Sessions:    d.Total.Sessions,
			Prompts:     d.Total.Prompts,
			Files:       d.Total.FilesTouched,
			Tokens:      d.Total.TokenUsage.InputTokens + d.Total.TokenUsage.OutputTokens,
		},
		ScoringGuide: "Scores are a rough interestingness signal, not a quality guarantee. " +
			"Higher scores suggest personality, emotion, or wit, but trust your editorial judgment. " +
			"Prioritize variety over raw score - never feature the same prompt twice.",
	}

	// Collect all conversations across all authors, sorted globally by score
	var allConvos []JSONConversation
	for _, author := range d.SortedAuthors() {
		for _, c := range author.Conversations {
			jc := JSONConversation{
				Score:     c.Score,
				Author:    c.Author,
				Branch:    c.Branch,
				Timestamp: c.Timestamp.Format("2006-01-02"),
			}
			for _, ex := range c.Exchanges {
				content := ex.Content
				if len(content) > maxJSONExchangeLen {
					content = content[:maxJSONExchangeLen] + "..."
				}
				jc.Exchanges = append(jc.Exchanges, JSONExchange{
					Role:    ex.Role,
					Content: content,
				})
			}
			allConvos = append(allConvos, jc)
		}
	}

	// Select diverse top conversations (score-descending with similarity filtering)
	allConvos = diverseTopN(allConvos, maxGlobalConversations)

	// Add rank numbers
	for i := range allConvos {
		allConvos[i].Rank = i + 1
	}
	output.TopConversations = allConvos

	// Per-author summaries
	for _, author := range d.SortedAuthors() {
		output.Authors = append(output.Authors, JSONAuthor{
			Name:     author.Name,
			Prompts:  len(author.Prompts),
			Sessions: author.SessionCount,
			Files:    len(author.FilesTouched),
			Tokens:   author.TokenUsage.InputTokens + author.TokenUsage.OutputTokens,
			Branches: author.SortedBranches(),
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(output)
}
