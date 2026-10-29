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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// On-disk layout, all in the per-user config directory, 0600, written durably
// (fsync of file and directory) because none of it can be rebuilt:
//
//	service-account-key-<key id>.json        the key itself; created once, never rewritten
//	service-account-key-<key id>.state.json  what the key is for: its account and any
//	                                         enrollment in flight
//	service-account-<svc>.active.json        the account's active key, plus the keys
//	                                         that still have to revoke themselves
//	service-accounts.lock                    held by every change to the above
//
// Every transition is a single atomic file replacement, ordered so a crash at
// any point leaves a state the next command recognises and finishes:
//
//   - A state file is written before its key file, and a key file is deleted
//     before its state file, so a key file never exists without its state.
//   - A key is written to disk before any server learns its public half.
//   - Switching keys moves the old key into the pointer's retiring list in the
//     same write that selects the new one, so the obligation to revoke it is
//     never lost; it leaves the list only after it is revoked and deleted.
//   - An enrolling key can leave disk only once its enrollment is definitively
//     denied, expired, rejected or cancelled (ForgetEnrollment).

const (
	serviceAccountKeyType     = "entire_service_account_key"
	serviceAccountStateType   = "entire_service_account_key_state"
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
	// KeyEnrolling has an enrollment in flight (being submitted, awaiting
	// approval, or approved but not yet bound).
	KeyEnrolling ServiceAccountKeyStatus = "enrolling"
	// KeyInactive is bound to an account but not selected: a staged successor
	// awaiting `key activate`.
	KeyInactive ServiceAccountKeyStatus = "inactive"
	// KeyRetired was replaced and still has to revoke itself on the server.
	KeyRetired ServiceAccountKeyStatus = "retired"
)

// EnrollmentPhase is how far a key's enrollment got.
type EnrollmentPhase string

const (
	// EnrollmentSubmitting: the key is on disk and the create request may or
	// may not have reached the server. Resubmitting is safe: the server
	// deduplicates by key id.
	EnrollmentSubmitting EnrollmentPhase = "submitting"
	// EnrollmentPending: the server returned an enrollment id; awaiting a
	// decision.
	EnrollmentPending EnrollmentPhase = "pending"
	// EnrollmentApproved: approved and bound to its account, but becoming the
	// active key (when the account has none) has not been settled yet.
	EnrollmentApproved EnrollmentPhase = "approved"
)

var (
	// ErrServiceAccountKeyExists: a key file with that id already exists.
	// Generating the same Ed25519 key twice is not possible, so this means the
	// same key was saved twice.
	ErrServiceAccountKeyExists = errors.New("a key with this id is already stored")
	// ErrEnrollmentInProgress: only one enrollment may be in flight per machine.
	ErrEnrollmentInProgress = errors.New("an enrollment is already in progress on this machine")
	// ErrKeyEnrolling: the key's enrollment is undecided, so the server may
	// still come to trust it; it cannot be revoked or deleted, only resumed or
	// cancelled.
	ErrKeyEnrolling = errors.New("this key's enrollment is still in progress; resume it with `entire auth service-account enroll --resume` or withdraw it with `--cancel`")
)

