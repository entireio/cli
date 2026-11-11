package redact

import (
	"strings"
	"testing"
)

// assertStringRedactionCasesWithoutScanners checks each case with only the
// key/value rules on, then restores the default scanners. The scanner
// configuration is process-wide, so the cases run one at a time.
func assertStringRedactionCasesWithoutScanners(t *testing.T, tests []stringRedactionCase) {
	t.Helper()
	t.Cleanup(func() {
		if err := ConfigureScanners(ScannersConfig{Betterleaks: true}); err != nil {
			t.Fatalf("restore default scanners: %v", err)
		}
	})
	if err := ConfigureScanners(ScannersConfig{}); err != nil {
		t.Fatalf("ConfigureScanners(none): %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := String(tt.input); got != tt.want {
				t.Errorf("String(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// A default on a positional or special parameter, an indirect reference or an
// array element is a value like the default in ${DB_PASSWORD:-hunter2}, and
// the key/value and URI rules must catch it with the scanners off. A single
// quoted default outside double quotes is literal text, nested ones included.
// Not parallel: scanner selection is process-global.
func TestString_ShellParameterDefaultsCaughtWithoutScanners(t *testing.T) {
	var cases []stringRedactionCase
	for _, value := range []string{
		"${1:-hunter2}", "${2:-x}", "${10-hunter2}", "${1:+hunter2}",
		"${@:-hunter2}", "${#:-hunter2}", "${--hunter2}", "${!ref:-hunter2}",
		"${pw[0]:-hunter2}", "${pw[idx[0]]:-hunter2}", "${pw[${idx[0]}]:-hunter2}",
		"${pw[$((i+1))]:-hunter2}", "${1/x/hunter2}", "${pw[idx[0]]/x/hunter2}",
		"${PW:-'$S3CRET'}", "${1:-'$S3CRET'}", "${A:-${B:-'$S3CRET'}}",
	} {
		cases = append(cases, stringRedactionCase{
			name:  value,
			input: "DB_PASSWORD=" + value,
			want:  "DB_PASSWORD=REDACTED",
		})
	}
	for input, want := range map[string]string{
		`export PGPASSWORD="${2:-s3cr3t}"`:                   `export PGPASSWORD="REDACTED"`,
		"API_KEY=${1:-hunter2}":                              "API_KEY=REDACTED",
		`API_KEY=${K:-'$S3CRET'}`:                            "API_KEY=REDACTED",
		"postgres://app:${1:-hunter2}@db/app":                "REDACTED",
		`postgres://app:${PW:-'$S3CRET'}@db.example.com/app`: "REDACTED",
		`DB_PASSWORD="${PW:-'hunter2'}"`:                     `DB_PASSWORD="REDACTED"`,
		`echo "DB_PASSWORD=${PW:-hunter2}"`:                  `echo "DB_PASSWORD=REDACTED`,
		// Quotes that close a string, are escaped, or sit in single quotes
		// leave the default outside double quotes.
		`psql ""postgres://app:${PW:-'$S3CRET'}@db/app`:                     `psql ""REDACTED`,
		`psql ""postgres://app:${PW:-'$S3CRET'}@db/app"`:                    `psql ""REDACTED"`,
		`psql "x"postgres://app:${PW:-'$S3CRET'}@db/app`:                    `psql "x"REDACTED`,
		`psql "a"postgres://app:${PW:-'$S3CRET'}@db/app "b"`:                `psql "a"REDACTED "b"`,
		`echo "a"DB_PASSWORD=${PW:-'$S3CRET'} b"`:                           `echo "a"DB_PASSWORD=REDACTED b"`,
		`echo "a DB_PASSWORD="${PW:-'$S3CRET'}"`:                            `echo "a DB_PASSWORD="REDACTED"`,
		`psql \"postgres://app:${PW:-'$S3CRET'}@db/app\"`:                   `psql \"REDACTED"`,
		`printf '%s\n' \"postgres://app:${PW:-'$S3CRET'}@db/app\"`:          `printf '%s\n' \"REDACTED"`,
		`echo \"DB_PASSWORD=${PW:-'$S3CRET'} more\"`:                        `echo \"DB_PASSWORD=REDACTED more\"`,
		`echo 'a"b' DB_PASSWORD=${PW:-'$S3CRET'}`:                           `echo 'a"b' DB_PASSWORD=REDACTED`,
		"echo \"a\nx \"DB_PASSWORD=${PW:-'$S3CRET'}\" y":                    "echo \"a\nx \"DB_PASSWORD=REDACTED y",
		"# note: \"\nDB_PASSWORD=${PW:-'$S3CRET'} printenv \"DB_PASSWORD\"": "# note: \"\nDB_PASSWORD=REDACTED printenv \"DB_PASSWORD\"",
		// The quoting is read from the start of the text, however long.
		strings.Repeat("echo ok\n", 200) + "echo \"a\nx \"DB_PASSWORD=${PW:-'$S3CRET'}\" y":                   strings.Repeat("echo ok\n", 200) + "echo \"a\nx \"DB_PASSWORD=REDACTED y",
		"echo \"" + strings.Repeat("a", 1100) + "\nx\" DB_PASSWORD=${PW:-'$S3CRET'} printenv \"DB_PASSWORD\"": "echo \"" + strings.Repeat("a", 1100) + "\nx\" DB_PASSWORD=REDACTED printenv \"DB_PASSWORD\"",
		strings.Repeat("echo ok\n", 200) + "echo \"a\nx \"postgres://app:${PW:-'$S3CRET'}@db/app \"y\"":       strings.Repeat("echo ok\n", 200) + "echo \"a\nx \"REDACTED \"y\"",
		`printf '%s' ok; DB_PASSWORD=${PW:-'$S3CRET'}`:                                                        `printf '%s' ok; DB_PASSWORD=REDACTED`,
		// An escaped blank does not end a word, so # after it starts no comment.
		// The value is the whole word ${PW:-'$S3CRET'}" y".
		"echo foo\\ #\"a\nx \"DB_PASSWORD=${PW:-'$S3CRET'}\" y\"": "echo foo\\ #\"a\nx \"DB_PASSWORD=REDACTED",
		// A here-document body is text: its quote opens no string.
		"cat <<EOF\nsay \"hi\nEOF\nDB_PASSWORD=${PW:-'$S3CRET'} cmd \"x\"": "cat <<EOF\nsay \"hi\nEOF\nDB_PASSWORD=REDACTED cmd \"x\"",
		// A line continuation leaves a word start as it was, so # after it
		// starts a comment.
		"echo ok; \\\n# \"\nDB_PASSWORD=${PW:-'$S3CRET'} cmd \"x\"":                  "echo ok; \\\n# \"\nDB_PASSWORD=REDACTED cmd \"x\"",
		"echo \\\n# note: \"\nDB_PASSWORD=${PW:-'$S3CRET'} printenv \"DB_PASSWORD\"": "echo \\\n# note: \"\nDB_PASSWORD=REDACTED printenv \"DB_PASSWORD\"",
		// An unquoted here-document delimiter joins continued body lines.
		"cat <<EOF\nfoo\\\nEOF\n\"\nEOF\nDB_PASSWORD=${PW:-'$S3CRET'} cmd \"x\"": "cat <<EOF\nfoo\\\nEOF\n\"\nEOF\nDB_PASSWORD=REDACTED cmd \"x\"",
		// case is a reserved word only where a command starts.
		`echo "$(echo case)" DB_PASSWORD=${PW:-'$S3CRET'} cmd "x"`:                   `echo "$(echo case)" DB_PASSWORD=REDACTED cmd "x"`,
		`echo "$(printf case)"; DB_PASSWORD=${PW:-'$S3CRET'} printenv "DB_PASSWORD"`: `echo "$(printf case)"; DB_PASSWORD=REDACTED printenv "DB_PASSWORD"`,
		`echo "$(echo case)"; psql postgres://app:${PW:-'$S3CRET'}@db/app "x"`:       `echo "$(echo case)"; psql REDACTED "x"`,
		// A command substitution starts a new quoting context.
		`echo "$(DB_PASSWORD=${PW:-'$S3CRET'} env)"`:  `echo "$(DB_PASSWORD=REDACTED env)"`,
		"echo \"`DB_PASSWORD=${PW:-'$S3CRET'} env`\"": "echo \"`DB_PASSWORD=REDACTED env`\"",
		// A case pattern's ) does not end the substitution.
		`echo "$(case x in x) DB_PASSWORD=${PW:-'$S3CRET'} env;; esac)"`:             `echo "$(case x in x) DB_PASSWORD=REDACTED env;; esac)"`,
		`echo "$(case x in x) psql postgres://app:${PW:-'$S3CRET'}@db/app ;; esac)"`: `echo "$(case x in x) psql REDACTED ;; esac)"`,
	} {
		cases = append(cases, stringRedactionCase{name: input, input: input, want: want})
	}
	assertStringRedactionCasesWithoutScanners(t, cases)
}

// Shell expansions on positional and special parameters, indirect references
// and array elements are references like ${DB_PASSWORD}: only a default or
// other text inside them is a secret.
func TestString_ShellParameterReferencesPreserved(t *testing.T) {
	t.Parallel()
	var cases []stringRedactionCase
	for _, value := range []string{
		"${1}", "${10}", "${1:-}", "${1-}", "${1:-$DEFAULT_PW}", "${2:-${FALLBACK_PASSWORD}}",
		"${@}", "${*}", "${#}", "${?}", "${-}", "${$}", "${!}", "${0}",
		"${#1}", "${##}", "${#@}", "${#pw[@]}",
		"${!ref}", "${!pre*}", "${!pw[@]}", "${pw[0]}", "${pw[@]}",
		"${pw[idx[0]]}", "${pw[${idx[0]}]}", "${#pw[idx[0]]}", "${pw[idx[0]]:-$DEFAULT_PW}",
		"${1:-''}", "${PW:-'changeme'}", "${PW:-'<password>'}",
		// Empty quoted pieces around a reference leave it a reference.
		"${PW:-''$FALLBACK''}",
	} {
		cases = append(cases, stringRedactionCase{
			name:  value,
			input: "DB_PASSWORD=" + value,
			want:  "DB_PASSWORD=" + value,
		})
	}
	assertStringRedactionCases(t, cases)
}

// Inside double quotes a single quote is a plain character, so the default in
// "${PW:-'$FALLBACK'}" expands $FALLBACK, as it does nested, in a URI, in a
// value that a double-quoted string holds with more text after it, or after
// other quoted words; only the unquoted form is the literal text $FALLBACK.
func TestString_DoubleQuotedShellDefaultsPreserved(t *testing.T) {
	t.Parallel()
	var cases []stringRedactionCase
	for _, input := range []string{
		`DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		`API_KEY="${KEY:-'$FALLBACK'}"`,
		`PGPASSWORD="${1:-'$FALLBACK'}" psql -h db`,
		`echo "DB_PASSWORD=${PW:-'$FALLBACK'}" done`,
		`echo "DB_PASSWORD=${PW:-'$FALLBACK'} more"`,
		`echo "x" "DB_PASSWORD=${PW:-'$FALLBACK'}"`,
		`echo "API_KEY=${KEY:-'$FALLBACK'} x"`,
		`DB_PASSWORD="${PW:-${FALLBACK:-'$DEFAULT_PASSWORD'}}"`,
		`DB_PASSWORD="${A:-${B:-'$FALLBACK'}}"`,
		`psql "postgres://app:${PW:-${F:-'$X'}}@db/app"`,
		`echo 'it' "DB_PASSWORD=${PW:-'$FALLBACK'}"`,
		`echo "x" 'y' "DB_PASSWORD=${PW:-'$FALLBACK'}" "z"`,
		`echo "DB_PASSWORD=${PW}" "z"`,
		`echo "API_KEY=$KEY" "--flag"`,
		strings.Repeat("echo ok\n", 200) + `echo "API_KEY=$KEY" "--flag"`,
		`printf '%s\n' "postgres://app:${PW:-'$FALLBACK'}@db/app"`,
		"echo \"a\nDB_PASSWORD=${PW:-'$FALLBACK'} b\"",
		"# don't\nDB_PASSWORD=\"${PW:-'$FALLBACK'}\"",
		// The quoting is read from the start of the text, however long.
		strings.Repeat("echo ok\n", 200) + `DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		"echo \"a\n" + strings.Repeat("b\n", 600) + "DB_PASSWORD=${PW:-'$FALLBACK'} c\"",
		`printf '%s' ok; DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		// Closed $'...' strings and command substitutions leave the quoting
		// after them as it was.
		`printf $'ok'; DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		`printf $'it\'s'; DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		`echo $(date); DB_PASSWORD="${PW:-'$FALLBACK'}"`,
		"echo `date`; DB_PASSWORD=\"${PW:-'$FALLBACK'}\"",
		`echo "$(echo "DB_PASSWORD=${PW:-'$FALLBACK'}" )"`,
		`echo $((1<<2)) "DB_PASSWORD=${PW:-'$FALLBACK'}"`,
		"(( n << 2 ))\nDB_PASSWORD=\"${PW:-'$FALLBACK'}\"",
		`echo "$(case $1 in a) echo "DB_PASSWORD=${PW:-'$FALLBACK'}" ;; esac)"`,
		`echo "$(if true; then case $1 in a) echo hi ;; esac; fi) DB_PASSWORD=${PW:-'$FALLBACK'}"`,
		"cat <<$'EOF'\nx\nEOF\nDB_PASSWORD=\"${PW:-'$FALLBACK'}\"",
		// A here-document body's apostrophe opens no single quoted string.
		"git commit -m \"$(cat <<'EOF'\nIt's done\nEOF\n)\" && DB_PASSWORD=\"${PW:-'$FALLBACK'}\" ./run",
		`echo "API_KEY=${KEY:-'$FALLBACK'}"`,
		`psql "postgres://app:${PW:-'$FALLBACK'}@db/app"`,
		`echo "DB_PASSWORD=${PW}"`,
		`echo "DB_PASSWORD=${PW:-}"`,
		`echo "DB_PASSWORD=${1:-$FALLBACK}"`,
	} {
		cases = append(cases, stringRedactionCase{name: input, input: input, want: input})
	}
	assertStringRedactionCases(t, cases)
}

func TestIsPlaceholderShellExpansion_ParameterForms(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"${1}":                   true,
		"${#1}":                  true,
		"${##}":                  true,
		"${!ref}":                true,
		"${pw[idx[0]]}":          true,
		"${pw[${idx[0]}]}":       true,
		"${1:-hunter2}":          false,
		"${#:-hunter2}":          false,
		"${pw[idx[0]]:-hunter2}": false,
		"${PW:-'$S3CRET'}":       false,
		"${PW:-'changeme'}":      true,
		"${PW:-$FALLBACK}":       true,
		"${PW:-''$FALLBACK''}":   true,
		"${PW:-'a'$X'b'}":        false,
		// A subscript nested two levels deep is not read as a name, as before.
		"${pw[a[b[0]]]:-hunter2}": true,
	} {
		if got := isPlaceholderShellExpansion(value); got != want {
			t.Errorf("isPlaceholderShellExpansion(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestDoubleQuotedText(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"${PW:-'$FALLBACK'}": "${PW:-$FALLBACK}",
		"${1:-'$FALLBACK'}":  "${1:-$FALLBACK}",
		"${PW:-$FALLBACK}":   "${PW:-$FALLBACK}",
		"${PW:-'a'b}":        "${PW:-'a'b}",
		"'$FALLBACK'":        "'$FALLBACK'",
		"${A}${B:-'x'}":      "${A}${B:-x}",
		"${PW:-${F:-'$X'}}":  "${PW:-${F:-$X}}",
	} {
		if got := doubleQuotedText(text); got != want {
			t.Errorf("doubleQuotedText(%q) = %q, want %q", text, got, want)
		}
	}
}

// Each prefix is followed by a value; the reader reports whether the shell
// reads that value inside double quotes.
func TestShellQuotes_DoubleQuotedAt(t *testing.T) {
	t.Parallel()
	for prefix, want := range map[string]bool{
		`echo "`:            true,
		`echo "a" "`:        true,
		`echo 'it' "`:       true,
		`echo 'a"b' `:       false,
		`echo \"`:           false,
		`echo "x\"y `:       true,
		"echo \"a\nb\" \"":  true,
		"echo \"a\nx \"":    false,
		`echo 'unclosed "`:  false,
		`echo "trailing \`:  false,
		`DB_PASSWORD=`:      false,
		"# note: \"\n":      false,
		"# don't\nx=\"":     true,
		"echo $# \"":        true,
		`echo a#"`:          true,
		`echo foo\ #"`:      true,
		`echo $'it\'s' "`:   true,
		`printf $'ok'; x="`: true,
		`echo $'unclosed "`: false,
		// A command substitution is a quoting context of its own.
		`echo "$(`:                        false,
		"echo \"`":                        false,
		`echo "$(echo "`:                  true,
		`echo "$(echo "a") `:              true,
		`echo $(date) "`:                  true,
		"echo `date` \"":                  true,
		"echo \"`echo \"a\"` ":            true,
		`echo "$(( (1+2) * 3 )) `:         true,
		`echo $((1<<2)) "`:                true,
		`echo "$(case x in (a) ;; esac) `: true,
		`echo "$(case x in a) echo "`:     true,
		`echo "$(case x in a) case y in b) ;; esac ;; esac) "`: false,
		"(( x << 2 ))\nx=\"": true,
		`echo "$(esac) `:     true,
		// case and esac are reserved words only where a command starts.
		`echo "$(echo case) `:                    true,
		`echo "$(if x; then case y in a) echo "`: true,
		`echo "$(x && case y in a) ;; esac) `:    true,
		// A line continuation leaves a word start as it was.
		"echo ok; \\\n# \"\n": false,
		"echo \\\n# \"\n":     false,
		`echo \#"`:            true,
		// Here-document bodies are text, not shell quoting.
		"cat <<EOF\nsay \"hi\nEOF\n":                                 false,
		"cat <<'EOF'\nit's\nEOF\nx=\"":                               true,
		"cat <<-\"EOF\"\n\tit's\n\tEOF\nx=\"":                        true,
		"cat <<A <<B\n\"\nA\n'\nB\nx=\"":                             true,
		"cat <<EOF # it's\n'\nEOF\nx=\"":                             true,
		"git commit -m \"$(cat <<'EOF'\nIt's done\nEOF\n)\" && x=\"": true,
		"cat <<EOF\nnever closed \"\n":                               false,
		"cat <<<\"x\" \"":                                            true,
		"cat <<EOF\nfoo\\\nEOF\n\"\nEOF\nx=\"":                       true,
		"cat <<'EOF'\nfoo\\\nEOF\nx=\"":                              true,
		"cat <<$'EOF'\nit's\nEOF\nx=\"":                              true,
		"cat <<$'E\\'F'\nit's\nE'F\nx=\"":                            true,
		"cat <<EOF\r\nit's\r\nEOF\r\nx=\"":                           true,
		"cat <<EOF\nit's\nEOF\r\n\"":                                 false,
	} {
		if got := newShellQuotes(prefix + "v").doubleQuotedAt(len(prefix)); got != want {
			t.Errorf("doubleQuotedAt after %q = %v, want %v", prefix, got, want)
		}
	}
}

// A reader asked about an earlier position reads s again from its start.
func TestShellQuotes_EarlierPositionRereads(t *testing.T) {
	t.Parallel()
	s := `a "b" c "d"`
	quotes := newShellQuotes(s)
	for _, tt := range []struct {
		at   int
		want bool
	}{{9, true}, {3, true}, {6, false}, {6, false}, {9, true}, {0, false}} {
		if got := quotes.doubleQuotedAt(tt.at); got != tt.want {
			t.Errorf("doubleQuotedAt(%d) in %q = %v, want %v", tt.at, s, got, tt.want)
		}
	}
}
