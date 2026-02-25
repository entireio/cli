package digest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParsePeriod(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 2, 24, 15, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		period  string
		wantErr bool
		check   func(t *testing.T, start time.Time)
	}{
		{
			name:   "7d",
			period: "7d",
			check: func(t *testing.T, start time.Time) {
				t.Helper()
				expected := now.AddDate(0, 0, -7)
				if !start.Equal(expected) {
					t.Errorf("got %v, want %v", start, expected)
				}
			},
		},
		{
			name:   "30d",
			period: "30d",
			check: func(t *testing.T, start time.Time) {
				t.Helper()
				expected := now.AddDate(0, 0, -30)
				if !start.Equal(expected) {
					t.Errorf("got %v, want %v", start, expected)
				}
			},
		},
		{
			name:   "1d",
			period: "1d",
			check: func(t *testing.T, start time.Time) {
				t.Helper()
				expected := now.AddDate(0, 0, -1)
				if !start.Equal(expected) {
					t.Errorf("got %v, want %v", start, expected)
				}
			},
		},
		{
			name:   "today",
			period: "today",
			check: func(t *testing.T, start time.Time) {
				t.Helper()
				expected := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
				if !start.Equal(expected) {
					t.Errorf("got %v, want %v", start, expected)
				}
			},
		},
		{
			name:   "all",
			period: "all",
			check: func(t *testing.T, start time.Time) {
				t.Helper()
				if !start.IsZero() {
					t.Errorf("expected zero time for 'all', got %v", start)
				}
			},
		},
		{
			name:    "invalid period",
			period:  "foo",
			wantErr: true,
		},
		{
			name:    "empty period",
			period:  "",
			wantErr: true,
		},
		{
			name:    "negative days",
			period:  "-5d",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			start, err := ParsePeriod(tt.period, now)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.check(t, start)
		})
	}
}

func TestIsNoisePrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		text  string
		noise bool
	}{
		{"normal prompt", "fix the login bug", false},
		{"task notification", "<task-notification>stuff</task-notification>", true},
		{"interrupted", "something [Request interrupted by user]", true},
		{"system reminder", "<system-reminder>stuff</system-reminder>", true},
		{"empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isNoisePrompt(tt.text)
			if got != tt.noise {
				t.Errorf("isNoisePrompt(%q) = %v, want %v", tt.text, got, tt.noise)
			}
		})
	}
}

func TestTeamDigest_SortedAuthors(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Authors: map[string]*AuthorDigest{
			"Alice": {Name: "Alice", Prompts: make([]Prompt, 3)},
			"Bob":   {Name: "Bob", Prompts: make([]Prompt, 10)},
			"Carol": {Name: "Carol", Prompts: make([]Prompt, 1)},
		},
	}

	sorted := d.SortedAuthors()
	if len(sorted) != 3 {
		t.Fatalf("expected 3 authors, got %d", len(sorted))
	}
	if sorted[0].Name != "Bob" {
		t.Errorf("expected Bob first (most prompts), got %s", sorted[0].Name)
	}
	if sorted[1].Name != "Alice" {
		t.Errorf("expected Alice second, got %s", sorted[1].Name)
	}
	if sorted[2].Name != "Carol" {
		t.Errorf("expected Carol third, got %s", sorted[2].Name)
	}
}

func TestAuthorDigest_SortedBranches(t *testing.T) {
	t.Parallel()

	a := &AuthorDigest{
		Branches: map[string]bool{
			"main":        true,
			"feat/auth":   true,
			"fix/bug-123": true,
		},
	}

	branches := a.SortedBranches()
	if len(branches) != 3 {
		t.Fatalf("expected 3 branches, got %d", len(branches))
	}
	if branches[0] != "feat/auth" {
		t.Errorf("expected feat/auth first (alphabetical), got %s", branches[0])
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"long", "hello world", 5, "hello..."},
		{"newlines", "hello\nworld", 20, "hello world"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestFormatTokenCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tokens int
		want   string
	}{
		{"small", 500, "500"},
		{"thousands", 5000, "5.0K"},
		{"millions", 1500000, "1.5M"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := formatTokenCount(tt.tokens)
			if got != tt.want {
				t.Errorf("formatTokenCount(%d) = %q, want %q", tt.tokens, got, tt.want)
			}
		})
	}
}

