package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// European street lines do not have the US shape addressRegex matches. The
// street type comes first (Via Roma 12, Calle Mayor 5), the number comes
// first and the type follows it (12 rue de la Paix), or the type ends the
// street's own word (Hauptstraße 12). Each family below is anchored on a
// closed list of street-type words and needs a house number. Like
// addressRegex, a match is the street line only: postal code and city stay.
//
// Name words must be title case (Roma, Mayor, Paix), so an acronym after the
// street type (Via SSH 22) is not a street name. Lowercase particles (di, de
// la, du, des) may sit between name words, and a name may start with a date
// (Via XX Settembre).
const (
	// euLetter is a letter or a combining mark, so a name written in
	// decomposed form (e followed by U+0301 for é) is still one word.
	euLetter = `[\p{L}\p{M}]`
	// euNameWord is one title-case word, optionally hyphenated
	// (Saint-Germain, Champs-Élysées) or elided onto an article (l'Église,
	// dell'Indipendenza, Sant'Ambrogio).
	euNameWord = `(?:(?:[dDlL]|[Dd]ell|[Aa]ll|[Ss]ant)['’])?\p{Lu}\p{M}*\p{Ll}` + euLetter + `*(?:-` + euLetter + `+)*`
	// euRomanNumeral may follow a name word (Vittorio Emanuele II). It cannot
	// start a name on its own, so an acronym such as CLI after Via does not
	// count. It must be a whole word, so the start of a word in capitals
	// after a French name (12 rue de la Paix VERSAILLES) is not read as a
	// numeral that endsStreetLine would then reject with the whole match.
	euRomanNumeral = `[IVX]{1,5}\b`
	// euMonth is a month in Italian, Spanish or French, in either case. An
	// accented French month also matches in decomposed form.
	euMonth = `(?i:gennaio|febbraio|marzo|aprile|maggio|giugno|luglio|agosto|settembre|ottobre|novembre|dicembre` +
		`|enero|febrero|abril|mayo|junio|julio|septiembre|setiembre|octubre|noviembre|diciembre` +
		`|janvier|f(?:é|e\x{301})vrier|mars|avril|mai|juin|juillet|ao(?:û|u\x{302})t|septembre|octobre|d(?:é|e\x{301})cembre)`
	// euDateName starts a street named after a date: Via XX Settembre, rue du
	// 8 Mai 1945, Avenida 9 de Julio. The day, as digits or a roman numeral,
	// counts only before a month, so a number or acronym alone still does not
	// start a name. The year must be a whole number, so it never takes the
	// first digits of a postal code (rue du 8 Mai 75000 Paris).
	euDateName = `(?:\d{1,2}|` + euRomanNumeral + `)` + euSpace + `(?:de` + euSpace + `)?` + euMonth + `(?:` + euSpace + `\d{4}\b)?`
	// euSpace separates the parts of a street line: spaces or tabs, never a
	// line break.
	euSpace = `[ \t]+`
	// euHouseNumber is the number that ends an Italian or Spanish street
	// line, optionally after n., n.º, nº or n°, with an optional letter or
	// sub-number (3/A, 12b, 7/2, 12/14A, 12/bis). A sub-number is digits with
	// an optional letter, one letter, bis or ter, so it never takes the next
	// street's type (Via Roma 1 / Via Milano 2). It ends at a word boundary in
	// the pattern
	// itself, not only in endsStreetLine, so when a date's year could take a
	// four-digit house number (Via XX Settembre 1234, 20121 Milano), the
	// regex reads 1234 as the house number instead of returning a match that
	// ends inside the postal code.
	euHouseNumber = `(?:n(?:\.[°º]?|[°º])[ \t]?)?\d{1,4}(?:[ \t]?/[ \t]?(?:bis|ter|[0-9]{1,3}[A-Za-z]?|[A-Za-z])|[A-Za-z])?\b`
	// euGermanAdjective may come before a German street's name: Alte
	// Schönhauser Straße, Große Hamburger Straße, Alter Postweg.
	euGermanAdjective = `(?:Alt|Neu|Gro(?:ß|ss)|Klein|Ober|Unter|Mittler|Vorder|Hinter|Hoh|Lang|Breit)e[nrs]?`
	// euLeftEdge is what may come right before a street line: the start of
	// the string, a character that is not part of a word, or a JSON
	// whitespace escape (\nVia Roma 12). Go's \b only knows ASCII letters, so
	// each pattern matches its left edge itself and keeps the street line in
	// group 1. That keeps a street type joined onto a word (xVia, item12 rue)
	// from starting a match, without hiding a street line later in the same
	// run of words (xVia Foo Via Roma 12).
	euLeftEdge = `(?:^|[^\p{L}\p{M}\p{N}_]|\\[nrt])`
	// euSlashTypeLeftEdge is euLeftEdge without a slash, for C/: its own
	// slash would otherwise make a path segment a street line
	// (docs/C/Readme 2).
	euSlashTypeLeftEdge = `(?:^|[^\p{L}\p{M}\p{N}_/]|\\[nrt])`
)

