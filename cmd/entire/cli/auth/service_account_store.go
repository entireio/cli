package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// On-disk layout, all in the per-user config directory and all 0600:
//
//	service-account-key-<key id>.json   one file per key, never overwritten in place
//	                                    except to record its own state
//	service-account-<svc>.active.json   which key the account currently uses
//	service-accounts.lock               held by every change to the above
//
// A key file is created exclusively (it can never replace another key) and is
// written before the server learns its public half, so the server never trusts
// a key whose private half does not exist on disk. Switching keys is a single
// atomic rewrite of the pointer: there is no moment without a usable key, and
// the previous key stays on disk, able to revoke itself, until it has.

const (
	serviceAccountKeyType     = "entire_service_account_key"
	serviceAccountPointerType = "entire_service_account_active_key"
	serviceAccountLockName    = "service-accounts.lock"
	serviceAccountKeyPrefix   = "service-account-key-"
)

// keyIDPattern is an RFC 7638 SHA-256 thumbprint, base64url without padding.
// The id becomes part of a file name, so nothing else is accepted.
var keyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// ServiceAccountKeyStatus is where a stored key is in its life cycle.
type ServiceAccountKeyStatus string

const (
	// KeyActive is the key the account's pointer selects: the one that signs.
	KeyActive ServiceAccountKeyStatus = "active"
	// KeyEnrolling is waiting for an admin to approve its enrollment.
	KeyEnrolling ServiceAccountKeyStatus = "enrolling"
	// KeyInactive is bound to an account but not selected: a staged successor
	// awaiting `key activate`.
	KeyInactive ServiceAccountKeyStatus = "inactive"
	// KeyRetired was replaced and still has to revoke itself on the server.
	KeyRetired ServiceAccountKeyStatus = "retired"
)

