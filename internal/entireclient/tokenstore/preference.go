package tokenstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// preferenceFileName is the marker, next to contexts.json in the per-user
// config directory, that remembers which credential backend last received a
// write. It exists so that a machine whose OS keyring is unavailable does not
// need ENTIRE_TOKEN_STORE in the environment of every entire process: login
// records the choice once and every later process reads it.
//
// It records a *write*, not a preference the user typed, because the store
// that most recently received tokens is the store that holds the freshest
// credential. Reads consult it; only writes change it. See rememberBackend.
//
// The marker is honored on every platform, including macOS and Windows where
// the automatic fallback (fallback.go) never writes it: an explicit
// ENTIRE_TOKEN_STORE=file login does, by design, so that choice sticks there
// too. Threat model: obeying a planted marker grants nothing new. It can only
// select a store that lives in this same directory, and a writer with access
// here can already repoint contexts.json's core_url at a hostile issuer. The
// file is created 0600 in a 0700 directory like its neighbours.
const preferenceFileName = "token_store.json"

// Backend names as they appear in BackendEnvVar and in the marker.
const (
	backendFile    = "file"
	backendKeyring = "keyring"
)

type storedPreference struct {
	Backend string `json:"backend"`
}

// persistedBackend returns the remembered backend, or "" when nothing is
// remembered. Unreadable, corrupt, or unknown markers all read as unset: the
// marker is an accelerator, and the fallback store recovers the same fact the
// slow way (one failed keyring call) if the marker is missing.
func persistedBackend() string {
	root, err := userdirs.ConfigRootForRead()
	if err != nil {
		return ""
	}
	data, err := osroot.ReadFileNoFollow(root, preferenceFileName)
	if err != nil {
		return ""
	}
	var p storedPreference
	if err := json.Unmarshal(data, &p); err != nil {
		return ""
	}
	if p.Backend == backendFile {
		return backendFile
	}
	return ""
}

// rememberBackend records name as the backend that last received a write.
// "file" writes the marker (only if it is not already there); "keyring"
// removes it, because the keyring is the platform default and needs no
// marker. Any other name is a programming error.
//
// Nothing is remembered while PathEnvVar is set. The marker can only say
// "file", and a later process without the variable would resolve that to the
// DEFAULT path, which does not hold the token; an explicit path is
// environment configuration and has to travel with the process that set it.
func rememberBackend(name string) error {
	switch name {
	case backendFile:
		if os.Getenv(PathEnvVar) != "" || persistedBackend() == backendFile {
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
