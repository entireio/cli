package coreapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The three add operations answer 201 when Core grants and 202 when it invites
// (ENT-2652). These pin that the generated client decodes both, including the
// 201 body a core from before the change sends, and that a 421 from the wrong
// core is still followed for them.

// todayMembershipBody is the 201 a core without invitations answers to the org
// add, verbatim in shape: every field Membership requires, plus the ones it
// carries optionally.
const todayMembershipBody = `{"$schema":"https://us.console.entire.io/api/v1/schemas/Membership.json","id":"01HZX7QABCDEFGHJKMNPQRSTM1","accountId":"01HZX7QABCDEFGHJKMNPQRSTA1","orgId":"01HZX7QABCDEFGHJKMNPQRSTO1","role":"member","status":"active","provider":"github","handle":"alice","createdAt":"2026-11-01T10:00:00Z","workosOrgMembershipId":"om_123"}`

// accountInvitedBody is the 202 body as Core's engineer gave it.
const accountInvitedBody = `{"$schema":"https://us.console.entire.io/api/v1/schemas/AccountInvited.json","status":"invited","invitation_id":"01M7BDT64K8BTHRT2996R6XNG2","expires_at":"2026-11-20T21:42:56.403044Z"}`

// statusClient is a client whose server answers every request with status and
// body, and hands back each request body it read.
func statusClient(t *testing.T, status int, body string) (*Client, <-chan []byte) {
	t.Helper()
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		bodies <- got
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("writing test response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, bearerOnlySource{})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, bodies
}

func requireInvited(t *testing.T, res any) {
	t.Helper()
	inv, ok := res.(*AccountInvited)
	if !ok {
		t.Fatalf("got %T, want *AccountInvited", res)
	}
	if inv.InvitationID != "01M7BDT64K8BTHRT2996R6XNG2" || inv.Status != AccountInvitedStatusInvited {
		t.Fatalf("got %+v", inv)
	}
	if want := time.Date(2026, 11, 20, 21, 42, 56, 403044000, time.UTC); !inv.ExpiresAt.Equal(want) {
		t.Fatalf("expires_at = %v, want %v", inv.ExpiresAt, want)
	}
}

// requireOptIn checks that a request body carried allow_invitation: true.
func requireOptIn(t *testing.T, raw []byte) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("request body %q: %v", raw, err)
	}
	if body["allow_invitation"] != true {
		t.Fatalf("allow_invitation = %v in %s", body["allow_invitation"], raw)
	}
}

func TestAddOrgMember_DecodesTodaysMembershipAndAnInvitation(t *testing.T) {
	t.Parallel()
	body := &AddOrgMemberInputBody{Provider: "github", ProviderUserId: "12345", AllowInvitation: NewOptBool(true)}
	params := AddOrgMemberParams{OrgId: "01HZX7QABCDEFGHJKMNPQRSTO1"}

	t.Run("201 Membership from a core without invitations", func(t *testing.T) {
		t.Parallel()
		c, bodies := statusClient(t, http.StatusCreated, todayMembershipBody)
		res, err := c.AddOrgMember(t.Context(), body, params)
		if err != nil {
			t.Fatalf("AddOrgMember: %v", err)
		}
		m, ok := res.(*Membership)
		if !ok {
			t.Fatalf("got %T, want *Membership", res)
		}
		if m.Role != "member" || m.Status != "active" {
			t.Fatalf("got %+v", m)
		}
		requireOptIn(t, <-bodies)
	})

	t.Run("202 AccountInvited", func(t *testing.T) {
		t.Parallel()
		c, bodies := statusClient(t, http.StatusAccepted, accountInvitedBody)
		res, err := c.AddOrgMember(t.Context(), body, params)
		if err != nil {
			t.Fatalf("AddOrgMember: %v", err)
		}
		requireInvited(t, res)
		requireOptIn(t, <-bodies)
	})
}

