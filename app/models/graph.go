package models

// GraphFinding represents structural evidence retrieved from Entire Graph analysis.
type GraphFinding struct {
	ID                 string   `json:"id"`
	QueryChange        string   `json:"query_change"`
	AffectedFiles      []string `json:"affected_files"`
	AffectedFunctions  []string `json:"affected_functions"`
	Callers            []string `json:"callers"`
	RoutesTypes        []string `json:"routes_types"`
	RiskInformation    string   `json:"risk_information"`
	VerificationStatus string   `json:"verification_status"`
	SourceEvidence     string   `json:"source_evidence"`
}
