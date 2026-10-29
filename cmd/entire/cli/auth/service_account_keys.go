package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/httpclient"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// Key-management calls a machine makes with its own key: enrolling it (an
// admin approves in the web UI, no public key is copied around) and revoking
// it. Each request carries a proof — an assertion signed by the key whose aud
// is that exact endpoint URL — so a captured proof is useless anywhere else.
//
// PROTOTYPE: these endpoints are proposed, not implemented in entire-core.
//
//	POST /api/v1/service-account-enrollments
//	     {public_key, key_id, service_account?, name, proof}
//	  -> {enrollment_id, user_code, verification_uri, verification_uri_complete, expires_in, interval}
//	POST /api/v1/service-account-enrollments/{enrollment_id}/poll   {proof}
//	  -> {status: pending|approved|denied|expired, service_account}
//	POST /api/v1/service-accounts/{svc}/keys/{key_id}/revoke         {proof}
//	  -> 204; 404 when the key is unknown or already revoked
//
// Enrollment proofs use the key id as iss/sub (no account yet); revoke proofs
// use the service account. A key may revoke only itself; revoking other keys
// is an admin action.

const (
	enrollmentsPath     = "/api/v1/service-account-enrollments"
	keyAPIRequestTimout = 30 * time.Second
	maxKeyAPIBody       = 64 << 10
)

