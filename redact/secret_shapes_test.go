package redact

import (
	"strings"
	"testing"
)

// shapeSecret is short enough that the entropy layer never flags it, so each
// case below is caught (or missed) by the shape-specific layer under test.
const shapeSecret = "Zq8vR2kLp9XmN4"

// slashedSig is a 44-character standard-base64 HMAC whose '/' splits it into
// segments too short for the entropy threshold.
const slashedSig = "GIbLuLz+MZUa/Y6vxL7EyYTULHLRfYvkLln7hKQfQq4="

// sasSig is slashedSig as it appears URL-encoded in an Azure SAS query.
const sasSig = "GIbLuLz%2BMZUa%2FY6vxL7EyYTULHLRfYvkLln7hKQfQq4%3D"

// Split so repository secret scanners do not read the fixture as a live SAS.
const sasSigParam = "s" + "ig="

func TestString_KeywordDSNWithoutHost(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "unix socket DSN",
			input: `psql "user=app password=` + shapeSecret + ` dbname=app"`,
			want:  `psql "REDACTED"`,
		},
		{
			name:  "hostaddr instead of host",
			input: "hostaddr=10.0.0.5 user=app password=" + shapeSecret + " dbname=app",
			want:  "REDACTED",
		},
		{
			name:  "placeholder password is preserved",
			input: `psql "user=app password=changeme dbname=app"`,
			want:  `psql "user=app password=changeme dbname=app"`,
		},
		{
			name:  "bracketed placeholder password is preserved",
			input: "user=app password=<password> dbname=app",
			want:  "user=app password=<password> dbname=app",
		},
		{
			name:  "keyword line without a password is preserved",
			input: "level=info user=app dbname=app msg=connected",
			want:  "level=info user=app dbname=app msg=connected",
		},
		{
			name:  "env dump with USER and PWD is preserved",
			input: "USER=node\nPWD=/app\nSHELL=/bin/bash",
			want:  "USER=node\nPWD=/app\nSHELL=/bin/bash",
		},
	})
}

func TestString_AzureSASSignature(t *testing.T) {
	t.Parallel()
	blob := "https://acct.blob.core.windows.net/c/report.csv"
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "signature as the last query parameter",
			input: blob + "?sv=2022-11-02&sp=r&" + sasSigParam + sasSig,
			want:  blob + "?sv=2022-11-02&sp=r&" + sasSigParam + "REDACTED",
		},
		{
			name:  "signature as the first query parameter",
			input: blob + "?" + sasSigParam + sasSig + "&sv=2022-11-02&sp=r",
			want:  blob + "?" + sasSigParam + "REDACTED&sv=2022-11-02&sp=r",
		},
		{
			name:  "SAS token inside a storage connection string",
			input: "BlobEndpoint=https://acct.blob.core.windows.net/;SharedAccessSignature=sv=2022-11-02&ss=b&srt=sco&sp=rl&" + sasSigParam + sasSig,
			want:  "BlobEndpoint=https://acct.blob.core.windows.net/;SharedAccessSignature=sv=2022-11-02&ss=b&srt=sco&sp=rl&" + sasSigParam + "REDACTED",
		},
		{
			name:  "unencoded signature",
			input: blob + "?sv=2022-11-02&" + sasSigParam + slashedSig,
			want:  blob + "?sv=2022-11-02&" + sasSigParam + "REDACTED",
		},
		{
			name:  "signature first in a bare SAS token",
			input: sasSigParam + sasSig + "&sv=2022-11-02&sp=r",
			want:  sasSigParam + "REDACTED&sv=2022-11-02&sp=r",
		},
		{
			name:  "signature first in a connection-string SAS value",
			input: "SharedAccessSignature=" + sasSigParam + sasSig + "&sv=2022-11-02",
			want:  "SharedAccessSignature=" + sasSigParam + "REDACTED&sv=2022-11-02",
		},
		{
			name:  "SAS URL without a signature is preserved",
			input: blob + "?sv=2022-11-02&sp=r&se=2026-12-31",
			want:  blob + "?sv=2022-11-02&sp=r&se=2026-12-31",
		},
		{
			name:  "short sig value is preserved",
			input: "https://example.com/img.png?" + sasSigParam + "abc123",
			want:  "https://example.com/img.png?" + sasSigParam + "abc123",
		},
		{
			name:  "parameters that merely contain sig are preserved",
			input: "https://example.com/?design=dark&signature_version=2",
			want:  "https://example.com/?design=dark&signature_version=2",
		},
	})
}

