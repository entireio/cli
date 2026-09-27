package coreapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/entireio/auth-go/crossjuris"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Not parallel: ENTIRE_TOKEN is process-global. The only transport substitution
// trusts httptest's TLS certificate; URL validation, federation validation,
// redirect following, exchange and generated request encoding are real.
func TestRepoAuthoritativeCrossRegion(t *testing.T) {
	for _, mode := range []string{"bearer", "environment", "environment cluster"} {
		t.Run(mode, func(t *testing.T) {
			var creates, reads, redirects, exchanges atomic.Int32
			var subjects authRecorder
			const id = "01HZX7QABCDEFGHJKMNPQRSTVW"
			home := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == crossjuris.TokenPath {
					exchanges.Add(1)
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					subjects.add(r.PostForm.Get("subject_token"))
					fmt.Fprint(w, `{"access_token":"home-exchanged-jwt","token_type":"Bearer","expires_in":300}`)
					return
				}
				if r.Header.Get("Authorization") != bearerHomeExchanged {
					w.WriteHeader(http.StatusUnauthorized)
					fmt.Fprint(w, `{"error":"invalid token"}`)
					return
				}
				state := "provisioning"
				switch r.Method {
				case http.MethodPost:
					creates.Add(1)
					assert.Equal(t, "/api/v1/repos", r.URL.Path)
					w.WriteHeader(http.StatusCreated)
				case http.MethodGet:
					assert.Equal(t, "/api/v1/repos/"+id, r.URL.Path)
					assert.Equal(t, "true", r.URL.Query().Get("authoritative"))
					if reads.Add(1) == 2 {
						state = "active"
					}
				default:
					t.Errorf("unexpected %s", r.Method)
				}
				fmt.Fprintf(w, `{"id":%q,"name":"web","owningProjectId":%q,"provider":"entire","state":%q,"capabilities":{"canManage":true,"canPush":true,"canPull":true}}`, id, id, state)
			}))
			defer home.Close()
			origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == crossjuris.WellKnownPath {
					writeTestFederation(w, []string{home.URL})
					return
				}
				redirects.Add(1)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusMisdirectedRequest)
				fmt.Fprintf(w, `{"error":%q,"home_core_url":%q,"jurisdiction":"eu"}`, `cluster is in jurisdiction "eu"; retry against the home core`, home.URL)
			}))
			defer origin.Close()
			token := makeAudJWT(origin.URL)
			var c *Client
			var err error
			switch mode {
			case "environment":
				t.Setenv(auth.EnvTokenVar, token)
				c, err = New()
			case "environment cluster":
				t.Setenv(auth.EnvTokenVar, token)
				c, err = NewForCluster(t.Context(), "unused.example")
			default:
				c, err = NewWithBearer(origin.URL, token)
			}
			require.NoError(t, err)
			require.Equal(t, origin.URL, c.CoreOrigin())
			rt, err := newCrossJurisRoundTripper(home.Client().Transport, false)
			require.NoError(t, err)
			c.cfg.Client = &http.Client{Transport: rt}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			response, err := c.CreateRepo(ctx, &CreateRepoInputBody{Name: "web", ProjectId: id})
			require.NoError(t, err)
			created := response.Response
			require.Equal(t, "provisioning", created.State.Or(""))
			for _, state := range []string{"provisioning", "active"} {
				snapshot, err := c.GetRepo(ctx, GetRepoParams{RepoId: created.ID, Authoritative: NewOptBool(true)})
				require.NoError(t, err)
				require.Equal(t, state, snapshot.State.Or(""))
			}
			require.EqualValues(t, 1, creates.Load())
			require.EqualValues(t, 2, reads.Load())
			require.EqualValues(t, 3, redirects.Load(), "the initial core is revisited for every logical call")
			require.EqualValues(t, 1, exchanges.Load(), "exchange token is cached across polls")
			require.Equal(t, []string{token}, subjects.snapshot())
		})
	}
}
