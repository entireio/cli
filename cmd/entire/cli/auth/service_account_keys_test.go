package auth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKeyAPI is the proposed key-management API, verifying every proof the
// way the server would: signature against the enrolled/registered public key,
// aud equal to the exact endpoint URL, iss the key id (enrollment) or the
// service account (revoke).
type fakeKeyAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	publicKey   string
	keyID       string
	pollReplies []string // statuses handed out in order; last one repeats
	polls       int
	interval    int
	expiresIn   int
	revokeCode  int
	revoked     []string
	cancelled   int
	badLink     string
	// onStart runs when the enrollment request arrives, before the reply.
	onStart func()
}

func newFakeKeyAPI(t *testing.T) *fakeKeyAPI {
	t.Helper()
	f := &fakeKeyAPI{t: t, revokeCode: http.StatusNoContent, interval: 1, expiresIn: 600}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKeyAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode body: %v", err)
	}
	endpoint := f.srv.URL + r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case enrollmentsPath:
		f.publicKey, f.keyID = body["public_key"], body["key_id"]
		f.checkProof(body["proof"], f.keyID, endpoint)
		if f.onStart != nil {
			f.onStart()
		}
		link := "https://entire.example/approve"
		if f.badLink != "" {
			link = f.badLink
		}
		f.reply(w, map[string]any{
			"enrollment_id": "enr_1", "user_code": "WXYZ-1234",
			"verification_uri": link, "expires_in": f.expiresIn, "interval": f.interval,
		})
	case enrollmentsPath + "/enr_1/poll":
		f.checkProof(body["proof"], f.keyID, endpoint)
		status := f.pollReplies[min(f.polls, len(f.pollReplies)-1)]
		f.polls++
		f.reply(w, map[string]string{"status": status, "service_account": testServiceAccount})
	case enrollmentsPath + "/enr_1/cancel":
		f.checkProof(body["proof"], f.keyID, endpoint)
		f.cancelled++
		w.WriteHeader(http.StatusNoContent)
	case "/api/v1/service-accounts/" + testServiceAccount + "/keys/" + f.keyID + "/revoke":
		f.checkProof(body["proof"], testServiceAccount, endpoint)
		f.revoked = append(f.revoked, f.keyID)
		w.WriteHeader(f.revokeCode)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeKeyAPI) reply(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("encode reply: %v", err)
	}
}

func (f *fakeKeyAPI) checkProof(proof, wantSubject, wantAud string) {
	f.t.Helper()
	_, claims := verifyTestAssertionPEM(f.t, f.publicKey, proof)
	if claims["iss"] != wantSubject || claims["sub"] != wantSubject {
		f.t.Errorf("proof iss/sub = %v/%v, want %s", claims["iss"], claims["sub"], wantSubject)
	}
	if claims["aud"] != wantAud {
		f.t.Errorf("proof aud = %v, want the endpoint %s", claims["aud"], wantAud)
	}
}

func stubEnrollmentSleep(t *testing.T) {
	t.Helper()
	t.Cleanup(SkipEnrollmentWaitForTest(t))
}

