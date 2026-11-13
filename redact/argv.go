package redact

import (
	"cmp"
	"slices"
	"sort"
	"strings"
)

// argvLine is a JSON array of strings read as the argument vector of one
// command, as Codex stores a command it ran ({"command":["mysql","-pX"]}).
// There a client and its password flag are separate leaves, and the
// command-line layer finds a password only with the client before it, so the
// array is joined into the command line a shell would run, each element one
// shell word, and the regions found there are mapped back to the elements.
//
// The regions are found once per array, on the first element asked about.
type argvLine struct {
	arr     []any
	read    bool
	regions [][]taggedRegion
}

// argvElement is a string leaf's place in an argvLine, the zero value for a
// leaf that is not an element of one.
type argvElement struct {
	line  *argvLine
	index int
}

// newArgvLine returns the argvLine for arr, nil when arr cannot be an argument
// vector: fewer than two elements, or a first element that names no program
// (no string, empty, or an option such as "--password"). The other elements
// are checked when the line is read.
func newArgvLine(arr []any) *argvLine {
	if len(arr) < 2 {
		return nil
	}
	if program, ok := arr[0].(string); !ok || program == "" || program[0] == '-' {
		return nil
	}
	return &argvLine{arr: arr}
}

// regions returns the command-line regions the element's argument vector
// finds in it, as offsets into the element. The slice is shared: callers
// append it to their own.
func (e argvElement) regions() []taggedRegion {
	if e.line == nil {
		return nil
	}
	if !e.line.read {
		e.line.read = true
		e.line.regions = e.line.find()
	}
	if e.line.regions == nil {
		return nil
	}
	return e.line.regions[e.index]
}

// argvRegionsByValue returns, for each string that is an element of an
// argument vector in v, the command-line regions its vectors find in it. A
// leaf anywhere in v with that text takes them too, since the splice writes
// one result into every copy of a string, and the re-encode fallback must
// write the same. Nil when there are none.
//
// Only arrays (and the maps that hold them) are walked, so a wide object
// with no argument vector does not pay a pre-pass over every leaf.
func argvRegionsByValue(v any) map[string][]taggedRegion {
	var byValue map[string][]taggedRegion
	collectArgvRegions(false, v, &byValue)
	if byValue == nil {
		return nil
	}
	// Copies in several vectors find the same regions; sorting drops the
	// repeats in one pass, however many regions a value has.
	for val, regions := range byValue {
		slices.SortFunc(regions, compareTaggedRegions)
		byValue[val] = slices.Compact(regions)
	}
	return byValue
}

// collectArgvRegions adds to byValue the command-line regions of each
// argument-vector element in v, following the same skip rules as
// walkRedactableStrings so a vector under an image object or a skipped
// field is not joined.
func collectArgvRegions(skipStrings bool, v any, byValue *map[string][]taggedRegion) {
	switch val := v.(type) {
	case map[string]any:
		if shouldSkipJSONLObject(val) {
			return
		}
		for k, child := range val {
			collectArgvRegions(shouldSkipJSONLField(k), child, byValue)
		}
	case []any:
		line := newArgvLine(val)
		for i, child := range val {
			s, ok := child.(string)
			if !ok {
				collectArgvRegions(skipStrings, child, byValue)
				continue
			}
			if skipStrings || line == nil {
				continue
			}
			found := argvElement{line, i}.regions()
			if len(found) == 0 {
				continue
			}
			if *byValue == nil {
				*byValue = make(map[string][]taggedRegion)
			}
			// The same text in several vectors takes the regions of each, so a
			// password one of them finds is hidden in every copy.
			(*byValue)[s] = append((*byValue)[s], found...)
		}
	}
}

// compareTaggedRegions orders regions by position, then by what found them.
func compareTaggedRegions(a, b taggedRegion) int {
	return cmp.Or(cmp.Compare(a.start, b.start), cmp.Compare(a.end, b.end), strings.Compare(a.label, b.label),
		cmp.Compare(a.layer, b.layer), compareBools(a.builtinIP, b.builtinIP))
}

