package corev1

import "time"

// Mirror is the wire representation of one mirror placement.
type Mirror struct {
	MirrorID       string     `json:"mirrorId"`
	Provider       string     `json:"provider"`
	Owner          string     `json:"owner"`
	Repo           string     `json:"repo"`
	ClusterHost    string     `doc:"Public host of the cluster serving this mirror." json:"clusterHost"`
	Jurisdiction   string     `json:"jurisdiction,omitempty"`
	InstallationID int64      `json:"installationId,omitempty"`
	IsPrivate      bool       `json:"isPrivate,omitempty"`
	IsArchived     bool       `json:"isArchived,omitempty"`
	CreatedAt      time.Time  `format:"date-time"                                    json:"createdAt"`
	SuspendedAt    *time.Time `format:"date-time"                                    json:"suspendedAt,omitempty"`
}

// ListMirrorsInput is the input for GET /api/v1/mirrors.
type ListMirrorsInput struct {
	Cluster  string `doc:"Optional: restrict to mirrors on this cluster (public host, e.g. royalcanin.partial.to)." query:"cluster"`
	Provider string `doc:"Optional: restrict to mirrors of this upstream provider (e.g. \"github\")."               query:"provider"`
	Owner    string `doc:"Optional: restrict to mirrors with this upstream owner login."                            query:"owner"`
}

// ListMirrorsOutput is the response for the mirror list.
type ListMirrorsOutput struct {
	Body struct {
		Mirrors []Mirror `json:"mirrors"`
	}
}

// GetMirrorInput is the path key for GET /api/v1/mirrors/{mirrorId}.
type GetMirrorInput struct {
	MirrorID string `path:"mirrorId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// GetMirrorOutput is the response for the single-mirror get.
type GetMirrorOutput struct {
	Body Mirror
}

// CreateMirrorInput is the body for POST /api/v1/mirrors.
type CreateMirrorInput struct {
	Body struct {
		Provider    string `enum:"github"                              json:"provider"    required:"true"`
		Owner       string `json:"owner"                               minLength:"1"      required:"true"`
		Repo        string `json:"repo"                                minLength:"1"      required:"true"`
		ClusterHost string `doc:"DNS host of the destination cluster." json:"clusterHost" minLength:"1"   required:"true"`
	}
}

// CreatedMirror describes the new placement.
type CreatedMirror struct {
	MirrorID  string `json:"mirrorId"`
	MirrorURL string `json:"mirrorUrl"`
	PublicURL string `json:"publicUrl"`
	Created   bool   `doc:"true on fresh creation; false when an existing mirror was returned." json:"created"`
}

// CreateMirrorOutput is the response.
type CreateMirrorOutput struct {
	Status int
	Body   CreatedMirror
}

// DeleteMirrorInput is the path key for DELETE /api/v1/mirrors/{mirrorId}.
type DeleteMirrorInput struct {
	MirrorID string `path:"mirrorId" pattern:"^[0-9A-HJKMNP-TV-Z]{26}$"`
}

// DeleteMirrorOutput is empty (204).
type DeleteMirrorOutput struct {
	Status int
}
