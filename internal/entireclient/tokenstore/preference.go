package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// preferenceFileName is the marker, next to contexts.json in the per-user
// config directory, that remembers which credential backend holds the
// freshest credential. It exists so that a machine whose OS keyring is
// unavailable does not need ENTIRE_TOKEN_STORE in the environment of every
// entire process: login records the choice once and every later process reads
// it.
//
// It records evidence, not a preference the user typed: an explicit write
// through ENTIRE_TOKEN_STORE records the store that just received the token,
// and the Linux fallback records the file store once it has proven it holds
// the credential — on a Get, Set or Delete that succeeded there (see switchTo
// in fallback.go) — but never after a keyring timeout, since the abandoned
// keyring call may still complete. Reads through the marker never change it;
// rememberBackend is the only writer.
//
// The marker is honored on every platform, including macOS and Windows where
// the automatic fallback (fallback.go) never writes it: an explicit
// ENTIRE_TOKEN_STORE=file login does, by design, so that choice sticks there
// too. Threat model: obeying a planted marker downgrades future credentials
// from the platform keystore to a 0600 file, but grants nothing new. It can
// only say `file`; where that file lives comes from the environment or this
// same directory. A writer with access here can already repoint
// contexts.json's core_url at a hostile issuer; if contexts.json ever gains
// integrity protection, revisit this. A login onto the file store always says
// so on stdout (persistLogin in the cli package), so a planted marker cannot
// redirect credentials unannounced. The file is created 0600 in a 0700
// directory like its neighbours.
const preferenceFileName = "token_store.json"

// Backend names as they appear in BackendEnvVar and in the marker.
const (
	backendFile    = "file"
	backendKeyring = "keyring"
)

type storedPreference struct {
	Backend string `json:"backend"`
}

// markerWarnOnce dedupes the unusable-marker warning to once per process:
// persistedBackend runs on every backend resolution and again behind
// FileBackendSelected, and a broken marker should be reported, not repeated.
// A pointer so tests can swap in a fresh Once.
var markerWarnOnce = new(sync.Once)

// persistedBackend returns the remembered backend, or "" when nothing is
// remembered. An absent marker — or an absent config directory — is the
// normal case and is silent. A marker that is present but unusable —
// unreadable, a refused symlink, corrupt JSON, a backend name this build does
// not know — or a config directory that exists but cannot be opened, also
// reads as unset, because the marker is an accelerator and the fallback store
// recovers the same fact the slow way, but it is reported once on stderr: on a
// platform with no fallback, silence would reproduce the very "not logged in"
// symptom this file exists to remove.
//
// Deliberately not memoized: switchTo writes the marker mid-process, and the
// next BackendDescription()/FileBackendSelected() must see it — that is what
// keeps `auth status`'s provenance line right after an in-process fallback.
// (With ENTIRE_TOKEN_STORE_PATH set no marker is written at all; the adoption
// flag in tokenstore.go carries that case.) It is one small root-confined
// read; do not "optimize" it with a sync.Once.
func persistedBackend() string {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			warnUnusableMarker(err)
		}
		return ""
	}
	data, err := osroot.ReadFileNoFollow(root, preferenceFileName)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			warnUnusableMarker(err)
		}
		return ""
	}
	var p storedPreference
	if err := json.Unmarshal(data, &p); err != nil {
		warnUnusableMarker(err)
		return ""
	}
	if p.Backend == backendFile {
		return backendFile
	}
	if p.Backend != "" {
		warnUnusableMarker(fmt.Errorf("unknown backend %q", p.Backend))
	}
	return ""
}

// warnUnusableMarker names the marker file but not the directory it is in:
// the wrapped error carries the path when it matters (an os.PathError does),
// and "the Entire config directory" is what a user will search for anyway.
// Printing that path is fine — a marker holds a backend name, never a secret.
func warnUnusableMarker(err error) {
	markerWarnOnce.Do(func() {
		fmt.Fprintf(fallbackNoticeW, "Warning: ignoring unusable token store preference %s in the Entire config directory: %v\nIf the Entire config directory itself is unusable, fix that first; otherwise delete the file, or run entire login to rewrite it.\n", preferenceFileName, err)
	})
}

// markerApplies reports whether the marker can describe this process's file
// store. Never while PathEnvVar is set: the marker cannot carry a path, and a
// bare "file" would point later processes at the default location, which does
// not hold the token. The rule lives here so rememberBackend and the fallback's
// notice cannot disagree about it. The marker is still honoured for READS while
// the variable is set — it selects the backend, never the path.
func markerApplies() bool { return os.Getenv(PathEnvVar) == "" }

// ChoiceIsRemembered reports whether a file-store selection made now would be
// remembered for later processes — the same rule rememberBackend and the
// fallback's notice follow (see markerApplies). Exported so login's hint
// cannot promise a memory the marker will not keep.
func ChoiceIsRemembered() bool { return markerApplies() }

// rememberBackend records name as the backend that now holds the credential.
// "file" writes the marker (only if it is not already there); "keyring"
// removes it, because the keyring is the platform default and needs no
// marker. Any other name is a programming error.
//
// Nothing is remembered while PathEnvVar is set. The marker can only say
// "file", and a later process without the variable would resolve that to the
// DEFAULT path, which does not hold the token; an explicit path is
// environment configuration and has to travel with the process that set it.
// See markerApplies.
func rememberBackend(name string) error {
	switch name {
	case backendFile:
		if !markerApplies() || persistedBackend() == backendFile {
			return nil
		}
		root, err := userdirs.ConfigRoot()
		if err != nil {
			return fmt.Errorf("open config dir for token store preference: %w", err)
		}
		data, err := json.Marshal(storedPreference{Backend: backendFile})
		if err != nil {
			return fmt.Errorf("encode token store preference: %w", err)
		}
		if err := jsonutil.WriteFileAtomicIn(root, preferenceFileName, data, 0o600); err != nil {
			return fmt.Errorf("write token store preference: %w", err)
		}
		return nil
	case backendKeyring:
		root, err := userdirs.ConfigRootForRead()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("open config dir for token store preference: %w", err)
		}
		// osroot.Remove already reports nil for a file that is not there.
		if err := osroot.Remove(root, preferenceFileName); err != nil {
			return fmt.Errorf("clear token store preference: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown token store backend %q", name)
	}
}