// serviceAccountKeyFile is the on-disk form. It is the only type that carries
// the encoded private key; ServiceAccountKey keeps it unexported.
type serviceAccountKeyFile struct {
	Type   string `json:"type"`
	KeyID  string `json:"key_id"`
	Server string `json:"server"`
	// ServiceAccount is empty while an enrollment that lets the approver pick
	// the account is pending.
	ServiceAccount      string     `json:"service_account,omitempty"`
	Name                string     `json:"name,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	EnrollmentID        string     `json:"enrollment_id,omitempty"`
	EnrollmentExpiresAt *time.Time `json:"enrollment_expires_at,omitempty"`
	RetiredAt           *time.Time `json:"retired_at,omitempty"`
	PrivateKey          string     `json:"private_key"`
}

type serviceAccountPointerFile struct {
	Type           string `json:"type"`
	ServiceAccount string `json:"service_account"`
	KeyID          string `json:"key_id"`
}

// ServiceAccountKey is one locally held key. Its private half is never
// exported; the zero value is not a usable key and its methods return errors
// instead of panicking.
type ServiceAccountKey struct {
	keyID               string
	server              string
	serviceAccount      string
	name                string
	createdAt           time.Time
	enrollmentID        string
	enrollmentExpiresAt time.Time
	retiredAt           time.Time
	signer              ed25519.PrivateKey
}

func (k *ServiceAccountKey) KeyID() string          { return k.keyID }
func (k *ServiceAccountKey) Server() string         { return k.server }
func (k *ServiceAccountKey) ServiceAccount() string { return k.serviceAccount }
func (k *ServiceAccountKey) Name() string           { return k.name }
func (k *ServiceAccountKey) CreatedAt() time.Time   { return k.createdAt }

// EnrollmentID is the pending enrollment this key waits on, or "".
func (k *ServiceAccountKey) EnrollmentID() string { return k.enrollmentID }

// EnrollmentExpiresAt is when the pending enrollment lapses on the server.
func (k *ServiceAccountKey) EnrollmentExpiresAt() time.Time { return k.enrollmentExpiresAt }

var errUnusableKey = errors.New("service-account key is not initialised")

func (k *ServiceAccountKey) usable() error {
	if k == nil || len(k.signer) != ed25519.PrivateKeySize || !keyIDPattern.MatchString(k.keyID) || k.server == "" {
		return errUnusableKey
	}
	return nil
}

// StoredServiceAccountKey is a key with its derived status.
type StoredServiceAccountKey struct {
	Key    *ServiceAccountKey
	Status ServiceAccountKeyStatus
}

// ServiceAccountKeyFileName is the key's file name inside the config
// directory, for display.
func ServiceAccountKeyFileName(keyID string) string {
	return serviceAccountKeyPrefix + keyID + ".json"
}

func serviceAccountPointerName(id string) string {
	return "service-account-" + id + ".active.json"
}

// ErrServiceAccountKeyExists is returned when a key file with that id is
// already stored: generating the same Ed25519 key twice is not possible, so
// this means the same key was saved twice.
var ErrServiceAccountKeyExists = errors.New("a key with this id is already stored")

// withServiceAccountLock serialises every change to key files and pointers
// across processes.
func withServiceAccountLock(ctx context.Context, fn func(root *os.Root) error) error {
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open config directory: %w", err)
	}
	release, err := flock.AcquireContextIn(ctx, root, serviceAccountLockName)
	if err != nil {
		return fmt.Errorf("lock service-account keys: %w", err)
	}
	defer release()
	return fn(root)
}

// CreateServiceAccountKey stores a new key bound to an account. If the
// account has no active key it becomes the active one; otherwise it is stored
// inactive and an explicit ActivateServiceAccountKey switches to it, so an
// existing working key is never displaced by one the server does not know yet.
func CreateServiceAccountKey(ctx context.Context, k *ServiceAccountKey) (activated bool, err error) {
	if err := k.usable(); err != nil {
		return false, err
	}
	if k.serviceAccount == "" {
		return false, errors.New("a created key must name its service account")
	}
	err = withServiceAccountLock(ctx, func(root *os.Root) error {
		if err := createKeyFile(root, k); err != nil {
			return err
		}
		if _, ok, err := readPointer(root, k.serviceAccount); err != nil || ok {
			return err
		}
		activated = true
		return writePointer(root, k.serviceAccount, k.keyID)
	})
	return activated, err
}

// SavePendingEnrollmentKey stores a key before its enrollment is submitted, so
// that a crash at any later point leaves the private half on disk for the
// public half the server may come to trust. serviceAccount is the proposed
// account, or "" to let the approver choose.
func SavePendingEnrollmentKey(ctx context.Context, k *ServiceAccountKey) error {
	if err := k.usable(); err != nil {
		return err
	}
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		return createKeyFile(root, k)
	})
}

// RecordEnrollment notes the server's enrollment id and expiry on the pending
// key so a later `enroll --resume` can keep polling it.
func RecordEnrollment(ctx context.Context, k *ServiceAccountKey, e *ServiceAccountEnrollment) error {
	return updateKeyFile(ctx, k, func(f *serviceAccountKeyFile) {
		exp := nowFunc().Add(time.Duration(e.ExpiresIn) * time.Second).UTC()
		f.EnrollmentID = e.ID
		f.EnrollmentExpiresAt = &exp
	})
}

// BindEnrolledKey records the approved account on the key. It becomes the
// active key only when the account has none on this machine; replacing a
// working key stays an explicit `key activate`.
func BindEnrolledKey(ctx context.Context, k *ServiceAccountKey, serviceAccount string) (activated bool, err error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return false, err
	}
	err = withServiceAccountLock(ctx, func(root *os.Root) error {
		if err := rewriteKeyFile(root, k, func(f *serviceAccountKeyFile) {
			f.ServiceAccount = id
			f.EnrollmentID = ""
			f.EnrollmentExpiresAt = nil
		}); err != nil {
			return err
		}
		if _, ok, err := readPointer(root, id); err != nil || ok {
			return err
		}
		activated = true
		return writePointer(root, id, k.keyID)
	})
	return activated, err
}

// ActivateServiceAccountKey makes k the account's active key and marks the
// previous one retired, returning it so the caller can have it revoke itself.
// The retired key stays on disk until DeleteServiceAccountKey.
func ActivateServiceAccountKey(ctx context.Context, k *ServiceAccountKey) (previous *ServiceAccountKey, err error) {
	if err := k.usable(); err != nil {
		return nil, err
	}
	if k.serviceAccount == "" || k.enrollmentID != "" {
		return nil, fmt.Errorf("key %s is still waiting for enrollment approval", k.keyID)
	}
	err = withServiceAccountLock(ctx, func(root *os.Root) error {
		// Re-read under the lock: the caller's copy may be stale.
		current, err := readKeyFile(root, k.keyID)
		if err != nil {
			return err
		}
		if current.serviceAccount != k.serviceAccount {
			return fmt.Errorf("key %s now belongs to %s, not %s", k.keyID, current.serviceAccount, k.serviceAccount)
		}
		if !current.retiredAt.IsZero() {
			return fmt.Errorf("key %s was retired; create or enroll a new key instead", k.keyID)
		}
		prevID, ok, err := readPointer(root, k.serviceAccount)
		if err != nil {
			return err
		}
		if ok && prevID == k.keyID {
			return nil
		}
		if err := writePointer(root, k.serviceAccount, k.keyID); err != nil {
			return err
		}
		if !ok {
			return nil
		}
		prev, err := readKeyFile(root, prevID)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := rewriteKeyFile(root, prev, func(f *serviceAccountKeyFile) {
			now := nowFunc().UTC()
			f.RetiredAt = &now
		}); err != nil {
			return err
		}
		previous = prev
		return nil
	})
	return previous, err
}

// DeleteServiceAccountKey removes k's file, its cached session, and the
// account's pointer when it selects k. Missing files are not an error; any
// other failure is returned so a revoked key is never reported as gone while
// it is still on disk.
func DeleteServiceAccountKey(ctx context.Context, k *ServiceAccountKey) error {
	if !keyIDPattern.MatchString(k.keyID) {
		return errUnusableKey
	}
	if err := removeServiceAccountSession(k.keyID); err != nil {
		return err
	}
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		if k.serviceAccount != "" {
			if id, ok, err := readPointer(root, k.serviceAccount); err != nil {
				return err
			} else if ok && id == k.keyID {
				if err := root.Remove(serviceAccountPointerName(k.serviceAccount)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("remove active-key pointer: %w", err)
				}
			}
		}
		if err := root.Remove(ServiceAccountKeyFileName(k.keyID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove key file: %w", err)
		}
		return nil
	})
}

// LoadActiveServiceAccountKey reads the key that signs for serviceAccount. It
// takes no lock: the pointer and key files are only ever replaced atomically.
func LoadActiveServiceAccountKey(serviceAccount string) (*ServiceAccountKey, error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return nil, err
	}
	root, err := configRootForRead()
	if err != nil {
		return nil, err
	}
	var keyID string
	ok := false
	if root != nil {
		keyID, ok, err = readPointer(root, id)
		if err != nil {
			return nil, err
		}
	}
	if !ok {
		return nil, fmt.Errorf("no active key stored for service account %s; run `entire auth service-account enroll --service-account %s`, or `entire auth service-account key create`", id, id)
	}
	k, err := readKeyFile(root, keyID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the active key %s for service account %s is missing from disk", keyID, id)
	}
	if err != nil {
		return nil, err
	}
	if k.serviceAccount != id {
		return nil, fmt.Errorf("the active key %s for service account %s belongs to %q", keyID, id, k.serviceAccount)
	}
	return k, nil
}

// LoadServiceAccountKey reads one stored key by id.
func LoadServiceAccountKey(keyID string) (*ServiceAccountKey, error) {
	if !keyIDPattern.MatchString(keyID) {
		return nil, fmt.Errorf("%q is not a key id", keyID)
	}
	root, err := configRootForRead()
	if err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("no key %s is stored on this machine", keyID)
	}
	k, err := readKeyFile(root, keyID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no key %s is stored on this machine", keyID)
	}
	return k, err
}

// ListServiceAccountKeys returns the stored keys of serviceAccount (every key,
// including unbound enrollments, when it is ""), oldest first. A file that
// cannot be read is reported rather than skipped.
func ListServiceAccountKeys(serviceAccount string) ([]StoredServiceAccountKey, error) {
	if serviceAccount != "" {
		id, err := ParseServiceAccountID(serviceAccount)
		if err != nil {
			return nil, err
		}
		serviceAccount = id
	}
	root, err := configRootForRead()
	if err != nil || root == nil {
		return nil, err
	}
	entries, err := osroot.ReadDir(root, ".")
	if err != nil {
		return nil, fmt.Errorf("list config directory: %w", err)
	}
	active := map[string]string{}
	var out []StoredServiceAccountKey
	for _, e := range entries {
		keyID, ok := strings.CutPrefix(e.Name(), serviceAccountKeyPrefix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		keyID, ok = strings.CutSuffix(keyID, ".json")
		if !ok || !keyIDPattern.MatchString(keyID) {
			continue
		}
		k, err := readKeyFile(root, keyID)
		if err != nil {
			return nil, err
		}
		if serviceAccount != "" && k.serviceAccount != serviceAccount {
			continue
		}
		status := KeyInactive
		switch {
		case k.enrollmentID != "" || k.serviceAccount == "":
			status = KeyEnrolling
		case !k.retiredAt.IsZero():
			status = KeyRetired
		default:
			activeID, seen := active[k.serviceAccount]
			if !seen {
				activeID, _, err = readPointer(root, k.serviceAccount)
				if err != nil {
					return nil, err
				}
				active[k.serviceAccount] = activeID
			}
			if activeID == k.keyID {
				status = KeyActive
			}
		}
		out = append(out, StoredServiceAccountKey{Key: k, Status: status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.createdAt.Before(out[j].Key.createdAt) })
	return out, nil
}

// configRootForRead returns the config root, or nil when the directory does
// not exist yet (nothing stored).
func configRootForRead() (*os.Root, error) {
	root, err := userdirs.ConfigRootForRead()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // a nil root is "nothing stored", which every caller handles
	}
	if err != nil {
		return nil, fmt.Errorf("open config directory: %w", err)
	}
	return root, nil
}

func readPointer(root *os.Root, id string) (string, bool, error) {
	data, err := osroot.ReadFileNoFollow(root, serviceAccountPointerName(id))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read active key of %s: %w", id, err)
	}
	var p serviceAccountPointerFile
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false, fmt.Errorf("parse active key of %s: %w", id, err)
	}
	if p.Type != serviceAccountPointerType || p.ServiceAccount != id || !keyIDPattern.MatchString(p.KeyID) {
		return "", false, fmt.Errorf("active-key file of %s is malformed", id)
	}
	return p.KeyID, true, nil
}

func writePointer(root *os.Root, id, keyID string) error {
	data, err := jsonutil.MarshalIndentWithNewline(serviceAccountPointerFile{
		Type: serviceAccountPointerType, ServiceAccount: id, KeyID: keyID,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active key: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, serviceAccountPointerName(id), data, 0o600); err != nil {
		return fmt.Errorf("write active key of %s: %w", id, err)
	}
	return nil
}

func encodeKeyFile(k *ServiceAccountKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k.signer)
	if err != nil {
		return nil, fmt.Errorf("encode private key: %w", err)
	}
	f := serviceAccountKeyFile{
		Type:           serviceAccountKeyType,
		KeyID:          k.keyID,
		Server:         k.server,
		ServiceAccount: k.serviceAccount,
		Name:           k.name,
		CreatedAt:      k.createdAt,
		EnrollmentID:   k.enrollmentID,
		PrivateKey:     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}
	if !k.enrollmentExpiresAt.IsZero() {
		f.EnrollmentExpiresAt = &k.enrollmentExpiresAt
	}
	if !k.retiredAt.IsZero() {
		f.RetiredAt = &k.retiredAt
	}
	data, err := jsonutil.MarshalIndentWithNewline(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode key file: %w", err)
	}
	return data, nil
}

// createKeyFile writes k's file only if no file of that name exists: the
// content goes to a private temp file that is then hard-linked into place,
// which fails rather than replaces.
func createKeyFile(root *os.Root, k *ServiceAccountKey) (retErr error) {
	data, err := encodeKeyFile(k)
	if err != nil {
		return err
	}
	name := ServiceAccountKeyFileName(k.keyID)
	f, tmp, err := jsonutil.CreateTempIn(root, name)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	defer func() {
		if rerr := root.Remove(tmp); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) && retErr == nil {
			retErr = fmt.Errorf("remove temp key file: %w", rerr)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write key file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close key file: %w", err)
	}
	if err := root.Link(tmp, name); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrServiceAccountKeyExists, k.keyID)
		}
		return fmt.Errorf("store key file: %w", err)
	}
	return nil
}

// updateKeyFile applies change to k's file under the lock and refreshes k.
func updateKeyFile(ctx context.Context, k *ServiceAccountKey, change func(*serviceAccountKeyFile)) error {
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		return rewriteKeyFile(root, k, change)
	})
}

// rewriteKeyFile re-reads k's file, applies change, writes it back atomically
// and copies the result into k. The private key and id are never changed.
func rewriteKeyFile(root *os.Root, k *ServiceAccountKey, change func(*serviceAccountKeyFile)) error {
	current, err := readKeyFile(root, k.keyID)
	if err != nil {
		return err
	}
	data, err := encodeKeyFile(current)
	if err != nil {
		return err
	}
	var f serviceAccountKeyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("decode key file: %w", err)
	}
	change(&f)
	updated, err := decodeKeyFile(f)
	if err != nil {
		return err
	}
	data, err = encodeKeyFile(updated)
	if err != nil {
		return err
	}
	if err := jsonutil.WriteFileAtomicIn(root, ServiceAccountKeyFileName(k.keyID), data, 0o600); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}
	*k = *updated
	return nil
}

// keyPermsWarnW receives the loose-permission warning; a var for tests.
var (
	keyPermsWarnW io.Writer = os.Stderr
	keyPermsWarn  sync.Map
)

func readKeyFile(root *os.Root, keyID string) (*ServiceAccountKey, error) {
	name := ServiceAccountKeyFileName(keyID)
	// Like the file token store: warn, do not refuse, when the key is readable
	// by group/others. A provisioned file may carry a mode the user cannot
	// change, and refusing would also block `key revoke`. Unix-only: Windows
	// reports synthetic modes.
	if runtime.GOOS != "windows" {
		if info, err := root.Lstat(name); err == nil && info.Mode().Perm()&0o077 != 0 {
			if _, warned := keyPermsWarn.LoadOrStore(keyID, true); !warned {
				fmt.Fprintf(keyPermsWarnW, "Warning: service-account key file %s is accessible by group/others (mode %04o) and holds a private key; run: chmod 0600 on it\n", name, info.Mode().Perm())
			}
		}
	}
	data, err := osroot.ReadFileNoFollow(root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // callers test for fs.ErrNotExist
		}
		return nil, fmt.Errorf("read key %s: %w", keyID, err)
	}
	var f serviceAccountKeyFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse key %s: %w", keyID, err)
	}
	if f.KeyID != keyID {
		return nil, fmt.Errorf("key file %s holds key %q", name, f.KeyID)
	}
	return decodeKeyFile(f)
}

func decodeKeyFile(f serviceAccountKeyFile) (*ServiceAccountKey, error) {
	if f.Type != serviceAccountKeyType {
		return nil, fmt.Errorf("key %s: not a service-account key file", f.KeyID)
	}
	if f.ServiceAccount != "" {
		if _, err := ParseServiceAccountID(f.ServiceAccount); err != nil {
			return nil, fmt.Errorf("key %s: %w", f.KeyID, err)
		}
	}
	server, err := parseStoredServer(f.Server)
	if err != nil {
		return nil, fmt.Errorf("key %s: %w", f.KeyID, err)
	}
	block, _ := pem.Decode([]byte(f.PrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("key %s has no PEM private key", f.KeyID)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key %s: %w", f.KeyID, err)
	}
	signer, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key %s is not an Ed25519 key", f.KeyID)
	}
	pub, ok := signer.Public().(ed25519.PublicKey)
	if !ok || ed25519Thumbprint(pub) != f.KeyID {
		return nil, fmt.Errorf("key %s does not match its key_id", f.KeyID)
	}
	k := &ServiceAccountKey{
		keyID:          f.KeyID,
		server:         server,
		serviceAccount: f.ServiceAccount,
		name:           f.Name,
		createdAt:      f.CreatedAt,
		enrollmentID:   f.EnrollmentID,
		signer:         signer,
	}
	if f.EnrollmentExpiresAt != nil {
		k.enrollmentExpiresAt = *f.EnrollmentExpiresAt
	}
	if f.RetiredAt != nil {
		k.retiredAt = *f.RetiredAt
	}
	return k, nil
}
