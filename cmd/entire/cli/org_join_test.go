package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// testInviteToken stands in for the credential in the invitation link. It is
// deliberately distinctive so a leak into any stream is unambiguous.
const testInviteToken = "inv_tok_3QXZ7K2M9WPLDN4RBVYF6HJ"

func testAcceptedInvitation() *coreapi.AcceptedInvitation {
	return &coreapi.AcceptedInvitation{
		OrgId:      testOrgULID,
		OrgName:    "acme",
		Membership: coreapi.Membership{ID: testDeleteULID, OrgId: testOrgULID, AccountId: testDeleteULID, Role: "member"},
	}
}

// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgJoin_AcceptsAndNamesTheOrg(t *testing.T) {
	var gotPath string
	var gotBody coreapi.AcceptInvitationInputBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, printJSON(w, testAcceptedInvitation()))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "join", testInviteToken)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/invitations/accept", gotPath)
	assert.Equal(t, testInviteToken, gotBody.Token, "the token must reach the server")
	assert.Contains(t, out, "✓ Joined org acme as member")
}

// Accepting twice is not an error: the server answers 200 for a membership
// that is already active.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgJoin_ReportsAnExistingMembership(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		assert.NoError(t, printJSON(w, &coreapi.AcceptInvitationOK{
			OrgId:      testOrgULID,
			OrgName:    "acme",
			Membership: coreapi.Membership{ID: testDeleteULID, OrgId: testOrgULID, AccountId: testDeleteULID, Role: "admin"},
		}))
	}))
	t.Cleanup(srv.Close)

	out, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "join", testInviteToken)
	require.NoError(t, err)
	assert.Contains(t, out, "Already a member of org acme as admin")
}

// TestOrgJoin_NeverPrintsTheToken is the non-disclosure guarantee: an
// invitation token is a bearer credential, so no stream the command writes may
// contain it, whatever the server answers. Every path that renders something is
// covered, including a server that quotes the token back in its problem detail
// and a --json run that dumps the whole wire object.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgJoin_NeverPrintsTheToken(t *testing.T) {
	cases := []struct {
		name    string
		respond func(w http.ResponseWriter)
		args    []string
		wantErr bool
	}{
		{
			name: "success",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, printJSON(w, testAcceptedInvitation()))
			},
		},
		{
			name: "success with --json",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, printJSON(w, testAcceptedInvitation()))
			},
			args: []string{"--json"},
		},
		{
			// The control plane should not echo a credential; the CLI must not
			// depend on that.
			name: "server quotes the token back in its problem detail",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, err := fmt.Fprintf(w, `{"status":404,"detail":"no invitation matches token %s"}`, testInviteToken)
				assert.NoError(t, err)
			},
			wantErr: true,
		},
		{
			name: "expired invitation",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusGone)
				_, err := fmt.Fprint(w, `{"status":410,"detail":"this invitation has expired"}`)
				assert.NoError(t, err)
			},
			wantErr: true,
		},
		{
			name: "server returns an undecodable body",
			respond: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, err := fmt.Fprint(w, `{"orgId":`)
				assert.NoError(t, err)
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				tc.respond(w)
			}))
			t.Cleanup(srv.Close)

			// ENTIRE_DEBUG turns on the transport's trace lines, the one other
			// place a request could be described on the way out.
			t.Setenv("ENTIRE_DEBUG", "1")

			args := append([]string{"join", testInviteToken}, tc.args...)
			out, errOut, err := runCoreCmd(t, newOrgCmd, srv.URL, args...)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.NotContains(t, out, testInviteToken, "stdout leaked the invitation token")
			assert.NotContains(t, errOut, testInviteToken, "stderr leaked the invitation token")
			if err != nil {
				assert.NotContains(t, err.Error(), testInviteToken, "the returned error leaked the invitation token")
			}
		})
	}
}

// A token beginning with a dash parses as a flag, and cobra's own error quotes
// the offending argument. The replacement message must explain the problem
// without reproducing the credential.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgJoin_AFlagLikeTokenDoesNotReachTheError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unparsed argument must not reach the control plane")
	}))
	t.Cleanup(srv.Close)

	dashed := "--" + testInviteToken
	out, errOut, err := runCoreCmd(t, newOrgCmd, srv.URL, "join", dashed)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), testInviteToken, "the flag-parse error leaked the invitation token")
	assert.NotContains(t, out, testInviteToken)
	assert.NotContains(t, errOut, testInviteToken)
	assert.Contains(t, err.Error(), "pass it after `--`")
}

// After `--`, the same token is a positional argument again.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgJoin_ADashedTokenWorksAfterTheSeparator(t *testing.T) {
	var gotBody coreapi.AcceptInvitationInputBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, printJSON(w, testAcceptedInvitation()))
	}))
	t.Cleanup(srv.Close)

	dashed := "--" + testInviteToken
	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "join", "--", dashed)
	require.NoError(t, err)
	assert.Equal(t, dashed, gotBody.Token)
}

func TestRedactToken(t *testing.T) {
	t.Parallel()
	err := errors.New("no invitation matches token " + testInviteToken + ", try again")
	redacted := redactToken(err, testInviteToken)
	require.EqualError(t, redacted, "no invitation matches token <redacted>, try again")

	untouched := errors.New("this invitation has expired")
	require.EqualError(t, redactToken(untouched, testInviteToken), "this invitation has expired")

	require.NoError(t, redactToken(nil, testInviteToken))
}

// The token travels in the request body, so a URL the transport logs or an
// error names carries no credential. Pinning that keeps a future move of the
// token to a path or query parameter from passing silently.
func TestOrgJoin_TokenTravelsInTheBodyNotTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotContains(t, r.URL.String(), testInviteToken, "the token must not travel in the URL")
		assert.NotContains(t, strings.Join(r.Header.Values("Authorization"), " "), testInviteToken)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, printJSON(w, testAcceptedInvitation()))
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgCmd, srv.URL, "join", testInviteToken)
	require.NoError(t, err)
}
