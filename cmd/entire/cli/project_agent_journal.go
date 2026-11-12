package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/auth-go/tokens"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

type projectAgentJournal struct {
	root   *os.Root
	name   string
	unlock func()
	// workspace is this worker's cache-relative review directory; the lock
	// makes it safe to sweep.
	workspace string
}

func openProjectAgentJournal(ctx context.Context, t *projectAgentTarget) (*projectAgentJournal, error) {
	identity, err := projectAgentWorkerIdentity()
	if err != nil {
		return nil, fmt.Errorf("resolve worker identity: %w", err)
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return nil, fmt.Errorf("open worker state: %w", err)
	}
	if err := osroot.MkdirAllNoSymlink(root, "agent-serve", 0o700); err != nil {
		return nil, fmt.Errorf("create worker state directory: %w", err)
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(identity+"\n"+t.client.BaseURL()+"\n"+t.projectID)))
	key := "agent-serve/" + hash
	unlock, err := flock.AcquireContextIn(ctx, root, key+".lock")
	if err != nil {
		return nil, fmt.Errorf("lock worker state: %w", err)
	}
	return &projectAgentJournal{root: root, name: key + ".json", unlock: unlock, workspace: "agent-work/" + hash}, nil
}

// projectAgentWorkerIdentity keys a service account the same whether
// ENTIRE_TOKEN holds its secret or a token exchanged from it, so both forms
// share one lock and journal.
func projectAgentWorkerIdentity() (string, error) {
	identity, err := auth.LocalIdentityCacheKey()
	if err != nil {
		return "", err //nolint:wrapcheck // wrapped by the caller
	}
	raw := strings.TrimSpace(os.Getenv(auth.EnvTokenVar))
	if raw == "" || auth.IsServiceAccountSecret(raw) {
		return identity, nil
	}
	claims, err := tokens.ParseClaims(raw)
	if err != nil || !strings.HasPrefix(claims.Subject, "svc_") {
		return identity, nil //nolint:nilerr // LocalIdentityCacheKey already accepted it
	}
	var cores []string
	for _, aud := range claims.Audience {
		if u, err := url.Parse(aud); err == nil && u.Scheme != "" {
			cores = append(cores, strings.TrimRight(aud, "/"))
		}
	}
	if len(cores) != 1 {
		return identity, nil
	}
	return "env-sa|" + cores[0] + "|" + claims.Subject, nil
}

// sweepWorkspaces removes review clones left by a killed worker.
func (j *projectAgentJournal) sweepWorkspaces() error {
	root, err := userdirs.CacheRoot()
	if err != nil {
		return fmt.Errorf("open workspace cache: %w", err)
	}
	if err := osroot.RemoveAllNoSymlinks(root, j.workspace); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale review workspaces: %w", err)
	}
	return nil
}

// path is for operator diagnostics only; all I/O remains rooted.
func (j *projectAgentJournal) path() string {
	return filepath.Join(j.root.Name(), filepath.FromSlash(j.name))
}

func (j *projectAgentJournal) save(s *projectAgentSubmission) error {
	if s == nil {
		err := osroot.RemoveNoSymlinks(j.root, j.name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove submitted result: %w", err)
		}
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode pending result: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(j.root, j.name, b, 0o600); err != nil {
		return fmt.Errorf("save pending result: %w", err)
	}
	return nil
}

func (j *projectAgentJournal) load() (*projectAgentSubmission, error) {
	b, err := osroot.ReadFileNoFollow(j.root, j.name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // No file means no pending submission.
	}
	if err != nil {
		return nil, fmt.Errorf("read pending result %q: %w", j.path(), err)
	}
	var s projectAgentSubmission
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("decode pending result %q: %w", j.path(), err)
	}
	return &s, nil
}
