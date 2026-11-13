package strategy

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/entiredir"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/redact"
)

// redactionPolicy is what a repository's redaction removes from a
// transcript, in the terms two repositories' redaction is compared in.
type redactionPolicy struct {
	betterleaks, goredact bool
	// pii is nil when PII redaction is off.
	pii *redact.PIIConfig
	// rules holds the regex of every custom rule: inline custom_redactions
	// and rule packs under .entire/redactors.
	rules map[string]bool
}

// foreignRedactionRefusal returns why this repository's redaction may not
// stand in for that of the session's home at homeWorktree, or "". A foreign
// session's transcript is redacted here, by this repository's rules alone,
// so they must remove at least everything the home's remove. The OpenAI
// Privacy Filter is not compared: it is deprecated and never runs at commit.
func foreignRedactionRefusal(ctx context.Context, homeWorktree string) string {
	home, err := loadRedactionPolicy(settings.WithWorktreeRoot(ctx, homeWorktree), func() (*os.Root, error) {
		return entiredir.OpenAtForRead(homeWorktree)
	})
	if err != nil {
		return "the session's home repository's redaction settings cannot be read"
	}
	here, err := loadRedactionPolicy(ctx, func() (*os.Root, error) { return entiredir.OpenForRead(ctx) })
	if err != nil {
		return "this repository's redaction settings cannot be read"
	}
	if !here.covers(home) {
		return "this repository's redaction does not cover everything the session's home repository redacts"
	}
	return ""
}

// loadRedactionPolicy reads the redaction of the repository settings.Load
// resolves for ctx, with rule packs from the .entire root openEntire opens.
// A pack that is skipped as unreadable or invalid is an error: the rule it
// holds cannot be compared.
func loadRedactionPolicy(ctx context.Context, openEntire func() (*os.Root, error)) (redactionPolicy, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return redactionPolicy{}, err //nolint:wrapcheck // callers report a fixed reason
	}
	p := redactionPolicy{betterleaks: s.BetterleaksEnabled(), goredact: s.GoredactEnabled(), rules: map[string]bool{}}
	if r := s.Redaction; r != nil {
		if r.PII != nil && r.PII.Enabled {
			pii := piiConfigFromSettings(r.PII, nil)
			p.pii = &pii
		}
		for _, re := range r.CustomRedactions {
			p.rules[re] = true
		}
	}
	root, err := openEntire()
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return redactionPolicy{}, err
	}
	packs, skipped, err := redact.LoadPacksCountingSkipped(root.FS(), redact.RedactorsDirName, slog.New(slog.DiscardHandler))
	if err != nil {
		return redactionPolicy{}, err //nolint:wrapcheck // callers report a fixed reason
	}
	if skipped > 0 {
		return redactionPolicy{}, errors.New("redactor pack skipped")
	}
	for _, pack := range packs {
		for _, rule := range pack.Rules {
			p.rules[rule.Regex] = true
		}
	}
	return p, nil
}

// covers reports whether p removes everything o does: o's scanner engines,
// its PII categories and patterns, and its custom rules, compared by regex.
func (p redactionPolicy) covers(o redactionPolicy) bool {
	if (o.betterleaks && !p.betterleaks) || (o.goredact && !p.goredact) {
		return false
	}
	for re := range o.rules {
		if !p.rules[re] {
			return false
		}
	}
	if o.pii == nil {
		return true
	}
	if p.pii == nil {
		return false
	}
	for category, on := range o.pii.Categories {
		if on && !p.pii.Categories[category] {
			return false
		}
	}
	patterns := map[string]bool{}
	for _, re := range p.pii.CustomPatterns {
		patterns[re] = true
	}
	for _, re := range o.pii.CustomPatterns {
		if !patterns[re] {
			return false
		}
	}
	return true
}
