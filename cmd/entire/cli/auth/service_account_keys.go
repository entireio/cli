package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/httpclient"
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
//	POST /api/v1/service-account-enrollments/{enrollment_id}/cancel {proof}
//	  -> 204; 404 when the enrollment is unknown or already decided
//	POST /api/v1/service-accounts/{svc}/keys/{key_id}/revoke         {proof}
//	  -> 204; 404 when the key is unknown or already revoked
//
// Enrollment proofs use the key id as iss/sub (no account yet); revoke proofs
// use the service account. A key may revoke only itself; revoking other keys
// is an admin action. The full contract, including what the server must
// enforce, is docs/architecture/service-account-keys.md.

const (
	enrollmentsPath     = "/api/v1/service-account-enrollments"
	keyAPIRequestTimout = 30 * time.Second
	maxKeyAPIBody       = 64 << 10
	maxServerErrorRunes = 300

	// Bounds on the server's enrollment timing. Values outside them are
	// clamped, so a hostile or broken response cannot make the box wait for
	// hours or overflow a time.Duration.
	defaultEnrollmentInterval = 5 * time.Second
	maxEnrollmentInterval     = time.Minute
	defaultEnrollmentLifetime = 15 * time.Minute
	maxEnrollmentLifetime     = time.Hour
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
	return SetEnrollmentSleepForTest(t, func(context.Context, time.Duration) error { return nil })
}

// SetEnrollmentSleepForTest replaces the wait between polls (for example to
// simulate an interrupt), returning a restore closure.
func SetEnrollmentSleepForTest(t interface{ Helper() }, sleep func(context.Context, time.Duration) error) func() {
	t.Helper()
	prev := enrollmentSleep
	enrollmentSleep = sleep
	return func() { enrollmentSleep = prev }
}

// NewEnrollmentKey generates a key to enroll on server, proposing
// serviceAccount ("" lets the approver choose). The caller stores it with
// SavePendingEnrollmentKey before StartServiceAccountEnrollment, so the
// server never learns a public key whose private half is not on disk.
func NewEnrollmentKey(server, serviceAccount, name string) (*ServiceAccountKey, error) {
	if serviceAccount != "" {
		id, err := ParseServiceAccountID(serviceAccount)
		if err != nil {
			return nil, err
		}
		serviceAccount = id
	}
	k, err := generateUnboundKey(server, name, nowFunc())
	if err != nil {
		return nil, err
	}
	k.serviceAccount = serviceAccount
	return k, nil
}

// StartServiceAccountEnrollment submits k's public key for approval, proposing
// k's service account when it has one. The returned timing is already
// clamped to sane bounds.
func StartServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey) (*ServiceAccountEnrollment, error) {
	pub, err := k.PublicKeyPEM()
	if err != nil {
		return nil, err
	}
	endpoint := k.server + enrollmentsPath
	proof, err := k.signAssertion(nowFunc(), k.keyID, endpoint)
	if err != nil {
		return nil, err
	}
	var out ServiceAccountEnrollment
	status, err := postKeyAPI(ctx, endpoint, map[string]string{
		"public_key":      pub,
		"key_id":          k.keyID,
		"service_account": k.serviceAccount,
		"name":            k.name,
		"proof":           proof,
	}, &out)
	if err != nil {
		return nil, fmt.Errorf("start enrollment at %s: %w", k.server, err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return nil, fmt.Errorf("start enrollment at %s: unexpected status %d", k.server, status)
	}
	if err := validateEnrollment(&out); err != nil {
		return nil, fmt.Errorf("start enrollment at %s: %w", k.server, err)
	}
	return &out, nil
}

// validateEnrollment refuses a response the box would print or put in a URL
// path unchecked: the codes and links are shown to a human who acts on them.
// Links must be printable ASCII (an IDN host arrives as punycode), which
// rules out control, C1, bidi and other format characters in one test.
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
		if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil || len(raw) > 2048 || strings.ContainsFunc(raw, notPrintableASCII) {
			return fmt.Errorf("server returned an unusable approval link %q", tuiutil.SanitizeDisplayText(raw))
		}
	}
	e.Interval = int(clampSeconds(e.Interval, defaultEnrollmentInterval, maxEnrollmentInterval) / time.Second)
	e.ExpiresIn = int(clampSeconds(e.ExpiresIn, defaultEnrollmentLifetime, maxEnrollmentLifetime) / time.Second)
	return nil
}

// clampSeconds converts a server-supplied seconds count to a duration within
// (0, upper], substituting def for a missing or non-positive value. It
// compares before multiplying, so no input can overflow.
func clampSeconds(v int, def, upper time.Duration) time.Duration {
	switch {
	case v < 1:
		return def
	case int64(v) > int64(upper/time.Second):
		return upper
	default:
		return time.Duration(v) * time.Second
	}
}