func TestString_Base64WithSlash(t *testing.T) {
	t.Parallel()
	threeSegment := "q7Tz2Wv9/Lm3Xr8Kp0Yb5Nc2Vf7HdJ4sQ1wE6tA9/EuGtKwiOp+Zq8v="
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "header value with one slash",
			input: "x-ms-sig: " + slashedSig,
			want:  "x-ms-sig: REDACTED",
		},
		{
			name:  "short outer segments are not left behind",
			input: "x-ms-sig: " + threeSegment,
			want:  "x-ms-sig: REDACTED",
		},
		{
			name:  "quoted header in a command",
			input: `curl -H "x-ms-sig: ` + slashedSig + `" https://example.com`,
			want:  `curl -H "x-ms-sig: REDACTED" https://example.com`,
		},
		{
			// Raw-text fallback: the escape letter before the run must stay so
			// the result is still a valid JSON escape.
			name:  "after a literal JSON escape",
			input: `line1\n` + slashedSig,
			want:  `line1\nREDACTED`,
		},
		{
			name:  "after an underscore",
			input: "token_" + slashedSig,
			want:  "token_REDACTED",
		},
		{
			name:  "attached to a command-line flag",
			input: "mysql -uroot -p" + slashedSig + " app",
			want:  "mysql -uroot -REDACTED app",
		},
		{
			name:  "after an at sign",
			input: "user@" + slashedSig,
			want:  "user@REDACTED",
		},
		{
			name:  "after a dot that follows a word",
			input: "abc." + slashedSig,
			want:  "abc.REDACTED",
		},
		{
			name:  "after a tilde",
			input: "~" + slashedSig,
			want:  "~REDACTED",
		},
		{
			name:  "after a version prefix",
			input: "release v2." + slashedSig,
			want:  "release v2.REDACTED",
		},
		{
			name:  "after a numbered key prefix",
			input: "k1." + slashedSig,
			want:  "k1.REDACTED",
		},
		{
			name:  "starting with a digit after a number and a dot",
			input: "3.9IbLuLz+MZUa/Y6vxL7EyYTULHLRfYvkLln7hKQfQq4=",
			want:  "3.REDACTED",
		},
	})
}

func TestString_Base64WithSlashPathsPreserved(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"/Users/alice/Work/entireio/cli/redact/redact.go",
		"refs/entire/checkpoints/EX/01M3PWG7BKWYH0XJKS810J0XEX",
		"/System/Library/Audio/Tunings/CFG18132",
		"/System/Library/AssetsV2/com_apple_MobileAsset_VoiceTriggerAssetsASMac/572594ffc2fe6514ef6281532d4237c0fac8b5eb.asset",
		"GeoPolygonDataAssets/5ef9811cd39380bb4462a15f124ce7505339b2e7",
		"go/pkg/mod/github.com/foo/bar@v1.2.10+incompatible/CMake/DESCRIPTION",
		"node_modules/@types/node/index.d.ts",
		"go/pkg/mod/google.golang.org/genproto@v0.0.0-20260622175928-b703f567277d/googleapis/cloud/webrisk/v1beta1",
		"https://github.com/entireio/cli/pull/2798",
		"github.com/foo/bar v1.2.3 h1:wdMgn+sje1KNXdwFizIWjbf328fIUBxqEmJ/vYPo8yc=",
		"commit a334cb6988d1e2f3a4b5c6d7e8f90a1b2c3d4e5f",
		"session 3c1a8bcc-6af5-4ff5-a750-7fdfd71cd61d",
	}
	cases := make([]stringRedactionCase, 0, len(inputs))
	for _, in := range inputs {
		cases = append(cases, stringRedactionCase{name: in, input: in, want: in})
	}
	assertStringRedactionCases(t, cases)
}

