package sem

import "io"

type Entity struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Signature   string `json:"signature"`
	StartLine   int    `json:"start_line"`
	EndLine     int    `json:"end_line"`
	BodyHash    string `json:"body_hash"`
	Fingerprint string `json:"fingerprint"`
}

type EntityChange struct {
	Type            string  `json:"type"`
	Kind            string  `json:"kind"`
	Name            string  `json:"name"`
	OldName         string  `json:"old_name,omitempty"`
	NewName         string  `json:"new_name,omitempty"`
	OldSignature    string  `json:"old_signature,omitempty"`
	NewSignature    string  `json:"new_signature,omitempty"`
	BeforeStartLine int     `json:"before_start_line,omitempty"`
	AfterStartLine  int     `json:"after_start_line,omitempty"`
	DependentsCount int     `json:"dependents_count"`
	Similarity      float64 `json:"similarity,omitempty"`
}

type FileChange struct {
	Path     string         `json:"path"`
	OldPath  string         `json:"old_path,omitempty"`
	Status   string         `json:"status"`
	Language string         `json:"language,omitempty"`
	Changes  []EntityChange `json:"changes"`
}

type Result struct {
	Checkpoint string       `json:"checkpoint,omitempty"`
	Base       string       `json:"base"`
	Head       string       `json:"head"`
	Files      []FileChange `json:"files"`
}

type Parser interface {
	Parse(path, content string) ([]Entity, string)
}

func WriteText(out io.Writer, result Result) {
	writeText(out, result)
}
