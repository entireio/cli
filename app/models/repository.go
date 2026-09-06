package models

// Repository represents a git code repository managed by Entire Checkpoint Intelligence.
type Repository struct {
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Owner         string                  `json:"owner"`
	URL           string                  `json:"url"`
	LocalPath     string                  `json:"local_path"`
	DefaultBranch string                  `json:"default_branch"`
	Description   string                  `json:"description"`
	Architecture  *RepositoryArchitecture `json:"architecture,omitempty"`
}

// RepositoryArchitecture holds the detected architectural summary of a repository.
type RepositoryArchitecture struct {
	Directories    []string `json:"directories"`
	ImportantFiles []string `json:"important_files"`
	EntryPoints    []string `json:"entry_points"`
	Components     []string `json:"components"`
	APIRoutes      []string `json:"api_routes"`
	TechStack      []string `json:"tech_stack"`
	ConfigFiles    []string `json:"config_files"`
	TestStructure  []string `json:"test_structure"`
	InferredInfo   []string `json:"inferred_info"` // Clearly labeled inferred information
	UnknownInfo    []string `json:"unknown_info"`  // Clearly labeled unknown information
}
