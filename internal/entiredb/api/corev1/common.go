// Package corev1 contains the request/response types for entire-core's
// user-facing JSON API served under /api/v1.
//
// The types are framework-independent (no huma imports beyond struct
// tags). Handlers live in core/coreapi.
package corev1

import "time"

const (
	// ULIDPattern matches a canonical 26-character Crockford-base32 ULID.
	// Used for repo, project, org, mirror, binding identifiers.
	ULIDPattern = "^[0-9A-HJKMNP-TV-Z]{26}$"

	// AccountIDPattern matches the prefixed account-identifier shape used
	// for user accounts ("01..." ULID) and service accounts ("svc_..." or
	// platform principal shapes).
	AccountIDPattern = "^(?:svc_)?[0-9A-HJKMNP-TV-Z]{26}$|^[a-z][a-z0-9-]{2,63}$"

	// HandlePattern matches a URL-safe handle (3–39 chars, lowercase
	// alphanumeric + hyphens, must start with alphanumeric).
	HandlePattern = "^[a-z0-9][a-z0-9-]{0,38}$"
)

// PageInput is embedded in list endpoints that paginate via opaque
// cursors. The cursor is server-defined; clients pass back whatever
// nextPageToken came in the previous response.
type PageInput struct {
	PageSize  int32  `doc:"Maximum entries to return; server may cap further."      maximum:"500"     minimum:"0" query:"pageSize"`
	PageToken string `doc:"Opaque cursor from a previous response's nextPageToken." query:"pageToken"`
}

// PageMeta is embedded in list response bodies that return a cursor.
type PageMeta struct {
	NextPageToken string `doc:"Pass back to fetch the next page; empty when no more entries." json:"nextPageToken,omitempty"`
}

// AuditEvent is a single entry returned by GET /api/v1/audit.
type AuditEvent struct {
	ID         string         `json:"id"`
	OccurredAt time.Time      `format:"date-time"                                  json:"occurredAt"`
	EventType  string         `json:"eventType"`
	ActorID    string         `json:"actorId"`
	IPAddress  string         `doc:"Source IP recorded when the event was logged." json:"ipAddress,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}