func TestGrantAccess_DecodesGrantedAndInvited(t *testing.T) {
	t.Parallel()
	body := &GrantAccessBody{Provider: NewOptString("github"), ProviderUserId: NewOptString("12345"), Role: GrantAccessBodyRoleReader, AllowInvitation: NewOptBool(true)}
	const granted = `{"status":"granted"}`

	t.Run("project 201", func(t *testing.T) {
		t.Parallel()
		c, _ := statusClient(t, http.StatusCreated, granted)
		res, err := c.GrantProjectAccess(t.Context(), body, GrantProjectAccessParams{ProjectId: "01HZX7QABCDEFGHJKMNPQRSTP1"})
		if err != nil {
			t.Fatalf("GrantProjectAccess: %v", err)
		}
		if out, ok := res.(*GrantProjectAccessOutputBody); !ok || out.Status != "granted" {
			t.Fatalf("got %#v", res)
		}
	})
	t.Run("project 202", func(t *testing.T) {
		t.Parallel()
		c, _ := statusClient(t, http.StatusAccepted, accountInvitedBody)
		res, err := c.GrantProjectAccess(t.Context(), body, GrantProjectAccessParams{ProjectId: "01HZX7QABCDEFGHJKMNPQRSTP1"})
		if err != nil {
			t.Fatalf("GrantProjectAccess: %v", err)
		}
		requireInvited(t, res)
	})
	t.Run("repo 201", func(t *testing.T) {
		t.Parallel()
		c, _ := statusClient(t, http.StatusCreated, granted)
		res, err := c.GrantRepoAccess(t.Context(), body, GrantRepoAccessParams{RepoId: "01HZX7QABCDEFGHJKMNPQRSTR1"})
		if err != nil {
			t.Fatalf("GrantRepoAccess: %v", err)
		}
		if out, ok := res.(*GrantRepoAccessOutputBody); !ok || out.Status != "granted" {
			t.Fatalf("got %#v", res)
		}
	})
	t.Run("repo 202", func(t *testing.T) {
		t.Parallel()
		c, _ := statusClient(t, http.StatusAccepted, accountInvitedBody)
		res, err := c.GrantRepoAccess(t.Context(), body, GrantRepoAccessParams{RepoId: "01HZX7QABCDEFGHJKMNPQRSTR1"})
		if err != nil {
			t.Fatalf("GrantRepoAccess: %v", err)
		}
		requireInvited(t, res)
	})
}

// TestGrantRepoAccess_421FollowsToHomeCore: an invitation is created on the
// scope's home core, and another core answers 421. The client's cross-juris
// transport follows it for the add like any other call, replaying the body
// with the opt-in, and the 202 the home core answers decodes.
func TestGrantRepoAccess_421FollowsToHomeCore(t *testing.T) {
	t.Parallel()
	homeBodies := make(chan []byte, 1)
	homeCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		got, _ := io.ReadAll(r.Body) //nolint:errcheck // test
		homeBodies <- got
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(accountInvitedBody)) //nolint:errcheck // test
	})
	wrongCore := newCrossJurisTestServer(t, func(s *crossJurisTestServer, w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusMisdirectedRequest)
		w.Write([]byte(`{"error":"misdirected","home_core_url":"` + homeCore.srv.URL + `","jurisdiction":"eu"}`)) //nolint:errcheck // test
	})
	wrongCore.peers = []string{homeCore.srv.URL}

	c, err := NewWithBearer(wrongCore.srv.URL, "user-jwt")
	if err != nil {
		t.Fatal(err)
	}
	body := &GrantAccessBody{Provider: NewOptString("github"), ProviderUserId: NewOptString("12345"), Role: GrantAccessBodyRoleReader, AllowInvitation: NewOptBool(true)}
	res, err := c.GrantRepoAccess(t.Context(), body, GrantRepoAccessParams{RepoId: "01HZX7QABCDEFGHJKMNPQRSTR1"})
	if err != nil {
		t.Fatalf("GrantRepoAccess: %v", err)
	}
	requireInvited(t, res)
	requireOptIn(t, <-homeBodies)
	if wrongCore.hits.Load() != 1 || homeCore.hits.Load() != 1 {
		t.Fatalf("hits: wrong core %d, home core %d", wrongCore.hits.Load(), homeCore.hits.Load())
	}
}
