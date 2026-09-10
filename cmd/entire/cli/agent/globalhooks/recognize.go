package globalhooks

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// IsCommand recognizes a complete generated invocation, including its arguments.
func IsCommand(command string) bool {
	if rest, ok := strings.CutPrefix(command, quote("/bin/sh")+" -c "+quote(posixScript)+" entire-global-hook "); ok {
		executable, rest, ok := takeQuotedWord(rest, `'"'"'`)
		if !ok || !strings.HasPrefix(executable, "/") {
			return false
		}
		rest, ok = strings.CutPrefix(rest, " hooks global ")
		if !ok {
			return false
		}
		name, verb, ok := invocationWords(rest, `'"'"'`, "")
		return ok && command == (Selection{Executable: executable, Launcher: "/bin/sh"}).Command(name, verb)
	}
	return isWindowsCommand(command)
}

func isWindowsCommand(command string) bool {
	launcher, encoded, ok := strings.Cut(command, ` -NoProfile -NonInteractive -EncodedCommand `)
	if !ok || !strings.HasPrefix(launcher, `"`) || !strings.HasSuffix(launcher, `\WindowsPowerShell\v1.0\powershell.exe"`) {
		return false
	}
	launcher = strings.TrimSuffix(strings.TrimPrefix(launcher, `"`), `"`)
	if strings.ContainsRune(launcher, '"') || !absoluteWindowsPath(launcher) {
		return false
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data)%2 != 0 {
		return false
	}
	units := make([]uint16, len(data)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2*i:])
	}
	rest, ok := strings.CutPrefix(string(utf16.Decode(units)), windowsScriptPrefix+"$exe=")
	if !ok {
		return false
	}
	executable, rest, ok := takeQuotedWord(rest, "''")
	if !ok || !absoluteWindowsPath(executable) {
		return false
	}
	rest, ok = strings.CutPrefix(rest, windowsInvocationPrefix)
	if !ok {
		return false
	}
	name, verb, ok := invocationWords(rest, "''", windowsInvocationSuffix)
	return ok && command == (Selection{Executable: executable, Launcher: launcher, Platform: windowsPlatform}).Command(name, verb)
}

func absoluteWindowsPath(path string) bool {
	return strings.HasPrefix(path, `\\`) || len(path) >= 3 &&
		((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func invocationWords(rest, escape, suffix string) (name, verb string, ok bool) {
	name, rest, ok = takeQuotedWord(rest, escape)
	if !ok || name == "" {
		return "", "", false
	}
	rest, ok = strings.CutPrefix(rest, " ")
	if !ok {
		return "", "", false
	}
	verb, rest, ok = takeQuotedWord(rest, escape)
	return name, verb, ok && verb != "" && rest == suffix
}

func takeQuotedWord(input, escape string) (word, rest string, ok bool) {
	rest, ok = strings.CutPrefix(input, "'")
	if !ok {
		return "", input, false
	}
	var value strings.Builder
	for {
		end := strings.IndexByte(rest, '\'')
		if end < 0 {
			return "", input, false
		}
		value.WriteString(rest[:end])
		rest = rest[end:]
		if strings.HasPrefix(rest, escape) {
			value.WriteByte('\'')
			rest = strings.TrimPrefix(rest, escape)
			continue
		}
		return value.String(), rest[1:], true
	}
}
