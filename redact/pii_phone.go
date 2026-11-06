package redact

import (
	"net/netip"
	"regexp"
	"strings"
)

// International phone detection for the "phone" PII category.
//
// phoneRegex covers North American numbers. intlPhoneRegex covers the rest,
// but only in international format: a + and a country code other than 1,
// then the national number with the usual separators (+44 20 7946 0958,
// +54 9 11 0000-0001, +33.1.99.00.12.34, +44 (0)20 7946 0958,
// (+39) 347 000 0001). A national number without the + (020 7946 0958) is
// left alone: in code it has the shape of counts, IDs and dates.
//
// A + followed by digits also opens lines of diff output, signed numbers,
// timezone offsets and semver build metadata, so the regex only finds the
// shape and redactableIntlPhone decides.
var intlPhoneRegex = regexp.MustCompile(`\+[2-9]\d*\)?(?:[-. ]?\(\d{1,4}\))?(?:[-. ]?\d+)*`)

// Digit counts for an international number, country code included. E.164
// allows at most 15; a country code and the 7 digits of the shortest common
// national numbers make 9.
const (
	intlPhoneMinDigits = 9
	intlPhoneMaxDigits = 15
)

// redactableIntlPhone reports whether the candidate s[start:end] is a phone
// number in international format, and its bounds: a "(+39)" country code
// starts one byte earlier, and digits past the 15th are left out.
func redactableIntlPhone(s string, start, end int) (int, int, bool) {
	ccEnd := start + 1
	for ccEnd < end && isDigit(s[ccEnd]) {
		ccEnd++
	}
	cc := ccEnd - start - 1
	if ccEnd < end && s[ccEnd] == ')' {
		if start > 0 && s[start-1] == '(' && cc <= 3 {
			// "(+39) 347 000 0001": the parentheses hold the country code.
			start--
		} else {
			// "(+442079460958)": the parenthesis closes the text around it.
			end = ccEnd
		}
	}
	if !intlPhoneStandsAlone(s, start) {
		return 0, 0, false
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
	if cc > 3 && s[ccEnd] != ' ' {
		return 0, 0, false
	}
	return start, end, dottedReadsAsPhone(s, start, ccEnd, end)
}

// trimIntlPhone returns the end of the number in s[start:end] once the groups
// after a space that take it past 15 digits are dropped, since a space also
// joins a number to the text after it (+44 20 7946 0958 2026). A dash or dot
// does not: +281475245147134.9375 is a decimal, not a number and its tail. It
// reports false when fewer than 9 digits remain or no space allows the cut.
func trimIntlPhone(s string, start, ccEnd, end int) (int, bool) {
	digits := countDigits(s[start:end])
	for digits > intlPhoneMaxDigits {
		i := strings.LastIndexByte(s[ccEnd:end], ' ')
		if i < 0 {
			return 0, false
		}
		digits -= countDigits(s[ccEnd+i : end])
		end = ccEnd + i
	}
	return end, digits >= intlPhoneMinDigits
}

// dotsReadAsPhone reports whether the dots in s[start:end], if any, separate
// a phone number. Apart from a space after the country code, dots must be the
// only separator, at least two of them, and the digits must not form an IPv4
// address: +33.1.99.00.12.34 and +33 1.99.00.12.34 are numbers, while a
// decimal (+48.8566140), +233.252.18.110 and +31.12.2024 23 are not.
func dotsReadAsPhone(s string, start, ccEnd, end int) bool {
	dots, other := 0, false
	for i := ccEnd; i < end; i++ {
		switch c := s[i]; {
		case isDigit(c), i == ccEnd && c == ' ':
		case c == '.':
			dots++
		default:
			other = true
		}
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

// dottedReadsAsPhone reports whether the dots in s[start:end], if any,
// separate a phone number. A space also joins a number to the text after it,
// so when only groups after a space break the rules, the number before them
// decides (+33 1.99.00.12.34 2026). The groups stay in the match, as they do
// in a number without dots (+44 20 7946 0958 202): one may belong to the
// number (+49 30123456 78.90), and trimIntlPhone has capped the digits.
func dottedReadsAsPhone(s string, start, ccEnd, end int) bool {
	for cut := end; !dotsReadAsPhone(s, start, ccEnd, cut); {
		i := strings.LastIndexByte(s[ccEnd+1:cut], ' ')
		if i < 0 {
			return false
		}
		cut = ccEnd + 1 + i
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
// belongs to an escape left in the text (\n+44 20 7946 0958), not to a word.
func intlPhoneStandsAlone(s string, start int) bool {
	if start == 0 {
		return true
	}
	prev := s[start-1]
	return (!isIPWordByte(prev) || followsEscape(s, start)) && prev != '.' && prev != '/'
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
	i := strings.LastIndexByte(s[ccEnd:end], ' ')
	if i < 0 {
		return 0, false
	}
	return ccEnd + i, true
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
