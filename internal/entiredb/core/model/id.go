package model

import (
	"crypto/rand"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oklog/ulid/v2"
)

func newULID() ulid.ULID { return ulid.MustNew(ulid.Now(), rand.Reader) }

func parseULID(s string) (ulid.ULID, error) {
	u, err := ulid.Parse(s)
	if err != nil {
		return ulid.ULID{}, fmt.Errorf("invalid id %q: %w", s, err)
	}
	return u, nil
}

// LooksLikeRawULID reports whether s has the canonical 26-character ULID
// shape: Crockford base32, case-insensitive, no separators, and a first
// character in 0-7. It deliberately does not accept the more permissive
// strings that ulid.Parse accepts after normalization.
func LooksLikeRawULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	if s[0] < '0' || s[0] > '7' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isCrockfordBase32(s[i]) {
			return false
		}
	}
	return true
}

func isCrockfordBase32(c byte) bool {
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	if c >= '0' && c <= '9' {
		return true
	}
	switch c {
	case 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'J', 'K', 'M', 'N', 'P', 'Q', 'R', 'S', 'T', 'V', 'W', 'X', 'Y', 'Z':
		return true
	default:
		return false
	}
}

func scanULID(id *ulid.ULID, src any) error {
	switch v := src.(type) {
	case string:
		u, err := ulid.Parse(v)
		if err != nil {
			return fmt.Errorf("scan id: %w", err)
		}
		*id = u
		return nil
	case []byte:
		if err := id.UnmarshalText(v); err != nil {
			return fmt.Errorf("scan id: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("scan id: unsupported type %T", src)
	}
}

func valueULID(id ulid.ULID) (driver.Value, error) { return id.String(), nil }

func marshalULID(id ulid.ULID) ([]byte, error) {
	b, err := json.Marshal(id.String())
	if err != nil {
		return nil, fmt.Errorf("marshal id: %w", err)
	}
	return b, nil
}

func unmarshalULID(id *ulid.ULID, data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal id: %w", err)
	}
	u, err := ulid.Parse(s)
	if err != nil {
		return fmt.Errorf("unmarshal id: %w", err)
	}
	*id = u
	return nil
}

func marshalBinaryULID(id ulid.ULID) ([]byte, error) {
	b, err := id.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal binary id: %w", err)
	}
	return b, nil
}

func unmarshalBinaryULID(id *ulid.ULID, data []byte) error {
	if err := id.UnmarshalBinary(data); err != nil {
		return fmt.Errorf("unmarshal binary id: %w", err)
	}
	return nil
}

// ID is the polymorphic ID type used at boundaries that accept any
// entity (e.g. grant grantee = account-or-team-or-org). String-backed
// like AccountID so it can carry both bare-ULID and prefixed-ULID
// forms — every other typed ID still narrows to ulid.ULID.
type ID string

func NewID() ID { return ID(newULID().String()) }

func ParseID(s string) (ID, error) {
	if s == "" {
		return "", errors.New("invalid id: empty")
	}
	return ID(s), nil
}

func (id ID) String() string { return string(id) }

func (id *ID) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*id = ID(v)
		return nil
	case []byte:
		*id = ID(v)
		return nil
	default:
		return fmt.Errorf("scan id: unsupported type %T", src)
	}
}

func (id ID) Value() (driver.Value, error) { return string(id), nil }

func (id ID) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(string(id))
	if err != nil {
		return nil, fmt.Errorf("marshal id: %w", err)
	}
	return b, nil
}

func (id *ID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal id: %w", err)
	}
	*id = ID(s)
	return nil
}

// ServiceAccountIDPrefix is the type-prefix on every service-account ID
// (`svc_<ulid>`). User-account IDs carry no prefix today; once COR-115
// lands they will adopt `user_<ulid>`. The prefix-shapes are disjoint
// within the shared accounts.id column, which lets the database remain
// the source of truth for "is this account a service account" while
// keeping the kind eyeball-readable in logs.
const ServiceAccountIDPrefix = "svc_"

// AccountID identifies an account. String-backed (rather than the bare
// ulid.ULID typedef used for OrgID/RepoID/etc.) so it can carry both the
// legacy bare-ULID shape user accounts hold today and the prefixed
// shapes service accounts (svc_<ulid>) and future user accounts
// (user_<ulid>) hold.
type AccountID string

func NewAccountID() AccountID { return AccountID(newULID().String()) }

// NewServiceAccountID returns a fresh `svc_<ulid>` account ID for a
// service account. The ULID body uses Crockford's alphabet; the full
// value is disjoint from bare-ULID user IDs.
func NewServiceAccountID() AccountID {
	return AccountID(ServiceAccountIDPrefix + newULID().String())
}

