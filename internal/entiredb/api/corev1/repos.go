package corev1

// RepoMirror describes a repo that mirrors an external upstream.
type RepoMirror struct {
	Provider       string `json:"provider"`
	RemoteOwner    string `json:"remoteOwner"`
	RemoteRepo     string `json:"remoteRepo"`
	RemoteURL      string `json:"remoteUrl,omitempty"`
	InstallationID int64  `json:"installationId,omitempty"`
}

// Repo is the wire representation of a repo. Foreign=true indicates a
// registry-only row (only ID/OwningProjectID/Name are set); State/Mirror/
// Provision-*/ClusterHost/Path are unavailable because the home regional
// plane owns them.
type Repo struct {
	ID                string      `json:"id"`
	OwningProjectID   string      `json:"owningProjectId"`
	Name              string      `json:"name"`
	State             string      `enum:"provisioning,active,failed"  json:"state,omitempty"`
	ProvisionAttempts int         `json:"provisionAttempts,omitempty"`
	ProvisionReason   string      `json:"provisionReason,omitempty"`
	Mirror            *RepoMirror `json:"mirror,omitempty"`
	ObjectFormat      string      `enum:"sha1,sha256"                 json:"objectFormat,omitempty"`
	Foreign           bool        `json:"foreign,omitempty"`
	// ClusterHost and Path locate the repo: the entire:// remote is
	// "entire://" + ClusterHost + Path (e.g. "entire://royalcanin.partial.to" +
	// "/et/<project>/<repo>"). Both are populated on single-repo responses
	// (create, get) and omitted from list responses, which would otherwise
	// need a per-row repo_urls + cluster lookup.
	ClusterHost string `json:"clusterHost,omitempty"`
	Path        string `json:"path,omitempty"`
}

// CreateRepoInput is the body for POST /api/v1/repos.
type CreateRepoInput struct {
	Body struct {
		ProjectID    string `json:"projectId"                                                                                                           pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" required:"true"`
		Name         string `json:"name"                                                                                                                minLength:"1"                      required:"true"`
		ClusterHost  string `doc:"Public host of the cluster to pin the repo to (e.g. royalcanin.partial.to); empty lands on the jurisdiction default." json:"clusterHost,omitempty"`
		ObjectFormat string `doc:"Hash format; defaults to sha1."                                                                                       enum:"sha1,sha256"                 json:"objectFormat,omitempty"`
	}
}

// CreateRepoOutput is the response. Status is 201 when active or 202
// when still provisioning.
type CreateRepoOutput struct {
	Status int
	Body   Repo
}

// GetRepoInput is the path key for GET /api/v1/repos/{repoId}.
type GetRepoInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// GetRepoOutput is the response.
type GetRepoOutput struct {
	Body Repo
}

// DeleteRepoInput is the path key for DELETE /api/v1/repos/{repoId}.
type DeleteRepoInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// DeleteRepoOutput is empty (204). Retryable 503 paths carry their
// Retry-After header on the problem+json error response via
// huma.ErrorWithHeaders (see DeleteRepo).
type DeleteRepoOutput struct {
	Status int
}

// LookupRepoByMirrorInput is the input for
// GET /api/v1/repos/by-mirror/{provider}/{owner}/{repo}?clusterHost=<host>.
type LookupRepoByMirrorInput struct {
	Provider    string `minLength:"1"                                                  path:"provider"`
	Owner       string `minLength:"1"                                                  path:"owner"`
	Repo        string `minLength:"1"                                                  path:"repo"`
	ClusterHost string `doc:"DNS host of the cluster to address the resolved repo on." query:"clusterHost" required:"true"`
}

// MirrorRepoPath is the body of a successful mirror lookup.
type MirrorRepoPath struct {
	RepoID   string `json:"repoId"`
	RepoPath string `doc:"Path under /git/ (e.g. \"owner/repo\")." json:"repoPath"`
}

// LookupRepoByMirrorOutput is the response.
type LookupRepoByMirrorOutput struct {
	Body MirrorRepoPath
}

// GrantRepoAccessInput is the body for POST /api/v1/repos/{repoId}/grants.
type GrantRepoAccessInput struct {
	RepoID string `path:"repoId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body   struct {
		Provider       string `json:"provider"       minLength:"1"           required:"true"`
		ProviderUserID string `json:"providerUserId" minLength:"1"           required:"true"`
		GranteeType    string `default:"account"     enum:"account,org,team" json:"granteeType,omitempty"`
		Role           string `json:"role"           minLength:"1"           required:"true"`
	}
}

// GrantRepoAccessOutput is the grant response.
type GrantRepoAccessOutput struct {
	Status int
	Body   struct {
		Status string `json:"status"`
	}
}

// ListProjectReposInput is the path key for
// GET /api/v1/projects/{projectId}/repos.
type ListProjectReposInput struct {
	ProjectID string `path:"projectId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// ListProjectReposOutput is the response.
type ListProjectReposOutput struct {
	Body struct {
		Repos []Repo `json:"repos"`
	}
}
