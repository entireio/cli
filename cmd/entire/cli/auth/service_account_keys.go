package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/tuiutil"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/internal/entireclient/httpclient"
)

// Key-management calls a machine makes with its own key: enrolling it (an
// admin approves in the web UI, no public key is copied around) and revoking
// it. Each request carries a proof signed by the key, bound to the exact
// endpoint URL, the HTTP method and the request body, so a captured proof is
// useless for any other call.
//
// PROTOTYPE: these endpoints are proposed, not implemented in entire-core.
// The contract, including the idempotency and status rules the client relies
// on below, is docs/architecture/service-account-keys.md.
//
//	POST /api/v1/service-account-enrollments                         (idempotent by key_id)
//	POST /api/v1/service-account-enrollments/{enrollment_id}/poll
//	POST /api/v1/service-account-enrollments/{enrollment_id}/cancel
//	POST /api/v1/service-accounts/{svc}/keys/{key_id}/revoke
//
// Everything that can delete a key from disk is decided here, and only on a
// definitive answer: a lost response, a 5xx or an unknown enrollment that may
// still be live never deletes a key the server might trust.

const (
	enrollmentsPath     = "/api/v1/service-account-enrollments"
	keyAPIRequestTimout = 30 * time.Second
	maxKeyAPIBody       = 64 << 10
	maxServerErrorRunes = 300

	// Bounds on the server's enrollment timing. Values outside them are
	// clamped, so a hostile or broken response cannot make the box wait for
	// hours or overflow a time.Duration. maxEnrollmentLifetime is also the
	// contract's upper bound on how long the server keeps an enrollment open.
	defaultEnrollmentInterval = 5 * time.Second
	maxEnrollmentInterval     = time.Minute
	defaultEnrollmentLifetime = 15 * time.Minute
	maxEnrollmentLifetime     = time.Hour

	// enrollmentExpirySkew is how long past its expiry an enrollment must be
	// before the client treats "unknown" or "still pending" as expired.
	enrollmentExpirySkew = 5 * time.Minute
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

// Definitive enrollment outcomes: the enrollment can never be approved, so
// its key may be deleted (ForgetEnrollment accepts only these).
var (
	ErrEnrollmentDenied    = errors.New("the enrollment request was denied")
	ErrEnrollmentExpired   = errors.New("the enrollment request expired before it was approved")
	ErrEnrollmentRejected  = errors.New("the server refused the enrollment request")
	ErrEnrollmentCancelled = errors.New("the enrollment request was cancelled")
)

// ErrEnrollmentAlreadyApproved: a cancel lost the race with an approval. The
// key is trusted on the server and must be kept (and bound).
var ErrEnrollmentAlreadyApproved = errors.New("the enrollment was approved before it could be cancelled")

// IsDefinitiveEnrollmentOutcome reports whether err means the enrollment can
// never be approved, so its key may be forgotten.
func IsDefinitiveEnrollmentOutcome(err error) bool {
	return isDefinitiveEnrollmentOutcome(err)
}

func isDefinitiveEnrollmentOutcome(err error) bool {
	return errors.Is(err, ErrEnrollmentDenied) || errors.Is(err, ErrEnrollmentExpired) ||
		errors.Is(err, ErrEnrollmentRejected) || errors.Is(err, ErrEnrollmentCancelled)
}

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
// SavePendingEnrollmentKey before SubmitServiceAccountEnrollment, so the
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

// SubmitServiceAccountEnrollment submits (or, for a key already submitting,
// resubmits) k's public key for approval and records the server's answer on
// the key. The server deduplicates by key id, so resubmitting after a lost
// response returns the same enrollment. Errors: ErrEnrollmentRejected when
// the server definitively refused (nothing was created), otherwise the
// outcome is unknown and the key stays submitting for --resume or --cancel.
func SubmitServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey) (*ServiceAccountEnrollment, error) {
	if k.enrollment.phase != EnrollmentSubmitting && k.enrollment.phase != EnrollmentPending {
		return nil, fmt.Errorf("key %s has no enrollment to submit", k.keyID)
	}
	pub, err := k.PublicKeyPEM()
	if err != nil {
		return nil, err
	}
	endpoint := k.server + enrollmentsPath
	status, body, err := postKeyAPI(ctx, k, k.keyID, endpoint, map[string]string{
		"public_key":      pub,
		"key_id":          k.keyID,
		"service_account": k.serviceAccount,
		"name":            k.name,
	})
	if err != nil {
		return nil, fmt.Errorf("submit enrollment to %s (outcome unknown, the key was kept): %w", k.server, err)
	}
	switch {
	case status == http.StatusOK || status == http.StatusCreated:
	case isDefinitiveRefusal(status):
		return nil, fmt.Errorf("%w: %s", ErrEnrollmentRejected, describeStatus(status, body))
	default:
		return nil, fmt.Errorf("submit enrollment to %s (outcome unknown, the key was kept): %s", k.server, describeStatus(status, body))
	}
	var out ServiceAccountEnrollment
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("submit enrollment to %s (outcome unknown, the key was kept): decode response: %w", k.server, err)
	}
	if err := validateEnrollment(&out); err != nil {
		return nil, fmt.Errorf("submit enrollment to %s (outcome unknown, the key was kept): %w", k.server, err)
	}
	if err := RecordEnrollment(ctx, k, &out); err != nil {
		return nil, fmt.Errorf("record enrollment %s (the key was kept; --resume resubmits): %w", out.ID, err)
	}
	return &out, nil
}

