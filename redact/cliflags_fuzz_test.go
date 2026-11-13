package redact

import (
	"strings"
	"testing"
	"time"
)

// fuzzCommandWords are the words FuzzString_CommandLinePasswordsLinear
// builds command lines from: client names, password flags and sources, the
// shell's quoting, operators, substitutions and expansions, the keywords of
// a .netrc entry, argument lists in code, and the programs that read a
// client's name as an operand.
var fuzzCommandWords = []string{
	"mysql", "mysqldump", "mariadb", "psql", "mongosh", "redis-cli", "docker", "podman", "helm", "login", "registry", "az",
	"openssl", "sshpass", "ssh", "sudo", "env", "env -S", "/usr/bin/env", "exec", "time", "nohup", "eval", "echo", "printf",
	"sh", "bash", "-c", "-lc", "kubectl", "--", "find", "-exec", "go", "run", "tar", "xargs", "case", "in", "esac", "if", "then",
	"-p", "-pX", "-P", "-u", "-h", "-e", "-i", "-o", "-l", "-f", "-a", "-pass", "-passin", "-in", "pass:", "pass:x", "env:", "file:",
	"--password", "--password=", "--passwd", "--pass", "--password-stdin", "--name", "--latest",
	"machine", "default", "login", "password", "account", "#",
	" ", " ", " ", "- ", "\t", "\n", "\r\n", ";", ";;", "|", "&&", "&", "(", ")", "$(", "$((", "<(", "`", "'", "\"", `\"`, `\`,
	"\\\n", "$'", "$", "${", "${PW:-", "}", "{{", "}}", "=", ":", ",", "/", "~/", ".exe", "@", "x", "Secret1",
	"[", "]", `"""`, "'''", "r\"", "subprocess.run([", `", "`, "exec.Command(", "<<", "EOF", "<<<", "2>", ">&",
}

// fuzzLineBytes is about how long the line that
// FuzzString_CommandLinePasswordsLinear repeats its words to is, and
// fuzzLineBudget how long reading it may take, raceSlowdown times that with
// the race detector. Read in linear time, such a line takes at most a few
// hundred milliseconds, and a few seconds with the race detector; read in
// quadratic time, the lines of the seeds below took over ten seconds.
const (
	fuzzLineBytes  = 128 << 10
	fuzzLineBudget = 5 * time.Second
	raceSlowdown   = 12
)

// FuzzString_CommandLinePasswordsLinear redacts the text that the bytes of
// its input pick words of fuzzCommandWords for, once as it is and once with
// the words after the first repeated to fuzzLineBytes. Neither may panic,
// and the long text must be read within fuzzLineBudget, which a reading in
// time quadratic in the length of a line exceeds. The seeds are texts whose
// repetitions were read in quadratic time once: commands after the closing
// quotes of strings, expansions that never close inside quoted strings or
// before client names, substitutions that never close before code spans,
// and substitutions nested in the arguments of a client.
func FuzzString_CommandLinePasswordsLinear(f *testing.F) {
	for _, words := range [][]string{
		{"- ", ")", "'", "sshpass", " "},
		{"- ", "psql", "${", " ", `"`},
		{"x", "mysql", " ", "-e", " ", "$(", "echo", " ", "`", "x", "`"},
		{"x", "mysql", " ", "-pX", " ", "$("},
		{"x", "openssl", " ", "-pass", " ", "pass:x", " ", "-in", " ", "$(", "printf", " ", "x", ";", "\n"},
		{"sh", " ", "-c", " ", `"`, "mysql", " ", "-e", " ", "x", ";", " "},
		{"x", "docker", " ", "login", " ", "-u", " ", "mysql", " ", "--password", " ", "-pX", " "},
		{"subprocess.run([", `"`, "mysql", `", "`, "-pX", `", "`},
		{"x", "machine", " ", "x", " ", "login", " ", "x", " ", "password", " ", "x", "\n"},
		{"x", "find", " ", "mysql", " ", "-exec", " ", "mysql", " ", "-pX", " "},
		{"x", "${PW:-", ")", "login", " ", "mysql", " "},
		{"x", "`", "${PW:-", ")", "mysql", " "},
	} {
		f.Add(fuzzPicks(f, words))
	}
	f.Fuzz(func(t *testing.T, picks []byte) {
		if len(picks) < 2 || len(picks) > 64 {
			return
		}
		var b strings.Builder
		for _, p := range picks[1:] {
			b.WriteString(fuzzCommandWords[int(p)%len(fuzzCommandWords)])
		}
		head, text := fuzzCommandWords[int(picks[0])%len(fuzzCommandWords)], b.String()
		String(head + text)
		line := head + strings.Repeat(text, fuzzLineBytes/len(text)+1)
		budget := fuzzLineBudget
		if raceDetector {
			budget *= raceSlowdown
		}
		start := time.Now()
		String(line)
		if took := time.Since(start); took > budget {
			t.Fatalf("String took %v on %q and %q repeated to %d bytes", took, head, text, len(line))
		}
	})
}

// fuzzPicks returns the input of FuzzString_CommandLinePasswordsLinear that
// picks words.
func fuzzPicks(f *testing.F, words []string) []byte {
	f.Helper()
	picks := make([]byte, 0, len(words))
	for _, w := range words {
		i := -1
		for j, word := range fuzzCommandWords {
			if word == w {
				i = j
				break
			}
		}
		if i < 0 || i > 255 {
			f.Fatalf("no fuzz word %q", w)
		}
		picks = append(picks, byte(i))
	}
	return picks
}
