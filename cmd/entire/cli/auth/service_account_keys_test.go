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

// fakeKeyAPI is the proposed key-management API. It verifies every proof the
// way the server must (signature against the submitted/registered public key,
// aud = the exact endpoint, htm = POST, bh = hash of the body without its
// proof, iss the key id or the service account) and implements the contract's
// idempotency and conflict rules, with switches to inject the failures the
// client must survive.
type fakeKeyAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	publicKey   string
	keyID       string
	submissions int    // submit requests received
	enrollments int    // enrollments actually created (idempotent by key id)
	submitCode  int    // non-zero: refuse submissions with this status
	loseSubmit  string // "500" or "drop": create the enrollment, then lose the response
	pollReplies []string
	polls       int
	poll404     bool
	interval    int
	expiresIn   int
	cancelReply string // "cancelled" (204), "approved"/"denied"/"expired" (409), "404"
	cancels     int
	revokeCode  int
	revoked     []string
	badLink     string
	onSubmit    func()
}

func newFakeKeyAPI(t *testing.T) *fakeKeyAPI {
	t.Helper()
	f := &fakeKeyAPI{t: t, revokeCode: http.StatusNoContent, interval: 1, expiresIn: 600, cancelReply: "cancelled"}
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
		f.submissions++
		if f.publicKey != "" && body["key_id"] != f.keyID {
			f.t.Errorf("a second key %s was submitted; one enrollment per machine", body["key_id"])
		}
		f.publicKey, f.keyID = body["public_key"], body["key_id"]
		f.checkProof(body, f.keyID, endpoint)
		if f.onSubmit != nil {
			f.onSubmit()
		}
		if f.submitCode != 0 {
			w.WriteHeader(f.submitCode)
			return
		}
		if f.enrollments == 0 {
			f.enrollments++ // idempotent by key id: later submissions return the same one
		}
		switch f.loseSubmit {
		case "500":
			f.loseSubmit = ""
			w.WriteHeader(http.StatusInternalServerError)
			return
		case "drop":
			f.loseSubmit = ""
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				f.t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close() // simulating a lost response
			return
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
		f.checkProof(body, f.keyID, endpoint)
		if f.poll404 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status := f.pollReplies[min(f.polls, len(f.pollReplies)-1)]
		f.polls++
		f.reply(w, map[string]string{"status": status, "service_account": testServiceAccount})
	case enrollmentsPath + "/enr_1/cancel":
		f.checkProof(body, f.keyID, endpoint)
		f.cancels++
		switch f.cancelReply {
		case "cancelled":
			w.WriteHeader(http.StatusNoContent)
		case "404":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusConflict)
			f.reply(w, map[string]string{"status": f.cancelReply, "service_account": testServiceAccount})
		}
	case "/api/v1/service-accounts/" + testServiceAccount + "/keys/" + f.keyID + "/revoke":
		f.checkProof(body, testServiceAccount, endpoint)
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

func (f *fakeKeyAPI) checkProof(body map[string]string, wantSubject, wantAud string) {
	f.t.Helper()
	_, claims := verifyTestAssertionPEM(f.t, f.publicKey, body["proof"])
	if claims["iss"] != wantSubject || claims["sub"] != wantSubject {
		f.t.Errorf("proof iss/sub = %v/%v, want %s", claims["iss"], claims["sub"], wantSubject)
	}
	if claims["aud"] != wantAud {
		f.t.Errorf("proof aud = %v, want the endpoint %s", claims["aud"], wantAud)
	}
	if claims["htm"] != http.MethodPost {
		f.t.Errorf("proof htm = %v, want POST", claims["htm"])
	}
	fields := map[string]string{}
	for k, v := range body {
		if k != "proof" {
			fields[k] = v
		}
	}
	want, err := keyProofBodyHash(fields)
	if err != nil || claims["bh"] != want {
		f.t.Errorf("proof bh = %v, want %s (the body was not bound)", claims["bh"], want)
	}
}

func stubEnrollmentSleep(t *testing.T) {
	t.Helper()
	t.Cleanup(SkipEnrollmentWaitForTest(t))
}

func injectStoreFault(t *testing.T, step string) {
	t.Helper()
	storeFault = func(s string) error {
		if s == step {
			return errors.New("injected crash at " + step)
		}
		return nil
	}
	t.Cleanup(func() { storeFault = nil })
}

func clearStoreFault() { storeFault = nil }

// newEnrollment stores a new enrolling key, as `enroll` does before submitting.
func newEnrollment(t *testing.T, api *fakeKeyAPI, proposed string) *ServiceAccountKey {
	t.Helper()
	key, err := NewEnrollmentKey(api.srv.URL, proposed, "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePendingEnrollmentKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	return key
}

// startEnrollment stores, submits and records an enrollment.
func startEnrollment(t *testing.T, api *fakeKeyAPI, proposed string) (*ServiceAccountKey, *ServiceAccountEnrollment) {
	t.Helper()
	key := newEnrollment(t, api, proposed)
	enrollment, err := SubmitServiceAccountEnrollment(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return key, enrollment
}

// reload drops the in-memory key, as a crashed process would, and reads it
// back from disk.
func reload(t *testing.T, k *ServiceAccountKey) *ServiceAccountKey {
	t.Helper()
	got, err := LoadServiceAccountKey(k.KeyID())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestServiceAccountEnrollment_ApprovedStoresBoundKey(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending", "pending", "approved"}
	api.interval = 7
	// The private half must be on disk before the server learns the public half.
	api.onSubmit = func() {
		if _, err := os.Stat(filepath.Join(configDir, ServiceAccountKeyFileName(api.keyID))); err != nil {
			t.Errorf("key not on disk when the enrollment was submitted: %v", err)
		}
	}

	key, enrollment := startEnrollment(t, api, "")
	if enrollment.UserCode != "WXYZ-1234" {
		t.Fatalf("user code = %q", enrollment.UserCode)
	}
	if got := reload(t, key).EnrollmentInterval(); got != 7*time.Second {
		t.Fatalf("persisted interval = %s, want the server's 7s", got)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyEnrolling})
	got, err := WaitForServiceAccountEnrollment(context.Background(), key)
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
	if loaded.KeyID() != key.KeyID() || loaded.EnrollmentPhase() != "" {
		t.Fatalf("active key = %s (phase %q), want the enrolled key with no enrollment left", loaded.KeyID(), loaded.EnrollmentPhase())
	}
}

// The server creates the enrollment but the response is lost (a 500, or the
// connection dropping). The key must stay, and resubmitting must find the same
// enrollment rather than create a second one.
func TestServiceAccountEnrollment_LostSubmitResponseIsResumable(t *testing.T) {
	for _, lose := range []string{"500", "drop"} {
		t.Run(lose, func(t *testing.T) {
			isolateServiceAccountDirs(t)
			stubEnrollmentSleep(t)
			api := newFakeKeyAPI(t)
			api.pollReplies = []string{"approved"}
			api.loseSubmit = lose

			key := newEnrollment(t, api, "")
			_, err := SubmitServiceAccountEnrollment(context.Background(), key)
			if err == nil || IsDefinitiveEnrollmentOutcome(err) {
				t.Fatalf("err = %v, want a non-definitive error", err)
			}
			if ferr := ForgetEnrollment(context.Background(), key, err); ferr == nil {
				t.Fatal("ForgetEnrollment accepted a lost response as a definitive outcome")
			}
			resumed := reload(t, key)
			if resumed.EnrollmentPhase() != EnrollmentSubmitting {
				t.Fatalf("phase = %q, want submitting", resumed.EnrollmentPhase())
			}

			if _, err := SubmitServiceAccountEnrollment(context.Background(), resumed); err != nil {
				t.Fatal(err)
			}
			if api.submissions != 2 || api.enrollments != 1 {
				t.Fatalf("submissions %d, enrollments %d; want 2 submissions for 1 enrollment", api.submissions, api.enrollments)
			}
			approved, err := WaitForServiceAccountEnrollment(context.Background(), resumed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := BindEnrolledKey(context.Background(), resumed, approved); err != nil {
				t.Fatal(err)
			}
			assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyActive})
		})
	}
}

func TestServiceAccountEnrollment_RefusedSubmitIsDefinitive(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	api.submitCode = http.StatusBadRequest
	key := newEnrollment(t, api, "")
	_, err := SubmitServiceAccountEnrollment(context.Background(), key)
	if !errors.Is(err, ErrEnrollmentRejected) {
		t.Fatalf("err = %v, want ErrEnrollmentRejected", err)
	}
	if err := ForgetEnrollment(context.Background(), key, err); err != nil {
		t.Fatal(err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{})

	// 429 is "try again", not a refusal: the key stays.
	api.submitCode = http.StatusTooManyRequests
	api.publicKey = "" // the first key is gone; a new one may enroll
	key = newEnrollment(t, api, "")
	if _, err := SubmitServiceAccountEnrollment(context.Background(), key); err == nil || IsDefinitiveEnrollmentOutcome(err) {
		t.Fatalf("429: err = %v, want a non-definitive error", err)
	}
}

// The approval wins a race with the cancel: the server's 409 says so, and the
// key, now trusted on the server, must not be deleted.
func TestCancelServiceAccountEnrollment_ApprovalWinsRace(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	api.cancelReply = "approved"
	key, _ := startEnrollment(t, api, "")

	approved, err := CancelServiceAccountEnrollment(context.Background(), key)
	if !errors.Is(err, ErrEnrollmentAlreadyApproved) || approved != testServiceAccount {
		t.Fatalf("approved %q err %v; want ErrEnrollmentAlreadyApproved for %s", approved, err, testServiceAccount)
	}
	if ferr := ForgetEnrollment(context.Background(), key, err); ferr == nil {
		t.Fatal("ForgetEnrollment deleted a key whose enrollment was approved")
	}
	if _, err := BindEnrolledKey(context.Background(), key, approved); err != nil {
		t.Fatal(err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyActive})
}

func TestCancelServiceAccountEnrollment_Outcomes(t *testing.T) {
	for reply, definitive := range map[string]bool{"cancelled": true, "denied": true, "expired": true, "404": false} {
		t.Run(reply, func(t *testing.T) {
			isolateServiceAccountDirs(t)
			api := newFakeKeyAPI(t)
			api.cancelReply = reply
			key, _ := startEnrollment(t, api, "")
			_, err := CancelServiceAccountEnrollment(context.Background(), key)
			if got := IsDefinitiveEnrollmentOutcome(err); got != definitive {
				t.Fatalf("err = %v: definitive %v, want %v", err, got, definitive)
			}
			if definitive {
				if err := ForgetEnrollment(context.Background(), key, err); err != nil {
					t.Fatal(err)
				}
				assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{})
			}
		})
	}
}

// "Unknown enrollment" only counts as expired once the enrollment has
// certainly lapsed (expiry plus skew); before that the key is kept.
func TestCancelServiceAccountEnrollment_UnknownBecomesExpiredOnlyAfterLapse(t *testing.T) {
	isolateServiceAccountDirs(t)
	now := pinClock(t)
	api := newFakeKeyAPI(t)
	api.cancelReply = "404"
	key, _ := startEnrollment(t, api, "")
	if _, err := CancelServiceAccountEnrollment(context.Background(), key); IsDefinitiveEnrollmentOutcome(err) {
		t.Fatalf("before expiry: err = %v, want non-definitive", err)
	}
	*now = now.Add(10*time.Minute + enrollmentExpirySkew + time.Second)
	if _, err := CancelServiceAccountEnrollment(context.Background(), key); !errors.Is(err, ErrEnrollmentExpired) {
		t.Fatalf("after expiry and skew: err = %v, want ErrEnrollmentExpired", err)
	}
}

func TestWaitForServiceAccountEnrollment_Poll404KeepsKeyUntilLapse(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.poll404 = true
	key, _ := startEnrollment(t, api, "")
	if _, err := WaitForServiceAccountEnrollment(context.Background(), key); err == nil || IsDefinitiveEnrollmentOutcome(err) {
		t.Fatalf("err = %v, want a non-definitive error for an unknown enrollment", err)
	}
}

// An enrolling key — even one proposing an account — cannot be revoked or
// deleted by any path but a confirmed enrollment outcome.
func TestEnrollingKey_CannotBeRevokedOrDeleted(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	key, _ := startEnrollment(t, api, testServiceAccount)
	if err := RevokeServiceAccountKey(context.Background(), key); !errors.Is(err, ErrKeyEnrolling) {
		t.Fatalf("revoke: err = %v, want ErrKeyEnrolling", err)
	}
	if len(api.revoked) != 0 {
		t.Fatal("a revoke request was sent for an enrolling key")
	}
	if err := DeleteServiceAccountKey(context.Background(), key); !errors.Is(err, ErrKeyEnrolling) {
		t.Fatalf("delete: err = %v, want ErrKeyEnrolling", err)
	}
	if _, err := ActivateServiceAccountKey(context.Background(), key); !errors.Is(err, ErrKeyEnrolling) {
		t.Fatalf("activate: err = %v, want ErrKeyEnrolling", err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyEnrolling})
}

// One enrollment per machine, enforced under the lock: concurrent attempts
// leave exactly one.
func TestSavePendingEnrollmentKey_OneAtATime(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	var ok, refused atomic.Int32
	var wg sync.WaitGroup
	for range 6 {
		key, err := NewEnrollmentKey(api.srv.URL, "", "box")
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			switch err := SavePendingEnrollmentKey(context.Background(), key); {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrEnrollmentInProgress):
				refused.Add(1)
			default:
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 1 || refused.Load() != 5 {
		t.Fatalf("%d stored, %d refused; want exactly one stored", ok.Load(), refused.Load())
	}
}

// A crash at every write of the bind leaves a state --resume finishes: the
// key is never lost, and it ends up active.
func TestBindEnrolledKey_RecoversFromCrashAtEveryStep(t *testing.T) {
	for _, step := range []string{"bind:approve", "bind:pointer", "bind:finish"} {
		t.Run(step, func(t *testing.T) {
			isolateServiceAccountDirs(t)
			stubEnrollmentSleep(t)
			api := newFakeKeyAPI(t)
			api.pollReplies = []string{"approved"}
			key, _ := startEnrollment(t, api, "")
			approved, err := WaitForServiceAccountEnrollment(context.Background(), key)
			if err != nil {
				t.Fatal(err)
			}
			injectStoreFault(t, step)
			if _, err := BindEnrolledKey(context.Background(), key, approved); err == nil {
				t.Fatal("injected fault did not fail the bind")
			}
			clearStoreFault()

			// What --resume does from whatever phase is on disk.
			resumed := reload(t, key)
			switch resumed.EnrollmentPhase() {
			case EnrollmentPending:
				if approved, err = WaitForServiceAccountEnrollment(context.Background(), resumed); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case EnrollmentApproved:
				if _, err := BindEnrolledKey(context.Background(), resumed, approved); err != nil {
					t.Fatal(err)
				}
			case "", EnrollmentSubmitting:
				t.Fatalf("unexpected phase %q after a bind crash", resumed.EnrollmentPhase())
			}
			assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyActive})
		})
	}
}

// A crash while switching keys never loses the old key's pending revoke, and
// a crash while deleting a revoked key never leaves the list wrong for long.
func TestActivation_RecoversFromCrashAtEveryStep(t *testing.T) {
	setup := func(t *testing.T) (oldKey, newKey *ServiceAccountKey) {
		t.Helper()
		isolateServiceAccountDirs(t)
		oldKey = newTestKey(t, "https://us.auth.example.com")
		newKey = newTestKey(t, "https://us.auth.example.com")
		for _, k := range []*ServiceAccountKey{oldKey, newKey} {
			if _, err := CreateServiceAccountKey(context.Background(), k); err != nil {
				t.Fatal(err)
			}
		}
		return oldKey, newKey
	}

	t.Run("activate:pointer", func(t *testing.T) {
		oldKey, newKey := setup(t)
		injectStoreFault(t, "activate:pointer")
		if _, err := ActivateServiceAccountKey(context.Background(), newKey); err == nil {
			t.Fatal("injected fault did not fail the activation")
		}
		clearStoreFault()
		assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{oldKey.KeyID(): KeyActive, newKey.KeyID(): KeyInactive})
	})

	for _, step := range []string{"delete:key", "delete:pointer"} {
		t.Run(step, func(t *testing.T) {
			oldKey, newKey := setup(t)
			retiring, err := ActivateServiceAccountKey(context.Background(), newKey)
			if err != nil || len(retiring) != 1 {
				t.Fatalf("retiring %v err %v", retiring, err)
			}
			injectStoreFault(t, step)
			if err := DeleteServiceAccountKey(context.Background(), retiring[0]); err == nil {
				t.Fatal("injected fault did not fail the delete")
			}
			clearStoreFault()

			// Re-running activation picks up whatever retirement is still owed.
			again, err := ActivateServiceAccountKey(context.Background(), newKey)
			if err != nil {
				t.Fatal(err)
			}
			for _, k := range again {
				if err := DeleteServiceAccountKey(context.Background(), k); err != nil {
					t.Fatal(err)
				}
			}
			assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{newKey.KeyID(): KeyActive})
			if _, err := LoadServiceAccountKey(oldKey.KeyID()); err == nil {
				t.Fatal("the retired key is still on disk")
			}
			// A delete that removed the files but not the list entry is tidied
			// by the next delete or skipped by readers.
			if _, err := LoadActiveServiceAccountKey(testServiceAccount); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestServiceAccountEnrollment_Denied(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending", "denied"}

	key, _ := startEnrollment(t, api, "")
	_, err := WaitForServiceAccountEnrollment(context.Background(), key)
	if !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("err = %v, want ErrEnrollmentDenied", err)
	}
	if err := ForgetEnrollment(context.Background(), key, err); err != nil {
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
	approved, err := WaitForServiceAccountEnrollment(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if activated, err := BindEnrolledKey(context.Background(), key, approved); err != nil || activated {
		t.Fatalf("bind: activated %v err %v; want stored inactive", activated, err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{existing.KeyID(): KeyActive, key.KeyID(): KeyInactive})
}

// A hostile interval longer than the enrollment's lifetime must not stall the
// box past expiry: every sleep is cut to the time remaining, and the final
// poll at expiry settles it.
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
	// One poll at 60s, then the 90s expiry cuts the next sleep to 30s and the
	// final poll settles it.
	api.pollReplies = []string{"pending", "expired"}
	api.interval = math.MaxInt
	api.expiresIn = 90

	key, enrollment := startEnrollment(t, api, "")
	if enrollment.Interval != int(maxEnrollmentInterval/time.Second) || enrollment.ExpiresIn != 90 {
		t.Fatalf("interval %d expires_in %d; want the interval clamped to %s", enrollment.Interval, enrollment.ExpiresIn, maxEnrollmentInterval)
	}
	_, err := WaitForServiceAccountEnrollment(context.Background(), key)
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

// Each poll request is bounded by the enrollment's expiry, not just the sleep
// before it: a server that hangs cannot hold the box past expiry.
func TestWaitForServiceAccountEnrollment_PollRequestBoundedByExpiry(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	release := make(chan struct{})
	var polls atomic.Int32
	api := newFakeKeyAPI(t)
	api.expiresIn = 1
	key, _ := startEnrollment(t, api, "")
	api.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) == 1 {
			select { // the first poll hangs past the expiry
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"expired"}`)) //nolint:errcheck // test server
	})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	_, err := WaitForServiceAccountEnrollment(context.Background(), key)
	if !errors.Is(err, ErrEnrollmentExpired) {
		t.Fatalf("err = %v, want ErrEnrollmentExpired from the final poll", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("waited %s; the hanging poll was not cut at the 1s expiry", elapsed)
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

// An unusable approval link is an unknown outcome (the server may have
// created the enrollment), so the key is kept.
func TestServiceAccountEnrollment_InsecureApprovalLinkKeepsKey(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	api.badLink = "http://entire.example/approve"
	key := newEnrollment(t, api, "")
	_, err := SubmitServiceAccountEnrollment(context.Background(), key)
	if err == nil || IsDefinitiveEnrollmentOutcome(err) {
		t.Fatalf("err = %v; an http:// approval link must fail without deleting the key", err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{key.KeyID(): KeyEnrolling})
}

// A signed proof must never follow a redirect to another origin.
func TestPostKeyAPI_RefusesRedirect(t *testing.T) {
	isolateServiceAccountDirs(t)
	var leaked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
	t.Cleanup(elsewhere.Close)
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound} {
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, code)
		}))
		t.Cleanup(redirector.Close)
		key := newTestKey(t, redirector.URL)
		if _, _, err := postKeyAPI(context.Background(), key, key.KeyID(), redirector.URL+enrollmentsPath, map[string]string{}); err == nil || !strings.Contains(err.Error(), "redirect") {
			t.Errorf("%d: err = %v, want a refused redirect", code, err)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("a proof was forwarded to the redirect target")
	}
}

func TestDescribeStatus_SanitizesServerError(t *testing.T) {
	t.Parallel()
	body := []byte(`{"error":"denied\u001b[31m\u202e\u0085x","message":"` + strings.Repeat("a", 1000) + `"}`)
	msg := describeStatus(http.StatusForbidden, body)
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
	for _, name := range []string{ServiceAccountKeyFileName(key.KeyID()), serviceAccountStateName(key.KeyID()), serviceAccountPointerName(testServiceAccount)} {
		if _, err := os.Stat(filepath.Join(configDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s still present after delete: %v", name, err)
		}
	}
	if err := DeleteServiceAccountKey(context.Background(), key); err != nil {
		t.Fatalf("deleting an absent key: %v", err)
	}
}

// Resuming after the local expiry still asks the server before concluding
// anything: an approval that landed while the box was away binds the key
// instead of deleting it.
func TestWaitForServiceAccountEnrollment_ResumeAfterExpiryStillPolls(t *testing.T) {
	isolateServiceAccountDirs(t)
	now := pinClock(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"approved"}
	key, _ := startEnrollment(t, api, "")

	*now = now.Add(2 * time.Hour) // far past expiry and skew
	approved, err := WaitForServiceAccountEnrollment(context.Background(), reload(t, key))
	if err != nil || approved != testServiceAccount {
		t.Fatalf("approved %q err %v; want the late approval found by a poll", approved, err)
	}
	if api.polls != 1 {
		t.Fatalf("polls = %d, want 1", api.polls)
	}
}

// One unreadable key file is skipped with a warning; it does not block
// listing (and so enroll, resume and activate).
func TestListServiceAccountKeys_SkipsUnreadableFile(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	var warn strings.Builder
	prev := keyPermsWarnW
	keyPermsWarnW = &warn
	t.Cleanup(func() { keyPermsWarnW = prev })

	good := newTestKey(t, "https://us.auth.example.com")
	if _, err := CreateServiceAccountKey(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	bad := strings.Repeat("A", 43)
	if err := os.WriteFile(filepath.Join(configDir, ServiceAccountKeyFileName(bad)), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertKeyStatuses(t, map[string]ServiceAccountKeyStatus{good.KeyID(): KeyActive})
	if !strings.Contains(warn.String(), bad) {
		t.Fatalf("warning = %q, want it to name the unreadable key", warn.String())
	}
}