// isDefinitiveRefusal: a 4xx means the request was refused and nothing was
// created, except the statuses that mean "try again" (timeout, too early,
// rate-limited), where the server may not have looked at it at all.
func isDefinitiveRefusal(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
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

// enrollmentLapsed reports whether k's enrollment is certainly over on the
// server: past its recorded expiry plus skew, or, while still submitting (no
// expiry known), past the contract's maximum lifetime since the key was made.
func enrollmentLapsed(k *ServiceAccountKey, now time.Time) bool {
	deadline := k.enrollment.expiresAt
	if deadline.IsZero() {
		deadline = k.createdAt.Add(maxEnrollmentLifetime)
	}
	return now.After(deadline.Add(enrollmentExpirySkew))
}

type pollResult struct {
	Status         string `json:"status"`
	ServiceAccount string `json:"service_account"`
}

// pollEnrollment asks for k's enrollment once.
func pollEnrollment(ctx context.Context, k *ServiceAccountKey) (pollResult, error) {
	endpoint := k.server + enrollmentsPath + "/" + url.PathEscape(k.enrollment.id) + "/poll"
	status, body, err := postKeyAPI(ctx, k, k.keyID, endpoint, map[string]string{})
	if err != nil {
		return pollResult{}, fmt.Errorf("poll enrollment: %w", err)
	}
	if status == http.StatusNotFound {
		if enrollmentLapsed(k, nowFunc()) {
			return pollResult{}, ErrEnrollmentExpired
		}
		return pollResult{}, errors.New("poll enrollment: the server does not know this enrollment; the key was kept, retry with --resume")
	}
	if status != http.StatusOK {
		return pollResult{}, fmt.Errorf("poll enrollment: %s", describeStatus(status, body))
	}
	var out pollResult
	if err := json.Unmarshal(body, &out); err != nil {
		return pollResult{}, fmt.Errorf("poll enrollment: decode response: %w", err)
	}
	return out, nil
}

// decidePoll maps a poll answer to the approved account, a definitive
// outcome, or (done=false) "keep waiting".
func decidePoll(r pollResult) (approved string, done bool, err error) {
	switch r.Status {
	case "pending":
		return "", false, nil
	case "approved":
		id, err := ParseServiceAccountID(r.ServiceAccount)
		if err != nil {
			return "", true, fmt.Errorf("enrollment approved with an invalid service account: %w", err)
		}
		return id, true, nil
	case "denied":
		return "", true, ErrEnrollmentDenied
	case "expired":
		return "", true, ErrEnrollmentExpired
	default:
		return "", true, fmt.Errorf("poll enrollment: unknown status %q", tuiutil.SanitizeDisplayText(r.Status))
	}
}

// WaitForServiceAccountEnrollment polls k's recorded enrollment at its stored
// interval until it is decided and returns the approved service account.
// Neither a sleep nor a request runs past the enrollment's expiry; at expiry
// one last poll settles the outcome, and anything short of a definitive
// answer is returned as a non-definitive error so the key is kept.
func WaitForServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey) (string, error) {
	if k.enrollment.phase != EnrollmentPending || k.enrollment.id == "" || k.enrollment.expiresAt.IsZero() {
		return "", fmt.Errorf("key %s has no recorded enrollment to wait for", k.keyID)
	}
	interval := k.enrollment.interval
	if interval <= 0 {
		interval = defaultEnrollmentInterval
	}
	interval = min(interval, maxEnrollmentInterval)
	deadline := k.enrollment.expiresAt
	for {
		remaining := deadline.Sub(nowFunc())
		if remaining <= 0 {
			break
		}
		if err := enrollmentSleep(ctx, min(interval, remaining)); err != nil {
			return "", err
		}
		if !nowFunc().Before(deadline) {
			break
		}
		pollCtx, cancel := context.WithDeadline(ctx, deadline)
		r, err := pollEnrollment(pollCtx, k)
		expired := errors.Is(pollCtx.Err(), context.DeadlineExceeded)
		cancel()
		if ctx.Err() != nil {
			return "", fmt.Errorf("wait for enrollment: %w", ctx.Err())
		}
		if err != nil {
			if expired {
				break // the request ran into the expiry; settle it below
			}
			return "", err
		}
		if approved, done, err := decidePoll(r); done {
			return approved, err
		}
	}
	// At expiry: one final, separately bounded poll. An approval that landed
	// at the last moment still binds; "pending" or no answer is not proof of
	// expiry until the skew has passed.
	r, err := pollEnrollment(ctx, k)
	if err != nil {
		return "", err
	}
	approved, done, err := decidePoll(r)
	if done {
		return approved, err
	}
	if enrollmentLapsed(k, nowFunc()) {
		return "", ErrEnrollmentExpired
	}
	return "", errors.New("the enrollment is still pending at its expiry; the key was kept, retry with --resume in a few minutes")
}

