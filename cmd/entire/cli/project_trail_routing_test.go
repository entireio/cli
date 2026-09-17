package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// Not parallel: command fixtures replace the routing constructors.
func TestProjectTrailCreationUsesCoreAPIURLWithoutCatalog(t *testing.T) {
	for _, forge := range []string{"gh", "et"} {
		t.Run(forge, func(t *testing.T) {
			calls := 0
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/"+forge+"/acme/trails", r.URL.Path)
				assert.NotEmpty(t, r.Header.Get("Idempotency-Key"))
				assert.NoError(t, json.NewEncoder(w).Encode(projectTrailTestResource()))
			})
			// Core may route to a hidden assigned cluster. Even a catalog row
			// with a matching slug must not override the authoritative apiUrl.
			core.clusters = []coreapi.Cluster{{Slug: "project-cell", Jurisdiction: "eu", IsDefault: true,
				ApiUrl: coreapi.NewOptString("https://wrong.api.example")}}
			_, _, err := executeProjectTrailTest(t, "create", "--project", forge+"/acme", "--no-branch", "--title", "Intent")
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, core.resolveCalls)
			require.Zero(t, core.catalogCalls)
		})
	}
}

func TestProjectTrailUnavailableRoutingRefusesCreation(t *testing.T) {
	// No parallelism: mutates the constructors in each subtest.
	for _, tt := range []struct{ name, routing, want string }{
		{"older server", `"region":"eu"`, "primaryProcessingCell"},
		{"unassigned", `"region":"eu","primaryProcessingCell":"","apiUrl":""`, "primaryProcessingCell"},
		{"unavailable URL", `"region":"eu","primaryProcessingCell":"project-cell","apiUrl":""`, "apiUrl"},
		{"absent URL", `"region":"eu","primaryProcessingCell":"project-cell"`, "apiUrl"},
		{"missing jurisdiction", `"primaryProcessingCell":"project-cell","apiUrl":"https://assigned.api.example"`, "jurisdiction"},
		{"invalid URL", `"region":"eu","primaryProcessingCell":"project-cell","apiUrl":"/relative"`, "invalid project apiUrl"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, _ := setupProjectTrailTest(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unavailable routing must not reach a cell: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			})
			core.resolutionJSON = fmt.Sprintf(`{"project":{"id":%q,%s},"reference":{"host":"gh","project":"acme"}}`, projectTrailTestProject, tt.routing)
			_, _, err := executeProjectTrailTest(t, "create", "--project", "gh/acme", "--no-branch", "--title", "Intent")
			require.ErrorContains(t, err, tt.want)
			require.Zero(t, core.catalogCalls, "no catalog fallback, even though it contains an available cell")
		})
	}
}

func TestProjectTrailResolvedCellTarget(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, url, region, want string }{
		{"origin", "https://assigned.api.example", "eu", "https://assigned.api.example"},
		{"slash", "https://assigned.api.example/", "EU", "https://assigned.api.example"},
		{"local development", "http://localhost:8123", "eu", "http://localhost:8123"},
		{"relative", "/api/v1", "eu", ""},
		{"userinfo", "https://user:secret@assigned.api.example", "eu", ""},
		{"path", "https://assigned.api.example/unexpected", "eu", ""},
		{"query", "https://assigned.api.example?override=1", "eu", ""},
		{"empty query", "https://assigned.api.example?", "eu", ""},
		{"fragment", "https://assigned.api.example#fragment", "eu", ""},
		{"empty fragment", "https://assigned.api.example#", "eu", ""},
		{"scheme", "file:///tmp/api", "eu", ""},
		{"malformed", "https://%", "eu", ""},
		{"region", "https://assigned.api.example", "invalid/region", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, err := projectTrailResolvedCellTarget(tt.url, "assigned-cell", tt.region)
			if tt.want == "" {
				require.Error(t, err)
				require.Nil(t, target)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, target.BaseURL)
			require.Equal(t, "eu", target.Jurisdiction)
		})
	}
}