// The street types of each family. Each pattern's alternation and its
// prefilter keywords (europeanAddressKeywords) are both built from these
// lists, so a type cannot reach one without the other.
var (
	italianStreetTypes = []string{"Via", "Viale", "Piazza", "Piazzale", "Corso", "Largo", "Vicolo", "Strada"}
	// spanishStreetTypes leaves out C/, which has a pattern of its own.
	spanishStreetTypes = []string{"Calle", "Avenida", "Avda.", "Plaza", "Paseo", "Camino", "Ronda", "Rambla"}
	// frenchStreetTypes are written in lowercase and also match capitalized
	// (12 Rue de la Paix).
	frenchStreetTypes = []string{"rue", "avenue", "boulevard", "place", "chemin", "allée", "impasse", "quai"}
	// germanStreetTypes end a longer word in lowercase (Hauptstraße), and
	// stand capitalized after a hyphen or on their own (Karl-Marx-Straße,
	// Berliner Straße).
	germanStreetTypes = []string{"straße", "strasse", "str.", "weg", "platz", "allee", "gasse"}
)

// euAlternation matches any one of words. An é in a word also matches its
// decomposed form (e followed by U+0301).
func euAlternation(words []string) string {
	alts := make([]string, len(words))
	for i, w := range words {
		alts[i] = strings.ReplaceAll(regexp.QuoteMeta(w), "é", `(?:é|e\x{301})`)
	}
	return `(?:` + strings.Join(alts, "|") + `)`
}

// capitalized lets one lowercase list serve the French and German types
// that are also written capitalized (Rue de la Paix, Karl-Marx-Straße), so
// the two spellings cannot drift apart.
func capitalized(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		r, n := utf8.DecodeRuneInString(w)
		out[i] = string(unicode.ToUpper(r)) + w[n:]
	}
	return out
}

// euStreetName is a street name of title-case words, with the given
// lowercase particles allowed before and between them; the first word may be
// a date. Words are separated by spaces or tabs only, so a name never runs on
// into the next line (a French name has no house number after it to stop it).
func euStreetName(particles string) string {
	p := `(?:(?:` + particles + `)` + euSpace + `)*`
	return p + `(?:` + euNameWord + `|` + euDateName + `)` +
		`(?:` + euSpace + `(?:` + p + euNameWord + `|` + euRomanNumeral + `))*`
}

// euStreetLine wraps a family's street-line pattern in euLeftEdge, with the
// street line as group 1.
func euStreetLine(pattern string) *regexp.Regexp {
	return euStreetLineAfter(euLeftEdge, pattern)
}

// euStreetLineAfter is euStreetLine with another left edge.
func euStreetLineAfter(leftEdge, pattern string) *regexp.Regexp {
	return regexp.MustCompile(leftEdge + `(` + pattern + `)`)
}

var (
	// italianAddressRegex: Via Roma 12, Piazza del Duomo 1, Via dei Mille 3/A.
	italianAddressRegex = euStreetLine(
		euAlternation(italianStreetTypes) + euSpace +
			euStreetName(`di|da|del|dello|della|delle|dei|degli|dal|dalla|al|alla|alle|e`) +
			`,?` + euSpace + euHouseNumber)

	// spanishAddressRegex: Calle Mayor 5, Calle de Alcalá, 42.
	spanishAddressRegex = euStreetLine(
		euAlternation(spanishStreetTypes) + euSpace + spanishNameAndNumber)

	// spanishSlashAddressRegex: C/ Mayor 5, C/Mayor 5. Not right after a
	// slash, where C/ is part of a path (docs/C/Readme 2).
	spanishSlashAddressRegex = euStreetLineAfter(euSlashTypeLeftEdge,
		`C/[ \t]?`+spanishNameAndNumber)

	spanishNameAndNumber = euStreetName(`de|del|la|las|los|el|y`) + `,?` + euSpace + euHouseNumber

	// frenchAddressRegex: 12 rue de la Paix, 8 bis boulevard Saint-Germain,
	// 12 B rue de la Paix. The street type may be capitalized (12 Rue de la
	// Paix).
	frenchAddressRegex = euStreetLine(
		`\d{1,4}(?:[ \t]?(?:bis|ter)\b|[ \t]?[A-D]\b|[a-d]\b)?,?` + euSpace +
			euAlternation(slices.Concat(frenchStreetTypes, capitalized(frenchStreetTypes))) + euSpace +
			euStreetName(`de|du|des|la|le|les|aux|au|sur`))

	// germanAddressRegex: Hauptstraße 12, Karl-Marx-Straße 5, Berliner Str. 5,
	// Lindenallee 4a. The type is a suffix of the street's last word
	// (Johann-Sebastian-Bachstraße), the last part of a hyphenated name, or
	// its own word after an adjective. An adjective such as Alte may come
	// first (Alte Jakobstraße 5).
	germanAddressRegex = euStreetLine(
		`(?:` + euGermanAdjective + euSpace + `)?` +
			`(?:\p{Lu}` + euLetter + `*(?:-` + euLetter + `+)*` + euAlternation(germanStreetTypes) +
			`|\p{Lu}` + euLetter + `*(?:-` + euLetter + `+)*-` + euAlternation(capitalized(germanStreetTypes)) +
			`|\p{Lu}\p{M}*\p{Ll}[\p{Ll}\p{M}]*` + euSpace + euAlternation(capitalized(germanStreetTypes)) + `)` +
			euSpace + `\d{1,4}[a-zA-Z]?`)
)

