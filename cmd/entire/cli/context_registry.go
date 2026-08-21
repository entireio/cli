package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

const contextRegistryRetention = 30 * 24 * time.Hour

type localContextSession struct {
	RepoID         string    `json:"repoId"`
	RepoName       string    `json:"repoName"`
	SessionID      string    `json:"sessionId"`
	Agent          string    `json:"agent"`
	WorktreeRoot   string    `json:"worktreeRoot"`
	GitCommonDir   string    `json:"gitCommonDir"`
	SessionDir     string    `json:"sessionDir"`
	TranscriptPath string    `json:"transcriptPath"`
	LastSeen       time.Time `json:"lastSeen"`
}

type contextRegistry struct {
	Sessions []localContextSession `json:"sessions"`
}

func contextRegistryPath(base, coreURL, accountID string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.ToLower(coreURL), "/") + "\x00" + accountID))
	namespace := hex.EncodeToString(sum[:16])
	return filepath.Join(base, namespace, "registry.json")
}

func defaultContextRegistryRoot() string {
	return filepath.Join(userdirs.Cache(), "context")
}

func contextNamespaceLockPath(registryPath string) string {
	return filepath.Dir(registryPath) + ".lock"
}

func readContextRegistryNoLock(path string) (contextRegistry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is derived from the user cache and a fixed filename.
	if errors.Is(err, os.ErrNotExist) {
		return contextRegistry{Sessions: []localContextSession{}}, nil
	}
	if err != nil {
		return contextRegistry{}, fmt.Errorf("read context registry: %w", err)
	}
	var registry contextRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return contextRegistry{}, fmt.Errorf("parse context registry: %w", err)
	}
	if registry.Sessions == nil {
		registry.Sessions = []localContextSession{}
	}
	return registry, nil
}

func readContextRegistry(path string) (contextRegistry, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return contextRegistry{}, fmt.Errorf("create context registry directory: %w", err)
	}
	release, err := flock.Acquire(contextNamespaceLockPath(path))
	if err != nil {
		return contextRegistry{}, fmt.Errorf("lock context registry: %w", err)
	}
	defer release()
	registry, err := readContextRegistryNoLock(path)
	if err == nil {
		before := len(registry.Sessions)
		pruneContextRegistry(&registry, time.Now())
		if len(registry.Sessions) != before {
			err = writeContextRegistryNoLock(path, registry)
		}
	}
	return registry, err
}

func writeContextRegistryNoLock(path string, registry contextRegistry) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create context registry directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // 0700 is the required permission for this directory, not a data file.
		return fmt.Errorf("secure context registry directory: %w", err)
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal context registry: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".registry.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create context registry temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure context registry temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write context registry temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync context registry temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close context registry temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace context registry: %w", err)
	}
	return nil
}

func writeContextRegistry(path string, registry contextRegistry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create context registry directory: %w", err)
	}
	release, err := flock.Acquire(contextNamespaceLockPath(path))
	if err != nil {
		return fmt.Errorf("lock context registry: %w", err)
	}
	defer release()
	return writeContextRegistryNoLock(path, registry)
}

func mutateContextRegistry(ctx context.Context, path string, fn func(*contextRegistry)) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create context registry directory: %w", err)
	}
	release, err := flock.AcquireContext(ctx, contextNamespaceLockPath(path))
	if err != nil {
		return fmt.Errorf("lock context registry: %w", err)
	}
	defer release()
	registry, err := readContextRegistryNoLock(path)
	if err != nil {
		return err
	}
	pruneContextRegistry(&registry, time.Now())
	fn(&registry)
	return writeContextRegistryNoLock(path, registry)
}

func refreshContextRegistrySession(ctx context.Context, path, sessionID string, now time.Time) error {
	return mutateContextRegistry(ctx, path, func(registry *contextRegistry) {
		for i := range registry.Sessions {
			if registry.Sessions[i].SessionID == sessionID {
				registry.Sessions[i].LastSeen = now
				return
			}
		}
	})
}

