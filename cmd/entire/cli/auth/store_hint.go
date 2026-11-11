package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/envflag"
	"github.com/entireio/cli/cmd/entire/cli/execx"
	"github.com/entireio/cli/internal/entireclient/tokenstore"
)

// FileStoreSteps returns the commands that switch to the file token store and
// log in again, naming the variables and paths tokenstore actually reads.
// Callers introduce it with their own lead sentence and gate it with
// FileStoreCanHelp; this is the one copy of the advice.
func FileStoreSteps() string {
	return fmt.Sprintf("  %s=file entire login\n\nor, to make it stick, put {\"token_store\": \"file\"} in %s.\n\nTokens are then written with 0600 permissions to %s (override the location with %s)",
		tokenstore.BackendEnvVar, tokenstore.ConfigFilePath(), tokenstore.FileBackendPath(), tokenstore.PathEnvVar)
}

// FileStoreCanHelp reports whether suggesting the file token store makes
// sense for err: not when the file store is already selected (by env or
// config.json), and not when config.json itself is bad, since the advice
// edits it.
func FileStoreCanHelp(err error) bool {
	return !errors.Is(err, tokenstore.ErrBadConfig) && !tokenstore.FileBackendSelected()
}

// secretServiceProviders are the binaries that provide the Secret Service on
// common Linux desktops, in the order they are reported.
var secretServiceProviders = []string{"gnome-keyring-daemon", "kwalletd6", "kwalletd5", "keepassxc"}

// installedSecretServiceProvider returns the first Secret Service provider
// binary found on PATH, or "". It only checks that a file exists: nothing is
// run, and the answer only orders the advice, so a planted or stale binary
// can at worst put "start it" first. Directories come from
// execx.PathScanDirs, so a relative PATH entry (the caller's repository)
// cannot influence it. Misses (a Flatpak/AppImage KeePassXC, a daemon under
// /usr/libexec) only demote that advice; they never hide it.
func installedSecretServiceProvider() string {
	dirs := execx.PathScanDirs()
	for _, name := range secretServiceProviders {
		for _, dir := range dirs {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
				return name
			}
		}
	}
	return ""
}

// KeyringServiceMissingAdvice is the way forward when no Secret Service
// provider is reachable (tokenstore.ErrKeyringServiceMissing): start one, or
// move to the file store. It starts with a blank line, ready to follow the
// error text.
func KeyringServiceMissingAdvice(err error) string {
	return keyringServiceMissingAdvice(installedSecretServiceProvider(), envflag.Enabled("CI"), FileStoreCanHelp(err))
}

// keyringServiceMissingAdvice orders the two remedies by what the machine
// looks like:
//
//   - A provider is installed: it is most likely a desktop reached over SSH
//     or tmux, where the keyring simply is not running. Starting it comes
//     first, naming the binary found.
//   - None is installed: a server or container. The file store comes first;
//     installing a provider stays as a closing line, since the PATH check
//     can miss one.
//   - CI: there is never a keyring to start, so only the file store is
//     offered.
//
// The file-store paragraph is left out when fileStoreCanHelp is false, and
// the provider line is never dropped when it would leave no advice at all.
func keyringServiceMissingAdvice(provider string, ci, fileStoreCanHelp bool) string {
	fileStore := ""
	if fileStoreCanHelp {
		fileStore = "store tokens in a file instead. You must log in again, because tokens saved in the keyring are not visible to the file store:\n\n" + FileStoreSteps()
	}
	switch {
	case provider != "" && !ci:
		advice := "\n\n" + provider + " is installed but not answering on this session's D-Bus. Start it (or another Secret Service provider) in this session, then re-run the command. " +
			"This is the usual fix on a desktop machine reached over SSH or tmux."
		if fileStore != "" {
			advice += "\n\nOr " + fileStore
		}
		return advice
	case ci && fileStore != "":
		return "\n\nCI has no OS keyring to start, so " + fileStore
	}
	install := "install and start a Secret Service provider (for example gnome-keyring, or KeePassXC with Secret Service integration enabled), then re-run the command."
	if fileStore == "" {
		return "\n\nTo use the OS keyring, " + install
	}
	return "\n\nNo Secret Service provider was found on PATH, so " + fileStore + "\n\nOr, to use the OS keyring, " + install
}
