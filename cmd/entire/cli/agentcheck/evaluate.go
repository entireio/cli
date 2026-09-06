package agentcheck

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type FindingCategory string

const (
	CategoryRequirementMiss   FindingCategory = "requirement_miss"
	CategoryBoundaryViolation FindingCategory = "boundary_violation"
	CategoryScopeCreep        FindingCategory = "scope_creep"
	CategoryIntentDeviation   FindingCategory = "intent_deviation"
)

type Severity string

type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
)

type Verdict string

// EvaluationResult is Teammate B's small result contract for deterministic
// AgentCheck evaluation. It is intentionally independent from Entire internals.
type EvaluationResult struct {
	Verdict  Verdict
	Summary  string
	Findings []Finding
}

type Finding struct {
	Category       FindingCategory
	Severity       Severity
	Title          string
	Description    string
	Evidence       []FindingEvidence
	Confidence     Confidence
	Recommendation string
}

type FindingEvidence struct {
	Kind      string
	Reference string
	Detail    string
}

type intentStatement struct {
	Text   string
	Source string
}

type requirement struct {
	Statement intentStatement
	Tokens    []string
}

type prohibition struct {
	Statement intentStatement
	Scope     string
	Tokens    []string
}

type onlyScope struct {
	Statement intentStatement
	Tokens    []string
}

type changedPath struct {
	Path   string
	Status string
	Source string
}

// EvaluateIntentBoundary evaluates explicit intent and boundary evidence in an
// AgentCheck context. It does not call Git, read storage, run verification, or
// infer intent from commit messages.
func EvaluateIntentBoundary(ctx Context) EvaluationResult {
	statements := collectIntentStatements(ctx)
	changes := collectChangedPaths(ctx)
	diffText := strings.TrimSpace(ctx.Git.Diff)

	var findings []Finding
	prohibitions := extractProhibitions(statements)
	requirements := extractRequirements(statements)
	onlyScopes := extractOnlyScopes(statements)

	for _, rule := range prohibitions {
		for _, change := range changes {
			if prohibitionMatchesPath(rule, change.Path) {
				findings = append(findings, boundaryViolation(rule, change))
			}
		}
	}

	if hasImplementationEvidence(changes, diffText) {
		for _, req := range requirements {
			if !requirementSatisfied(req, changes, diffText) {
				findings = append(findings, requirementMiss(req, changes, ctx))
			}
		}
	}

	for _, scope := range onlyScopes {
		for _, change := range changes {
			if !pathMatchesAnyToken(change.Path, scope.Tokens) {
				findings = append(findings, intentDeviation(scope, change))
			}
		}
	}

	for _, change := range materialScopeCreep(requirements, changes) {
		findings = append(findings, scopeCreep(requirements, change))
	}

	sortFindings(findings)
	return EvaluationResult{
		Verdict:  DetermineVerdict(findings),
		Summary:  evaluationSummary(findings),
		Findings: findings,
	}
}

func DetermineVerdict(findings []Finding) Verdict {
	if len(findings) == 0 {
		return Verdict(VerdictTrusted)
	}
	for _, finding := range findings {
		if finding.Category == CategoryBoundaryViolation && strings.EqualFold(string(finding.Severity), SeverityCritical) {
			return Verdict(VerdictFail)
		}
	}
	return Verdict(VerdictReviewRequired)
}

func collectIntentStatements(ctx Context) []intentStatement {
	var out []intentStatement
	if text := strings.TrimSpace(ctx.DeveloperPrompt); text != "" {
		out = append(out, splitStatements(text, "DeveloperPrompt")...)
	}
	for _, prompt := range ctx.ScopedPrompts {
		if text := strings.TrimSpace(prompt.Text); text != "" {
			source := fmt.Sprintf("ScopedPrompts[%d]", prompt.PromptIndex)
			out = append(out, splitStatements(text, source)...)
		}
	}
	return out
}