// compareBools lets compareTaggedRegions order regions by every field, so
// equal regions end up next to each other for slices.Compact.
func compareBools(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// find joins the elements into a command line and maps the command-line
// regions found in it to the elements, nil when there are none.
func (l *argvLine) find() [][]taggedRegion {
	args := make([]string, len(l.arr))
	maybe := false
	for i, v := range l.arr {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		args[i] = s
		// The layer reads only flags, which start with '-', and .netrc
		// entries, which name a password in any case.
		maybe = maybe || strings.Contains(s, "-") || containsFoldASCII(s, "password")
	}
	if !maybe {
		return nil
	}
	var b []byte
	starts := make([]int, len(args))
	for i, arg := range args {
		if i > 0 {
			b = append(b, ' ')
		}
		starts[i] = len(b)
		b, _ = appendArgvWord(b, arg, argvQuotesValue(args, i), false)
	}
	found := detectCommandLinePasswords(string(b))
	if len(found) == 0 {
		return nil
	}
	regions := make([][]taggedRegion, len(args))
	positions := make(map[int][]int)
	for _, r := range found {
		// The first element whose word ends after the region starts.
		k := sort.Search(len(args), func(k int) bool { return wordEnd(starts, len(b), k) > r.start })
		for ; k < len(args) && starts[k] < r.end; k++ {
			pos, ok := positions[k]
			if !ok {
				_, pos = appendArgvWord(nil, args[k], argvQuotesValue(args, k), true)
				positions[k] = pos
			}
			start, end := argvRegion(pos, r.start-starts[k], r.end-starts[k])
			if start < end {
				regions[k] = append(regions[k], taggedRegion{region: region{start, end}, label: r.label, layer: layerConnectionString})
			}
		}
	}
	return regions
}

// argvQuotesValue reports whether element i is written in quotes because it
// is the value of a long password flag in the element before it. The layer
// reads a bare word there as prose when more words follow (--password to
// set it), but an element is a value of its own, whatever its words.
func argvQuotesValue(args []string, i int) bool {
	if i == 0 || args[i] == "" {
		return false
	}
	switch args[i-1] {
	case "--password", "--passwd", "--pass":
		// A following flag stays bare; a password value is quoted so the
		// layer reads it as one word, even when it looks like prose.
		return args[i][0] != '-'
	}
	// A .netrc password that starts with a dash would otherwise be split
	// as a bare option plus a quoted rest, and the netrc reader would
	// stop at the bare part. Only those values are quoted: quoting every
	// word after "password" also quotes a later password keyword when the
	// login name is itself "password", and the netrc reader then misses
	// the entry.
	return strings.EqualFold(args[i-1], "password") && args[i][0] == '-'
}

// wordEnd returns where the word of element k ends in a command line of
// length n whose words start at starts.
func wordEnd(starts []int, n, k int) int {
	if k+1 < len(starts) {
		return starts[k+1] - 1
	}
	return n
}

// argvRegion returns the bytes of an element written at the word offsets in
// [start, end), given the offset of each of its bytes, empty when the region
// covers only the quotes around them.
func argvRegion(pos []int, start, end int) (int, int) {
	first := sort.SearchInts(pos, start)
	last := sort.SearchInts(pos, end)
	return first, last
}

// appendArgvWord appends arg to b as one shell word, and with positions also
// returns the offset in that word of each byte of arg. The quoting is the
// plainest one the layer reads as that word:
//
//   - none for letters, digits and @%+=:./_-
//   - single quotes for a word with no single quote in it
//   - double quotes for one with nothing a double-quoted string expands
//   - a backslash before each other byte for one with no blank in it, since
//     the layer reads a value quoted that way ('$x', "a'b") whole, while it
//     reads a quote it escapes as ending the quotes the flag is in
//   - otherwise single quotes, each quote in the word closing them, escaped,
//     and opening them again
//
// An option joined to a value that needs quotes keeps the option bare and
// quotes the value (--password=' x', -p' x'), as one types it, since the layer
// reads a value that starts with a blank only in quotes of its own.
// quoteValue quotes a bare word too. A line break is written as '0', a digit,
// which is part of a word in every quoting, so the command line stays on one
// line, where the layer reads quoted strings, and which no placeholder holds,
// so a password with a line break (secret<newline>here) never reads as one
// (secret_here).
func appendArgvWord(b []byte, arg string, quoteValue, positions bool) ([]byte, []int) {
	start := len(b)
	var pos []int
	quoting := argvQuoting(arg)
	option := 0
	if quoting != argvBare && !quoteValue {
		option = argvOptionPrefix(arg)
		quoting = argvQuoting(arg[option:])
	}
	if quoteValue && quoting == argvBare {
		quoting = argvSingle
	}
	// at records that the next byte of arg is written skip bytes into what is
	// about to be appended for it.
	at := func(skip int) {
		if positions {
			pos = append(pos, len(b)-start+skip)
		}
	}
	for i := range option {
		at(0)
		b = append(b, arg[i])
	}
	rest := arg[option:]
	switch quoting {
	case argvBare:
		for i := range len(rest) {
			at(0)
			b = append(b, argvWordByte(rest[i]))
		}
		return b, pos
	case argvEscaped:
		for i := range len(rest) {
			if c := argvWordByte(rest[i]); !argvSafe(c) {
				at(1)
				b = append(b, '\\', c)
				continue
			}
			at(0)
			b = append(b, argvWordByte(rest[i]))
		}
		return b, pos
	}
	quote := byte('\'')
	if quoting == argvDouble {
		quote = '"'
	}
	b = append(b, quote)
	for i := range len(rest) {
		if rest[i] == '\'' && quoting == argvSingle {
			// '\'' closes the quotes, writes an escaped quote and opens
			// them again: the quote itself is the third byte.
			at(2)
			b = append(b, '\'', '\\', '\'', '\'')
			continue
		}
		at(0)
		b = append(b, argvWordByte(rest[i]))
	}
	return append(b, quote), pos
}

const (
	argvBare = iota
	argvSingle
	argvDouble
	argvEscaped
)

// argvWordByte is the byte appendArgvWord writes for c.
func argvWordByte(c byte) byte {
	if c == '\n' || c == '\r' {
		return '0'
	}
	return c
}

// argvSafe reports whether c stays as it is in a bare shell word.
func argvSafe(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("@%+=:./_-", c) >= 0
}

// argvOptionPrefix returns the length of the option that starts arg: a dash,
// letters, digits and dashes, and an '=' after them, 0 when arg starts with
// no option.
func argvOptionPrefix(arg string) int {
	if len(arg) < 2 || arg[0] != '-' {
		return 0
	}
	j := 1
	for j < len(arg) {
		c := arg[j]
		isLetter := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		isDigit := '0' <= c && c <= '9'
		if c != '-' && !isLetter && !isDigit {
			break
		}
		j++
	}
	if j == 1 || strings.Trim(arg[:j], "-") == "" {
		return 0
	}
	if j < len(arg) && arg[j] == '=' {
		j++
	}
	return j
}

func argvQuoting(arg string) int {
	if arg == "" {
		return argvSingle
	}
	bare, apostrophe, expands, blank := true, false, false, false
	for i := range len(arg) {
		switch c := argvWordByte(arg[i]); {
		case argvSafe(c):
		default:
			bare = false
			apostrophe = apostrophe || c == '\''
			expands = expands || strings.IndexByte("\"\\$`", c) >= 0
			blank = blank || c == ' ' || c == '\t'
		}
	}
	switch {
	case bare:
		return argvBare
	case !apostrophe:
		return argvSingle
	case !expands:
		return argvDouble
	case !blank:
		return argvEscaped
	}
	return argvSingle
}