// CancelServiceAccountEnrollment withdraws k's enrollment. It returns
// ErrEnrollmentCancelled, ErrEnrollmentDenied, ErrEnrollmentExpired or
// ErrEnrollmentRejected when the enrollment can never be approved (the key may
// then be forgotten), ErrEnrollmentAlreadyApproved with the account when the
// approval won the race (the key must be kept and bound), and any other error
// when the outcome is unknown (the key must be kept).
func CancelServiceAccountEnrollment(ctx context.Context, k *ServiceAccountKey) (approved string, err error) {
	switch k.enrollment.phase {
	case EnrollmentApproved:
		return k.serviceAccount, ErrEnrollmentAlreadyApproved
	case EnrollmentSubmitting:
		// Learn the enrollment id first; resubmitting is idempotent.
		if _, err := SubmitServiceAccountEnrollment(ctx, k); err != nil {
			if !errors.Is(err, ErrEnrollmentRejected) && enrollmentLapsed(k, nowFunc()) {
				return "", ErrEnrollmentExpired
			}
			return "", err
		}
	case EnrollmentPending:
	default:
		return "", fmt.Errorf("key %s has no enrollment to cancel", k.keyID)
	}
	endpoint := k.server + enrollmentsPath + "/" + url.PathEscape(k.enrollment.id) + "/cancel"
	status, body, err := postKeyAPI(ctx, k, k.keyID, endpoint, map[string]string{})
	if err != nil {
		return "", fmt.Errorf("cancel enrollment (outcome unknown, the key was kept): %w", err)
	}
	switch status {
	case http.StatusOK, http.StatusNoContent:
		return "", ErrEnrollmentCancelled
	case http.StatusConflict:
		var decided pollResult
		if err := json.Unmarshal(body, &decided); err != nil {
			return "", fmt.Errorf("cancel enrollment: decode conflict: %w", err)
		}
		approved, done, err := decidePoll(decided)
		switch {
		case !done:
			return "", errors.New("cancel enrollment: the server reported a conflict but the enrollment is still pending; the key was kept")
		case err == nil:
			return approved, ErrEnrollmentAlreadyApproved
		default:
			return "", err
		}
	case http.StatusNotFound:
		if enrollmentLapsed(k, nowFunc()) {
			return "", ErrEnrollmentExpired
		}
		return "", errors.New("cancel enrollment: the server does not know this enrollment; the key was kept until the enrollment has certainly expired")
	default:
		return "", fmt.Errorf("cancel enrollment (outcome unknown, the key was kept): %s", describeStatus(status, body))
	}
}

