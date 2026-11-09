package redact

import (
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
		// A line break in a password element.
		{`{"command":["mysql","-phunter2\nfoo"]}`, `{"command":["mysql","-pREDACTED"]}`},
		// An element after --password is its value, though it reads as prose.
		{`{"command":["docker","login","--password","instead","registry"]}`, `{"command":["docker","login","--password","REDACTED","registry"]}`},
		// .netrc keywords in any case.
		{`{"command":["echo","MACHINE","host","LOGIN","alice","PASSWORD","hunter2"]}`, `{"command":["echo","MACHINE","host","LOGIN","alice","PASSWORD","REDACTED"]}`},
		// A password that is also an object key leaves the key as it is.
		{`{"type":"x","command":["redis-cli","-a","type"]}`, `{"type":"x","command":["redis-cli","-a","REDACTED"]}`},
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
// quoted forms map back in linear time.
func TestJSONLBytes_LongArgvArraysStayLinear(t *testing.T) {
	t.Parallel()
	for _, elem := range []string{`"-phunter2"`, `"-p'x y'"`, `"it's"`, `"a\nb"`} {
		var b strings.Builder
		b.WriteString(`{"command":["mysql"`)
		for range 20000 {
			b.WriteString("," + elem)
		}
		b.WriteString("]}\n")
		got, err := JSONLBytes([]byte(b.String()))
		require.NoError(t, err)
		if elem == `"-phunter2"` {
			require.NotContains(t, string(got.Bytes()), "hunter2")
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
		{"a\nb'c\\", false, `a_b\'c\\`},
		// An option keeps its name bare when its value needs quotes.
		{"--password= hunter", false, "--password=' hunter'"},
		{"-phunter 2", false, "-phunter' 2'"},
		{"-p'x'", false, `-p"'x'"`},
		{"--", false, "--"},
		{"-'x'", false, `"-'x'"`},
		{"instead", true, "'instead'"},
		{"-x\ny", false, "-x_y"},
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
}
