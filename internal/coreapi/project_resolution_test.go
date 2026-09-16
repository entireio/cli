package coreapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveProjectPublicReference(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/projects/resolve/gh/Acme", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		_, err := w.Write([]byte(`{"project":{"id":"project-id","name":"private-storage-name","region":"eu","primaryProcessingCell":"cell-eu"},"reference":{"host":"gh","project":"acme"}}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := NewWithBearer(server.URL, "test-token")
	require.NoError(t, err)
	out, err := client.ResolveProject(t.Context(), "gh", "Acme")
	require.NoError(t, err)
	require.Equal(t, "acme", out.Reference.Project)
	require.Equal(t, "eu", out.Project.Region)
	require.Equal(t, "cell-eu", out.Project.PrimaryProcessingCell)
}

func TestResolveProjectFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"denied", 404, `{"detail":"Project not found"}`, "HTTP 404 Project not found"},
		{"malformed", 200, `{`, "decode project resolution"},
		{"missing", 200, `{"project":null}`, "missing project identity"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, err := w.Write([]byte(tt.body))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)
			client, err := NewWithBearer(server.URL, "test-token")
			require.NoError(t, err)
			_, err = client.ResolveProject(t.Context(), "gh", "acme")
			require.ErrorContains(t, err, tt.want)
		})
	}
}