// ServiceAccountEnrollment is what the box shows while it waits for approval.
type ServiceAccountEnrollment struct {
	ID                      string `json:"enrollment_id"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

var (
	enrollmentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	userCodePattern     = regexp.MustCompile(`^[A-Z0-9-]{4,16}$`)
)

// ErrEnrollmentDenied and ErrEnrollmentExpired end the wait without a key.
var (
	ErrEnrollmentDenied  = errors.New("the enrollment request was denied")
	ErrEnrollmentExpired = errors.New("the enrollment request expired before it was approved")
)

// enrollmentSleep waits between polls; a var so tests do not sleep.
var enrollmentSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SkipEnrollmentWaitForTest makes enrollment polling immediate, returning a
// restore closure — the package's set/restore convention for test seams.
func SkipEnrollmentWaitForTest(t interface{ Helper() }) func() {
	t.Helper()
	prev := enrollmentSleep
	enrollmentSleep = func(context.Context, time.Duration) error { return nil }
	return func() { enrollmentSleep = prev }
}

// NewEnrollmentKey generates a key to enroll on server. It is held in memory
// until approval: an abandoned enrollment leaves nothing on disk.
func NewEnrollmentKey(server string) (*ServiceAccountKey, error) {
	return generateUnboundKey(server, nowFunc())
}

// StartServiceAccountEnrollment submits k's public key for approval.
// serviceAccount may be empty, letting the approving admin choose (or create)
// the account; name is a label shown to the approver, e.g. the hostname.
func StartServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey, serviceAccount, name string) (*ServiceAccountEnrollment, error) {
	if serviceAccount != "" {
		id, err := ParseServiceAccountID(serviceAccount)
		if err != nil {
			return nil, err
		}
		serviceAccount = id
	}
	pub, err := k.PublicKeyPEM()
	if err != nil {
		return nil, err
	}
	endpoint := k.Server + enrollmentsPath
	proof, err := k.signAssertion(nowFunc(), k.KeyID, endpoint)
	if err != nil {
		return nil, err
	}
	var out ServiceAccountEnrollment
	status, err := postKeyAPI(ctx, endpoint, map[string]string{
		"public_key":      pub,
		"key_id":          k.KeyID,
		"service_account": serviceAccount,
		"name":            name,
		"proof":           proof,
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("start enrollment at %s: %w", k.Server, err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("start enrollment at %s: unexpected status %d", k.Server, status)
	}
	if err := validateEnrollment(&out); err != nil {
		return nil, fmt.Errorf("start enrollment at %s: %w", k.Server, err)
	}
	return &out, nil
}

// validateEnrollment refuses a response the box would print or put in a URL
// path unchecked: the codes and links are shown to a human who acts on them.
func validateEnrollment(e *ServiceAccountEnrollment) error {
	if !enrollmentIDPattern.MatchString(e.ID) {
		return errors.New("server returned a malformed enrollment id")
	}
	if !userCodePattern.MatchString(e.UserCode) {
		return errors.New("server returned a malformed user code")
	}
	links := []string{e.VerificationURI}
	if e.VerificationURIComplete != "" { // optional, like RFC 8628's
		links = append(links, e.VerificationURIComplete)
	}
	for _, raw := range links {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || strings.ContainsFunc(raw, isControlRune) {
			return fmt.Errorf("server returned an unusable approval link %q", raw)
		}
	}
	if e.Interval < 1 {
		e.Interval = 5
	}
	if e.ExpiresIn < 1 {
		e.ExpiresIn = 900
	}
	return nil
}

func isControlRune(r rune) bool { return r < 0x20 || r == 0x7f }

// WaitForServiceAccountEnrollment polls until the enrollment is decided and
// returns the approved service account. It honours ctx and the server's
// interval and gives up at the enrollment's expiry.
func WaitForServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey, e *ServiceAccountEnrollment) (string, error) {
	endpoint := k.Server + enrollmentsPath + "/" + url.PathEscape(e.ID) + "/poll"
	deadline := nowFunc().Add(time.Duration(e.ExpiresIn) * time.Second)
	interval := time.Duration(e.Interval) * time.Second
	for {
		if err := enrollmentSleep(ctx, interval); err != nil {
			return "", err
		}
		proof, err := k.signAssertion(nowFunc(), k.KeyID, endpoint)
		if err != nil {
			return "", err
		}
		var out struct {
			Status         string `json:"status"`
			ServiceAccount string `json:"service_account"`
		}
		status, err := postKeyAPI(ctx, endpoint, map[string]string{"proof": proof}, &out)
		if err != nil {
			return "", fmt.Errorf("poll enrollment: %w", err)
		}
		switch {
		case status == http.StatusNotFound || out.Status == "expired":
			return "", ErrEnrollmentExpired
		case status != http.StatusOK:
			return "", fmt.Errorf("poll enrollment: unexpected status %d", status)
		case out.Status == "denied":
			return "", ErrEnrollmentDenied
		case out.Status == "approved":
			id, err := ParseServiceAccountID(out.ServiceAccount)
			if err != nil {
				return "", fmt.Errorf("enrollment approved with an invalid service account: %w", err)
			}
			return id, nil
		case out.Status != "pending":
			return "", fmt.Errorf("poll enrollment: unknown status %q", out.Status)
		}
		if !nowFunc().Before(deadline) {
			return "", ErrEnrollmentExpired
		}
	}
}

// BindEnrolledKey ties an approved key to its service account and stores it.
func BindEnrolledKey(k *ServiceAccountKey, serviceAccount string, overwrite bool) (string, error) {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return "", err
	}
	k.ServiceAccount = id
	return SaveServiceAccountKey(k, overwrite)
}

// ErrServiceAccountKeyUnknown means the server no longer knows the key:
// revoked earlier, or never registered.
var ErrServiceAccountKeyUnknown = errors.New("the server does not know this key (already revoked, or never registered)")

// RevokeServiceAccountKey revokes k on the server using k itself as the
// credential. It deletes nothing locally; see DeleteServiceAccountKey.
func RevokeServiceAccountKey(ctx context.Context, k *ServiceAccountKey) error {
	endpoint := k.Server + "/api/v1/service-accounts/" + url.PathEscape(k.ServiceAccount) + "/keys/" + url.PathEscape(k.KeyID) + "/revoke"
	proof, err := k.signAssertion(nowFunc(), k.ServiceAccount, endpoint)
	if err != nil {
		return err
	}
	status, err := postKeyAPI(ctx, endpoint, map[string]string{"proof": proof}, nil)
	if err != nil {
		return fmt.Errorf("revoke key %s at %s: %w", k.KeyID, k.Server, err)
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrServiceAccountKeyUnknown
	default:
		return fmt.Errorf("revoke key %s at %s: unexpected status %d", k.KeyID, k.Server, status)
	}
}

// DeleteServiceAccountKey removes the stored key and its cached session.
// Missing files are not an error.
func DeleteServiceAccountKey(serviceAccount string) error {
	id, err := ParseServiceAccountID(serviceAccount)
	if err != nil {
		return err
	}
	if root, err := userdirs.CacheRoot(); err == nil {
		if err := root.Remove(serviceAccountTokenCacheName(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove cached session: %w", err)
		}
	}
	root, err := userdirs.ConfigRoot()
	if err != nil {
		return fmt.Errorf("open config directory: %w", err)
	}
	if err := root.Remove(serviceAccountKeyFileName(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove key file: %w", err)
	}
	return nil
}

// postKeyAPI POSTs body as JSON and decodes a 2xx JSON response into out (when
// non-nil). Non-2xx statuses are returned for the caller to classify, with
// the server's error text in err only when it is not a status the caller
// handles (404).
func postKeyAPI(ctx context.Context, endpoint string, body any, out any) (int, error) {
	if !strings.HasPrefix(endpoint, schemeHTTPS+"://") && !isLoopbackHTTP(endpoint) && !insecureHTTPEnabled() {
		return 0, fmt.Errorf("refusing to send a key proof over plain HTTP to %s", endpoint)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, keyAPIRequestTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := httpclient.New(httpclient.Options{Timeout: keyAPIRequestTimout, Transport: versioninfo.WrapTransport(nil)}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyAPIBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, nil
		}
		var apiErr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &apiErr) //nolint:errcheck // best-effort detail
		detail := strings.Map(func(r rune) rune {
			if isControlRune(r) {
				return -1
			}
			return r
		}, strings.TrimSpace(apiErr.Error+" "+apiErr.Message))
		if detail != "" {
			return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, detail)
		}
		return resp.StatusCode, nil
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}
