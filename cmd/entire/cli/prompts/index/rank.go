package index

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kljensen/snowball"
	"golang.org/x/text/unicode/norm"
)

var wordBoundaryRegex = regexp.MustCompile(`[^\pL\pN]+`)
var specialCharRegex = regexp.MustCompile(`[${}\[\]().*+?^|\\]`)

var stopWords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "but": true, "by": true, "for": true, "if": true, "in": true,
	"into": true, "is": true, "it": true, "no": true, "not": true, "of": true,
	"on": true, "or": true, "such": true, "that": true, "the": true,
	"their": true, "then": true, "there": true, "these": true, "they": true,
	"this": true, "to": true, "was": true, "were": true, "what": true,
	"when": true, "where": true, "which": true, "who": true, "will": true, "with": true,
}

func Tokenize(text string) []string {
	normalized := norm.NFC.String(strings.ToLower(text))
	tokens := wordBoundaryRegex.Split(normalized, -1)
	stemmed := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if len(t) < 2 {
			continue
		}
		if stopWords[t] {
			continue
		}
		result, err := snowball.Stem(t, "english", true)
		if err != nil {
			stemmed = append(stemmed, t)
			continue
		}
		stemmed = append(stemmed, result)
	}
	return stemmed
}

type SearchQuery struct {
	Phrase  string
	Tokens  []string
	RawText string
}

func ParseQuery(raw string) SearchQuery {
	cleaned := specialCharRegex.ReplaceAllString(raw, " ")
	cleaned = strings.TrimSpace(cleaned)

	if len(cleaned) < 2 {
		return SearchQuery{}
	}

	var phrase string
	var phraseTokens []string

	for i, r := range raw {
		if r == '"' {
			end := strings.Index(raw[i+1:], "\"")
			if end >= 0 {
				phrase = raw[i+1 : i+1+end]
				phraseTokens = Tokenize(phrase)
				raw = raw[:i] + raw[i+1+end+1:]
				break
			}
		}
	}

	tokens := Tokenize(raw)
	if len(phraseTokens) > 0 {
		tokens = append(phraseTokens, tokens...)
	}

	return SearchQuery{
		Phrase:  phrase,
		Tokens:  tokens,
		RawText: raw,
	}
}

type ScoredEntry struct {
	Entry          Entry
	Score          float64
	TruncatedMatch bool
}

func ScoreEntry(entry Entry, query SearchQuery) ScoredEntry {
	if len(query.Tokens) == 0 {
		return ScoredEntry{Entry: entry, Score: 0}
	}

	promptTokens := Tokenize(entry.PromptText)
	promptTokenSet := make(map[string]bool)
	for _, t := range promptTokens {
		promptTokenSet[t] = true
	}

	score := 0.0

	if query.Phrase != "" && len(query.Tokens) > 0 {
		lowerPrompt := strings.ToLower(entry.PromptText)
		lowerPhrase := strings.ToLower(query.Phrase)
		if strings.Contains(lowerPrompt, lowerPhrase) {
			score += 10
		}
	}

	allFound := true
	for _, qt := range query.Tokens {
		if !promptTokenSet[qt] {
			allFound = false
			break
		}
	}
	if allFound && len(query.Tokens) > 0 {
		score += 5
	}

	anyFound := false
	matchCount := 0
	for _, qt := range query.Tokens {
		if promptTokenSet[qt] {
			anyFound = true
			matchCount++
		}
	}
	if anyFound {
		score++
	}

	if len(promptTokens) > 0 {
		termDensity := float64(matchCount) / float64(len(promptTokens))
		score += termDensity * 2
	}

	truncated := entry.PromptTruncated && anyFound

	return ScoredEntry{
		Entry:          entry,
		Score:          score,
		TruncatedMatch: truncated,
	}
}

func Search(entries []Entry, cfg SearchConfig) []ScoredEntry {
	query := ParseQuery(cfg.Query)

	scored := make([]ScoredEntry, 0, len(entries))
	for _, entry := range entries {
		if !matchesFilter(entry, cfg) {
			continue
		}
		result := ScoreEntry(entry, query)
		if result.Score > 0 {
			scored = append(scored, result)
		}
	}

	sortByScoreAndTime(scored)

	if cfg.Limit > 0 && len(scored) > cfg.Limit {
		scored = scored[:cfg.Limit]
	}
	return scored
}

func matchesFilter(entry Entry, cfg SearchConfig) bool {
	if cfg.Agent != "" && !strings.EqualFold(entry.Agent, cfg.Agent) {
		return false
	}
	if cfg.Branch != "" && !strings.EqualFold(entry.Branch, cfg.Branch) {
		return false
	}
	if cfg.Kind != "" && !strings.EqualFold(entry.Kind, cfg.Kind) {
		return false
	}
	if cfg.After != "" {
		if t, err := time.Parse("2006-01-02", cfg.After); err == nil {
			if entry.CreatedAt.Before(t) {
				return false
			}
		}
	}
	if cfg.Files != "" {
		found := false
		fileFilter := strings.ToLower(cfg.Files)
		for _, f := range entry.FilesTouched {
			if strings.Contains(strings.ToLower(f), fileFilter) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sortByScoreAndTime(entries []ScoredEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		return entries[i].Entry.CreatedAt.After(entries[j].Entry.CreatedAt)
	})
}