// startEnrollment runs the command's sequence up to the wait: persist, submit,
// record.
func startEnrollment(t *testing.T, api *fakeKeyAPI, proposed string) (*ServiceAccountKey, *ServiceAccountEnrollment) {
	t.Helper()
	key, err := NewEnrollmentKey(api.srv.URL, proposed, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePendingEnrollmentKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	enrollment, err := StartServiceAccountEnrollment(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordEnrollment(context.Background(), key, enrollment); err != nil {
		t.Fatal(err)
	}
	return key, enrollment
}

func TestServiceAccountEnrollment_ApprovedStoresBoundKey(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending", "pending", "approved"}
	// The private half must be on disk before the server learns the public half.
	api.onStart = func() {
		if _, err := os.Stat(filepath.Join(configDir, ServiceAccountKeyFileName(api.keyID))); err != nil {
			t.Errorf("key not on disk when the enrollment was submitted: %v", err)
		}
	}

	key, enrollment := startEnrollment(t, api, "")
	if enrollment.UserCode != "WXYZ-1234" {
		t.Fatalf("user code = %q", enrollment.UserCode)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyEnrolling})
	got, err := WaitForServiceAccountEnrollment(context.Background(), key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != testServiceAccount || api.polls != 3 {
		t.Fatalf("approved %q after %d polls; want %s after 3", got, api.polls, testServiceAccount)
	}
	if activated, err := BindEnrolledKey(context.Background(), key, got); err != nil || !activated {
		t.Fatalf("bind: activated %v err %v", activated, err)
	}
	loaded, err := LoadActiveServiceAccountKey(testServiceAccount)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.KeyID() != key.KeyID() || loaded.EnrollmentID() != "" {
		t.Fatalf("active key = %s (enrollment %q), want the enrolled key with no pending enrollment", loaded.KeyID(), loaded.EnrollmentID())
	}
}

// The process dies after the server recorded the enrollment (approval may land
// at any point after). Nothing in memory survives; the key file alone is
// enough to resume, and the approved key is bound to its private half.
func TestServiceAccountEnrollment_ResumesAfterCrash(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"approved"}

	crashed, _ := startEnrollment(t, api, "")
	crashedID := crashed.KeyID()

	keys, err := ListServiceAccountKeys("")
	if err != nil || len(keys) != 1 || keys[0].Status != KeyEnrolling {
		t.Fatalf("after the crash: keys %v err %v; want one enrolling key", keys, err)
	}
	resumed := keys[0].Key
	if resumed.KeyID() != crashedID || resumed.EnrollmentID() != "enr_1" {
		t.Fatalf("resumed key %s enrollment %q", resumed.KeyID(), resumed.EnrollmentID())
	}
	approved, err := WaitForServiceAccountEnrollment(context.Background(), resumed, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindEnrolledKey(context.Background(), resumed, approved); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadActiveServiceAccountKey(testServiceAccount); err != nil {
		t.Fatal(err)
	}
}

// The approver may bind the key to a different account than proposed; that
// must never replace a key that account already uses on this machine.
func TestServiceAccountEnrollment_ApprovalNeverReplacesActiveKey(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	existing := newTestKey(t, "https://us.auth.example.com")
	if _, err := CreateServiceAccountKey(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"approved"}

	key, _ := startEnrollment(t, api, "svc_01HKZ3J4N5P6Q7R8S9T0V1W2X4")
	approved, err := WaitForServiceAccountEnrollment(context.Background(), key, 0)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := BindEnrolledKey(context.Background(), key, approved); err != nil || activated {
		t.Fatalf("bind: activated %v err %v; want stored inactive", activated, err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{existing.KeyID(): KeyActive, key.KeyID(): KeyInactive})
}

func TestServiceAccountEnrollment_Denied(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending", "denied"}

	key, _ := startEnrollment(t, api, "")
	if _, err := WaitForServiceAccountEnrollment(context.Background(), key, time.Second); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("err = %v, want ErrEnrollmentDenied", err)
	}
}

func TestCancelServiceAccountEnrollment(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	key, _ := startEnrollment(t, api, "")
	if err := CancelServiceAccountEnrollment(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if api.cancelled != 1 {
		t.Fatalf("cancel calls = %d, want 1", api.cancelled)
	}
}

// A hostile interval longer than the enrollment's lifetime must not stall the
// box past expiry: every sleep is cut to the time remaining.
func TestWaitForServiceAccountEnrollment_NeverSleepsPastExpiry(t *testing.T) {
	isolateServiceAccountDirs(t)
	now := pinClock(t)
	var slept []time.Duration
	prev := enrollmentSleep
	enrollmentSleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		*now = now.Add(d)
		return nil
	}
	t.Cleanup(func() { enrollmentSleep = prev })

	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending"}
	api.interval = math.MaxInt
	api.expiresIn = 90

	key, enrollment := startEnrollment(t, api, "")
	if enrollment.Interval != int(maxEnrollmentInterval/time.Second) || enrollment.ExpiresIn != 90 {
		t.Fatalf("interval %d expires_in %d; want the interval clamped to %s", enrollment.Interval, enrollment.ExpiresIn, maxEnrollmentInterval)
	}
	_, err := WaitForServiceAccountEnrollment(context.Background(), key, time.Duration(enrollment.Interval)*time.Second)
	if !errors.Is(err, ErrEnrollmentExpired) {
		t.Fatalf("err = %v, want ErrEnrollmentExpired", err)
	}
	var total time.Duration
	for _, d := range slept {
		if d <= 0 || d > maxEnrollmentInterval {
			t.Fatalf("slept %s, want within (0, %s]", d, maxEnrollmentInterval)
		}
		total += d
	}
	if total != 90*time.Second {
		t.Fatalf("slept %s in total (%v), want exactly the 90s lifetime", total, slept)
	}
}

func TestValidateEnrollment_ClampsTimingAndRejectsUnsafeLinks(t *testing.T) {
	t.Parallel()
	base := func() ServiceAccountEnrollment {
		return ServiceAccountEnrollment{ID: "enr_1", UserCode: "WXYZ-1234", VerificationURI: "https://entire.example/approve"}
	}
	e := base()
	e.Interval, e.ExpiresIn = math.MaxInt, math.MaxInt
	if err := validateEnrollment(&e); err != nil {
		t.Fatal(err)
	}
	if e.Interval != 60 || e.ExpiresIn != 3600 {
		t.Fatalf("interval %d expires_in %d, want 60/3600", e.Interval, e.ExpiresIn)
	}
	e = base()
	if err := validateEnrollment(&e); err != nil || e.Interval != 5 || e.ExpiresIn != 900 {
		t.Fatalf("defaults: %d/%d err %v, want 5/900", e.Interval, e.ExpiresIn, err)
	}
	for _, link := range []string{
		"http://entire.example/approve",
		"https://entire.example/approve\x1b[2J",
		"https://entire.example/\u202eevorppa", // bidi override
		"https://entire.example/\u0085x",       // C1 next-line
		"https://user@entire.example/approve",
	} {
		e = base()
		e.VerificationURI = link
		if err := validateEnrollment(&e); err == nil {
			t.Errorf("link %q accepted", link)
		}
	}
}

func TestServiceAccountEnrollment_RejectsInsecureApprovalLink(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	api.badLink = "http://entire.example/approve"
	key, err := NewEnrollmentKey(api.srv.URL, "", "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartServiceAccountEnrollment(context.Background(), key); err == nil {
		t.Fatal("an http:// approval link was accepted; the box must only print https links")
	}
}

// A signed proof must never follow a redirect to another origin.
func TestPostKeyAPI_RefusesRedirect(t *testing.T) {
	t.Parallel()
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	t.Cleanup(elsewhere.Close)
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, code)
		}))
		t.Cleanup(redirector.Close)
		if _, err := postKeyAPI(context.Background(), redirector.URL+enrollmentsPath, map[string]string{"proof": "p"}, nil); err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Errorf("%d: err = %v, want a refused redirect", code, err)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("a proof was forwarded to the redirect target")
	}
}