// IsServiceAccount reports whether the account ID belongs to a service
// account (has the svc_ prefix). The database row in service_accounts
// remains authoritative; this helper is a fast eyeball check that
// matches the prefix without a query.
func (id AccountID) IsServiceAccount() bool {
	return strings.HasPrefix(string(id), ServiceAccountIDPrefix)
}

func ParseAccountID(s string) (AccountID, error) {
	if s == "" {
		return "", errors.New("invalid account id: empty")
	}
	body := s
	if rest, ok := strings.CutPrefix(s, ServiceAccountIDPrefix); ok {
		body = rest
	}
	if _, err := ulid.Parse(body); err != nil {
		return "", fmt.Errorf("invalid account id %q: %w", s, err)
	}
	return AccountID(s), nil
}

func (id AccountID) String() string { return string(id) }

func (id *AccountID) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*id = AccountID(v)
		return nil
	case []byte:
		*id = AccountID(v)
		return nil
	default:
		return fmt.Errorf("scan account id: unsupported type %T", src)
	}
}

func (id AccountID) Value() (driver.Value, error) { return string(id), nil }

func (id AccountID) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(string(id))
	if err != nil {
		return nil, fmt.Errorf("marshal account id: %w", err)
	}
	return b, nil
}

func (id *AccountID) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("unmarshal account id: %w", err)
	}
	*id = AccountID(s)
	return nil
}

// MarshalBinary serializes the ID as a 16-byte ULID for the entiredb
// meta-repo path, which stores user IDs in fixed-width protobuf bytes
// fields. Errors on prefixed IDs (svc_*) — service accounts don't enter
// the meta-repo path, so an attempt to round-trip one through binary
// encoding is a programmer error worth flagging loudly.
func (id AccountID) MarshalBinary() ([]byte, error) {
	u, err := ulid.Parse(string(id))
	if err != nil {
		return nil, fmt.Errorf("marshal binary account id: %q is not bare-ULID shaped: %w", string(id), err)
	}
	return marshalBinaryULID(u)
}

// UnmarshalBinary decodes a 16-byte ULID and stores its string form.
// Inverse of MarshalBinary; only meaningful for bare-ULID account IDs.
func (id *AccountID) UnmarshalBinary(data []byte) error {
	var u ulid.ULID
	if err := unmarshalBinaryULID(&u, data); err != nil {
		return err
	}
	*id = AccountID(u.String())
	return nil
}

// Bytes returns the 16-byte ULID form for bare-ULID account IDs, or nil
// for prefixed IDs. Prefixed IDs don't have a binary form.
func (id AccountID) Bytes() []byte {
	u, err := ulid.Parse(string(id))
	if err != nil {
		return nil
	}
	return u.Bytes()
}

// OrgID identifies an organization.
type OrgID ulid.ULID

func NewOrgID() OrgID { return OrgID(newULID()) }
func ParseOrgID(s string) (OrgID, error) {
	u, err := parseULID(s)
	return OrgID(u), err
}

func (id OrgID) String() string                   { return ulid.ULID(id).String() }
func (id *OrgID) Scan(src any) error              { return scanULID((*ulid.ULID)(id), src) }
func (id OrgID) Value() (driver.Value, error)     { return valueULID(ulid.ULID(id)) }
func (id OrgID) MarshalJSON() ([]byte, error)     { return marshalULID(ulid.ULID(id)) }
func (id *OrgID) UnmarshalJSON(data []byte) error { return unmarshalULID((*ulid.ULID)(id), data) }
func (id OrgID) MarshalBinary() ([]byte, error)   { return marshalBinaryULID(ulid.ULID(id)) }
func (id *OrgID) UnmarshalBinary(data []byte) error {
	return unmarshalBinaryULID((*ulid.ULID)(id), data)
}
func (id OrgID) Bytes() []byte { return ulid.ULID(id).Bytes() }

// IsZero reports whether id is the zero value. Used by the WorkOS SSO
// path to distinguish "no org binding" from "org_xxx binding" without
// allocating a comparison literal at each call site.
func (id OrgID) IsZero() bool { return id == OrgID{} }

// ProjectID identifies a project.
type ProjectID ulid.ULID

func NewProjectID() ProjectID { return ProjectID(newULID()) }
func ParseProjectID(s string) (ProjectID, error) {
	u, err := parseULID(s)
	return ProjectID(u), err
}

func (id ProjectID) String() string                   { return ulid.ULID(id).String() }
func (id *ProjectID) Scan(src any) error              { return scanULID((*ulid.ULID)(id), src) }
func (id ProjectID) Value() (driver.Value, error)     { return valueULID(ulid.ULID(id)) }
func (id ProjectID) MarshalJSON() ([]byte, error)     { return marshalULID(ulid.ULID(id)) }
func (id *ProjectID) UnmarshalJSON(data []byte) error { return unmarshalULID((*ulid.ULID)(id), data) }
func (id ProjectID) MarshalBinary() ([]byte, error)   { return marshalBinaryULID(ulid.ULID(id)) }
func (id *ProjectID) UnmarshalBinary(data []byte) error {
	return unmarshalBinaryULID((*ulid.ULID)(id), data)
}
func (id ProjectID) Bytes() []byte { return ulid.ULID(id).Bytes() }