func notPrintableASCII(r rune) bool { return r < 0x21 || r > 0x7e }

// WaitForServiceAccountEnrollment polls k's recorded enrollment until it is
// decided and returns the approved service account. It never waits past the
// enrollment's expiry: each sleep is cut to the time remaining.
func WaitForServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey, interval time.Duration) (string, error) {
	if k.enrollmentID == "" || k.enrollmentExpiresAt.IsZero() {
		return "", fmt.Errorf("key %s has no recorded enrollment", k.keyID)
	}
	interval = min(max(interval, time.Second), maxEnrollmentInterval)
	endpoint := k.server + enrollmentsPath + "/" + url.PathEscape(k.enrollmentID) + "/poll"
	deadline := k.enrollmentExpiresAt
	for {
		remaining := deadline.Sub(nowFunc())
		if remaining <= 0 {
			return "", ErrEnrollmentExpired
		}
		if err := enrollmentSleep(ctx, min(interval, remaining)); err != nil {
			return "", err
		}
		proof, err := k.signAssertion(nowFunc(), k.keyID, endpoint)
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
			return "", fmt.Errorf("poll enrollment: unknown status %q", tuiutil.SanitizeDisplayText(out.Status))
		}
	}
}

// CancelServiceAccountEnrollment withdraws k's pending enrollment on the
// server. A 404 (already decided or expired) is not an error.
func CancelServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey) error {
	if k.enrollmentID == "" {
		return nil
	}
	endpoint := k.server + enrollmentsPath + "/" + url.PathEscape(k.enrollmentID) + "/cancel"
	proof, err := k.signAssertion(nowFunc(), k.keyID, endpoint)
	if err != nil {
		return err
	}
	status, err := postKeyAPI(ctx, endpoint, map[string]string{"proof": proof}, nil)
	if err != nil {
		return fmt.Errorf("cancel enrollment at %s: %w", k.server, err)
	}
	if status >= 300 && status != http.StatusNotFound {
		return fmt.Errorf("cancel enrollment at %s: unexpected status %d", k.server, status)
	}
	return nil
}

// ErrServiceAccountKeyUnknown means the server no longer knows the key:
// revoked earlier, or never registered.
var ErrServiceAccountKeyUnknown = errors.New("the server does not know this key (already revoked, or never registered)")

// RevokeServiceAccountKey revokes k on the server using k itself as the
// credential. It deletes nothing locally; see DeleteServiceAccountKey.
func RevokeServiceAccountKey(ctx context.Context, k *ServiceAccountKey) error {
	if k.serviceAccount == "" {
		return fmt.Errorf("key %s is not bound to a service account; cancel its enrollment instead", k.keyID)
	}
	endpoint := k.server + "/api/v1/service-accounts/" + url.PathEscape(k.serviceAccount) + "/keys/" + url.PathEscape(k.keyID) + "/revoke"
	proof, err := k.signAssertion(nowFunc(), k.serviceAccount, endpoint)
	if err != nil {
		return err
	}
	status, err := postKeyAPI(ctx, endpoint, map[string]string{"proof": proof}, nil)
	if err != nil {
		return fmt.Errorf("revoke key %s at %s: %w", k.keyID, k.server, err)
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrServiceAccountKeyUnknown
	default:
		return fmt.Errorf("revoke key %s at %s: unexpected status %d", k.keyID, k.server, status)
	}
}

// postKeyAPI POSTs body as JSON and decodes a 2xx JSON response into out (when
// non-nil). Non-2xx statuses are returned for the caller to classify, with
// the server's error text in err only when it is not a status the caller
// handles (404). Redirects are never followed: a 307/308 would resend the
// signed proof to wherever the response points.
func postKeyAPI(ctx context.Context, endpoint string, body any, out any) (int, error) {
	if err := requireKeyTransport(endpoint); err != nil {
		return 0, err
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
	resp, err := httpclient.New(httpclient.Options{
		Timeout:   keyAPIRequestTimout,
		Transport: versioninfo.WrapTransport(nil),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Do(req)
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyAPIBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return resp.StatusCode, fmt.Errorf("refusing to follow a %d redirect for a key-signed request", resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, nil
		}
		var apiErr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &apiErr) //nolint:errcheck // best-effort detail
		detail := sanitizeServerText(apiErr.Error + " " + apiErr.Message)
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

// sanitizeServerText makes server-supplied error text safe to print on one
// line: the shared terminal filter (escapes, control, C1, bidi and other
// format characters), whitespace collapsed, and a length cap.
func sanitizeServerText(s string) string {
	s = strings.Join(strings.Fields(tuiutil.SanitizeTerminalText(s)), " ")
	if r := []rune(s); len(r) > maxServerErrorRunes {
		s = string(r[:maxServerErrorRunes]) + "…"
	}
	return s
}
