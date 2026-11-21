package api

import "time"

// ProjectTrailAttachmentIntent is POST /{host}/{project}/trails/{trail}/attachments:
// it registers an upload and returns where to PUT the bytes.
type ProjectTrailAttachmentIntent struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
}

// ProjectTrailAttachmentUpload is the registered upload: the attachment's ID
// and a short-lived presigned PUT URL for its bytes.
type ProjectTrailAttachmentUpload struct {
	ID     string `json:"id"`
	Upload struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
		MaxBytes  int64     `json:"max_bytes"`
	} `json:"upload"`
}

// ProjectTrailAttachment is one attachment as list and complete return it.
type ProjectTrailAttachment struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	TrailID     string    `json:"trail_id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	SizeBytes   int64     `json:"size_bytes"`
	IsReady     bool      `json:"is_ready"`
	Kind        string    `json:"kind"`
	CreatedBy   string    `json:"created_by_account_id"`
	CreatedAt   time.Time `json:"created_at"`
	Width       *int      `json:"width"`
	Height      *int      `json:"height"`
}

// ProjectTrailAttachmentList is GET .../attachments.
type ProjectTrailAttachmentList struct {
	Attachments []ProjectTrailAttachment `json:"attachments"`
}

// ProjectTrailAttachmentComplete is POST .../attachments/{id}/complete.
type ProjectTrailAttachmentComplete struct {
	Attachment ProjectTrailAttachment `json:"attachment"`
}
