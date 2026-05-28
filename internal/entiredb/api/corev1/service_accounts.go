package corev1

import (
	"encoding/json"
	"time"
)

// ServiceAccount is the wire representation of a service account.
type ServiceAccount struct {
	AccountID     string    `json:"accountId"`
	Name          string    `json:"name"`
	OrgID         string    `json:"orgId"`
	Status        string    `json:"status"`
	SystemManaged bool      `json:"systemManaged"`
	CreatedAt     time.Time `format:"date-time"   json:"createdAt"`
}

// ServiceAccountWithGrants extends ServiceAccount with platform-level
// SpiceDB relations the SA holds.
type ServiceAccountWithGrants struct {
	ServiceAccount

	Grants []string `json:"grants"`
}

// CreateServiceAccountInput is the body for POST /api/v1/service-accounts.
type CreateServiceAccountInput struct {
	Body struct {
		OrgID string `json:"orgId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" required:"true"`
		Name  string `json:"name"  minLength:"1"                      required:"true"`
	}
}

// CreateServiceAccountOutput is the response.
type CreateServiceAccountOutput struct {
	Status int
	Body   ServiceAccount
}

// ListServiceAccountsInput is the input for GET /api/v1/service-accounts.
type ListServiceAccountsInput struct {
	OrgID string `pattern:"^[0-9A-HJKMNP-TV-Z]{26}$" query:"orgId" required:"true"`
}

// ListServiceAccountsOutput is the response.
type ListServiceAccountsOutput struct {
	Body struct {
		ServiceAccounts []ServiceAccountWithGrants `json:"serviceAccounts"`
	}
}

// GetServiceAccountInput is the path key.
type GetServiceAccountInput struct {
	AccountID string `minLength:"1" path:"accountId"`
}

// GetServiceAccountOutput is the response.
type GetServiceAccountOutput struct {
	Body ServiceAccount
}

// DeleteServiceAccountInput is the path key.
type DeleteServiceAccountInput struct {
	AccountID string `minLength:"1" path:"accountId"`
}

// DeleteServiceAccountOutput is empty (204).
type DeleteServiceAccountOutput struct {
	Status int
}

// ServiceAccountGrant is one (resourceType, resourceId, role) row.
type ServiceAccountGrant struct {
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceId"`
	ResourceName string `json:"resourceName,omitempty"`
	Role         string `json:"role"`
}

// GrantServiceAccountAccessInput is the body for granting access.
type GrantServiceAccountAccessInput struct {
	AccountID string `minLength:"1" path:"accountId"`
	Body      struct {
		ResourceType string `enum:"repo,project" json:"resourceType" required:"true"`
		ResourceID   string `json:"resourceId"   minLength:"1"       required:"true"`
		Role         string `json:"role"         minLength:"1"       required:"true"`
	}
}

// GrantServiceAccountAccessOutput is the grant response.
type GrantServiceAccountAccessOutput struct {
	Status int
	Body   struct {
		Status string `json:"status"`
	}
}

// ListServiceAccountGrantsInput is the path key.
type ListServiceAccountGrantsInput struct {
	AccountID string `minLength:"1" path:"accountId"`
}

// ListServiceAccountGrantsOutput is the response.
type ListServiceAccountGrantsOutput struct {
	Body struct {
		Grants []ServiceAccountGrant `json:"grants"`
	}
}

// RevokeServiceAccountAccessInput is the path key.
type RevokeServiceAccountAccessInput struct {
	AccountID    string `minLength:"1"       path:"accountId"`
	ResourceType string `enum:"repo,project" path:"resourceType"`
	ResourceID   string `minLength:"1"       path:"resourceId"`
}

// RevokeServiceAccountAccessOutput is empty (204).
type RevokeServiceAccountAccessOutput struct {
	Status int
}

// Binding is the wire representation of an external-identity binding.
type Binding struct {
	ID              string          `json:"id"`
	AccountID       string          `json:"accountId"`
	ProviderID      string          `json:"providerId"`
	AttributeFilter json.RawMessage `json:"attributeFilter"`
	CreatedAt       time.Time       `format:"date-time"     json:"createdAt"`
}

// CreateBindingInput is the body for binding creation.
type CreateBindingInput struct {
	AccountID string `minLength:"1" path:"accountId"`
	Body      struct {
		ProviderID      string          `json:"providerId"                                                minLength:"1"                    required:"true"`
		AttributeFilter json.RawMessage `doc:"Exact-match key/value map; empty filter matches any token." json:"attributeFilter,omitempty"`
	}
}

// CreateBindingOutput is the response.
type CreateBindingOutput struct {
	Status int
	Body   Binding
}

// ListBindingsInput is the path key.
type ListBindingsInput struct {
	AccountID string `minLength:"1" path:"accountId"`
}

// ListBindingsOutput is the response.
type ListBindingsOutput struct {
	Body struct {
		Bindings []Binding `json:"bindings"`
	}
}

// DeleteBindingInput is the path key.
type DeleteBindingInput struct {
	AccountID string `minLength:"1" path:"accountId"`
	BindingID string `minLength:"1" path:"bindingId"`
}

// DeleteBindingOutput is empty (204).
type DeleteBindingOutput struct {
	Status int
}
