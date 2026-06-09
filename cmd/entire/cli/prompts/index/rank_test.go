package index

import (
	"testing"
	"time"
)

func TestTokenize_stemming(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected []string
	}{
		{"caching", []string{"cach"}},
		{"authentication", []string{"authent"}},
		{"running", []string{"run"}},
		{"implemented", []string{"implement"}},
	}

	for _, tt := range tests {
		result := Tokenize(tt.input)
		if len(result) != len(tt.expected) {
			t.Errorf("Tokenize(%q) = %v, want %v", tt.input, result, tt.expected)
			continue
		}
		for i := range result {
			if result[i] != tt.expected[i] {
				t.Errorf("Tokenize(%q)[%d] = %v, want %v", tt.input, i, result[i], tt.expected[i])
			}
		}
	}
}

func TestTokenize_stopwords(t *testing.T) {
	t.Parallel()

	result := Tokenize("the quick brown fox")
	expected := []string{"quick", "brown", "fox"}

	if len(result) != len(expected) {
		t.Fatalf("Tokenize() = %v, want %v", result, expected)
	}
	for i := range result {
		if result[i] != expected[i] {
			t.Errorf("Tokenize()[%d] = %v, want %v", i, result[i], expected[i])
		}
	}
}

func TestTokenize_unicode(t *testing.T) {
	t.Parallel()

	result := Tokenize("café")
	if len(result) == 0 {
		t.Error("Tokenize(café) should not be empty")
	}
}

func TestTokenize_specialChars(t *testing.T) {
	t.Parallel()

	result := Tokenize("$redis*")
	if len(result) == 0 {
		t.Error("Tokenize($redis*) should not be empty")
	}
}

func TestParseQuery_basic(t *testing.T) {
	t.Parallel()

	q := ParseQuery("cache decision")
	if len(q.Tokens) != 2 {
		t.Errorf("ParseQuery() tokens = %d, want 2", len(q.Tokens))
	}
}

func TestParseQuery_phrase(t *testing.T) {
	t.Parallel()

	q := ParseQuery(`"cache decision"`)
	if q.Phrase != "cache decision" {
		t.Errorf("ParseQuery().Phrase = %q, want 'cache decision'", q.Phrase)
	}
}

func TestParseQuery_specialChars(t *testing.T) {
	t.Parallel()

	q := ParseQuery("fix $auth")
	if len(q.Tokens) == 0 {
		t.Error("ParseQuery should handle special chars without panic")
	}
}

func TestParseQuery_tooShort(t *testing.T) {
	t.Parallel()

	q := ParseQuery("a")
	if len(q.Tokens) != 0 {
		t.Errorf("ParseQuery('a') tokens = %d, want 0", len(q.Tokens))
	}
}

func TestScore_exactPhrase(t *testing.T) {
	t.Parallel()

	entry := Entry{
		PromptText: "I need to add caching to improve performance",
	}

	query := ParseQuery(`"add caching"`) // Use quotes for exact phrase

	result := ScoreEntry(entry, query)
	if result.Score == 0 {
		t.Errorf("ScoreEntry() = %v, want > 0", result.Score)
	}
	if result.Score < 10 {
		t.Errorf("ScoreEntry() = %v, want >= 10 for phrase match", result.Score)
	}
}

func TestScore_allTokens(t *testing.T) {
	t.Parallel()

	entry := Entry{
		PromptText: "I need to add caching to improve performance",
	}

	query := ParseQuery("caching performance")

	result := ScoreEntry(entry, query)
	if result.Score < 5 {
		t.Errorf("ScoreEntry() = %v, want >= 5 for all tokens", result.Score)
	}
}

func TestScore_termDensity(t *testing.T) {
	t.Parallel()

	entry := Entry{
		PromptText: "cache cache cache", // 3 tokens, 3 matches
	}

	query := ParseQuery("cache")

	result := ScoreEntry(entry, query)
	// Should have: exact phrase (0) + all tokens (5) + any token (1) + density (3/3 * 2 = 2)
	if result.Score < 5 {
		t.Errorf("ScoreEntry() = %v, want >= 5", result.Score)
	}
}

func TestSearch_returnsRanked(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{PromptText: "add caching for performance", CreatedAt: time.Now()},
		{PromptText: "fix auth bug", CreatedAt: time.Now().Add(-time.Hour)},
		{PromptText: "update docs", CreatedAt: time.Now().Add(-2 * time.Hour)},
	}

	cfg := SearchConfig{Query: "cache", Limit: 10}
	results := Search(entries, cfg)

	if len(results) != 1 {
		t.Errorf("Search() returned %d results, want 1", len(results))
	}
	if results[0].Entry.PromptText != "add caching for performance" {
		t.Errorf("Search() returned wrong entry")
	}
}

func TestSearch_emptyQuery(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{PromptText: "test", CreatedAt: time.Now()},
	}

	cfg := SearchConfig{Query: "", Limit: 10}
	results := Search(entries, cfg)

	if len(results) != 0 {
		t.Errorf("Search() with empty query returned %d results, want 0", len(results))
	}
}

func TestSearch_filters(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Agent: "claude-code", Branch: "main", PromptText: "add caching", CreatedAt: time.Now()},
		{Agent: "gemini", Branch: "main", PromptText: "fix bug", CreatedAt: time.Now()},
		{Agent: "claude-code", Branch: "feature", PromptText: "update docs", CreatedAt: time.Now()},
	}

	cfg := SearchConfig{Query: "cach", Agent: "claude-code"}
	results := Search(entries, cfg)

	if len(results) != 1 {
		t.Errorf("Search() with agent filter returned %d results, want 1", len(results))
	}
	if results[0].Entry.Agent != "claude-code" {
		t.Errorf("Search() returned wrong agent")
	}
}

func BenchmarkTokenize(b *testing.B) {
	text := "the quick brown fox jumps over the lazy dog authentication caching implemented"
	for range b.N {
		Tokenize(text)
	}
}

func BenchmarkSearch1K(b *testing.B) {
	entries := make([]Entry, 1000)
	for i := range entries {
		entries[i] = Entry{
			PromptText: "test prompt with some words here for testing",
			CreatedAt:  time.Now().Add(-time.Duration(i) * time.Hour),
		}
	}

	b.ResetTimer()
	for range b.N {
		Search(entries, SearchConfig{Query: "test", Limit: 20})
	}
}
