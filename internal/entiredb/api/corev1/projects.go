package corev1

import "time"

// Project is the wire representation of a project.
type Project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	OwnerType string    `enum:"org,account" json:"ownerType"`
	OwnerID   string    `json:"ownerId"`
	Region    string    `json:"region"`
	CreatedAt time.Time `format:"date-time" json:"createdAt"`
}

// CreateProjectInput is the body for POST /api/v1/projects.
type CreateProjectInput struct {
	Body struct {
		Name      string `json:"name"             maxLength:"100"  minLength:"1"   required:"true"`
		OwnerType string `enum:"org,account"      json:"ownerType" required:"true"`
		OwnerID   string `json:"ownerId"          minLength:"1"    required:"true"`
		Region    string `json:"region,omitempty"`
	}
}

// CreateProjectOutput is the response for project creation.
type CreateProjectOutput struct {
	Status int
	Body   Project
}

// ListProjectsInput is the input for GET /api/v1/projects. When Name is
// set the response is a single project (404 if no match). Otherwise the
// response is the projects accessible to the caller.
type ListProjectsInput struct {
	Name string `doc:"Optional: exact-match project name." query:"name"`
}

// ListProjectsOutput is the response for project listing/get-by-name.
type ListProjectsOutput struct {
	Body struct {
		Project  *Project  `json:"project,omitempty"`
		Projects []Project `json:"projects,omitempty"`
	}
}

// ListOrgProjectsInput is the input for GET /api/v1/orgs/{orgId}/projects.
type ListOrgProjectsInput struct {
	OrgID string `path:"orgId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// ListOrgProjectsOutput is the response for org-scoped project listing.
type ListOrgProjectsOutput struct {
	Body struct {
		Projects []Project `json:"projects"`
	}
}

// ProjectGrant is the wire representation of one grant on a project.
type ProjectGrant struct {
	GranteeType string `json:"granteeType"`
	GranteeID   string `json:"granteeId"`
	Role        string `json:"role"`
}

// ListProjectMembersInput is the input for
// GET /api/v1/projects/{projectId}/members.
type ListProjectMembersInput struct {
	ProjectID string `path:"projectId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// ListProjectMembersOutput is the response for member listing.
type ListProjectMembersOutput struct {
	Body struct {
		Members []ProjectGrant `json:"members"`
	}
}

// GrantProjectAccessInput is the body for
// POST /api/v1/projects/{projectId}/grants.
type GrantProjectAccessInput struct {
	ProjectID string `path:"projectId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body      struct {
		Provider       string `json:"provider"       minLength:"1"           required:"true"`
		ProviderUserID string `json:"providerUserId" minLength:"1"           required:"true"`
		GranteeType    string `default:"account"     enum:"account,org,team" json:"granteeType,omitempty"`
		Role           string `json:"role"           minLength:"1"           required:"true"`
	}
}

// GrantProjectAccessOutput is the response for a grant write.
type GrantProjectAccessOutput struct {
	Status int
	Body   struct {
		Status string `json:"status"`
	}
}

// RevokeProjectAccessByProviderInput is the path key for
// DELETE /api/v1/projects/{projectId}/grants/account/{provider}/{providerUserId}.
type RevokeProjectAccessByProviderInput struct {
	ProjectID      string `path:"projectId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Provider       string `minLength:"1"    path:"provider"`
	ProviderUserID string `minLength:"1"    path:"providerUserId"`
}

// RevokeProjectAccessInput is the path key for
// DELETE /api/v1/projects/{projectId}/grants/{granteeType}/{granteeId}.
type RevokeProjectAccessInput struct {
	ProjectID   string `path:"projectId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	GranteeType string `minLength:"1"    path:"granteeType"`
	GranteeID   string `minLength:"1"    path:"granteeId"`
}

// RevokeProjectAccessOutput is empty (204).
type RevokeProjectAccessOutput struct {
	Status int
}
