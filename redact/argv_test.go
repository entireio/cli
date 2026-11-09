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
		// fallback, which reads the array the same way.
		{`{"command":["mysql","-phunter2"]}`, `{"command":["mysql","-pREDACTED"]}`},
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
	cases := []struct{ arg, word string }{
		{"-phunter2", "-phunter2"},
		{"pass:x@y%z+=,./_", "'pass:x@y%z+=,./_'"},
		{"", "''"},
		{"two words", "'two words'"},
		{"it's", `"it's"`},
		{`it's"x"`, `it\'s\"x\"`},
		{`it's "x"`, `'it'\''s "x"'`},
		{"a\nb'c\\", `$'a\nb\'c\\'`},
	}
	for _, tc := range cases {
		word, pos := appendArgvWord(nil, tc.arg, true)
		require.Equal(t, tc.word, string(word), tc.arg)
		require.Len(t, pos, len(tc.arg), tc.arg)
		for i, p := range pos {
			// Each byte of the element is where the word writes it, or
			// the start of the escape that writes it.
			switch tc.arg[i] {
			case '\n':
				require.Equal(t, `\n`, string(word[p:p+2]), tc.arg)
			case '\\':
				require.Equal(t, `\\`, string(word[p:p+2]), tc.arg)
			default:
				if word[0] == '$' && tc.arg[i] == '\'' {
					require.Equal(t, `\'`, string(word[p:p+2]), tc.arg)
					continue
				}
				require.Equal(t, tc.arg[i], word[p], tc.arg)
			}
		}
	}
}