func TestFormatTerminal_Empty(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period:  "7d",
		Authors: map[string]*AuthorDigest{},
	}

	var buf strings.Builder
	FormatTerminal(&buf, d, nil, FormatOptions{})

	output := buf.String()
	if !strings.Contains(output, "No activity found") {
		t.Errorf("expected 'No activity found' in output, got:\n%s", output)
	}
}

func TestFormatTerminal_WithData(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period: "7d",
		Authors: map[string]*AuthorDigest{
			"Myk": {
				Name:         "Myk",
				SessionCount: 2,
				Branches:     map[string]bool{"main": true},
				FilesTouched: map[string]bool{"file.go": true},
				Prompts: []Prompt{
					{Content: "fix the login bug"},
					{Content: "add tests"},
				},
			},
		},
		Total: Stats{
			Checkpoints:  5,
			Sessions:     2,
			Prompts:      2,
			FilesTouched: 1,
		},
	}

	var buf strings.Builder
	FormatTerminal(&buf, d, nil, FormatOptions{})

	output := buf.String()
	if !strings.Contains(output, "Team Digest") {
		t.Error("expected 'Team Digest' header")
	}
	if !strings.Contains(output, "Myk") {
		t.Error("expected author name 'Myk'")
	}
	if !strings.Contains(output, "fix the login bug") {
		t.Error("expected prompt text")
	}
}

func TestFormatMarkdown_WithData(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period: "30d",
		Authors: map[string]*AuthorDigest{
			"Bob": {
				Name:         "Bob",
				SessionCount: 1,
				Branches:     map[string]bool{"feat/auth": true},
				FilesTouched: map[string]bool{"auth.go": true},
				Prompts: []Prompt{
					{Content: "implement OAuth flow"},
				},
			},
		},
		Total: Stats{
			Checkpoints:  1,
			Sessions:     1,
			Prompts:      1,
			FilesTouched: 1,
		},
	}

	var buf strings.Builder
	FormatMarkdown(&buf, d, nil, FormatOptions{})

	output := buf.String()
	if !strings.Contains(output, "# Team Digest") {
		t.Error("expected markdown header")
	}
	if !strings.Contains(output, "## Bob") {
		t.Error("expected author section")
	}
	if !strings.Contains(output, "implement OAuth flow") {
		t.Error("expected prompt in markdown")
	}
}

func TestFormatTerminal_WithHighlights(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period: "7d",
		Authors: map[string]*AuthorDigest{
			"Alice": {
				Name:         "Alice",
				SessionCount: 1,
				Branches:     map[string]bool{"main": true},
				FilesTouched: map[string]bool{},
				Prompts:      []Prompt{{Content: "hello"}},
			},
		},
		Total: Stats{Checkpoints: 1, Sessions: 1, Prompts: 1},
	}

	highlights := &Highlights{
		Fun: []Highlight{
			{Author: "Alice", Quote: "Thank you, Captain Obvious", Context: "after a verbose explanation"},
		},
	}

	var buf strings.Builder
	FormatTerminal(&buf, d, highlights, FormatOptions{})

	output := buf.String()
	if !strings.Contains(output, "HIGHLIGHTS") {
		t.Error("expected HIGHLIGHTS section")
	}
	if !strings.Contains(output, "Thank you, Captain Obvious") {
		t.Error("expected highlight quote")
	}
}