// RepoID identifies a control-plane repository resource.
type RepoID ulid.ULID

func NewRepoID() RepoID { return RepoID(newULID()) }
func ParseRepoID(s string) (RepoID, error) {
	u, err := parseULID(s)
	return RepoID(u), err
}

func (id RepoID) String() string                   { return ulid.ULID(id).String() }
func (id *RepoID) Scan(src any) error              { return scanULID((*ulid.ULID)(id), src) }
func (id RepoID) Value() (driver.Value, error)     { return valueULID(ulid.ULID(id)) }
func (id RepoID) MarshalJSON() ([]byte, error)     { return marshalULID(ulid.ULID(id)) }
func (id *RepoID) UnmarshalJSON(data []byte) error { return unmarshalULID((*ulid.ULID)(id), data) }
func (id RepoID) MarshalBinary() ([]byte, error)   { return marshalBinaryULID(ulid.ULID(id)) }
func (id *RepoID) UnmarshalBinary(data []byte) error {
	return unmarshalBinaryULID((*ulid.ULID)(id), data)
}
func (id RepoID) Bytes() []byte { return ulid.ULID(id).Bytes() }

// TeamID identifies a team.
type TeamID ulid.ULID

func NewTeamID() TeamID { return TeamID(newULID()) }
func ParseTeamID(s string) (TeamID, error) {
	u, err := parseULID(s)
	return TeamID(u), err
}

func (id TeamID) String() string                   { return ulid.ULID(id).String() }
func (id *TeamID) Scan(src any) error              { return scanULID((*ulid.ULID)(id), src) }
func (id TeamID) Value() (driver.Value, error)     { return valueULID(ulid.ULID(id)) }
func (id TeamID) MarshalJSON() ([]byte, error)     { return marshalULID(ulid.ULID(id)) }
func (id *TeamID) UnmarshalJSON(data []byte) error { return unmarshalULID((*ulid.ULID)(id), data) }
func (id TeamID) MarshalBinary() ([]byte, error)   { return marshalBinaryULID(ulid.ULID(id)) }
func (id *TeamID) UnmarshalBinary(data []byte) error {
	return unmarshalBinaryULID((*ulid.ULID)(id), data)
}
func (id TeamID) Bytes() []byte { return ulid.ULID(id).Bytes() }

// MembershipID identifies an org membership.
type MembershipID ulid.ULID

func NewMembershipID() MembershipID { return MembershipID(newULID()) }
func ParseMembershipID(s string) (MembershipID, error) {
	u, err := parseULID(s)
	return MembershipID(u), err
}

func (id MembershipID) String() string               { return ulid.ULID(id).String() }
func (id *MembershipID) Scan(src any) error          { return scanULID((*ulid.ULID)(id), src) }
func (id MembershipID) Value() (driver.Value, error) { return valueULID(ulid.ULID(id)) }
func (id MembershipID) MarshalJSON() ([]byte, error) { return marshalULID(ulid.ULID(id)) }
func (id *MembershipID) UnmarshalJSON(data []byte) error {
	return unmarshalULID((*ulid.ULID)(id), data)
}
func (id MembershipID) MarshalBinary() ([]byte, error) { return marshalBinaryULID(ulid.ULID(id)) }
func (id *MembershipID) UnmarshalBinary(data []byte) error {
	return unmarshalBinaryULID((*ulid.ULID)(id), data)
}
func (id MembershipID) Bytes() []byte { return ulid.ULID(id).Bytes() }

// CredentialID identifies a credential.
type CredentialID ulid.ULID

func NewCredentialID() CredentialID { return CredentialID(newULID()) }
func ParseCredentialID(s string) (CredentialID, error) {
	u, err := parseULID(s)
	return CredentialID(u), err
}

func (id CredentialID) String() string               { return ulid.ULID(id).String() }
func (id *CredentialID) Scan(src any) error          { return scanULID((*ulid.ULID)(id), src) }
func (id CredentialID) Value() (driver.Value, error) { return valueULID(ulid.ULID(id)) }
func (id CredentialID) MarshalJSON() ([]byte, error) { return marshalULID(ulid.ULID(id)) }
func (id *CredentialID) UnmarshalJSON(data []byte) error {
	return unmarshalULID((*ulid.ULID)(id), data)
}
func (id CredentialID) MarshalBinary() ([]byte, error) { return marshalBinaryULID(ulid.ULID(id)) }
func (id *CredentialID) UnmarshalBinary(data []byte) error {
	return unmarshalBinaryULID((*ulid.ULID)(id), data)
}
func (id CredentialID) Bytes() []byte { return ulid.ULID(id).Bytes() }
