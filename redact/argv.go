package redact

import (
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
// vector: fewer than two elements, or a first element that is no string. The
// other elements are checked when the line is read.
func newArgvLine(arr []any) *argvLine {
	if len(arr) < 2 {
		return nil
	}
	if _, ok := arr[0].(string); !ok {
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
		// entries, which name a password.
		maybe = maybe || strings.Contains(s, "-") || strings.Contains(s, "password")
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
		b, _ = appendArgvWord(b, arg, false)
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
				_, pos = appendArgvWord(nil, args[k], true)
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
	return first, max(first, last)
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
// A word with a line break is ANSI-C quoted, so the command line stays on one
// line, where the layer reads quoted strings.
func appendArgvWord(b []byte, arg string, positions bool) ([]byte, []int) {
	start := len(b)
	var pos []int
	// at records that the next byte of arg is written skip bytes into what is
	// about to be appended for it.
	at := func(skip int) {
		if positions {
			pos = append(pos, len(b)-start+skip)
		}
	}
	quoting := argvQuoting(arg)
	switch quoting {
	case argvBare:
		for i := range len(arg) {
			at(i)
		}
		return append(b, arg...), pos
	case argvEscaped:
		for i := range len(arg) {
			if c := arg[i]; !argvSafe(c) {
				at(1)
				b = append(b, '\\', c)
				continue
			}
			at(0)
			b = append(b, arg[i])
		}
		return b, pos
	case argvANSIC:
		b = append(b, '$', '\'')
		for i := range len(arg) {
			at(0)
			switch c := arg[i]; c {
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\\', '\'':
				b = append(b, '\\', c)
			default:
				b = append(b, c)
			}
		}
		return append(b, '\''), pos
	}
	quote := byte('\'')
	if quoting == argvDouble {
		quote = '"'
	}
	b = append(b, quote)
	for i := range len(arg) {
		if arg[i] == '\'' && quoting == argvSingle {
			// '\'' closes the quotes, writes an escaped quote and opens
			// them again: the quote itself is the third byte.
			at(2)
			b = append(b, '\'', '\\', '\'', '\'')
			continue
		}
		at(0)
		b = append(b, arg[i])
	}
	return append(b, quote), pos
}

const (
	argvBare = iota
	argvSingle
	argvDouble
	argvEscaped
	argvANSIC
)

// argvSafe reports whether c stays as it is in a bare shell word.
func argvSafe(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("@%+=:./_-", c) >= 0
}

func argvQuoting(arg string) int {
	if arg == "" {
		return argvSingle
	}
	bare, apostrophe, expands, blank := true, false, false, false
	for i := range len(arg) {
		switch c := arg[i]; {
		case c == '\n' || c == '\r':
			return argvANSIC
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