func TestFormatTerminal_CuratedHighlights(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period: "7d",
		Authors: map[string]*AuthorDigest{
			"Myk": {
				Name:         "Myk",
				SessionCount: 1,
				Branches:     map[string]bool{"main": true},
				FilesTouched: map[string]bool{},
				Prompts:      []Prompt{{Content: "hello"}},
			},
		},
		Total: Stats{Checkpoints: 1, Sessions: 1, Prompts: 1},
	}

	highlights := &Highlights{
		CuratedItems: []CuratedHighlight{
			{
				Title:      "Thank you, Captain Obvious",
				Author:     "Myk Melez",
				DateCtx:    "Feb 21, multi-segment recording branch",
				Exchange:   "Myk: \"Thank you, Captain Obvious.\"",
				Commentary: "Classic frustrated-developer-with-AI moment.",
			},
		},
	}

	var buf strings.Builder
	FormatTerminal(&buf, d, highlights, FormatOptions{})

	output := buf.String()
	if !strings.Contains(output, "HIGHLIGHTS") {
		t.Error("expected HIGHLIGHTS section")
	}
	if !strings.Contains(output, "Thank you, Captain Obvious") {
		t.Error("expected curated title")
	}
	if !strings.Contains(output, "Myk Melez") {
		t.Error("expected author name")
	}
	if !strings.Contains(output, "Classic frustrated") {
		t.Error("expected editorial commentary")
	}
}

func TestWrapText(t *testing.T) {
	t.Parallel()

	lines := wrapText("This is a test of the word wrapping function that should break at word boundaries", 30)
	if len(lines) < 2 {
		t.Errorf("expected multiple lines, got %d", len(lines))
	}
	for _, line := range lines {
		if len(line) > 35 { // allow some slack for long words
			t.Errorf("line too long: %q (%d chars)", line, len(line))
		}
	}
}

func TestScorePrompt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		wantAbove int // score should be > this
		wantBelow int // score should be < this
	}{
		{"short excited", "nice!", 4, 100},                  // short +3, punctuation +2, celebration +3
		{"frustration", "why is this broken?!", 4, 100},     // frustration +3, punctuation +2
		{"celebration", "finally worked!", 4, 100},          // celebration +3, punctuation +2, short +3
		{"long paste", strings.Repeat("word ", 200), -3, 1}, // long -2
		{"session continuation", "This session is being continued from a previous run", -6, 0},
		{"slash command", "/commit", -1, 4}, // slash -1, short +3 = 2
		{"normal prompt", "please add error handling to the login service", -1, 5},

		// Sarcasm signals
		{"captain obvious", "Thank you, Captain Obvious", 4, 100},              // short +3, sarcasm +3
		{"thanks for nothing", "thanks for nothing, that was useless", 2, 100}, // sarcasm +3

		// Single-word disbelief
		{"really", "Really?", 7, 100},       // short +3, single-word +4, punctuation +2
		{"seriously", "Seriously?", 7, 100}, // short +3, single-word +4, punctuation +2
		{"no period", "No.", 6, 100},        // short +3, single-word +4

		// Context-amnesia
		{"forgotten everything", "Have you forgotten everything about this project?", 3, 100}, // punctuation +2, amnesia +3
		{"you just told me", "You just told me the opposite", 2, 100},                         // amnesia +3

		// AI self-reference
		{"are you stuck", "Are you stuck?", 7, 100}, // short +3, punctuation +2, frustration("stuck") +3, ai-self-ref +2
		{"is bash stuck", "Is bash stuck?", 7, 100}, // short +3, punctuation +2, frustration("stuck") +3, ai-self-ref +2

		// Captain Obvious should beat a routine prompt
		{"sarcasm beats routine", "Thank you, Captain Obvious", 4, 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			score := scorePrompt(tt.text)
			if score <= tt.wantAbove {
				t.Errorf("scorePrompt(%q) = %d, want > %d", tt.text, score, tt.wantAbove)
			}
			if score >= tt.wantBelow {
				t.Errorf("scorePrompt(%q) = %d, want < %d", tt.text, score, tt.wantBelow)
			}
		})
	}
}