func splitStatements(text, source string) []intentStatement {
	var out []intentStatement
	for _, part := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ';' || r == '.'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, intentStatement{Text: part, Source: source})
		}
	}
	return out
}

func collectChangedPaths(ctx Context) []changedPath {
	seen := map[string]bool{}
	var out []changedPath
	add := func(path, status, source string) {
		path = strings.TrimSpace(path)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, changedPath{Path: path, Status: strings.TrimSpace(status), Source: source})
	}
	for _, path := range ctx.FilesTouched {
		add(path, "", "FilesTouched")
	}
	for _, change := range ctx.ChangedFiles {
		add(change.Path, change.Status, "ChangedFiles")
	}
	for _, change := range ctx.Git.ChangedFiles {
		add(change.Path, change.Status, "Git.ChangedFiles")
	}
	return out
}

func extractProhibitions(statements []intentStatement) []prohibition {
	markers := []string{"do not ", "don't ", "must not ", "never "}
	var out []prohibition
	for _, statement := range statements {
		lower := strings.ToLower(statement.Text)
		for _, marker := range markers {
			if idx := strings.Index(lower, marker); idx >= 0 {
				scope := strings.TrimSpace(statement.Text[idx+len(marker):])
				out = append(out, prohibition{
					Statement: statement,
					Scope:     scope,
					Tokens:    significantTokens(scope),
				})
				break
			}
		}
	}
	return out
}

func extractRequirements(statements []intentStatement) []requirement {
	verbs := []string{"implement ", "add ", "create ", "support ", "ensure ", "update ", "fix ", "preserve "}
	var out []requirement
	for _, statement := range statements {
		lower := strings.ToLower(strings.TrimSpace(statement.Text))
		for _, verb := range verbs {
			if strings.HasPrefix(lower, verb) {
				tokens := significantTokens(strings.TrimSpace(statement.Text[len(verb):]))
				if len(tokens) > 0 {
					out = append(out, requirement{Statement: statement, Tokens: tokens})
				}
				break
			}
		}
	}
	return out
}

func extractOnlyScopes(statements []intentStatement) []onlyScope {
	var out []onlyScope
	for _, statement := range statements {
		lower := strings.ToLower(strings.TrimSpace(statement.Text))
		scopeText := ""
		switch {
		case strings.HasPrefix(lower, "only "):
			scopeText = strings.TrimSpace(statement.Text[len("only "):])
		case strings.Contains(lower, " only "):
			scopeText = strings.TrimSpace(statement.Text[strings.Index(lower, " only ")+len(" only "):])
		case strings.HasPrefix(lower, "limit changes to "):
			scopeText = strings.TrimSpace(statement.Text[len("limit changes to "):])
		}
		if scopeText == "" {
			continue
		}
		tokens := significantTokens(scopeText)
		if len(tokens) > 0 {
			out = append(out, onlyScope{Statement: statement, Tokens: tokens})
		}
	}
	return out
}

func prohibitionMatchesPath(rule prohibition, path string) bool {
	lowerScope := strings.ToLower(rule.Scope)
	lowerPath := strings.ToLower(path)
	switch {
	case containsAny(lowerScope, "database", "schema", "migration", "migrations"):
		return strings.Contains(lowerPath, "migration") || strings.HasSuffix(lowerPath, ".sql") || strings.Contains(lowerPath, "schema") || strings.Contains(lowerPath, "database")
	case containsAny(lowerScope, "test", "tests"):
		return isTestPath(lowerPath)
	case containsAny(lowerScope, "documentation", "docs", "readme"):
		return isDocPath(lowerPath)
	case containsAny(lowerScope, "api", "endpoint", "endpoints"):
		return pathHasSegment(lowerPath, "api") || strings.Contains(lowerPath, "openapi")
	case containsAny(lowerScope, "checkpoint", "session", "strategy", "graph", "verification"):
		for _, token := range significantTokens(lowerScope) {
			if strings.Contains(lowerPath, token) {
				return true
			}
		}
	}
	return pathMatchesAnyToken(path, rule.Tokens)
}

