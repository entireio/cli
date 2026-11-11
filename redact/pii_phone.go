package redact

import (
	"net/netip"
	"regexp"
	"strings"
	"unicode/utf8"
)

// International phone detection for the "phone" PII category.
//
// phoneRegex covers North American numbers. intlPhoneRegex covers the rest,
// but only in international format: a + and a country code other than 1,
// then the national number with the usual separators (+44 20 7946 0958,
// +54 9 11 0000-0001, +33.1.99.00.12.34, +44 (0)20 7946 0958,
// (+39) 347 000 0001, +49 30/23125678). Tabs and no-break spaces work as
// spaces, and groups may be separated by several (+44  20 7946 0958). A national number without the + (020 7946 0958) is left alone: in
// code it has the shape of counts, IDs and dates.
//
// A + followed by digits also opens lines of diff output, signed numbers,
// timezone offsets and semver build metadata, so the regex only finds the
// shape and redactableIntlPhone decides.
var intlPhoneRegex = regexp.MustCompile(`\+[2-9]\d*\)?(?:(?:[-./]|[ \t\x{a0}]+)?\(\d{1,4}\))?(?:(?:[-./]|[ \t\x{a0}]+)?\d+)*`)

// Digit counts for an international number, country code included. E.164
// allows at most 15; a country code and the 7 digits of the shortest common
// national numbers make 9.
const (
	intlPhoneMinDigits = 9
	intlPhoneMaxDigits = 15
)

// redactableIntlPhone reports whether the candidate s[start:end] is a phone
// number in international format, and its bounds: a "(+39)" country code
// starts one byte earlier, and trailing space-separated groups that take it
// past 15 digits are left out.
func redactableIntlPhone(s string, start, end int) (int, int, bool) {
	// The byte before the + decides, before a "(" around the country code
	// joins the match: the "(" is the boundary (Tel.(+44) 20 7946 0958).
	if !intlPhoneStandsAlone(s, start) {
		return 0, 0, false
	}
	ccEnd := start + 1
	for ccEnd < end && isDigit(s[ccEnd]) {
		ccEnd++
	}
	cc := ccEnd - start - 1
	switch {
	case ccEnd == end:
	case s[ccEnd] == ')' && cc <= 3 && start > 0 && s[start-1] == '(':
		// "(+39) 347 000 0001": the parentheses hold the country code.
		start--
	case s[ccEnd] == ')', s[ccEnd] == '/' && cc > 3:
		// "(+442079460958)" and "+442079460958/123": the number is written
		// without separators, and the text around it goes on.
		end = ccEnd
	}
	end, ok := endIntlPhone(s, ccEnd, end)
	if !ok {
		return 0, 0, false
	}
	end, ok = trimIntlPhone(s, start, ccEnd, end)
	if !ok {
		return 0, 0, false
	}
	if ccEnd == end {
		// Written without separators (+442079460958).
		return start, end, true
	}
	// The first group is the country code. Only a space may follow a longer
	// one (+447700 900123, a national number with its 0 swapped for +44), so
	// a date that starts with the year (+2026-11-06 19:24) never passes.
	if cc > 3 && phoneSpaceAt(s, ccEnd) == 0 {
		return 0, 0, false
	}
	return start, end, groupsReadAsPhone(s, start, ccEnd, end)
}

// trimIntlPhone returns the end of the number in s[start:end] once the groups
// after a space that take it past 15 digits are dropped, since a space also
// joins a number to the text after it (+44 20 7946 0958 2026). A dash or dot
// does not: +281475245147134.9375 is a decimal, not a number and its tail. It
// reports false when fewer than 9 digits remain or no space allows the cut.
func trimIntlPhone(s string, start, ccEnd, end int) (int, bool) {
	digits := countDigits(s[start:end])
	for digits > intlPhoneMaxDigits {
		i := lastPhoneSpace(s[ccEnd:end])
		if i < 0 {
			return 0, false
		}
		digits -= countDigits(s[ccEnd+i : end])
		end = ccEnd + i
	}
	return end, digits >= intlPhoneMinDigits
}