func TestFormatTerminal_Short(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period: "7d",
		Authors: map[string]*AuthorDigest{
			"Myk": {
				Name:         "Myk",
				SessionCount: 2,
				Branches:     map[string]bool{"main": true},
				FilesTouched: map[string]bool{"file.go": true},
				Prompts: []Prompt{
					{Content: "fix the login bug"},
					{Content: "add tests"},
				},
			},
		},
		Total: Stats{
			Checkpoints:  5,
			Sessions:     2,
			Prompts:      2,
			FilesTouched: 1,
		},
	}

	var buf strings.Builder
	FormatTerminal(&buf, d, nil, FormatOptions{Short: true})

	output := buf.String()
	if !strings.Contains(output, "Myk") {
		t.Error("expected author name in short output")
	}
	if strings.Contains(output, "fix the login bug") {
		t.Error("short mode should not include prompt text")
	}
	if !strings.Contains(output, "2 prompts") {
		t.Error("expected prompt count in short output")
	}
}

func TestFormatJSON_BasicOutput(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period: "7d",
		Authors: map[string]*AuthorDigest{
			"Myk": {
				Name:         "Myk",
				SessionCount: 2,
				Branches:     map[string]bool{"main": true, "feat/auth": true},
				FilesTouched: map[string]bool{"file.go": true, "auth.go": true},
				Prompts: []Prompt{
					{Content: "fix the login bug", Score: 3},
					{Content: "nice!", Score: 8},
				},
				Conversations: []Conversation{
					{
						Author:    "Myk",
						Branch:    "main",
						Timestamp: time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
						Score:     8,
						Exchanges: []Exchange{
							{Role: "user", Content: "nice!"},
							{Role: "assistant", Content: "Glad it worked!"},
						},
					},
					{
						Author:    "Myk",
						Branch:    "feat/auth",
						Timestamp: time.Date(2026, 2, 21, 10, 0, 0, 0, time.UTC),
						Score:     5,
						Exchanges: []Exchange{
							{Role: "user", Content: "why is this broken?!"},
							{Role: "assistant", Content: "The issue is..."},
							{Role: "user", Content: "Really?"},
						},
					},
				},
			},
		},
		Total: Stats{
			Checkpoints:  5,
			Sessions:     2,
			Prompts:      2,
			FilesTouched: 2,
		},
	}

	var buf strings.Builder
	FormatJSON(&buf, d)

	// Parse to verify valid JSON
	var output JSONOutput
	if err := json.Unmarshal([]byte(buf.String()), &output); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, buf.String())
	}

	// Verify structure
	if output.Period != "7d" {
		t.Errorf("period = %q, want %q", output.Period, "7d")
	}
	if output.Stats.Checkpoints != 5 {
		t.Errorf("stats.checkpoints = %d, want 5", output.Stats.Checkpoints)
	}
	if len(output.TopConversations) != 2 {
		t.Fatalf("top_conversations length = %d, want 2", len(output.TopConversations))
	}

	// Verify sorted by score descending
	if output.TopConversations[0].Score != 8 {
		t.Errorf("first conversation score = %d, want 8", output.TopConversations[0].Score)
	}
	if output.TopConversations[1].Score != 5 {
		t.Errorf("second conversation score = %d, want 5", output.TopConversations[1].Score)
	}

	// Verify rank numbering
	if output.TopConversations[0].Rank != 1 {
		t.Errorf("first conversation rank = %d, want 1", output.TopConversations[0].Rank)
	}

	// Verify exchanges
	if len(output.TopConversations[0].Exchanges) != 2 {
		t.Errorf("first convo exchanges = %d, want 2", len(output.TopConversations[0].Exchanges))
	}

	// Verify scoring guide
	if output.ScoringGuide == "" {
		t.Error("scoring_guide should not be empty")
	}

	// Verify authors
	if len(output.Authors) != 1 {
		t.Fatalf("authors length = %d, want 1", len(output.Authors))
	}
	if output.Authors[0].Name != "Myk" {
		t.Errorf("author name = %q, want %q", output.Authors[0].Name, "Myk")
	}
	if len(output.Authors[0].Branches) != 2 {
		t.Errorf("author branches = %d, want 2", len(output.Authors[0].Branches))
	}
}