func requirementSatisfied(req requirement, changes []changedPath, diffText string) bool {
	for _, change := range changes {
		if tokenMatchCount(pathTokens(change.Path), req.Tokens) >= requiredMatchCount(req.Tokens) {
			return true
		}
	}
	diffTokens := significantTokens(diffText)
	return tokenMatchCount(diffTokens, req.Tokens) >= requiredMatchCount(req.Tokens)
}

func materialScopeCreep(requirements []requirement, changes []changedPath) []changedPath {
	if len(requirements) == 0 || len(changes) < 2 {
		return nil
	}
	var inScope, outOfScope []changedPath
	for _, change := range changes {
		if pathMatchesAnyRequirement(change.Path, requirements) {
			inScope = append(inScope, change)
		} else {
			outOfScope = append(outOfScope, change)
		}
	}
	if len(inScope) == 0 {
		return nil
	}
	return outOfScope
}

func hasImplementationEvidence(changes []changedPath, diffText string) bool {
	return len(changes) > 0 || strings.TrimSpace(diffText) != ""
}

func pathMatchesAnyRequirement(path string, requirements []requirement) bool {
	for _, req := range requirements {
		if tokenMatchCount(pathTokens(path), req.Tokens) >= requiredMatchCount(req.Tokens) {
			return true
		}
	}
	return false
}

func pathMatchesAnyToken(path string, tokens []string) bool {
	return tokenMatchCount(pathTokens(path), tokens) > 0
}

func tokenMatchCount(haystack, needles []string) int {
	have := make(map[string]bool, len(haystack))
	for _, token := range haystack {
		have[token] = true
	}
	count := 0
	for _, token := range needles {
		if have[token] {
			count++
		}
	}
	return count
}

func requiredMatchCount(tokens []string) int {
	if len(tokens) < 2 {
		return len(tokens)
	}
	return 2
}

func boundaryViolation(rule prohibition, change changedPath) Finding {
	return Finding{
		Category:    CategoryBoundaryViolation,
		Severity:    Severity(SeverityCritical),
		Title:       "Explicit boundary violation",
		Description: "A changed file matches a developer prohibition.",
		Evidence: []FindingEvidence{
			intentEvidence(rule.Statement),
			changeEvidence(change),
		},
		Confidence:     ConfidenceHigh,
		Recommendation: "Review the prohibited change and remove it unless the boundary is intentionally revised.",
	}
}

func requirementMiss(req requirement, changes []changedPath, ctx Context) Finding {
	evidence := []FindingEvidence{intentEvidence(req.Statement)}
	for _, change := range firstChanges(changes, 5) {
		evidence = append(evidence, changeEvidence(change))
	}
	if reason := strings.TrimSpace(ctx.Git.DiffUnavailableReason); reason != "" {
		evidence = append(evidence, FindingEvidence{Kind: "git_diff", Reference: "Git.DiffUnavailableReason", Detail: reason})
	}
	return Finding{
		Category:       CategoryRequirementMiss,
		Severity:       Severity(SeverityHigh),
		Title:          "Requirement not evidenced",
		Description:    "The available changed-file and diff evidence does not show this explicit requirement was implemented.",
		Evidence:       evidence,
		Confidence:     ConfidenceMedium,
		Recommendation: "Add evidence that satisfies the requirement or revise the checkpoint before trusting it.",
	}
}

func scopeCreep(requirements []requirement, change changedPath) Finding {
	evidence := []FindingEvidence{intentEvidence(requirements[0].Statement), changeEvidence(change)}
	return Finding{
		Category:       CategoryScopeCreep,
		Severity:       Severity(SeverityMedium),
		Title:          "Possible scope creep",
		Description:    "A changed file does not match the explicit task scope while other changed files do.",
		Evidence:       evidence,
		Confidence:     ConfidenceMedium,
		Recommendation: "Confirm the additional file is necessary for the requested work.",
	}
}

