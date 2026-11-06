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
// la, du, des) may sit between name words.
const (
	// euNameWord is one title-case word, optionally hyphenated
	// (Saint-Germain, Champs-Élysées) or elided onto an article (l'Église,
	// dell'Indipendenza, Sant'Ambrogio).
	euNameWord = `(?:(?:[dDlL]|[Dd]ell|[Aa]ll|[Ss]ant)['’])?\p{Lu}\p{Ll}\p{L}*(?:-\p{L}+)*`
	// euRomanNumeral may follow a name word (Vittorio Emanuele II). It cannot
	// start a name, so an acronym such as CLI after Via does not count.
	euRomanNumeral = `[IVX]{1,5}`
	// euSpace separates the parts of a street line: spaces or tabs, never a
	// line break.
	euSpace = `[ \t]+`
	// euHouseNumber is the number that ends an Italian or Spanish street
	// line, optionally after n., nº or n°, with an optional letter or
	// sub-number (3/A, 12b, 7/2).
	euHouseNumber = `(?:n(?:\.[°º]?|[°º])[ \t]?)?\d{1,4}(?:[ \t]?/[ \t]?[0-9A-Za-z]{1,3}|[A-Za-z])?`
)

// euStreetName is a street name of title-case words, with the given
// lowercase particles allowed before and between them. Words are separated by
// spaces or tabs only, so a name never runs on into the next line (a French
// name has no house number after it to stop it).
func euStreetName(particles string) string {
	p := `(?:(?:` + particles + `)` + euSpace + `)*`
	return p + euNameWord + `(?:` + euSpace + `(?:` + p + euNameWord + `|` + euRomanNumeral + `))*`
}

var (
	// italianAddressRegex: Via Roma 12, Piazza del Duomo 1, Via dei Mille 3/A.
	italianAddressRegex = regexp.MustCompile(
		`(?:Via|Viale|Piazza|Piazzale|Corso|Largo|Vicolo|Strada)` + euSpace +
			euStreetName(`di|da|del|dello|della|delle|dei|degli|dal|dalla|al|alla|alle|e`) +
			`,?` + euSpace + euHouseNumber)

	// spanishAddressRegex: Calle Mayor 5, Calle de Alcalá, 42, C/ Mayor 5.
	spanishAddressRegex = regexp.MustCompile(
		`(?:(?:Calle|Avenida|Avda\.|Plaza|Paseo|Camino|Ronda|Rambla)` + euSpace + `|C/[ \t]?)` +
			euStreetName(`de|del|la|las|los|el|y`) +
			`,?` + euSpace + euHouseNumber)

	// frenchAddressRegex: 12 rue de la Paix, 8 bis boulevard Saint-Germain.
	// The street type may be capitalized (12 Rue de la Paix).
	frenchAddressRegex = regexp.MustCompile(
		`\d{1,4}(?:[ \t]?(?:bis|ter)\b|[A-Da-d]\b)?,?` + euSpace +
			`(?:[Rr]ue|[Aa]venue|[Bb]oulevard|[Pp]lace|[Cc]hemin|[Aa]llée|[Ii]mpasse|[Qq]uai)` + euSpace +
			euStreetName(`de|du|des|la|le|les|aux|au|sur`))

	// germanAddressRegex: Hauptstraße 12, Karl-Marx-Straße 5, Berliner Str. 5,
	// Lindenallee 4a. The type is a suffix of the street's last word
	// (Johann-Sebastian-Bachstraße), the last part of a hyphenated name, or
	// its own word after an adjective.
	germanAddressRegex = regexp.MustCompile(
		`(?:\p{Lu}\p{L}*(?:-\p{L}+)*(?:straße|strasse|str\.|weg|platz|allee|gasse)` +
			`|\p{Lu}\p{L}*(?:-\p{L}+)*-(?:Straße|Strasse|Str\.|Weg|Platz|Allee|Gasse)` +
			`|\p{Lu}\p{Ll}+` + euSpace + `(?:Straße|Strasse|Str\.|Weg|Platz|Allee|Gasse))` +
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

func streetTypes(inWord bool, words ...string) []streetKeyword {
	keywords := make([]streetKeyword, len(words))
	for i, w := range words {
		keywords[i] = streetKeyword{word: w, inWord: inWord}
	}
	return keywords
}

// europeanAddressKeywords maps each European address pattern to the street
// types one of which a string must hold for the pattern to match. Finding
// them is far cheaper than running the pattern, and most strings hold none.
// The word checks matter: true holds rue and Caller holds Calle.
var europeanAddressKeywords = map[*regexp.Regexp][]streetKeyword{
	italianAddressRegex: streetTypes(false, "Via", "Viale", "Piazza", "Piazzale", "Corso", "Largo", "Vicolo", "Strada"),
	spanishAddressRegex: append(
		streetTypes(false, "Calle", "Avenida", "Avda.", "Plaza", "Paseo", "Camino", "Ronda", "Rambla"),
		streetKeyword{word: "C/", nameFollows: true}),
	frenchAddressRegex: streetTypes(false,
		"rue", "Rue", "avenue", "Avenue", "boulevard", "Boulevard", "place", "Place",
		"chemin", "Chemin", "allée", "Allée", "impasse", "Impasse", "quai", "Quai"),
	germanAddressRegex: append(
		streetTypes(false, "Straße", "Strasse", "Str.", "Weg", "Platz", "Allee", "Gasse"),
		streetTypes(true, "straße", "strasse", "str.", "weg", "platz", "allee", "gasse")...),
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
		before := k.inWord || (!isASCIIAlnum(prev) && prev != '_') || followsJSONEscape(s, start)
		after := end < len(s) && (s[end] == ' ' || s[end] == '\t' || k.nameFollows)
		if before && after {
			return true
		}
		i = start + 1
	}
}

// addressStandsAlone reports whether the European address match s[start:end],
// found without \b since Go regexps only know ASCII letters, is a whole street
// line rather than part of a longer word or number: no letter, digit or
// underscore touches either end (xHauptstraße 12, Via Roma 12345), and the
// house number does not continue as a decimal or version (Via Node 18.2).
// A JSON escape before the match (\nVia Roma 12) is a line break, not a letter.
func addressStandsAlone(s string, start, end int) bool {
	if start > 0 {
		prev, _ := utf8.DecodeLastRuneInString(s[:start])
		if isAddressWordRune(prev) && !followsJSONEscape(s, start) {
			return false
		}
	}
	if end < len(s) {
		next, _ := utf8.DecodeRuneInString(s[end:])
		if isAddressWordRune(next) {
			return false
		}
		if (next == '.' || next == ',') && end+1 < len(s) && isDigit(s[end+1]) && isDigit(s[end-1]) {
			return false
		}
	}
	return true
}

func isAddressWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
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
