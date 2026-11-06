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
// starts one byte earlier.
func redactableIntlPhone(s string, start, end int) (int, int, bool) {
	ccEnd := start + 1
	for ccEnd < end && isDigit(s[ccEnd]) {
		ccEnd++
	}
	cc := ccEnd - start - 1
	if ccEnd < end && s[ccEnd] == ')' {
		// "(+39) 347 000 0001": the parentheses hold the country code only.
		if start == 0 || s[start-1] != '(' || cc > 3 {
			return 0, 0, false
		}
		start--
	}
	if !intlPhoneStandsAlone(s, start, end) {
		return 0, 0, false
	}
	digits, dots, otherSeparator := 0, 0, false
	for i := start; i < end; i++ {
		switch c := s[i]; {
		case isDigit(c):
			digits++
		case c == '.':
			dots++
		case c != '+':
			otherSeparator = true
		}
	}
	if digits < intlPhoneMinDigits || digits > intlPhoneMaxDigits {
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
	// Dots must be the only separator, at least two of them, and the digits
	// must not form an IPv4 address: +33.1.99.00.12.34 is a number, while a
	// decimal (+48.8566140), +233.252.18.110 and +31.12.2024 23 are not.
	if dots > 0 {
		if otherSeparator || dots < 2 {
			return 0, 0, false
		}
		if _, err := netip.ParseAddr(strings.TrimPrefix(s[start:end], "+")); err == nil {
			return 0, 0, false
		}
	}
	return start, end, true
}

// intlPhoneStandsAlone reports whether s[start:end] is not part of a longer
// token: semver build metadata (1.2.3+20241029), base64 (ab/+2345678901),
// or an identifier on either side. A letter after a backslash belongs to an
// escape left in the text (\n+44 20 7946 0958), not to a word.
func intlPhoneStandsAlone(s string, start, end int) bool {
	if start > 0 {
		prev := s[start-1]
		if (isIPWordByte(prev) && !followsEscape(s, start)) || prev == '.' || prev == '/' {
			return false
		}
	}
	return end == len(s) || !isIPWordByte(s[end])
}