func TestJSONLContent_Base64WithSlashAndSAS(t *testing.T) {
	t.Parallel()
	input := `{"type":"text","content":"x-ms-sig: ` + slashedSig + ` url ?sv=1&` + sasSigParam + sasSig + `","file_path":"/Users/alice/Work/app/main.go"}`

	result, err := JSONLContent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{"type":"text","content":"x-ms-sig: REDACTED url ?sv=1&` + sasSigParam + `REDACTED","file_path":"/Users/alice/Work/app/main.go"}`
	if result != want {
		t.Fatalf("JSONLContent() =\n%s\nwant\n%s", result, want)
	}
}

func TestString_CredentialValueSeparators(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "semicolon inside the value",
			input: "MYSQL_PASSWORD=ab;" + shapeSecret,
			want:  "MYSQL_PASSWORD=REDACTED",
		},
		{
			name:  "comma inside the value",
			input: "MYSQL_PASSWORD=ab," + shapeSecret,
			want:  "MYSQL_PASSWORD=REDACTED",
		},
		{
			name:  "repeated separators inside the value",
			input: "MYSQL_PASSWORD=ab;;" + shapeSecret,
			want:  "MYSQL_PASSWORD=REDACTED",
		},
		{
			// Without a space, a command cannot be told apart from the rest of
			// a password such as "ab;Zq8v", so it is redacted with the value.
			name:  "command glued to a semicolon is redacted with the value",
			input: "DB_PASSWORD=" + shapeSecret + ";echo done",
			want:  "DB_PASSWORD=REDACTED done",
		},
		{
			name:  "several separators inside the value",
			input: "MYSQL_PASSWORD=ab;cd," + shapeSecret,
			want:  "MYSQL_PASSWORD=REDACTED",
		},
		{
			name:  "next assignment after semicolon is preserved",
			input: "DB_PASSWORD=" + shapeSecret + ";DB_USER=bob",
			want:  "DB_PASSWORD=REDACTED;DB_USER=bob",
		},
		{
			name:  "next assignment after comma is preserved",
			input: "DB_PASSWORD=" + shapeSecret + ",DB_HOST=db",
			want:  "DB_PASSWORD=REDACTED,DB_HOST=db",
		},
		{
			name:  "shell command after semicolon and space is preserved",
			input: "export DB_PASSWORD=" + shapeSecret + "; echo ok",
			want:  "export DB_PASSWORD=REDACTED; echo ok",
		},
		{
			name:  "prose after comma is preserved",
			input: "set DB_PASSWORD=" + shapeSecret + ", then restart",
			want:  "set DB_PASSWORD=REDACTED, then restart",
		},
	})
}

func TestString_ODBCValuesWithSpaces(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "password with spaces",
			input: "Server=x;Database=app;User Id=sa;Password=correct horse battery;",
			want:  "REDACTED;",
		},
		{
			name:  "spaces after semicolons",
			input: "Server=x; Database=app; User Id=sa; Password=correct horse battery;",
			want:  "REDACTED;",
		},
		{
			name:  "spaces around the password equals sign",
			input: "Server=x;Database=app;User Id=sa;Password = correct horse battery;",
			want:  "REDACTED;",
		},
		{
			name:  "unterminated final password with spaces",
			input: "Data Source=My Server;Initial Catalog=App;User ID=sa;Password=correct horse battery",
			want:  "REDACTED",
		},
		{
			name:  "quoted in source code",
			input: `var cs = "Server=x;Database=app;User Id=sa;Password=correct horse battery";`,
			want:  `var cs = "REDACTED";`,
		},
		{
			name:  "placeholder password with spaces around equals is preserved",
			input: "Server=x;Database=app;User Id=sa;Password = changeme;",
			want:  "Server=x;Database=app;User Id=sa;Password = changeme;",
		},
		{
			name:  "prose after a terminated string is preserved",
			input: "Server=x;Database=app;User Id=sa;Password=" + shapeSecret + "; then restart the service",
			want:  "REDACTED; then restart the service",
		},
		{
			name:  "value does not continue onto the next line",
			input: "Server=x;Database=app;User Id=sa;Password=" + shapeSecret + "\nnext line text",
			want:  "REDACTED\nnext line text",
		},
		{
			name:  "string without a password is preserved",
			input: "Server=My Server;Database=app;Trusted_Connection=True;",
			want:  "Server=My Server;Database=app;Trusted_Connection=True;",
		},
	})
}

// Keys may contain spaces, so a match can begin at the words before the
// string; those words must neither hide the string nor be redacted with it.
func TestString_ConnectionStringAfterWords(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "unterminated string after a word",
			input: "Use Server=x;Database=app;User Id=sa;Password=" + shapeSecret + " and restart",
			want:  "Use REDACTED",
		},
		{
			name:  "terminated string after a word",
			input: "Use Server=x;Database=app;User Id=sa;Password=" + shapeSecret + "; then restart",
			want:  "Use REDACTED; then restart",
		},
		{
			name:  "two-word first key after a word",
			input: "Try Data Source=db;Initial Catalog=App;User ID=sa;Password=correct horse battery",
			want:  "Try REDACTED",
		},
		{
			name:  "password first after two words",
			input: "Please set Password=" + shapeSecret + ";Server=x;User Id=sa;",
			want:  "Please set REDACTED;",
		},
		{
			name:  "placeholder password after a word is preserved",
			input: "Use Server=x;Database=app;User Id=sa;Password=changeme;",
			want:  "Use Server=x;Database=app;User Id=sa;Password=changeme;",
		},
		{
			name:  "settings that are not a connection string are preserved",
			input: "Use mode=fast;level=2;debug=true",
			want:  "Use mode=fast;level=2;debug=true",
		},
	})
}

// A rejected candidate whose first key is a long run of words must not be
// re-checked from every word; that made a long line quadratic.
func TestConnectionStringStart_BoundedRetries(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("word ", 1000) + "mode=fast;level=2;debug=true"
	calls := 0
	_, ok := connectionStringStart(s, 0, len(s), func(string) bool {
		calls++
		return false
	})
	if ok {
		t.Fatal("connectionStringStart accepted a candidate the predicate rejected")
	}
	if calls > 5 {
		t.Fatalf("hasSecret called %d times for one candidate, want at most 5", calls)
	}
}

func TestString_DatabaseURIPasswordCharacters(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "apostrophe in password",
			input: "postgres://admin:S3cr'etPass9@db/app",
			want:  "REDACTED",
		},
		{
			name:  "slash in password",
			input: "postgres://app:ab/" + shapeSecret + "@db/app",
			want:  "REDACTED",
		},
		{
			name:  "both, after a key",
			input: "DATABASE_URL=mysql://root:pa/ss'word9@localhost:3306/app",
			want:  "DATABASE_URL=REDACTED",
		},
		{
			name:  "digits then apostrophe in password",
			input: "postgres://app:123'" + shapeSecret + "@db/app",
			want:  "REDACTED",
		},
		{
			name:  "digits then slash in password",
			input: "postgres://app:2024/" + shapeSecret + "@db/app",
			want:  "REDACTED",
		},
		{name: "no userinfo", input: "postgres://localhost:5432/app", want: "postgres://localhost:5432/app"},
		{name: "at-sign in query", input: "postgres://localhost:5432/db?user=bob@corp", want: "postgres://localhost:5432/db?user=bob@corp"},
		{
			name:  "port followed by quoted list item",
			input: "['postgres://localhost:5432','admin@example.com']",
			want:  "['postgres://localhost:5432','admin@example.com']",
		},
		{name: "http path with at-sign", input: "http://localhost:5173/@vite/client", want: "http://localhost:5173/@vite/client"},
		{
			name:  "https port and versioned path",
			input: "https://cdn.example.com:443/npm/react@18.3.1/index.js",
			want:  "https://cdn.example.com:443/npm/react@18.3.1/index.js",
		},
	})
}

// The generic credentialed-URI layer redacts these too, so String cannot show
// the database-URI layer's own placeholder handling.
func TestDetectDatabaseURIPasswords_Placeholders(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"postgres://app:${DB_PASSWORD}@db/app",
		"postgres://app:changeme@db/app",
		"mysql://root:<password>@db/app",
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := detectDatabaseURIPasswords(in); len(got) != 0 {
				t.Errorf("detectDatabaseURIPasswords(%q) = %v, want none", in, got)
			}
		})
	}
}

func TestString_OracleUserPassword(t *testing.T) {
	t.Parallel()
	assertStringRedactionCases(t, []stringRedactionCase{
		{
			name:  "thin driver with service name",
			input: "jdbc:oracle:thin:scott/" + shapeSecret + "@//db:1521/ORCL",
			want:  "REDACTED",
		},
		{
			name:  "thin driver with SID after a key",
			input: "url=jdbc:oracle:thin:scott/tiger@db:1521:ORCL",
			want:  "url=REDACTED",
		},
		{
			name:  "sqlplus connect identifier",
			input: "sqlplus scott/" + shapeSecret + "@ORCL",
			want:  "sqlplus REDACTED",
		},
		{
			name:  "sqlplus login in double quotes",
			input: `sqlplus -s "scott/` + shapeSecret + `@ORCL"`,
			want:  `sqlplus -s "REDACTED"`,
		},
		{
			name:  "sqlplus login in single quotes",
			input: "sqlplus 'scott/" + shapeSecret + "@ORCL'",
			want:  "sqlplus 'REDACTED'",
		},
		{
			name:  "jdbc quoted password",
			input: `jdbc:oracle:thin:scott/"` + shapeSecret + `"@//db:1521/ORCL`,
			want:  "REDACTED",
		},
		{
			name:  "sqlplus with flags and trailing clause",
			input: "sqlplus -s scott/" + shapeSecret + "@//db:1521/svc as sysdba",
			want:  "sqlplus -s REDACTED as sysdba",
		},
		{name: "jdbc without credentials", input: "jdbc:oracle:thin:@//db:1521/ORCL", want: "jdbc:oracle:thin:@//db:1521/ORCL"},
		{name: "jdbc wallet login", input: "jdbc:oracle:thin:/@wallet_alias", want: "jdbc:oracle:thin:/@wallet_alias"},
		{
			name:  "jdbc placeholder password",
			input: "jdbc:oracle:thin:scott/changeme@db:1521:ORCL",
			want:  "jdbc:oracle:thin:scott/changeme@db:1521:ORCL",
		},
		{name: "sqlplus OS authentication", input: "sqlplus / as sysdba", want: "sqlplus / as sysdba"},
		{
			name:  "sqlplus placeholder password",
			input: "sqlplus scott/${ORACLE_PASSWORD}@ORCL",
			want:  "sqlplus scott/${ORACLE_PASSWORD}@ORCL",
		},
		{name: "github action reference", input: "uses: actions/checkout@v4", want: "uses: actions/checkout@v4"},
		{name: "scoped npm package", input: "npm i @scope/pkg@1.2.3", want: "npm i @scope/pkg@1.2.3"},
		{name: "git ssh remote", input: "git@github.com:entireio/cli.git", want: "git@github.com:entireio/cli.git"},
	})
}