func TestFormatJSON_TruncatesLongExchanges(t *testing.T) {
	t.Parallel()

	longContent := strings.Repeat("a", 1000)
	d := &TeamDigest{
		Period: "7d",
		Authors: map[string]*AuthorDigest{
			"Test": {
				Name:         "Test",
				Branches:     map[string]bool{},
				FilesTouched: map[string]bool{},
				Conversations: []Conversation{
					{
						Author: "Test",
						Score:  5,
						Exchanges: []Exchange{
							{Role: "assistant", Content: longContent},
						},
					},
				},
			},
		},
	}

	var buf strings.Builder
	FormatJSON(&buf, d)

	var output JSONOutput
	if err := json.Unmarshal([]byte(buf.String()), &output); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(output.TopConversations) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(output.TopConversations))
	}
	content := output.TopConversations[0].Exchanges[0].Content
	if len(content) > maxJSONExchangeLen+10 { // +10 for "..."
		t.Errorf("exchange content too long: %d chars (max %d)", len(content), maxJSONExchangeLen)
	}
	if !strings.HasSuffix(content, "...") {
		t.Error("truncated content should end with ...")
	}
}

func TestFormatJSON_GlobalCap(t *testing.T) {
	t.Parallel()

	// Create more conversations than the global max
	d := &TeamDigest{
		Period:  "all",
		Authors: map[string]*AuthorDigest{},
	}

	author := &AuthorDigest{
		Name:         "Prolific",
		Branches:     map[string]bool{},
		FilesTouched: map[string]bool{},
	}
	for i := range 50 {
		// Each prompt is a single unique word - no word overlap between conversations
		author.Conversations = append(author.Conversations, Conversation{
			Author: "Prolific",
			Score:  i,
			Exchanges: []Exchange{
				{Role: "user", Content: fmt.Sprintf("xyzzy%d", i)},
			},
		})
	}
	d.Authors["Prolific"] = author

	var buf strings.Builder
	FormatJSON(&buf, d)

	var output JSONOutput
	if err := json.Unmarshal([]byte(buf.String()), &output); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(output.TopConversations) != maxGlobalConversations {
		t.Errorf("expected %d conversations (global cap), got %d", maxGlobalConversations, len(output.TopConversations))
	}

	// Verify highest scores are kept
	if output.TopConversations[0].Score != 49 {
		t.Errorf("highest score should be 49, got %d", output.TopConversations[0].Score)
	}
}

func TestFormatJSON_Empty(t *testing.T) {
	t.Parallel()

	d := &TeamDigest{
		Period:  "7d",
		Authors: map[string]*AuthorDigest{},
	}

	var buf strings.Builder
	FormatJSON(&buf, d)

	var output JSONOutput
	if err := json.Unmarshal([]byte(buf.String()), &output); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if len(output.TopConversations) != 0 {
		t.Errorf("expected empty top_conversations for empty digest, got %d", len(output.TopConversations))
	}
}

