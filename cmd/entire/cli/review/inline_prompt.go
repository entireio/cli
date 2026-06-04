// Package review — see env.go for package-level rationale.
//
// inline_prompt.go provides the ReadSingleKey helper used by the
// post-review fix prompt ([Y]es / [s]elect / [n]o / [A]lways). It is a
// minimal single-rune reader so the inline prompt does not require a
// full huh.Form for a one-character answer.
package review

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"unicode"
)

// KeyChoice describes the menu offered by ReadSingleKey.
//
// Default is the rune returned on EOF or on plain Enter. (ReadSingleKey
// does not detect TTY-ness itself; callers gate on CanPromptInteractively
// before reading, so a non-TTY simply hits EOF and falls back to Default.)
// Allowed is a documentation+matching string: each rune in Allowed is one
// of the legal answers. Matching is case-insensitive, but the returned
// rune is in the case it appears in Allowed (so the caller can decide
// whether a key is the "default-shown" form).
type KeyChoice struct {
	Default rune
	Allowed string
}

// ReadSingleKey reads runes from r until one matches an entry in
// choice.Allowed (case-insensitive). Behavior:
//
//   - EOF, newline, or carriage-return returns choice.Default.
//   - Whitespace (other than newline / carriage-return) is skipped.
//   - Unknown runes are swallowed; the loop continues.
//   - Returns choice.Default if r is nil.
//
// The returned rune is in the case present in choice.Allowed, not the
// case the user typed.
func ReadSingleKey(r io.Reader, choice KeyChoice) (rune, error) {
	if r == nil {
		return choice.Default, nil
	}
	br := bufio.NewReader(r)
	for {
		b, _, err := br.ReadRune()
		if errors.Is(err, io.EOF) {
			return choice.Default, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read key: %w", err)
		}
		if b == '\n' || b == '\r' {
			return choice.Default, nil
		}
		if unicode.IsSpace(b) {
			continue
		}
		for _, a := range choice.Allowed {
			if unicode.ToLower(a) == unicode.ToLower(b) {
				return a, nil
			}
		}
		// Unknown — try next rune.
	}
}
