package model

import (
	"time"
)

// HandleState is multi-valued because IdP lifecycle is multi-valued.
// Don't collapse this to a boolean: "suspended by IdP/admin" and
// "removed from directory" need different semantics (the latter drops
// memberships and keeps an audit tombstone).
//
// State lives on the handle, not the account: a person whose WorkOS
// handle gets deprovisioned at one job should keep their other handles
// (GitHub, Bluesky) working. Per the architecture mapping, our `Account`
// is the architecture's "Person" and our `Handle` is the architecture's
// "Account" — the unit of access control and lifecycle.
type HandleState string

const (
	HandleStateActive    HandleState = "active"
	HandleStateSuspended HandleState = "suspended"
	HandleStateDeleted   HandleState = "deleted"
)

type Account struct {
	ID               AccountID `json:"id"`
	HomeJurisdiction string    `json:"home_jurisdiction"`
	CreatedAt        time.Time `json:"created_at"`
}

type Handle struct {
	AccountID      AccountID   `json:"-"`
	Provider       string      `json:"provider"`
	Handle         string      `json:"handle"`
	IsPrimary      bool        `json:"is_primary"`
	ProviderUserID string      `json:"provider_user_id"`
	State          HandleState `json:"state"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// PublicProfile is the global, non-PII portion of a user's profile.
// Only contains data that is not personally identifiable — provider
// avatar URLs and the handle (which is the provider's username).
type PublicProfile struct {
	AccountID AccountID `json:"account_id"`
	AvatarURL string    `json:"avatar_url"`
}

type Credential struct {
	ID        CredentialID `json:"id"`
	AccountID AccountID    `json:"account_id"`
	Type      string       `json:"type"`
	PublicKey []byte       `json:"public_key"`
	Label     string       `json:"label"`
	Scope     string       `json:"scope"`
	ExpiresAt *time.Time   `json:"expires_at,omitempty"`
	CreatedAt time.Time    `json:"created_at"`
}

// PrivateProfile is the regional PII portion of a user's profile.
// Stored in the user's home jurisdiction, never leaves the region.
type PrivateProfile struct {
	AccountID     AccountID
	DisplayName   string
	Email         string
	EmailVerified bool
	Bio           string
	Company       string
	Location      string
}

type AuditEntry struct {
	ID        ID
	AccountID AccountID
	EventType string
	IPAddress string
	Metadata  map[string]any
	CreatedAt time.Time
}

type Org struct {
	ID                   OrgID
	Name                 string
	Region               string
	WorkOSOrganizationID string
	CreatedAt            time.Time
}

type Membership struct {
	ID        MembershipID `json:"id"`
	AccountID AccountID    `json:"account_id"`
	OrgID     OrgID        `json:"org_id"`
	Role      string       `json:"role"`   // "owner", "admin", "member"
	Status    string       `json:"status"` // "active", "inactive"
	// WorkOSOrgMembershipID is omitted from JSON for non-WorkOS rows so
	// non-SSO orgs don't see a leaked empty field in their membership API.
	WorkOSOrgMembershipID string    `json:"workos_org_membership_id,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

type Team struct {
	ID            TeamID
	OrgID         OrgID
	ParentTeamID  *TeamID
	Name          string
	WorkOSGroupID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Project struct {
	ID        ProjectID
	Name      string
	OwnerType string // "account", "org"
	OwnerID   ID     // polymorphic: AccountID or OrgID
	Region    string
	CreatedAt time.Time
}

// RepoState is the provisioning lifecycle state for a repo row. Values
// are mirrored in the repos_state_check SQL CHECK constraint; keep in sync.
type RepoState string

const (
	RepoStateProvisioning RepoState = "provisioning"
	RepoStateActive       RepoState = "active"
	RepoStateFailed       RepoState = "failed"
)

// Repo is the jurisdictional record: the full lifecycle and operational
// metadata for a repo, stored in its home jurisdiction's database. The
// jurisdiction itself isn't a field — every row lives in exactly the
// jurisdiction whose store it was read from; the regional store stamps
// its own jurisdiction onto registry rows at publish time.
type Repo struct {
	ID                RepoID
	OwningProjectID   ProjectID
	Name              string
	State             RepoState
	ProvisionAttempts int
	ProvisionReason   string
	Mirror            *MirrorMetadata
	// ClusterSlug names the cluster within the home jurisdiction that hosts
	// this repo. Stamped at create/adopt time from the request's selector
	// (or the jurisdiction's default cluster when the caller didn't pin one)
	// and read back at MarkRepoActive so the same slug forwards to the
	// global registry. Empty for rows created before the cluster-stamping
	// rollout — backfill covers them.
	ClusterSlug string
	// ObjectFormat is the git object hash format for this repo: "sha1" or
	// "sha256". Chosen at create time and immutable thereafter — Git has no
	// in-place conversion between formats. The data plane reads this back
	// on every hashing/packfile path. String rather than a typed enum so
	// core/model stays free of any data-plane (go-git fork) dependency; the
	// bridge to formatcfg.ObjectFormat lives at the proto/data-plane edge.
	// Empty on rows created before the rollout — InsertRepo writes "sha1"
	// when the caller leaves it empty so the Go and SQL sides stay aligned.
	ObjectFormat string
}

// MirrorMetadata describes a repo that mirrors an external upstream. Persisted
// as the mirror JSONB column on regional.repos; nil on the model means the
// row has no mirror metadata (a normal entiredb-native repo).
type MirrorMetadata struct {
	Provider       string `json:"provider"`
	RemoteOwner    string `json:"remote_owner"`
	RemoteRepo     string `json:"remote_repo"`
	RemoteURL      string `json:"remote_url"`
	InstallationID int64  `json:"installation_id,omitempty"`
}

// RepoRegistration is the global registry row — what exists and where it
// lives. Written when a jurisdictional provisioning succeeds; read for
// id → jurisdiction routing and cross-jurisdiction admin listings.
//
// ClusterSlug names the cluster within Jurisdiction that hosts the repo.
// Empty for rows written before the cluster-stamping rollout — the backfill
// covers them. Slug rather than cluster ID because clusters.slug is globally
// unique (migration 009) and is the same identifier embedded in JWT
// audiences (service:entiredb:<slug>).
type RepoRegistration struct {
	ID              RepoID
	OwningProjectID ProjectID
	Name            string
	Jurisdiction    string
	ClusterSlug     string
}

// MirrorRegistration is the global mirror-coords pointer. One row per
// jurisdictional mirror; (provider, remote_owner, remote_repo, jurisdiction)
// is unique. Read by STS to resolve a `mirror/<provider>/<owner>/<repo>`
// path to its repo_id without needing to live in the same jurisdiction
// that owns the mirror.
type MirrorRegistration struct {
	RepoID       RepoID
	Provider     string
	RemoteOwner  string
	RemoteRepo   string
	Jurisdiction string
}

// MirrorPlacement points an upstream-mirror tuple at the cluster that homes
// it. One per jurisdictional mirror, joined from mirror_registry and
// repo_registry. A single (provider, remote_owner, remote_repo) can resolve
// to several placements when the same upstream is mirrored in multiple
// jurisdictions.
type MirrorPlacement struct {
	RepoID       RepoID
	Jurisdiction string
	ClusterSlug  string
}