func refreshLocalContextSessionHeartbeat(ctx context.Context, sessionID string) error {
	path, err := currentContextRegistryPath(ctx)
	if err != nil {
		return err
	}
	return refreshContextRegistrySession(ctx, path, sessionID, time.Now())
}

func pruneContextRegistry(registry *contextRegistry, now time.Time) {
	cutoff := now.Add(-contextRegistryRetention)
	kept := registry.Sessions[:0]
	for _, session := range registry.Sessions {
		if !session.LastSeen.Before(cutoff) {
			kept = append(kept, session)
		}
	}
	registry.Sessions = kept
}

func transcriptPathWithinSessionDir(transcriptPath, sessionDir string) bool {
	transcriptPath, err := canonicalPathForContainment(transcriptPath)
	if err != nil {
		return false
	}
	sessionDir, err = canonicalPathForContainment(sessionDir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(sessionDir, transcriptPath)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func canonicalPathForContainment(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("resolve absolute path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve path symlinks: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("resolve parent directory symlinks: %w", err)
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func openTranscriptWithinSessionDir(transcriptPath, sessionDir string) (*os.File, error) {
	transcriptPath, err := filepath.Abs(filepath.Clean(transcriptPath))
	if err != nil {
		return nil, fmt.Errorf("resolve transcript path: %w", err)
	}
	sessionDir, err = filepath.Abs(filepath.Clean(sessionDir))
	if err != nil {
		return nil, fmt.Errorf("resolve session directory: %w", err)
	}
	rel, err := filepath.Rel(sessionDir, transcriptPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("transcript is outside the agent session directory")
	}
	root, err := os.OpenRoot(sessionDir)
	if err != nil {
		return nil, fmt.Errorf("open agent session directory: %w", err)
	}
	file, openErr := root.Open(rel)
	closeErr := root.Close()
	if openErr != nil {
		return nil, fmt.Errorf("open transcript beneath agent session directory: %w", openErr)
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, fmt.Errorf("close agent session directory: %w", closeErr)
	}
	return file, nil
}

func accountIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Subject == "" {
		return "", errors.New("JWT has no account subject")
	}
	return claims.Subject, nil
}

func currentContextRegistryPath(ctx context.Context) (string, error) {
	if raw, ok := os.LookupEnv(auth.EnvTokenVar); ok {
		coreURL, token, err := auth.ParseEnvToken(raw)
		if err != nil {
			return "", err
		}
		return contextRegistryPathForToken(coreURL, token)
	}
	target, err := auth.ResolveControlPlaneTarget()
	if err != nil {
		return "", fmt.Errorf("resolve control plane target: %w", err)
	}
	token, err := target.TokenSource(ctx)
	if err != nil {
		return "", fmt.Errorf("load control plane token: %w", err)
	}
	return contextRegistryPathForToken(target.CoreURL, token)
}

func contextRegistryPathForToken(coreURL, token string) (string, error) {
	accountID, err := accountIDFromJWT(token)
	if err != nil {
		return "", err
	}
	return contextRegistryPath(defaultContextRegistryRoot(), coreURL, accountID), nil
}

func removeLocalContextNamespace(ctx context.Context) error {
	path, err := currentContextRegistryPath(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if dir == defaultContextRegistryRoot() || filepath.Dir(dir) != defaultContextRegistryRoot() {
		return errors.New("refusing to remove invalid context namespace")
	}
	return removeContextNamespace(ctx, path)
}

func removeContextNamespace(ctx context.Context, registryPath string) error {
	lockPath := contextNamespaceLockPath(registryPath)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create context cache root: %w", err)
	}
	release, err := flock.AcquireContext(ctx, lockPath)
	if err != nil {
		return fmt.Errorf("lock context namespace: %w", err)
	}
	defer release()
	dir := filepath.Dir(registryPath)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove context namespace: %w", err)
	}
	return nil
}