func TestPostKeyAPI_SanitizesServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"denied\u001b[31m\u202e\u0085x","message":"` + strings.Repeat("a", 1000) + `"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)
	_, err := postKeyAPI(context.Background(), srv.URL+"/x", map[string]string{}, nil)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\x1b\u202e\u0085") || len([]rune(msg)) > maxServerErrorRunes+40 {
		t.Fatalf("unsanitized or unbounded server error: %q", msg)
	}
}

func TestRevokeServiceAccountKey(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	key := newTestKey(t, api.srv.URL)
	api.publicKey, _ = key.PublicKeyPEM() //nolint:errcheck // verified by the fake on use
	api.keyID = key.KeyID()
	if _, err := CreateServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	if err := RevokeServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if len(api.revoked) != 1 || api.revoked[0] != key.KeyID() {
		t.Fatalf("revoked = %v, want [%s]", api.revoked, key.KeyID())
	}

	api.revokeCode = http.StatusNotFound
	if err := RevokeServiceAccountKey(context.Background(), key); !errors.Is(err, ErrServiceAccountKeyUnknown) {
		t.Fatalf("404: err = %v, want ErrServiceAccountKeyUnknown", err)
	}
	api.revokeCode = http.StatusInternalServerError
	if err := RevokeServiceAccountKey(context.Background(), key); err == nil || errors.Is(err, ErrServiceAccountKeyUnknown) {
		t.Fatalf("500: err = %v, want a plain failure", err)
	}

	if err := DeleteServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configDir, ServiceAccountKeyFileName(key.KeyID()))); !os.IsNotExist(err) {
		t.Fatalf("key file still present after delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, serviceAccountPointerName(testServiceAccount))); !os.IsNotExist(err) {
		t.Fatalf("active pointer still present after deleting the active key: %v", err)
	}
	if err := DeleteServiceAccountKey(context.Background(), key); err != nil {
		t.Fatalf("deleting an absent key: %v", err)
	}
}
