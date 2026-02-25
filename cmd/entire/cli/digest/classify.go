package digest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Highlights contains AI-curated highlights from team sessions.
// When CuratedItems is populated (from AI curation), it takes priority.
// Fun/Clever/Notable are the legacy format used when CuratedItems is empty.
type Highlights struct {
	CuratedItems []CuratedHighlight `json:"moments,omitempty"`

	// Legacy fields for backwards compatibility with --ai-curate=false tests
	Fun     []Highlight `json:"fun,omitempty"`
	Clever  []Highlight `json:"clever,omitempty"`
	Notable []Highlight `json:"notable,omitempty"`
}

// CuratedHighlight is a single curated moment with editorial commentary.
type CuratedHighlight struct {
	Title      string `json:"title"`
	Author     string `json:"author"`
	DateCtx    string `json:"date_context"`
	Exchange   string `json:"exchange"`
	Commentary string `json:"commentary"`
}

// Highlight is a single classified prompt (legacy format).
type Highlight struct {
	Author  string `json:"author"`
	Quote   string `json:"quote"`
	Context string `json:"context"`
	Why     string `json:"why"`
}

const curatePromptTemplate = `You're writing the highlights section of a team's weekly digest of their AI coding sessions. Below are conversation excerpts from the team working with AI coding assistants.

Your job: find the 5 most entertaining, clever, or notable moments and write them up with personality and editorial commentary - like the best moments from a team Slack channel.

<conversations>
%s
</conversations>

For each highlight, write:
- A punchy title (the best quote or a witty summary)
- Who said it and when
- The relevant exchange (quote the actual back-and-forth, not just one message)
- 2-3 sentences of editorial commentary explaining why this moment is great

What makes a good highlight:
- Frustrated developer moments ("Thank you, Captain Obvious", "Are you stuck? You seem to be stuck.")
- Escalating exchanges where the human gets increasingly exasperated ("Really?" after three rounds)
- The same frustration hitting multiple team members independently
- Clever meta-usage of AI (teaching Claude to use its own conversation history)
- Beautifully terse prompts ("ls" as an opening move, one-word responses)
- Moments where the AI clearly went off the rails and the human called it out
- The human's personality showing through brevity or humor

What to skip:
- Long copy-pasted specs or implementation plans (boring)
- Routine "fix the tests" without interesting exchange context
- Auto-generated session continuations
- Anything where the exchange is just normal back-and-forth work

Style guide:
- Be affectionate and celebratory, never mocking. Think "inside joke among friends"
- Include enough context that someone not in the session gets why it's funny
- Quote actual words, don't paraphrase
- Commentary should have voice: "Classic frustrated-developer-with-AI moment", "chef's kiss", "like a dog that won't stop fetching"

Return a JSON array of exactly 5 items (or fewer if there aren't enough good moments):
[{"title": "...", "author": "...", "date_context": "...", "exchange": "...", "commentary": "..."}]

Return ONLY the JSON array, no markdown formatting or explanation.`

// Classifier generates AI-curated highlights from digest data.
type Classifier struct {
	// ClaudePath is the path to the claude CLI. Defaults to "claude".
	ClaudePath string

	// Model is the Claude model to use. Defaults to "sonnet".
	Model string

	// CommandRunner allows injection for testing.
	CommandRunner func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// claudeCLIResponse represents the JSON response from the Claude CLI.
type claudeCLIResponse struct {
	Result string `json:"result"`
}

// maxExchangeLen is the max characters per exchange turn sent to the classifier.
const maxExchangeLen = 500

// Classify analyzes digest conversations and returns curated highlights.
func (c *Classifier) Classify(ctx context.Context, d *TeamDigest) (*Highlights, error) {
	// Build input from conversation windows across all authors
	var sb strings.Builder
	for _, author := range d.SortedAuthors() {
		for _, conv := range author.Conversations {
			fmt.Fprintf(&sb, "--- %s (%s, %s) ---\n",
				author.Name, conv.Branch, conv.Timestamp.Format("Jan 2"))
			for _, ex := range conv.Exchanges {
				label := "User"
				if ex.Role == "assistant" {
					label = "Claude"
				}
				content := ex.Content
				if len(content) > maxExchangeLen {
					content = content[:maxExchangeLen] + "..."
				}
				fmt.Fprintf(&sb, "%s: %s\n", label, content)
			}
			fmt.Fprintln(&sb)
		}
	}

	if sb.Len() == 0 {
		return nil, errors.New("no conversations to classify")
	}

	prompt := fmt.Sprintf(curatePromptTemplate, sb.String())

	runner := c.CommandRunner
	if runner == nil {
		runner = exec.CommandContext
	}

	claudePath := c.ClaudePath
	if claudePath == "" {
		claudePath = "claude"
	}

	model := c.Model
	if model == "" {
		model = "sonnet"
	}

	cmd := runner(ctx, claudePath, "--print", "--output-format", "json", "--model", model, "--setting-sources", "")
	cmd.Dir = os.TempDir()
	cmd.Env = stripSessionEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return nil, fmt.Errorf("claude CLI not found: %w", err)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			errMsg := stderr.String()
			if errMsg == "" {
				errMsg = stdout.String()
			}
			return nil, fmt.Errorf("claude CLI failed (exit %d): %s", exitErr.ExitCode(), errMsg)
		}
		return nil, fmt.Errorf("failed to run claude CLI: %w", err)
	}

	// Parse CLI response
	var cliResponse claudeCLIResponse
	if err := json.Unmarshal(stdout.Bytes(), &cliResponse); err != nil {
		return nil, fmt.Errorf("failed to parse claude CLI response: %w", err)
	}

	resultJSON := extractJSONFromMarkdown(cliResponse.Result)

	// Try parsing as curated highlights array
	var items []CuratedHighlight
	if err := json.Unmarshal([]byte(resultJSON), &items); err != nil {
		return nil, fmt.Errorf("failed to parse highlights JSON: %w (response: %s)", err, resultJSON)
	}

	return &Highlights{CuratedItems: items}, nil
}

// stripSessionEnv returns a copy of env with GIT_*, CLAUDECODE*, and CLAUDE_CODE_* variables removed.
// Stripping these allows the Claude CLI subprocess to run inside a Claude Code session.
func stripSessionEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_") || strings.HasPrefix(e, "CLAUDECODE") || strings.HasPrefix(e, "CLAUDE_CODE_") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// extractJSONFromMarkdown attempts to extract JSON from markdown code blocks.
func extractJSONFromMarkdown(s string) string {
	s = strings.TrimSpace(s)

	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}

	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}

	return s
}
