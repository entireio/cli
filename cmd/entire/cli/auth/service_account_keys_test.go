package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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
	revokeCode  int
	revoked     []string
	badLink     bool
}

func newFakeKeyAPI(t *testing.T) *fakeKeyAPI {
	t.Helper()
	f := &fakeKeyAPI{t: t, revokeCode: http.StatusNoContent}
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
		link := "https://entire.example/approve"
		if f.badLink {
			link = "http://entire.example/approve"
		}
		f.reply(w, ServiceAccountEnrollment{
			ID: "enr_1", UserCode: "WXYZ-1234",
			VerificationURI: link, VerificationURIComplete: link + "?code=WXYZ-1234",
			ExpiresIn: 600, Interval: 1,
		})
	case enrollmentsPath + "/enr_1/poll":
		f.checkProof(body["proof"], f.keyID, endpoint)
		status := f.pollReplies[min(f.polls, len(f.pollReplies)-1)]
		f.polls++
		f.reply(w, map[string]string{"status": status, "service_account": testServiceAccount})
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

func TestServiceAccountEnrollment_ApprovedStoresBoundKey(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending", "pending", "approved"}

	key, err := NewEnrollmentKey(api.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := StartServiceAccountEnrollment(context.Background(), key, "", "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if enrollment.UserCode != "WXYZ-1234" {
		t.Fatalf("user code = %q", enrollment.UserCode)
	}
	got, err := WaitForServiceAccountEnrollment(context.Background(), key, enrollment)
	if err != nil {
		t.Fatal(err)
	}
	if got != testServiceAccount || api.polls != 3 {
		t.Fatalf("approved %q after %d polls; want %s after 3", got, api.polls, testServiceAccount)
	}
	if _, err := BindEnrolledKey(key, got, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadServiceAccountKey(testServiceAccount)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.KeyID != key.KeyID {
		t.Fatal("stored key is not the enrolled key")
	}
}

func TestServiceAccountEnrollment_Denied(t *testing.T) {
	isolateServiceAccountDirs(t)
	stubEnrollmentSleep(t)
	api := newFakeKeyAPI(t)
	api.pollReplies = []string{"pending", "denied"}

	key, err := NewEnrollmentKey(api.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := StartServiceAccountEnrollment(context.Background(), key, "", "box-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WaitForServiceAccountEnrollment(context.Background(), key, enrollment); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("err = %v, want ErrEnrollmentDenied", err)
	}
}

func TestServiceAccountEnrollment_RejectsInsecureApprovalLink(t *testing.T) {
	isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	api.badLink = true
	key, err := NewEnrollmentKey(api.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartServiceAccountEnrollment(context.Background(), key, "", "box-1"); err == nil {
		t.Fatal("an http:// approval link was accepted; the box must only print https links")
	}
}

func TestRevokeServiceAccountKey(t *testing.T) {
	configDir := isolateServiceAccountDirs(t)
	api := newFakeKeyAPI(t)
	key, err := GenerateServiceAccountKey(testServiceAccount, api.srv.URL, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	api.publicKey, _ = key.PublicKeyPEM() //nolint:errcheck // verified by the fake on use
	api.keyID = key.KeyID
	if _, err := SaveServiceAccountKey(key, false); err != nil {
		t.Fatal(err)
	}

	if err := RevokeServiceAccountKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if len(api.revoked) != 1 || api.revoked[0] != key.KeyID {
		t.Fatalf("revoked = %v, want [%s]", api.revoked, key.KeyID)
	}

	api.revokeCode = http.StatusNotFound
	if err := RevokeServiceAccountKey(context.Background(), key); !errors.Is(err, ErrServiceAccountKeyUnknown) {
		t.Fatalf("404: err = %v, want ErrServiceAccountKeyUnknown", err)
	}
	api.revokeCode = http.StatusInternalServerError
	if err := RevokeServiceAccountKey(context.Background(), key); err == nil || errors.Is(err, ErrServiceAccountKeyUnknown) {
		t.Fatalf("500: err = %v, want a plain failure", err)
	}

	if err := DeleteServiceAccountKey(testServiceAccount); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configDir, serviceAccountKeyFileName(testServiceAccount))); !os.IsNotExist(err) {
		t.Fatalf("key file still present after delete: %v", err)
	}
	if err := DeleteServiceAccountKey(testServiceAccount); err != nil {
		t.Fatalf("deleting an absent key: %v", err)
	}
}
