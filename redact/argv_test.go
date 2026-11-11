package redact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A command stored as an argument vector splits a client and its password
// flag across array elements. JSONLBytes is the checkpoint-writing path, so
// these go through it rather than through String.
func TestJSONLBytes_CommandLinePasswordsInArgvArrays(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{`{"type":"CommandExecution","id":"e1","command":["mysql","-phunter2"],"status":"completed"}`, `{"type":"CommandExecution","id":"e1","command":["mysql","-pREDACTED"],"status":"completed"}`},
		{`{"command":["/usr/bin/mysql","-uroot","-phunter2","app"]}`, `{"command":["/usr/bin/mysql","-uroot","-pREDACTED","app"]}`},
		{`{"command":["docker","login","-u","me","-p","hunter2","registry.example"]}`, `{"command":["docker","login","-u","me","-p","REDACTED","registry.example"]}`},
		{`{"command":["openssl","enc","-aes-256-cbc","-pass","pass:hunter2"]}`, `{"command":["openssl","enc","-aes-256-cbc","-pass","pass:REDACTED"]}`},
		{`{"command":["wget","--password","hunter2","https://example.com"]}`, `{"command":["wget","--password","REDACTED","https://example.com"]}`},
		{`{"command":["sudo","-u","root","mysql","-phunter2"]}`, `{"command":["sudo","-u","root","mysql","-pREDACTED"]}`},
		{`{"command":["sshpass","-p","pw 2026","ssh","host"]}`, `{"command":["sshpass","-p","REDACTED","ssh","host"]}`},
		{`{"payload":{"item":{"command":["redis-cli","-a","hunter2","ping"]}}}`, `{"payload":{"item":{"command":["redis-cli","-a","REDACTED","ping"]}}}`},
		// An element that needs quotes is still one word: blanks, a quote,
		// shell operators, backslashes and line breaks in other elements.
		{`{"command":["mysql","-phunter 2"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-pit's2026"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-h","db","-p'quoted'"]}`, `{"command":["mysql","-h","db","-pREDACTED"]}`},
		{`{"command":["redis-cli","-a","it's 2026"]}`, `{"command":["redis-cli","-a","REDACTED"]}`},
		{`{"command":["mysql","-p\"x;y\"z"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-pa\\b"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-p'$x'"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-p'a\"b'"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["openssl","enc","-pass","pass:'$x'"]}`, `{"command":["openssl","enc","-pass","pass:REDACTED"]}`},
		{`{"command":["wget","--password","it's \"x\""]}`, `{"command":["wget","--password","REDACTED"]}`},
		{`{"command":["mysql","-e","select 1;\nselect 2","-phunter2"]}`, `{"command":["mysql","-e","select 1;\nselect 2","-pREDACTED"]}`},
		// An element is passed as typed, with no shell to expand it.
		{`{"command":["mysql","-p$PASSWORD"]}`, `{"command":["mysql","-pREDACTED"]}`},
		// The password repeated elsewhere on the line is replaced too, as an
		// array element already is.
		{`{"command":["mysql","-phunter2"],"echo":"-phunter2"}`, `{"command":["mysql","-pREDACTED"],"echo":"-pREDACTED"}`},
		// A spelling the splice does not find goes through the structural
		// fallback, which reads the array the same way, and hides the
		// password in every copy, as the splice does.
		{`{"command":["mysql","\u002dphunter2"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-phunter2"],"echo":"\u002dphunter2"}`, `{"command":["mysql","-pREDACTED"],"echo":"-pREDACTED"}`},
		// A value that starts with a blank, joined to its option.
		{`{"command":["wget","--password= hunter"]}`, `{"command":["wget","--password=REDACTED"]}`},
		{`{"command":["mysql","-p hunter"]}`, `{"command":["mysql","-pREDACTED"]}`},
		// A line break in a password element, also one that would make it
		// read as a placeholder (secret_here) if written as a blank or "_".
		{`{"command":["mysql","-phunter2\nfoo"]}`, `{"command":["mysql","-pREDACTED"]}`},
		{`{"command":["mysql","-psecret\nhere"]}`, `{"command":["mysql","-pREDACTED"]}`},
		// An element after --password is its value, though it reads as prose.
		{`{"command":["docker","login","--password","instead","registry"]}`, `{"command":["docker","login","--password","REDACTED","registry"]}`},
		// .netrc keywords in any case.
		{`{"command":["echo","MACHINE","host","LOGIN","alice","PASSWORD","hunter2"]}`, `{"command":["echo","MACHINE","host","LOGIN","alice","PASSWORD","REDACTED"]}`},
		// A .netrc password written with '\'' (an apostrophe and a character
		// a double-quoted string would expand) is still one token.
		{`{"command":["echo","machine","host","login","alice","password","it's $2026"]}`, `{"command":["echo","machine","host","login","alice","password","REDACTED"]}`},
		{`{"command":["echo","machine","host","login","alice","password","it's \"2026\""]}`, `{"command":["echo","machine","host","login","alice","password","REDACTED"]}`},
		{`{"command":["echo","machine","host","login","alice","password","\"it's 2026\""]}`, `{"command":["echo","machine","host","login","alice","password","REDACTED"]}`},
		// A .netrc password that starts with a dash and contains a blank
		// is quoted whole, so the netrc reader does not stop at a bare option prefix.
		{`{"command":["echo","machine","h","login","me","password","-hunter2 Summer2026"]}`, `{"command":["echo","machine","h","login","me","password","REDACTED"]}`},
		// A login name that is itself "password" must not quote the later
		// password keyword, or the netrc reader misses the entry.
		{`{"command":["echo","machine","h","login","password","password","hunter2"]}`, `{"command":["echo","machine","h","login","password","password","REDACTED"]}`},
		// A password that is also an object key leaves the key as it is, and
		// one after an escaped quote inside another string leaves that
		// string as it is. A key after a comma, or written with spaces
		// around the colon, is still a key.
		{`{"type":"x","command":["redis-cli","-a","type"]}`, `{"type":"x","command":["redis-cli","-a","REDACTED"]}`},
		{`{"event":"hello\"type","command":["redis-cli","-a","type"]}`, `{"event":"hello\"type","command":["redis-cli","-a","REDACTED"]}`},
		{`{"a":1,"type":"x","command":["redis-cli","-a","type"]}`, `{"a":1,"type":"x","command":["redis-cli","-a","REDACTED"]}`},
		{`{"a":1, "type" : "x", "command":["redis-cli","-a","type"]}`, `{"a":1, "type" : "x", "command":["redis-cli","-a","REDACTED"]}`},
		// A token the element's own detectors already flag is hidden in
		// the key as well, as it is without the array.
		{`{"tokens":{"` + highEntropySecret + `":{"scope":"repo"}},"command":["docker","login","-u","me","-p","` + highEntropySecret + `"]}`, `{"tokens":{"REDACTED":{"scope":"repo"}},"command":["docker","login","-u","me","-p","REDACTED"]}`},
		// A credential field with the same text as an argv element is
		// hidden whole, as it is without the array.
		{`{"host":"db","user":"u","pwd":"-pa","command":["mysql","-pa"]}`, `{"host":"db","user":"u","pwd":"REDACTED","command":["mysql","-pREDACTED"]}`},
		// The same element in two vectors takes the regions of both, so the
		// mysql password is hidden in the echo's copy too.
		{`{"a":["echo","-phunter2 --password=Summer2026"],"b":["mysql","-phunter2 --password=Summer2026"]}`, `{"a":["echo","-pREDACTED"],"b":["mysql","-pREDACTED"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLBytes([]byte(tc.in + "\n"))
			require.NoError(t, err)
			require.Equal(t, tc.want+"\n", string(got.Bytes()))
			content, err := JSONLContent(tc.in)
			require.NoError(t, err)
			require.Equal(t, tc.want, content)
		})
	}
}

// The walk leaves identifiers, paths and signatures as they are. A copy of
// an argv password there keeps its text, though a values-only splice would
// rewrite every value with that text, so such a line is rebuilt
// structurally instead.
func TestJSONLBytes_ArgvPasswordCopiesInSkippedFieldsStay(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, key, secret string
		want            any
	}{
		{`{"id":"type","command":["redis-cli","-a","type"]}`, "id", "type", "type"},
		{`{"session_id":"type","command":["redis-cli","-a","type"]}`, "session_id", "type", "type"},
		{`{"cwd":"/srv","command":["redis-cli","-a","/srv"]}`, "cwd", "/srv", "/srv"},
		{`{"ids":["type"],"command":["redis-cli","-a","type"]}`, "ids", "type", []any{"type"}},
		{`{"thinkingSignature":"-phunter2","command":["mysql","-phunter2"]}`, "thinkingSignature", "-phunter2", "-phunter2"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLBytes([]byte(tc.in + "\n"))
			require.NoError(t, err)
			content, err := JSONLContent(tc.in)
			require.NoError(t, err)
			require.Equal(t, content+"\n", string(got.Bytes()))
			var obj map[string]any
			require.NoError(t, json.Unmarshal([]byte(content), &obj))
			require.Equal(t, tc.want, obj[tc.key])
			command, ok := obj["command"].([]any)
			require.True(t, ok)
			require.NotContains(t, command, tc.secret)
			require.Contains(t, content, "REDACTED")
		})
	}
}

func TestJSONLBytes_SelfDetectedArgvPasswordDoesNotRewriteSkippedFields(t *testing.T) {
	t.Parallel()
	tok := highEntropySecret
	cases := []struct {
		name, in, skipKey string
		wantKeyRedacted   bool
	}{
		{"id", `{"id":"` + tok + `","command":["docker","login","-u","me","-p","` + tok + `"]}`, "id", false},
		{"thinkingSignature", `{"thinkingSignature":"` + tok + `","command":["docker","login","-u","me","-p","` + tok + `"]}`, "thinkingSignature", false},
		{"id and secret key", `{"id":"` + tok + `","tokens":{"` + tok + `":{"scope":"repo"}},"command":["docker","login","-u","me","-p","` + tok + `"]}`, "id", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLBytes([]byte(tc.in + "\n"))
			require.NoError(t, err)
			content, err := JSONLContent(tc.in)
			require.NoError(t, err)
			require.Equal(t, content+"\n", string(got.Bytes()))
			var obj map[string]any
			require.NoError(t, json.Unmarshal([]byte(content), &obj))
			require.Equal(t, tok, obj[tc.skipKey], content)
			require.Contains(t, content, "REDACTED")
			command, ok := obj["command"].([]any)
			require.True(t, ok)
			require.NotContains(t, command, tok)
			if tc.wantKeyRedacted {
				tokens, ok := obj["tokens"].(map[string]any)
				require.True(t, ok, content)
				_, hasTok := tokens[tok]
				require.False(t, hasTok, content)
				_, hasRedacted := tokens["REDACTED"]
				require.True(t, hasRedacted, content)
				stored := strings.Count(content, RedactedPlaceholder)
				require.NotNil(t, got.Counts())
				require.Equal(t, stored, got.Counts().Replacements, content)
			}
		})
	}
}

// Renaming self-detected secret keys in the structural fallback keeps every
// member of the object: a renamed key that meets a REDACTED key already
// there, or another renamed one, is numbered, and the count matches the
// placeholders the line gained.
func TestJSONLBytes_RenamedSecretKeysKeepEveryMember(t *testing.T) {
	t.Parallel()
	tok := highEntropySecret
	tok2 := "sk-ant-api03-pQ7wE2rT9yU4iO1aS6dF3gH8jK5lZ0xC"
	cases := []struct {
		name, in string
		want     map[string]any
	}{
		{
			"renamed key meets a REDACTED key",
			`{"id":"` + tok + `","tokens":{"` + tok + `":{"scope":"repo"},"REDACTED":{"scope":"admin"}},"command":["docker","login","-u","me","-p","` + tok + `"]}`,
			map[string]any{"REDACTED": map[string]any{"scope": "admin"}, "REDACTED_2": map[string]any{"scope": "repo"}},
		},
		{
			"two renamed keys",
			`{"id":"` + tok + `","tokens":{"` + tok + `":{"scope":"repo"},"` + tok2 + `":{"scope":"admin"}},"command":["docker","login","-p","` + tok + `"],"other":["docker","login","-p","` + tok2 + `"]}`,
			map[string]any{"REDACTED": map[string]any{"scope": "admin"}, "REDACTED_2": map[string]any{"scope": "repo"}},
		},
		{
			"renamed keys meet a numbered key",
			`{"id":"` + tok + `","tokens":{"` + tok + `":{"scope":"repo"},"` + tok2 + `":{"scope":"admin"},"REDACTED_2":{"scope":"read"}},"command":["docker","login","-p","` + tok + `"],"other":["docker","login","-p","` + tok2 + `"]}`,
			map[string]any{"REDACTED": map[string]any{"scope": "admin"}, "REDACTED_2": map[string]any{"scope": "read"}, "REDACTED_3": map[string]any{"scope": "repo"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var first string
			for range 20 {
				got, err := JSONLBytes([]byte(tc.in + "\n"))
				require.NoError(t, err)
				out := string(got.Bytes())
				if first == "" {
					first = out
				}
				require.Equal(t, first, out, "the output must not depend on map order")
				var obj map[string]any
				require.NoError(t, json.Unmarshal([]byte(out), &obj))
				require.Equal(t, tok, obj["id"], out)
				require.Equal(t, tc.want, obj["tokens"], out)
				require.NotContains(t, out, tok2, out)
				require.NotNil(t, got.Counts())
				added := strings.Count(out, RedactedPlaceholder) - strings.Count(tc.in, RedactedPlaceholder)
				require.Equal(t, added, got.Counts().Replacements, out)
			}
		})
	}
}

func TestJSONLBytes_FallbackStillHidesUnrelatedSecretInSkippedField(t *testing.T) {
	t.Parallel()
	a := highEntropySecret
	b := githubPATFixture
	// B sits in an ordinary array, so its replacement is unkeyed. The argv
	// password A in id forces the structural fallback; that must still hide
	// B in thinkingSignature, as the splice would have.
	in := `{"id":"` + a + `","command":["docker","login","-p","` + a + `"],"thinkingSignature":"` + b + `","items":["` + b + `"]}`
	got, err := JSONLBytes([]byte(in + "\n"))
	require.NoError(t, err)
	out := string(got.Bytes())
	var obj map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &obj))
	require.Equal(t, a, obj["id"], out)
	require.Equal(t, RedactedPlaceholder, obj["thinkingSignature"], out)
	items, ok := obj["items"].([]any)
	require.True(t, ok, out)
	require.Equal(t, []any{RedactedPlaceholder}, items, out)
	command, ok := obj["command"].([]any)
	require.True(t, ok)
	require.NotContains(t, command, a)
	require.NotContains(t, out, b)
	stored := strings.Count(out, RedactedPlaceholder)
	require.Equal(t, 3, stored, out)
	require.NotNil(t, got.Counts())
	require.Equal(t, stored, got.Counts().Replacements)
}

func TestJSONLBytes_ArgvArraysWithoutPasswordsStayVisible(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`{"type":"CommandExecution","id":"e2","command":["grep","-c","two words","notes.txt"],"exit_code":0}`,
		`{"command":["/bin/zsh","-lc","mysql -p$DB_PASS app"]}`,
		// mysql's -p takes its value only in the same word.
		`{"command":["mysql","-p","app"]}`,
		`{"command":["docker","run","-p","3306:3306","mysql:8"]}`,
		`{"command":["docker","run","--name","mysql","-p","3306:3306","mysql"]}`,
		`{"command":["ssh","-p","2222","db.example.com"]}`,
		`{"command":["git","commit","-m","fix mysql -p flag"]}`,
		`{"command":["mysql","-e","select -p1"]}`,
		`{"command":["docker","login","--password-stdin","registry.example"]}`,
		`{"command":["mysql","-p",""]}`,
		`{"command":["",""]}`,
		// Arrays that are no command, and elements of separate arrays.
		`{"items":["Use mysql with","-pSecret for login"]}`,
		`{"files":["src/mysql.go","-p.txt","a-b"]}`,
		`{"a":["mysql"],"b":["-phunter2"]}`,
		`{"command":"mysql","args":["-phunter2"]}`,
		// An array that starts with an option names no program.
		`{"flags":["--password","--help"]}`,
		`{"opts":["-u","root","--password","--verbose"]}`,
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := JSONLBytes([]byte(in + "\n"))
			require.NoError(t, err)
			require.Equal(t, in+"\n", string(got.Bytes()))
		})
	}
}

// Each array is read once, however many of its elements are flags, and the
// quoted forms map back with the password hidden. Timing for large arrays
// lives in BenchmarkJSONLBytes_LongArgvArrays.
func TestJSONLBytes_LongArgvArraysStayLinear(t *testing.T) {
	t.Parallel()
	for _, elem := range []string{`"-phunter2"`, `"-p'x y'"`, `"it's"`, `"a\nb"`, `"machine h login u password pw1 machine h login u password pw2"`} {
		var b strings.Builder
		b.WriteString(`{"command":["mysql"`)
		for range 256 {
			b.WriteString("," + elem)
		}
		b.WriteString("]}\n")
		got, err := JSONLBytes([]byte(b.String()))
		require.NoError(t, err)
		out := string(got.Bytes())
		require.True(t, json.Valid([]byte(out)), elem)
		switch elem {
		case `"-phunter2"`:
			require.NotContains(t, out, "hunter2")
			require.Contains(t, out, "REDACTED")
			require.Equal(t, 256, strings.Count(out, "-pREDACTED"))
		case `"machine h login u password pw1 machine h login u password pw2"`:
			require.NotContains(t, out, "pw1")
			require.NotContains(t, out, "pw2")
			require.Contains(t, out, "REDACTED")
		}
	}
}

func BenchmarkJSONLBytes_LongArgvArrays(b *testing.B) {
	var buf strings.Builder
	buf.WriteString(`{"command":["mysql"`)
	for range 20000 {
		buf.WriteString(`,"-phunter2"`)
	}
	buf.WriteString("]}\n")
	input := []byte(buf.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	for b.Loop() {
		got, err := JSONLBytes(input)
		if err != nil {
			b.Fatal(err)
		}
		if strings.Contains(string(got.Bytes()), "hunter2") {
			b.Fatal("password left visible")
		}
	}
}

func TestAppendArgvWord(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg        string
		quoteValue bool
		word       string
	}{
		{"-phunter2", false, "-phunter2"},
		{"pass:x@y%z+=,./_", false, "'pass:x@y%z+=,./_'"},
		{"", false, "''"},
		{"two words", false, "'two words'"},
		{"it's", false, `"it's"`},
		{`it's"x"`, false, `it\'s\"x\"`},
		{`it's "x"`, false, `'it'\''s "x"'`},
		{"a\nb'c\\", false, `a0b\'c\\`},
		// An option keeps its name bare when its value needs quotes.
		{"--password= hunter", false, "--password=' hunter'"},
		{"-phunter 2", false, "-phunter' 2'"},
		{"-p'x'", false, `-p"'x'"`},
		{"--", false, "--"},
		{"-'x'", false, `"-'x'"`},
		{"instead", true, "'instead'"},
		{"-x\ny", false, "-x0y"},
		// A .netrc password starting with a dash and containing a blank is
		// quoted whole when quoteValue is set, not split as an option prefix.
		{"-hunter2 Summer2026", true, "'-hunter2 Summer2026'"},
	}
	for _, tc := range cases {
		word, pos := appendArgvWord(nil, tc.arg, tc.quoteValue, true)
		require.Equal(t, tc.word, string(word), tc.arg)
		require.Len(t, pos, len(tc.arg), tc.arg)
		for i, p := range pos {
			// Each byte of the element is where the word writes it, after
			// the backslash or quotes that escape it.
			require.Equal(t, argvWordByte(tc.arg[i]), word[p], tc.arg)
			if i > 0 {
				require.Greater(t, p, pos[i-1], tc.arg)
			}
		}
	}

	// at(2) records the apostrophe itself in '\'' , the third byte of the
	// four-byte escape. A looser check that only looks at word[pos[i]] still
	// passes if at(2) is changed to at(0), because that points at the
	// closing quote, also an apostrophe.
	word, pos := appendArgvWord(nil, `it's "x"`, false, true)
	require.Equal(t, `'it'\''s "x"'`, string(word))
	require.Equal(t, []int{1, 2, 5, 7, 8, 9, 10, 11}, pos)
}