func intentDeviation(scope onlyScope, change changedPath) Finding {
	return Finding{
		Category:    CategoryIntentDeviation,
		Severity:    Severity(SeverityHigh),
		Title:       "Intent deviation",
		Description: "A changed file falls outside an explicit only/limit scope in the developer prompt.",
		Evidence: []FindingEvidence{
			intentEvidence(scope.Statement),
			changeEvidence(change),
		},
		Confidence:     ConfidenceHigh,
		Recommendation: "Limit the change to the requested scope or get explicit approval for the extra work.",
	}
}

func intentEvidence(statement intentStatement) FindingEvidence {
	return FindingEvidence{Kind: "intent", Reference: statement.Source, Detail: statement.Text}
}

func changeEvidence(change changedPath) FindingEvidence {
	detail := change.Path
	if change.Status != "" {
		detail += " (" + change.Status + ")"
	}
	return FindingEvidence{Kind: "changed_file", Reference: change.Source, Detail: detail}
}

func firstChanges(changes []changedPath, limit int) []changedPath {
	if len(changes) <= limit {
		return changes
	}
	return changes[:limit]
}

func evaluationSummary(findings []Finding) string {
	switch DetermineVerdict(findings) {
	case Verdict(VerdictTrusted):
		return "No deterministic intent or boundary findings were found."
	case Verdict(VerdictFail):
		return "A critical explicit boundary violation was found."
	default:
		return "One or more deterministic intent or boundary findings need review."
	}
}

func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left := severityRank(string(findings[i].Severity))
		right := severityRank(string(findings[j].Severity))
		if left != right {
			return left < right
		}
		if findings[i].Category != findings[j].Category {
			return findings[i].Category < findings[j].Category
		}
		return findings[i].Title < findings[j].Title
	})
}

func significantTokens(text string) []string {
	seen := map[string]bool{}
	var tokens []string
	for _, token := range splitTokens(text) {
		if isStopToken(token) || seen[token] {
			continue
		}
		seen[token] = true
		tokens = append(tokens, token)
	}
	return tokens
}

func pathTokens(path string) []string {
	tokens := significantTokens(path)
	lower := strings.ToLower(path)
	if strings.Contains(lower, "auth") || strings.Contains(lower, "oauth") || strings.Contains(lower, "login") {
		tokens = appendUnique(tokens, "auth", "authentication", "oauth", "login")
	}
	if strings.Contains(lower, "docs/") || strings.HasSuffix(lower, ".md") {
		tokens = appendUnique(tokens, "docs", "documentation")
	}
	if strings.Contains(lower, "test") || strings.Contains(lower, "spec") {
		tokens = appendUnique(tokens, "test", "tests")
	}
	if strings.Contains(lower, "migration") || strings.HasSuffix(lower, ".sql") {
		tokens = appendUnique(tokens, "database", "schema", "migration")
	}
	return tokens
}

func splitTokens(text string) []string {
	lower := strings.ToLower(text)
	return strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func appendUnique(tokens []string, extra ...string) []string {
	seen := map[string]bool{}
	for _, token := range tokens {
		seen[token] = true
	}
	for _, token := range extra {
		if !seen[token] {
			tokens = append(tokens, token)
			seen[token] = true
		}
	}
	return tokens
}

func isStopToken(token string) bool {
	switch token {
	case "", "a", "an", "and", "are", "be", "by", "change", "changes", "code", "existing", "file", "files", "for", "in", "implementation", "into", "is", "it", "keep", "minimal", "modify", "of", "only", "preserve", "requested", "the", "this", "to", "update", "updates", "with":
		return true
	default:
		return len(token) < 3
	}
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func isTestPath(path string) bool {
	return strings.Contains(path, "_test.") || pathHasSegment(path, "test") || pathHasSegment(path, "tests")
}

func isDocPath(path string) bool {
	return strings.HasSuffix(path, ".md") || pathHasSegment(path, "docs") || strings.Contains(path, "readme")
}

func pathHasSegment(path, segment string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == segment {
			return true
		}
	}
	return false
}
