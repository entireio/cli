package corev1

// MeAuth describes the IdP-side identity that authenticated the calling
// account on the current session.
type MeAuth struct {
	Provider       string `json:"provider"`
	ProviderUserID string `json:"providerUserId"`
}

// MeIdentityHandle is one provider/handle pair the calling account is
// known under (account may have multiple linked identities).
type MeIdentityHandle struct {
	Provider       string `json:"provider"`
	Handle         string `json:"handle"`
	ProviderUserID string `json:"providerUserId"`
}

// MeGlobal is the cross-jurisdiction view of the calling account: shape
// that's safe to expose anywhere because it carries no PII.
type MeGlobal struct {
	AccountID        string             `json:"accountId"`
	Handle           string             `json:"handle,omitempty"`
	HomeJurisdiction string             `json:"homeJurisdiction,omitempty"`
	AvatarURL        string             `json:"avatarUrl,omitempty"`
	Handles          []MeIdentityHandle `json:"handles"`
}

// MeRegional is the PII-bearing slice of the account, only populated
// when the responding node is the account's home jurisdiction.
type MeRegional struct {
	DisplayName string `json:"displayName,omitempty"`
	Email       string `json:"email,omitempty"`
	Bio         string `json:"bio,omitempty"`
	Company     string `json:"company,omitempty"`
	Location    string `json:"location,omitempty"`
}

// MeRegionalUnavailable is returned in place of MeRegional when the
// responding node is NOT the account's home jurisdiction. It tells the
// caller where to fetch the regional view.
//
// `error` is the load-bearing discriminator — SPAs and SDKs pattern-
// match on `error === "foreign_jurisdiction"` to dispatch the
// "open in home console" state instead of treating an empty regional
// block as "no profile yet".
type MeRegionalUnavailable struct {
	Error        string `doc:"Always 'foreign_jurisdiction'. Discriminator for client-side state machines." enum:"foreign_jurisdiction" json:"error"`
	Jurisdiction string `doc:"The account's home jurisdiction (e.g. 'us', 'eu')."                           json:"jurisdiction"`
	HomeCoreURL  string `doc:"Deep link into the home console for this account."                            json:"homeCoreUrl"`
	Message      string `doc:"Human-readable copy ready to surface in a UI."                                json:"message"`
}

// GetMeOutput is the response for GET /api/v1/me.
type GetMeOutput struct {
	Body struct {
		Global              MeGlobal               `json:"global"`
		Regional            *MeRegional            `json:"regional,omitempty"`
		RegionalUnavailable *MeRegionalUnavailable `json:"regionalUnavailable,omitempty"`
		Auth                MeAuth                 `json:"auth"`
		Mode                string                 `enum:"standalone,global,regional"    json:"mode,omitempty"`
		Jurisdiction        string                 `json:"jurisdiction,omitempty"`
	}
}

// ListAuditEventsOutput is the response for GET /api/v1/audit.
type ListAuditEventsOutput struct {
	Body struct {
		Events []AuditEvent `json:"events"`
	}
}

// ResolveHandleInput is the path-keyed input for
// GET /api/v1/identity/handles/{provider}/{handle}.
type ResolveHandleInput struct {
	Provider string `doc:"IdP slug (e.g. \"github\")."          minLength:"1" path:"provider"`
	Handle   string `doc:"User-visible handle at the provider." minLength:"1" path:"handle"`
}

// ResolvedIdentity is the body of a successful handle lookup.
type ResolvedIdentity struct {
	AccountID      string `json:"accountId"`
	Provider       string `json:"provider"`
	Handle         string `json:"handle"`
	ProviderUserID string `json:"providerUserId"`
}

// ResolveHandleOutput is the response for the handle lookup.
type ResolveHandleOutput struct {
	Body ResolvedIdentity
}

// OIDCProvider is the public view of a federated OIDC provider that
// callers can mint service-account bindings against.
type OIDCProvider struct {
	ID          string `json:"id"`
	Issuer      string `json:"issuer"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
}

// ListOIDCProvidersOutput is the response for GET /api/v1/oidc-providers.
type ListOIDCProvidersOutput struct {
	Body struct {
		Providers []OIDCProvider `json:"providers"`
	}
}

// LookupResourcesInput is the input for GET /api/v1/access/{resourceType}.
// When Permission is set, the response is a flat list of resource IDs.
// When Permission is empty, the response is a richer per-resource list
// with every permission the caller has on each.
type LookupResourcesInput struct {
	ResourceType string `doc:"SpiceDB resource type (e.g. \"repo\", \"project\", \"org\")."        minLength:"1"      path:"resourceType"`
	Permission   string `doc:"Optional: only list resources where the caller has this permission." query:"permission"`
}

// ResourceAccess pairs a resource ID with the permissions the caller
// holds on it.
type ResourceAccess struct {
	ResourceID  string   `json:"resourceId"`
	Permissions []string `json:"permissions"`
}

// LookupResourcesOutput is the response for the resource lookup.
type LookupResourcesOutput struct {
	Body struct {
		ResourceType string           `json:"resourceType"`
		Permission   string           `json:"permission,omitempty"`
		ResourceIDs  []string         `json:"resourceIds,omitempty"`
		Resources    []ResourceAccess `json:"resources,omitempty"`
	}
}

// GetPermissionsInput is the input for
// GET /api/v1/access/{resourceType}/{resourceId}.
type GetPermissionsInput struct {
	ResourceType string `minLength:"1" path:"resourceType"`
	ResourceID   string `minLength:"1" path:"resourceId"`
	// Explain triggers SpiceDB's explain trace for one specific
	// permission. The trace structure is provider-defined; emitted as
	// arbitrary JSON.
	Explain string `doc:"If set, return the SpiceDB trace for this permission instead of the permission list." query:"explain"`
}

// GetPermissionsOutput is the response for the permissions lookup.
// Exactly one of Permissions / Explain is populated.
type GetPermissionsOutput struct {
	Body struct {
		ResourceType string         `json:"resourceType"`
		ResourceID   string         `json:"resourceId"`
		Permissions  []string       `json:"permissions,omitempty"`
		Explain      map[string]any `json:"explain,omitempty"`
	}
}

// LookupRef is a single (type, id) pair sent to the batch lookup endpoint.
type LookupRef struct {
	Type string `doc:"Resource type slug; \"org\", \"project\", \"repo\" are enriched, unknown types pass through." json:"type"   minLength:"1"`
	ID   string `json:"id"                                                                                          minLength:"1"`
}

// LookupRefResult is the enriched form of a LookupRef. For unknown types
// only Type and ID are populated. For known types, fields are populated
// when the caller has visibility; inaccessible refs are omitted entirely
// from the response.
type LookupRefResult struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	OwnerID   string `json:"ownerId,omitempty"`
	OwnerType string `enum:"org,account"         json:"ownerType,omitempty"`
	ProjectID string `json:"projectId,omitempty"`
	URL       string `json:"url,omitempty"`
}

// BatchLookupInput is the body for POST /api/v1/lookup.
type BatchLookupInput struct {
	Body struct {
		Refs []LookupRef `json:"refs" maxItems:"100" minItems:"1" required:"true"`
	}
}

// BatchLookupOutput is the response for POST /api/v1/lookup.
type BatchLookupOutput struct {
	Body struct {
		Refs []LookupRefResult `json:"refs"`
	}
}