// streetKeyword is a street type as a European address match uses it: a
// whole word followed by a space or tab, unless a field says otherwise.
type streetKeyword struct {
	word string
	// inWord marks a German suffix, which ends a longer word (Hauptstraße).
	inWord bool
	// nameFollows marks C/, which the name may follow without a space
	// (C/Mayor 5).
	nameFollows bool
}

// streetTypes returns words as keywords. A word with an é is also listed in
// decomposed form, as euAlternation matches it.
func streetTypes(inWord bool, words ...string) []streetKeyword {
	var keywords []streetKeyword
	for _, w := range words {
		keywords = append(keywords, streetKeyword{word: w, inWord: inWord})
		if d := strings.ReplaceAll(w, "é", "e\u0301"); d != w {
			keywords = append(keywords, streetKeyword{word: d, inWord: inWord})
		}
	}
	return keywords
}

// europeanAddressKeywords maps each European address pattern to the street
// types one of which a string must hold for the pattern to match. Finding
// them is far cheaper than running the pattern, and most strings hold none.
// The word checks matter: true holds rue and Caller holds Calle.
var europeanAddressKeywords = map[*regexp.Regexp][]streetKeyword{
	italianAddressRegex:      streetTypes(false, italianStreetTypes...),
	spanishAddressRegex:      streetTypes(false, spanishStreetTypes...),
	spanishSlashAddressRegex: {{word: "C/", nameFollows: true}},
	frenchAddressRegex:       streetTypes(false, slices.Concat(frenchStreetTypes, capitalized(frenchStreetTypes))...),
	germanAddressRegex: slices.Concat(
		streetTypes(false, capitalized(germanStreetTypes)...),
		streetTypes(true, germanStreetTypes...)),
}

func isEuropeanAddressRegex(re *regexp.Regexp) bool {
	_, ok := europeanAddressKeywords[re]
	return ok
}

// mayContainEuropeanAddress reports whether s holds one of re's street types
// where a match could use it.
func mayContainEuropeanAddress(re *regexp.Regexp, s string) bool {
	return slices.ContainsFunc(europeanAddressKeywords[re], func(k streetKeyword) bool {
		return hasStreetKeyword(s, k)
	})
}

// hasStreetKeyword reports whether s holds k with nothing before it that would
// join it to a longer word (a JSON escape such as \n does not), and with a
// space or tab after it, as every pattern needs after the street type.
func hasStreetKeyword(s string, k streetKeyword) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], k.word)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(k.word)
		prev := byte(0)
		if start > 0 {
			prev = s[start-1]
		}
		atWordEdge := !isASCIIAlnum(prev) && prev != '_'
		before := k.inWord || atWordEdge || followsJSONEscape(s, start)
		after := end < len(s) && (s[end] == ' ' || s[end] == '\t' || k.nameFollows)
		if before && after {
			return true
		}
		i = start + 1
	}
}

// europeanAddressMatches returns the street lines re, a European address
// pattern, finds in s, without the left edge it matches before each one.
func europeanAddressMatches(re *regexp.Regexp, s string) [][]int {
	var locs [][]int
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		if endsStreetLine(s, m[3]) {
			locs = append(locs, m[2:4])
		}
	}
	return locs
}

// endsStreetLine reports whether a European street line ending at s[end] ends
// there rather than inside a longer word or number: no letter, digit or
// underscore follows it (Via Roma 12345, Hauptstraße 12px), and the house
// number does not go on as a decimal or version (Via Node 18.2, Via Node
// 18,2). A postal code right after a comma (Via Roma 12,20121 Milano) is not
// a decimal.
func endsStreetLine(s string, end int) bool {
	if end == len(s) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(s[end:])
	switch {
	case isAddressWordRune(next):
		return false
	case (next == '.' || next == ',') && isDigit(s[end-1]):
		rest := s[end+1:]
		n := len(rest) - len(strings.TrimLeft(rest, "0123456789"))
		return n == 0 || (next == ',' && (n == 4 || n == 5) && !startsWithAddressWordRune(rest[n:]))
	}
	return true
}

func startsWithAddressWordRune(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return s != "" && isAddressWordRune(r)
}

func isAddressWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r)
}

// followsJSONEscape reports whether s[:start] ends in a JSON whitespace
// escape (\n, \r, \t).
func followsJSONEscape(s string, start int) bool {
	if start < 2 || s[start-2] != '\\' {
		return false
	}
	switch s[start-1] {
	case 'n', 'r', 't':
		return true
	}
	return false
}
