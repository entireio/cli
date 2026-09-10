// Package globalhooks stores the explicitly selected user-hook installation.
package globalhooks

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

const recordName = "global-hook-installation.json"
const windowsPlatform = "windows"

// Selection names stable installation paths, allowing in-place upgrades.
type Selection struct {
	Executable string `json:"executable"`
	Launcher   string `json:"launcher"`
	Platform   string `json:"platform"`
}

// New is used only by explicit enrollment, never by reconciliation.
func New(executable string) (Selection, error) {
	launcher, err := platformLauncher()
	if err != nil {
		return Selection{}, err
	}
	s := Selection{Executable: executable, Launcher: launcher, Platform: runtime.GOOS}
	return s, s.Validate()
}

func (s Selection) Validate() error {
	if s.Platform != runtime.GOOS {
		return errors.New("selected hook installation belongs to another platform")
	}
	for _, path := range []string{s.Executable, s.Launcher} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
			return fmt.Errorf("hook installation requires an absolute path: %q", path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("selected hook installation unavailable: %w", err)
		}
		if !info.Mode().IsRegular() || (runtime.GOOS != windowsPlatform && info.Mode().Perm()&0o111 == 0) {
			return fmt.Errorf("selected hook installation is not executable: %s", path)
		}
	}
	return nil
}

func Load() (Selection, error) {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		return Selection{}, fmt.Errorf("read selected hook installation: %w", err)
	}
	data, err := osroot.ReadFileNoFollow(root, recordName)
	if err != nil {
		return Selection{}, fmt.Errorf("read selected hook installation: %w", err)
	}
	var s Selection
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("decode selected hook installation: %w", err)
	}
	return s, s.Validate()
}

func Save(ctx context.Context, s Selection) error {
	if err := s.Validate(); err != nil {
		return err
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open hook installation directory: %w", err)
	}
	release, err := flock.AcquireContextIn(ctx, root, recordName+".lock")
	if err != nil {
		return fmt.Errorf("lock selected hook installation: %w", err)
	}
	defer release()
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode selected hook installation: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, recordName, data, 0o600); err != nil {
		return fmt.Errorf("save selected hook installation: %w", err)
	}
	return nil
}

const posixScript = `# entire-global-hook-v1
if [ ! -x "$1" ]; then printf '%s\n' 'Entire: selected hook installation is unavailable; use entire agent to select an installation.' >&2; exit 0; fi
exec "$@"`
const windowsScriptPrefix = "$ErrorActionPreference='Stop'; # entire-global-hook-v1\n"
const windowsInvocationPrefix = "; if (!(Test-Path -LiteralPath $exe -PathType Leaf)) { [Console]::Error.WriteLine('Entire: selected hook installation is unavailable; use entire agent to select an installation.'); exit 0 }; & $exe 'hooks' 'global' "
const windowsInvocationSuffix = "; exit $LASTEXITCODE"

func quote(s string) string   { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Command renders a launcher using only the enrolled absolute paths.
func (s Selection) Command(agent, hook string) string {
	if s.Platform == windowsPlatform {
		script := windowsScriptPrefix + "$exe=" + psQuote(s.Executable) + windowsInvocationPrefix + psQuote(agent) + " " + psQuote(hook) + windowsInvocationSuffix
		units := utf16.Encode([]rune(script))
		data := make([]byte, 2*len(units))
		for i, u := range units {
			binary.LittleEndian.PutUint16(data[i*2:], u)
		}
		return `"` + s.Launcher + `" -NoProfile -NonInteractive -EncodedCommand ` + base64.StdEncoding.EncodeToString(data)
	}
	return quote(s.Launcher) + " -c " + quote(posixScript) + " entire-global-hook " + quote(s.Executable) + " hooks global " + quote(agent) + " " + quote(hook)
}
