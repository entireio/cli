package corev1

import "time"

// Org is the wire representation of an organization.
type Org struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Region               string    `json:"region"`
	WorkOSOrganizationID string    `json:"workosOrganizationId,omitempty"`
	CreatedAt            time.Time `format:"date-time"                    json:"createdAt"`
}

// CreateOrgInput is the body for POST /api/v1/orgs.
type CreateOrgInput struct {
	Body struct {
		Name   string `doc:"Display name."                                                  json:"name"             maxLength:"100" minLength:"1" required:"true"`
		Region string `doc:"Jurisdiction slug; defaults to the server's home jurisdiction." json:"region,omitempty"`
	}
}

// CreateOrgOutput is the response for POST /api/v1/orgs.
type CreateOrgOutput struct {
	Status int
	Body   Org
}

// ListOrgsOutput is the response for GET /api/v1/orgs.
type ListOrgsOutput struct {
	Body struct {
		Orgs []Org `json:"orgs"`
	}
}

// Membership is the wire representation of an org membership row.
type Membership struct {
	ID                    string    `json:"id"`
	AccountID             string    `json:"accountId"`
	OrgID                 string    `json:"orgId"`
	Role                  string    `json:"role"`
	Status                string    `json:"status"`
	WorkOSOrgMembershipID string    `json:"workosOrgMembershipId,omitempty"`
	CreatedAt             time.Time `format:"date-time"                     json:"createdAt"`
}

// AddOrgMemberInput is the body for POST /api/v1/orgs/{orgId}/members.
type AddOrgMemberInput struct {
	OrgID string `path:"orgId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Body  struct {
		Provider       string `json:"provider"       minLength:"1"                              required:"true"`
		ProviderUserID string `json:"providerUserId" minLength:"1"                              required:"true"`
		Role           string `default:"member"      doc:"Role at the org; defaults to member." enum:"owner,admin,member" json:"role,omitempty"`
	}
}

// AddOrgMemberOutput is the response for adding a member.
type AddOrgMemberOutput struct {
	Status int
	Body   Membership
}

// RemoveOrgMemberInput is the path key for
// DELETE /api/v1/orgs/{orgId}/members/{provider}/{providerUserId}.
type RemoveOrgMemberInput struct {
	OrgID          string `path:"orgId"  pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
	Provider       string `minLength:"1" path:"provider"`
	ProviderUserID string `minLength:"1" path:"providerUserId"`
}

// RemoveOrgMemberOutput is empty — the operation is a 204.
type RemoveOrgMemberOutput struct {
	Status int
}

// ListOrgMembersInput is the path key for GET /api/v1/orgs/{orgId}/members.
type ListOrgMembersInput struct {
	OrgID string `path:"orgId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// ListOrgMembersOutput is the response for listing members.
type ListOrgMembersOutput struct {
	Body struct {
		Members []Membership `json:"members"`
	}
}