func TestDeduplicateConversations(t *testing.T) {
	t.Parallel()

	convos := []Conversation{
		{Score: 8, Exchanges: []Exchange{{Role: "user", Content: "Are you stuck?"}}},
		{Score: 8, Exchanges: []Exchange{{Role: "user", Content: "Are you stuck?"}}},
		{Score: 8, Exchanges: []Exchange{{Role: "user", Content: "Are you stuck?"}}},
		{Score: 5, Exchanges: []Exchange{{Role: "user", Content: "Thank you, Captain Obvious"}}},
		{Score: 3, Exchanges: []Exchange{{Role: "user", Content: "ls"}}},
	}

	result := deduplicateConversations(convos)
	if len(result) != 3 {
		t.Errorf("expected 3 unique conversations, got %d", len(result))
	}
	// First should be "Are you stuck?" (highest score, first seen)
	if result[0].Score != 8 {
		t.Errorf("first result score = %d, want 8", result[0].Score)
	}
	// Second should be "Captain Obvious"
	if result[1].Score != 5 {
		t.Errorf("second result score = %d, want 5", result[1].Score)
	}
}

func TestDeduplicateConversations_CaseInsensitive(t *testing.T) {
	t.Parallel()

	convos := []Conversation{
		{Score: 8, Exchanges: []Exchange{{Role: "user", Content: "Are You Stuck?"}}},
		{Score: 7, Exchanges: []Exchange{{Role: "user", Content: "are you stuck?"}}},
		{Score: 5, Exchanges: []Exchange{{Role: "user", Content: "Something different"}}},
	}

	result := deduplicateConversations(convos)
	if len(result) != 2 {
		t.Errorf("expected 2 unique conversations (case-insensitive dedup), got %d", len(result))
	}
}

func TestDiverseTopN_FiltersSimilar(t *testing.T) {
	t.Parallel()

	convos := []JSONConversation{
		{Score: 10, Exchanges: []JSONExchange{{Role: "user", Content: "Are you stuck?"}}},
		{Score: 9, Exchanges: []JSONExchange{{Role: "user", Content: "Are you stuck again?"}}},
		{Score: 8, Exchanges: []JSONExchange{{Role: "user", Content: "Are you still stuck?"}}},
		{Score: 5, Exchanges: []JSONExchange{{Role: "user", Content: "Thank you, Captain Obvious"}}},
		{Score: 3, Exchanges: []JSONExchange{{Role: "user", Content: "ls"}}},
	}

	result := diverseTopN(convos, 5)
	// Should keep the first "stuck" variant but skip the similar ones, picking Captain Obvious and ls instead
	if len(result) < 3 {
		t.Errorf("expected at least 3 diverse conversations, got %d", len(result))
	}
	// First should be highest scoring
	if result[0].Score != 10 {
		t.Errorf("first result score = %d, want 10", result[0].Score)
	}
	// Should NOT have all three "stuck" variants
	stuckCount := 0
	for _, c := range result {
		for _, ex := range c.Exchanges {
			if ex.Role == "user" && strings.Contains(strings.ToLower(ex.Content), "stuck") {
				stuckCount++
			}
		}
	}
	if stuckCount > 1 {
		t.Errorf("expected at most 1 'stuck' conversation, got %d", stuckCount)
	}
}

func TestDiverseTopN_PreservesUniqueConversations(t *testing.T) {
	t.Parallel()

	convos := []JSONConversation{
		{Score: 10, Exchanges: []JSONExchange{{Role: "user", Content: "Are you stuck?"}}},
		{Score: 8, Exchanges: []JSONExchange{{Role: "user", Content: "Thank you, Captain Obvious"}}},
		{Score: 5, Exchanges: []JSONExchange{{Role: "user", Content: "Really?"}}},
		{Score: 3, Exchanges: []JSONExchange{{Role: "user", Content: "ls"}}},
	}

	result := diverseTopN(convos, 4)
	if len(result) != 4 {
		t.Errorf("expected all 4 unique conversations, got %d", len(result))
	}
}

func TestExtractJSONFromMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain json", `{"key": "value"}`, `{"key": "value"}`},
		{"json code block", "```json\n{\"key\": \"value\"}\n```", `{"key": "value"}`},
		{"generic code block", "```\n{\"key\": \"value\"}\n```", `{"key": "value"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractJSONFromMarkdown(tt.input)
			if got != tt.want {
				t.Errorf("extractJSONFromMarkdown(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