// ErrServiceAccountKeyUnknown means the server no longer knows the key:
// revoked earlier, or never registered.
var ErrServiceAccountKeyUnknown = errors.New("the server does not know this key (already revoked, or never registered)")

// RevokeServiceAccountKey revokes k on the server using k itself as the
// credential. It deletes nothing locally; see DeleteServiceAccountKey. A key
// whose enrollment is undecided is refused: it is cancelled, not revoked.
func RevokeServiceAccountKey(ctx context.Context, k *ServiceAccountKey) error {
	status, err := ServiceAccountKeyStatusOf(k)
	if err != nil {
		return err
	}
	if status == KeyEnrolling {
		return fmt.Errorf("key %s: %w", k.keyID, ErrKeyEnrolling)
	}
	endpoint := k.server + "/api/v1/service-accounts/" + url.PathEscape(k.serviceAccount) + "/keys/" + url.PathEscape(k.keyID) + "/revoke"
	code, body, err := postKeyAPI(ctx, k, k.serviceAccount, endpoint, map[string]string{})
	if err != nil {
		return fmt.Errorf("revoke key %s at %s: %w", k.keyID, k.server, err)
	}
	switch code {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrServiceAccountKeyUnknown
	default:
		return fmt.Errorf("revoke key %s at %s: %s", k.keyID, k.server, describeStatus(code, body))
	}
}

// keyProofBodyHash is the "bh" claim: base64url SHA-256 of the request body
// without its proof, serialised as sorted-key JSON without HTML escaping
// (RFC 8785 JCS for these string-only objects).
func keyProofBodyHash(fields map[string]string) (string, error) {
	canonical, err := jsonutil.MarshalWithNoHTMLEscape(fields)
	if err != nil {
		return "", fmt.Errorf("encode request body: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// postKeyAPI signs a proof for exactly this call (aud = endpoint, htm = POST,
// bh = hash of fields), POSTs fields plus the proof as JSON, and returns the
// status and body for the caller to classify. err is only for failures where
// no status was received (network, timeout, a refused redirect), i.e. the
// outcome is unknown. Redirects are never followed: a 307/308 would resend
// the signed proof to wherever the response points.
func postKeyAPI(ctx context.Context, k *ServiceAccountKey, subject, endpoint string, fields map[string]string) (int, []byte, error) {
	if err := requireKeyTransport(endpoint); err != nil {
		return 0, nil, err
	}
	bh, err := keyProofBodyHash(fields)
	if err != nil {
		return 0, nil, err
	}
	proof, err := k.signAssertion(nowFunc(), subject, endpoint, map[string]any{"htm": http.MethodPost, "bh": bh})
	if err != nil {
		return 0, nil, err
	}
	body := make(map[string]string, len(fields)+1)
	for name, v := range fields {
		body[name] = v
	}
	body["proof"] = proof
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("encode request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, keyAPIRequestTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
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
		return 0, nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, nil, fmt.Errorf("refusing to follow a %d redirect for a key-signed request", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyAPIBody))
	if err != nil {
		return 0, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// describeStatus renders an unexpected status with the server's error text,
// sanitised.
func describeStatus(status int, body []byte) string {
	var apiErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &apiErr) //nolint:errcheck // best-effort detail
	if detail := sanitizeServerText(apiErr.Error + " " + apiErr.Message); detail != "" {
		return fmt.Sprintf("status %d: %s", status, detail)
	}
	return fmt.Sprintf("status %d", status)
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