// separatorsReadAsPhone reports whether the slashes and dots in s[start:end],
// if any, separate a phone number. One slash separates an area code
// (+49 30/23125678); two make a date (+31/12/2024 23:59). With dots, apart
// from spaces after the country code and after the parenthesized group, dots
// must be the only separator, at least two of them, and the digits must not
// form an IPv4 address: +33.1.99.00.12.34, +33 1.99.00.12.34,
// +33 (0)1.99.00.12.34, +33 (0) 1.99.00.12.34 and (+33) 1.99.00.12.34 are
// numbers, while a decimal (+48.8566140), +233.252.18.110 and
// +31.12.2024 23 are not.
func separatorsReadAsPhone(s string, start, ccEnd, end int) bool {
	s = s[:end]
	i := ccEnd
	if i < end && s[i] == ')' {
		i++
	}
	if i < end {
		i += phoneSpaceAt(s, i)
	}
	dots, slashes, other := 0, 0, false
	for ; i < end; i++ {
		switch c := s[i]; {
		case isDigit(c), c == '(':
		case c == ')':
			i += phoneSpaceAt(s, i+1)
		case c == '.':
			dots++
		case c == '/':
			slashes++
			other = true
		default:
			other = true
		}
	}
	if slashes > 1 {
		return false
	}
	if dots == 0 {
		return true
	}
	if other || dots < 2 {
		return false
	}
	_, err := netip.ParseAddr(strings.TrimPrefix(s[start:end], "+"))
	return err != nil
}

// groupsReadAsPhone reports whether the slashes and dots in s[start:end], if
// any, separate a phone number. A space also joins a number to the text after
// it, so when only groups after a space break the rules, the number before
// them decides (+33 1.99.00.12.34 2026, +44 20 7946 0958 1/2/3). The groups
// stay in the match, as they do in a number without dots
// (+44 20 7946 0958 202): one may belong to the number (+49 30123456 78.90),
// and trimIntlPhone has capped the digits.
func groupsReadAsPhone(s string, start, ccEnd, end int) bool {
	for cut := end; !separatorsReadAsPhone(s, start, ccEnd, cut); {
		i := lastPhoneSpace(s[ccEnd:cut])
		if i < 0 {
			return false
		}
		cut = ccEnd + i
		if countDigits(s[start:cut]) < intlPhoneMinDigits {
			return false
		}
	}
	return true
}

func countDigits(s string) int {
	n := 0
	for i := range len(s) {
		if isDigit(s[i]) {
			n++
		}
	}
	return n
}

// intlPhoneStandsAlone reports whether the number starting at s[start] does
// not continue a longer token: semver build metadata (1.2.3+20241029),
// base64 (ab/+2345678901), or an identifier. A letter after a backslash
// belongs to an escape left in the text (\n+44 20 7946 0958), not to a word,
// and a slash only ends a URI scheme (tel://+442079460958).
func intlPhoneStandsAlone(s string, start int) bool {
	if start == 0 {
		return true
	}
	switch prev := s[start-1]; {
	case prev == '/':
		return strings.HasSuffix(s[:start], "://")
	case isIPWordByte(prev):
		return followsEscape(s, start)
	}
	return true
}

// endIntlPhone returns where the number ending the candidate at end stops,
// and false when it runs on into a word. An extension attached with x or ext
// stays visible, as with North American numbers (+44 20 7946 0958x123,
// +33 1 99 00 12 34ext123). A last
// group that starts a word after a space belongs to the text after the
// number (+44 20 7946 0958 9am), so the number ends before that space.
func endIntlPhone(s string, ccEnd, end int) (int, bool) {
	if end == len(s) || !isIPWordByte(s[end]) || isPhoneExtension(s, end) {
		return end, true
	}
	i := lastPhoneSpace(s[ccEnd:end])
	if i < 0 {
		return 0, false
	}
	return ccEnd + i, true
}

func isPhoneSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\u00a0'
}

// phoneSpaceAt returns the length of the spaces, tabs and no-break spaces
// starting at s[i], or 0 when there are none.
func phoneSpaceAt(s string, i int) int {
	n := strings.IndexFunc(s[i:], func(r rune) bool { return !isPhoneSpace(r) })
	if n < 0 {
		return len(s) - i
	}
	return n
}

// lastPhoneSpace returns the index where the last run of spaces, tabs and
// no-break spaces in s starts, or -1 when there is none, so that a number
// cut there keeps none of them.
func lastPhoneSpace(s string) int {
	i := strings.LastIndexFunc(s, isPhoneSpace)
	for i > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:i])
		if !isPhoneSpace(r) {
			break
		}
		i -= n
	}
	return i
}

// isPhoneExtension reports whether s[i:] starts with an extension that ends
// the word: x123, ext123 or ext.123, with or without a space before the
// digits (ext 123).
func isPhoneExtension(s string, i int) bool {
	switch {
	case s[i]|0x20 == 'x':
		i++
	case len(s)-i >= 3 && strings.EqualFold(s[i:i+3], "ext"):
		i += 3
		if i < len(s) && s[i] == '.' {
			i++
		}
	default:
		return false
	}
	if i < len(s) && s[i] == ' ' {
		i++
	}
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	return j > i && (j == len(s) || !isIPWordByte(s[j]))
}