type serviceAccountKeyFile struct {
	Type       string    `json:"type"`
	KeyID      string    `json:"key_id"`
	Server     string    `json:"server"`
	Name       string    `json:"name,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	PrivateKey string    `json:"private_key"`
}

type serviceAccountStateFile struct {
	Type  string `json:"type"`
	KeyID string `json:"key_id"`
	// ServiceAccount is the bound account, the proposed one while enrolling,
	// or empty while an enrollment lets the approver choose.
	ServiceAccount string               `json:"service_account,omitempty"`
	Enrollment     *enrollmentStateFile `json:"enrollment,omitempty"`
}

type enrollmentStateFile struct {
	Phase           EnrollmentPhase `json:"phase"`
	ID              string          `json:"id,omitempty"`
	ExpiresAt       *time.Time      `json:"expires_at,omitempty"`
	IntervalSeconds int             `json:"interval_seconds,omitempty"`
}

type serviceAccountPointerFile struct {
	Type           string   `json:"type"`
	ServiceAccount string   `json:"service_account"`
	KeyID          string   `json:"key_id,omitempty"`
	Retiring       []string `json:"retiring,omitempty"`
}

// ServiceAccountKey is one locally held key. Its private half is never
// exported; the zero value is not a usable key and its methods return errors
// instead of panicking.
type ServiceAccountKey struct {
	keyID          string
	server         string
	name           string
	createdAt      time.Time
	signer         ed25519.PrivateKey
	serviceAccount string
	enrollment     enrollmentState
}

type enrollmentState struct {
	phase     EnrollmentPhase
	id        string
	expiresAt time.Time
	interval  time.Duration
}

func (k *ServiceAccountKey) KeyID() string          { return k.keyID }
func (k *ServiceAccountKey) Server() string         { return k.server }
func (k *ServiceAccountKey) ServiceAccount() string { return k.serviceAccount }
func (k *ServiceAccountKey) Name() string           { return k.name }
func (k *ServiceAccountKey) CreatedAt() time.Time   { return k.createdAt }

// EnrollmentPhase is the key's enrollment progress, "" when none is in flight.
func (k *ServiceAccountKey) EnrollmentPhase() EnrollmentPhase { return k.enrollment.phase }

// EnrollmentID is the server's enrollment id, once known.
func (k *ServiceAccountKey) EnrollmentID() string { return k.enrollment.id }

// EnrollmentExpiresAt is when the enrollment lapses on the server.
func (k *ServiceAccountKey) EnrollmentExpiresAt() time.Time { return k.enrollment.expiresAt }

// EnrollmentInterval is the server's polling interval, kept for --resume.
func (k *ServiceAccountKey) EnrollmentInterval() time.Duration { return k.enrollment.interval }

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

func serviceAccountStateName(keyID string) string {
	return serviceAccountKeyPrefix + keyID + ".state.json"
}

func serviceAccountPointerName(id string) string {
	return "service-account-" + id + ".active.json"
}

// legacyServiceAccountKeyName is the single-file format of the first
// prototype, which kept one key per account. It is not migrated (it never
// shipped outside the prototype branch); its presence only adds a hint.
func legacyServiceAccountKeyName(id string) string {
	return "service-account-" + id + ".json"
}

// storeFault, when set by a test, runs before each named write of a
// multi-step transition and can fail it, simulating a crash at that point.
var storeFault func(step string) error

func faultAt(step string) error {
	if storeFault == nil {
		return nil
	}
	return storeFault(step)
}

// withServiceAccountLock serialises every change to key, state and pointer
// files across processes.
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

// CreateServiceAccountKey stores a new key bound to an account (registered by
// hand). If the account has no active key it becomes the active one;
// otherwise it is stored inactive and ActivateServiceAccountKey switches to
// it, so a working key is never displaced by one the server does not know yet.
func CreateServiceAccountKey(ctx context.Context, k *ServiceAccountKey) (activated bool, err error) {
	if err := k.usable(); err != nil {
		return false, err
	}
	if k.serviceAccount == "" || k.enrollment.phase != "" {
		return false, errors.New("a created key must name its service account and have no enrollment")
	}
	err = withServiceAccountLock(ctx, func(root *os.Root) error {
		if err := writeNewKey(root, k); err != nil {
			return err
		}
		ptr, err := readPointer(root, k.serviceAccount)
		if err != nil || ptr.KeyID != "" {
			return err
		}
		ptr.KeyID = k.keyID
		activated = true
		return writePointer(root, ptr)
	})
	return activated, err
}

// SavePendingEnrollmentKey stores a key before its enrollment is submitted,
// so the server never learns a public key whose private half is not on disk.
// serviceAccount on k is the proposed account, or "" to let the approver
// choose. Refuses while another enrollment is in flight; the check and the
// write happen under one lock.
func SavePendingEnrollmentKey(ctx context.Context, k *ServiceAccountKey) error {
	if err := k.usable(); err != nil {
		return err
	}
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		keys, err := listKeys(root, "")
		if err != nil {
			return err
		}
		for _, s := range keys {
			if s.Status == KeyEnrolling {
				return fmt.Errorf("%w (key %s)", ErrEnrollmentInProgress, s.Key.keyID)
			}
		}
		k.enrollment = enrollmentState{phase: EnrollmentSubmitting}
		return writeNewKey(root, k)
	})
}

// RecordEnrollment notes the server's enrollment id, expiry and polling
// interval on the key so `enroll --resume` can continue exactly where the
// wait stopped.
func RecordEnrollment(ctx context.Context, k *ServiceAccountKey, e *ServiceAccountEnrollment) error {
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		cur, err := readKey(root, k.keyID)
		if err != nil {
			return err
		}
		if cur.enrollment.phase != EnrollmentSubmitting && cur.enrollment.phase != EnrollmentPending {
			return fmt.Errorf("key %s is not waiting for an enrollment id", k.keyID)
		}
		cur.enrollment = enrollmentState{
			phase:     EnrollmentPending,
			id:        e.ID,
			expiresAt: nowFunc().Add(time.Duration(e.ExpiresIn) * time.Second).UTC(),
			interval:  time.Duration(e.Interval) * time.Second,
		}
		if err := writeState(root, cur); err != nil {
			return err
		}
		*k = *cur
		return nil
	})
}

// BindEnrolledKey records the approved account on the key and, when the
// account has no active key here, makes it the active one. Three writes, each
// recoverable: the key is first marked approved (an interrupted bind resumes
// with `enroll --resume`), then the pointer is set, then the enrollment is
// cleared. Replacing a working key stays an explicit ActivateServiceAccountKey.
func BindEnrolledKey(ctx context.Context, k *ServiceAccountKey, serviceAccount string) (activated bool, err error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return false, err
	}
	err = withServiceAccountLock(ctx, func(root *os.Root) error {
		cur, err := readKey(root, k.keyID)
		if err != nil {
			return err
		}
		if cur.enrollment.phase == "" {
			return fmt.Errorf("key %s has no enrollment to bind", k.keyID)
		}
		if cur.enrollment.phase == EnrollmentApproved && cur.serviceAccount != id {
			return fmt.Errorf("key %s was already bound to %s", k.keyID, cur.serviceAccount)
		}
		if err := faultAt("bind:approve"); err != nil {
			return err
		}
		cur.serviceAccount = id
		cur.enrollment.phase = EnrollmentApproved
		if err := writeState(root, cur); err != nil {
			return err
		}
		ptr, err := readPointer(root, id)
		if err != nil {
			return err
		}
		switch ptr.KeyID {
		case "":
			if err := faultAt("bind:pointer"); err != nil {
				return err
			}
			ptr.KeyID = k.keyID
			if err := writePointer(root, ptr); err != nil {
				return err
			}
			activated = true
		case k.keyID:
			activated = true
		}
		if err := faultAt("bind:finish"); err != nil {
			return err
		}
		cur.enrollment = enrollmentState{}
		if err := writeState(root, cur); err != nil {
			return err
		}
		*k = *cur
		return nil
	})
	return activated, err
}

// ActivateServiceAccountKey makes k the account's active key. In the same
// atomic write the previous active key joins the retiring list, so the
// obligation to revoke it survives any crash. It returns every key still
// retiring (including ones left over from an earlier, interrupted
// activation) for the caller to revoke and then delete. Activating the
// already-active key changes nothing and just returns that list.
func ActivateServiceAccountKey(ctx context.Context, k *ServiceAccountKey) (retiring []*ServiceAccountKey, err error) {
	if err := k.usable(); err != nil {
		return nil, err
	}
	err = withServiceAccountLock(ctx, func(root *os.Root) error {
		cur, err := readKey(root, k.keyID)
		if err != nil {
			return err
		}
		if cur.serviceAccount == "" || cur.serviceAccount != k.serviceAccount {
			return fmt.Errorf("key %s does not belong to %s", k.keyID, k.serviceAccount)
		}
		ptr, err := readPointer(root, cur.serviceAccount)
		if err != nil {
			return err
		}
		switch keyStatus(cur, ptr) {
		case KeyEnrolling:
			return fmt.Errorf("key %s: %w", k.keyID, ErrKeyEnrolling)
		case KeyRetired:
			return fmt.Errorf("key %s was retired; create or enroll a new key instead", k.keyID)
		case KeyInactive:
			if err := faultAt("activate:pointer"); err != nil {
				return err
			}
			if ptr.KeyID != "" {
				ptr.Retiring = append(ptr.Retiring, ptr.KeyID)
			}
			ptr.KeyID = k.keyID
			if err := writePointer(root, ptr); err != nil {
				return err
			}
		case KeyActive:
		}
		for _, id := range ptr.Retiring {
			old, err := readKey(root, id)
			if errors.Is(err, fs.ErrNotExist) {
				continue // deleted after its revoke; the next delete tidies the list
			}
			if err != nil {
				return err
			}
			retiring = append(retiring, old)
		}
		return nil
	})
	return retiring, err
}

// DeleteServiceAccountKey removes a key that is active, inactive or retired:
// after a successful revoke, or on an explicit --local-only. It refuses an
// enrolling key (ErrKeyEnrolling): only ForgetEnrollment may delete one. The
// files go first, then the account's pointer stops naming the key; a crash in
// between leaves a pointer entry with no key, which every reader skips.
// Missing files are not an error; any other failure is.
func DeleteServiceAccountKey(ctx context.Context, k *ServiceAccountKey) error {
	if !keyIDPattern.MatchString(k.keyID) {
		return errUnusableKey
	}
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		cur, err := readKey(root, k.keyID)
		if errors.Is(err, fs.ErrNotExist) {
			cur = &ServiceAccountKey{keyID: k.keyID, serviceAccount: k.serviceAccount}
		} else if err != nil {
			return err
		}
		ptr, err := readPointer(root, cur.serviceAccount)
		if err != nil {
			return err
		}
		if cur.signer != nil && keyStatus(cur, ptr) == KeyEnrolling {
			return fmt.Errorf("key %s: %w", k.keyID, ErrKeyEnrolling)
		}
		if err := deleteKeyFiles(root, cur.keyID); err != nil {
			return err
		}
		if cur.serviceAccount == "" || (ptr.KeyID != cur.keyID && !slices.Contains(ptr.Retiring, cur.keyID)) {
			return nil
		}
		if err := faultAt("delete:pointer"); err != nil {
			return err
		}
		if ptr.KeyID == cur.keyID {
			ptr.KeyID = ""
		}
		ptr.Retiring = slices.DeleteFunc(ptr.Retiring, func(id string) bool { return id == cur.keyID })
		return writePointer(root, ptr)
	})
}

// ForgetEnrollment deletes an enrolling key once its enrollment can no longer
// be approved. outcome must be one of the definitive enrollment errors
// (denied, expired, rejected at submission, cancelled); anything else, such
// as a lost response, keeps the key so the enrollment can be resumed.
func ForgetEnrollment(ctx context.Context, k *ServiceAccountKey, outcome error) error {
	if !isDefinitiveEnrollmentOutcome(outcome) {
		return fmt.Errorf("refusing to delete key %s: the enrollment outcome is not definitive (%w)", k.keyID, outcome)
	}
	return withServiceAccountLock(ctx, func(root *os.Root) error {
		cur, err := readKey(root, k.keyID)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		ptr, err := readPointer(root, cur.serviceAccount)
		if err != nil {
			return err
		}
		if keyStatus(cur, ptr) != KeyEnrolling || cur.enrollment.phase == EnrollmentApproved {
			return fmt.Errorf("key %s is no longer an undecided enrollment", k.keyID)
		}
		return deleteKeyFiles(root, cur.keyID)
	})
}

// deleteKeyFiles removes the key file, then its state, then its cached
// session, in that order (see the layout comment).
func deleteKeyFiles(root *os.Root, keyID string) error {
	if err := faultAt("delete:key"); err != nil {
		return err
	}
	if err := root.Remove(ServiceAccountKeyFileName(keyID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove key file: %w", err)
	}
	if err := root.Remove(serviceAccountStateName(keyID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove key state: %w", err)
	}
	return removeServiceAccountSession(keyID)
}

// ServiceAccountKeyStatusOf reads k's current status from disk.
func ServiceAccountKeyStatusOf(k *ServiceAccountKey) (ServiceAccountKeyStatus, error) {
	root, err := configRootForRead()
	if err != nil {
		return "", err
	}
	if root == nil {
		return "", fmt.Errorf("no key %s is stored on this machine", k.keyID)
	}
	cur, err := readKey(root, k.keyID)
	if err != nil {
		return "", err
	}
	ptr, err := readPointer(root, cur.serviceAccount)
	if err != nil {
		return "", err
	}
	return keyStatus(cur, ptr), nil
}

func keyStatus(k *ServiceAccountKey, ptr serviceAccountPointerFile) ServiceAccountKeyStatus {
	switch {
	case k.serviceAccount != "" && ptr.KeyID == k.keyID:
		return KeyActive
	case k.serviceAccount != "" && slices.Contains(ptr.Retiring, k.keyID):
		return KeyRetired
	case k.enrollment.phase != "" || k.serviceAccount == "":
		return KeyEnrolling
	default:
		return KeyInactive
	}
}

// LoadActiveServiceAccountKey reads the key that signs for serviceAccount. It
// takes no lock: every file is only ever replaced atomically.
func LoadActiveServiceAccountKey(serviceAccount string) (*ServiceAccountKey, error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return nil, err
	}
	root, err := configRootForRead()
	if err != nil {
		return nil, err
	}
	var ptr serviceAccountPointerFile
	if root != nil {
		if ptr, err = readPointer(root, id); err != nil {
			return nil, err
		}
	}
	if ptr.KeyID == "" {
		msg := fmt.Sprintf("no active key stored for service account %s; run `entire auth service-account enroll --service-account %s` or `entire auth service-account key create`, or `key activate` if `key list` shows an inactive key", id, id)
		if root != nil {
			if _, err := root.Lstat(legacyServiceAccountKeyName(id)); err == nil {
				msg += fmt.Sprintf(" (%s is a key in the earlier prototype format, which is not read any more; enroll a new key, then delete that file)", legacyServiceAccountKeyName(id))
			}
		}
		return nil, errors.New(msg)
	}
	k, err := readKey(root, ptr.KeyID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("the active key %s for service account %s is missing from disk", ptr.KeyID, id)
	}
	if err != nil {
		return nil, err
	}
	if k.serviceAccount != id {
		return nil, fmt.Errorf("the active key %s for service account %s belongs to %q", ptr.KeyID, id, k.serviceAccount)
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
	k, err := readKey(root, keyID)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no key %s is stored on this machine", keyID)
	}
	return k, err
}

// ListServiceAccountKeys returns the stored keys of serviceAccount (every key,
// including enrollments without an account, when it is ""), oldest first. A
// file that cannot be read is reported rather than skipped.
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
	return listKeys(root, serviceAccount)
}

func listKeys(root *os.Root, serviceAccount string) ([]StoredServiceAccountKey, error) {
	entries, err := osroot.ReadDir(root, ".")
	if err != nil {
		return nil, fmt.Errorf("list config directory: %w", err)
	}
	pointers := map[string]serviceAccountPointerFile{}
	var out []StoredServiceAccountKey
	for _, e := range entries {
		keyID, ok := strings.CutPrefix(e.Name(), serviceAccountKeyPrefix)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		keyID, ok = strings.CutSuffix(keyID, ".json")
		if !ok || !keyIDPattern.MatchString(keyID) {
			continue // state files and temp files
		}
		k, err := readKey(root, keyID)
		if errors.Is(err, fs.ErrNotExist) {
			continue // deleted since the directory was read
		}
		if err != nil {
			return nil, err
		}
		if serviceAccount != "" && k.serviceAccount != serviceAccount {
			continue
		}
		ptr, seen := pointers[k.serviceAccount]
		if !seen {
			if ptr, err = readPointer(root, k.serviceAccount); err != nil {
				return nil, err
			}
			pointers[k.serviceAccount] = ptr
		}
		out = append(out, StoredServiceAccountKey{Key: k, Status: keyStatus(k, ptr)})
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

// readPointer returns the account's pointer, or an empty one (no active key,
// nothing retiring) when there is none.
func readPointer(root *os.Root, id string) (serviceAccountPointerFile, error) {
	empty := serviceAccountPointerFile{Type: serviceAccountPointerType, ServiceAccount: id}
	if id == "" {
		return empty, nil
	}
	data, err := osroot.ReadFileNoFollow(root, serviceAccountPointerName(id))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("read active key of %s: %w", id, err)
	}
	var p serviceAccountPointerFile
	if err := json.Unmarshal(data, &p); err != nil {
		return empty, fmt.Errorf("parse active key of %s: %w", id, err)
	}
	valid := p.Type == serviceAccountPointerType && p.ServiceAccount == id && (p.KeyID == "" || keyIDPattern.MatchString(p.KeyID))
	for _, r := range p.Retiring {
		valid = valid && keyIDPattern.MatchString(r)
	}
	if !valid {
		return empty, fmt.Errorf("active-key file of %s is malformed", id)
	}
	return p, nil
}

// writePointer replaces the account's pointer, or removes it once it names
// nothing at all.
func writePointer(root *os.Root, p serviceAccountPointerFile) error {
	name := serviceAccountPointerName(p.ServiceAccount)
	if p.KeyID == "" && len(p.Retiring) == 0 {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove active key of %s: %w", p.ServiceAccount, err)
		}
		return nil
	}
	data, err := jsonutil.MarshalIndentWithNewline(p, "", "  ")
	if err != nil {
		return fmt.Errorf("encode active key: %w", err)
	}
	if err := jsonutil.WriteFileDurableIn(root, name, data, 0o600); err != nil {
		return fmt.Errorf("write active key of %s: %w", p.ServiceAccount, err)
	}
	return nil
}

// writeNewKey writes k's state, then creates its key file exclusively.
func writeNewKey(root *os.Root, k *ServiceAccountKey) error {
	if err := writeState(root, k); err != nil {
		return err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k.signer)
	if err != nil {
		return fmt.Errorf("encode private key: %w", err)
	}
	data, err := jsonutil.MarshalIndentWithNewline(serviceAccountKeyFile{
		Type:       serviceAccountKeyType,
		KeyID:      k.keyID,
		Server:     k.server,
		Name:       k.name,
		CreatedAt:  k.createdAt,
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode key file: %w", err)
	}
	if err := jsonutil.CreateFileDurableIn(root, ServiceAccountKeyFileName(k.keyID), data, 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrServiceAccountKeyExists, k.keyID)
		}
		return fmt.Errorf("store key file: %w", err)
	}
	return nil
}

func writeState(root *os.Root, k *ServiceAccountKey) error {
	s := serviceAccountStateFile{Type: serviceAccountStateType, KeyID: k.keyID, ServiceAccount: k.serviceAccount}
	if e := k.enrollment; e.phase != "" {
		s.Enrollment = &enrollmentStateFile{Phase: e.phase, ID: e.id, IntervalSeconds: int(e.interval / time.Second)}
		if !e.expiresAt.IsZero() {
			s.Enrollment.ExpiresAt = &e.expiresAt
		}
	}
	data, err := jsonutil.MarshalIndentWithNewline(s, "", "  ")
	if err != nil {
		return fmt.Errorf("encode key state: %w", err)
	}
	if err := jsonutil.WriteFileDurableIn(root, serviceAccountStateName(k.keyID), data, 0o600); err != nil {
		return fmt.Errorf("write key state: %w", err)
	}
	return nil
}

// keyPermsWarnW receives the loose-permission warning; a var for tests.
var (
	keyPermsWarnW io.Writer = os.Stderr
	keyPermsWarn  sync.Map
)

func readKey(root *os.Root, keyID string) (*ServiceAccountKey, error) {
	name := ServiceAccountKeyFileName(keyID)
	// Like the file token store: warn, do not refuse, when the key is readable
	// by group/others. A provisioned file may carry a mode the user cannot
	// change, and refusing would also block `key revoke`. Unix-only: Windows
	// reports synthetic modes.
	if runtime.GOOS != "windows" {
		if info, err := root.Lstat(name); err == nil && info.Mode().Perm()&0o077 != 0 {
			if _, warned := keyPermsWarn.LoadOrStore(keyID, true); !warned {
				fmt.Fprintf(keyPermsWarnW, "Warning: service-account key file %s is accessible by group/others (mode %04o) and holds a private key; run chmod 0600 on it\n", name, info.Mode().Perm())
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
	if f.Type != serviceAccountKeyType || f.KeyID != keyID {
		return nil, fmt.Errorf("key file %s is not service-account key %s", name, keyID)
	}
	k, err := decodeKey(f)
	if err != nil {
		return nil, err
	}
	stateData, err := osroot.ReadFileNoFollow(root, serviceAccountStateName(keyID))
	if err != nil {
		return nil, fmt.Errorf("read state of key %s: %w", keyID, err)
	}
	var s serviceAccountStateFile
	if err := json.Unmarshal(stateData, &s); err != nil {
		return nil, fmt.Errorf("parse state of key %s: %w", keyID, err)
	}
	if s.Type != serviceAccountStateType || s.KeyID != keyID {
		return nil, fmt.Errorf("state file of key %s is malformed", keyID)
	}
	if s.ServiceAccount != "" {
		if _, err := ParseServiceAccountID(s.ServiceAccount); err != nil {
			return nil, fmt.Errorf("key %s: %w", keyID, err)
		}
	}
	k.serviceAccount = s.ServiceAccount
	if e := s.Enrollment; e != nil {
		switch e.Phase {
		case EnrollmentSubmitting, EnrollmentPending, EnrollmentApproved:
		default:
			return nil, fmt.Errorf("key %s: unknown enrollment phase %q", keyID, e.Phase)
		}
		k.enrollment = enrollmentState{phase: e.Phase, id: e.ID, interval: time.Duration(e.IntervalSeconds) * time.Second}
		if e.ExpiresAt != nil {
			k.enrollment.expiresAt = *e.ExpiresAt
		}
	}
	return k, nil
}

func decodeKey(f serviceAccountKeyFile) (*ServiceAccountKey, error) {
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
	return &ServiceAccountKey{keyID: f.KeyID, server: server, name: f.Name, createdAt: f.CreatedAt, signer: signer}, nil
}
