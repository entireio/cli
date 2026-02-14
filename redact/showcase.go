package redact

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// ShowcaseConfig controls showcase-specific redaction for public sharing.
// This applies additional privacy-focused redaction beyond standard entropy detection.
type ShowcaseConfig struct {
	RedactPaths       bool     // Normalize file paths (e.g., /Users/x/project/src → src/)
	RedactUsernames   bool     // Replace usernames/emails
	RedactProjectInfo bool     // Replace repo/project names from git remotes
	AllowedPaths      []string // Paths to preserve (e.g., "src/", "lib/")
	AllowedDomains    []string // Domains to keep (e.g., "@example.com")
	CustomBlocklist   []string // Additional terms to redact (glob patterns)
}

// DefaultShowcaseConfig returns sensible defaults for showcase redaction.
func DefaultShowcaseConfig() ShowcaseConfig {
	return ShowcaseConfig{
		RedactPaths:       true,
		RedactUsernames:   true,
		RedactProjectInfo: true,
		AllowedPaths:      []string{"src/", "lib/", "cmd/", "pkg/", "internal/"},
		AllowedDomains:    []string{},
		CustomBlocklist:   []string{},
	}
}

// Showcase applies showcase redaction after standard entropy-based redaction.
// Call String() or JSONLBytes() first, then apply Showcase() for layered protection.
func Showcase(s string, cfg ShowcaseConfig) string {
	result := s

	// Layer 1: File path redaction (must run first to prevent paths from matching other patterns)
	if cfg.RedactPaths {
		result = redactFilePaths(result, cfg.AllowedPaths)
	}

	// Layer 2: Pattern-based redaction (non-email patterns)
	result = redactPatterns(result)

	// Layer 3: Structural redaction (git remotes before emails, then blocklist)
	// Redact git remotes before emails to avoid matching git@host as an email
	if cfg.RedactProjectInfo {
		result = redactProjectInfo(result)
	}
	if cfg.RedactUsernames {
		result = redactUsernames(result, cfg.AllowedDomains)
	}

	// Layer 4: Blocklist matching (runs last to catch any remaining sensitive terms)
	result = redactBlocklist(result, cfg.CustomBlocklist)

	return result
}

// ShowcaseJSONL applies showcase redaction to JSONL session data.
// Preserves JSON structure while redacting values.
func ShowcaseJSONL(b []byte, cfg ShowcaseConfig) ([]byte, error) {
	var buf bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(b))

	// Increase buffer size for large lines (up to 10MB per line)
	maxCapacity := 10 * 1024 * 1024
	scanBuf := make([]byte, maxCapacity)
	scanner.Buffer(scanBuf, maxCapacity)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse JSON to preserve structure
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			// Fallback: treat as plain string
			redacted := Showcase(string(line), cfg)
			buf.WriteString(redacted)
			buf.WriteByte('\n')
			continue
		}

		// Recursively redact values (but not keys)
		redactJSONValues(entry, cfg)

		// Re-serialize
		redactedLine, err := json.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal redacted JSON: %w", err)
		}

		buf.Write(redactedLine)
		buf.WriteByte('\n')
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan JSONL: %w", err)
	}

	return buf.Bytes(), nil
}

// redactJSONValues recursively redacts values in JSON objects/arrays while preserving keys.
func redactJSONValues(v any, cfg ShowcaseConfig) {
	switch val := v.(type) {
	case map[string]any:
		for key, child := range val {
			switch childVal := child.(type) {
			case string:
				// Redact string values (but not keys)
				val[key] = Showcase(childVal, cfg)
			case map[string]any, []any:
				// Recurse into nested structures
				redactJSONValues(childVal, cfg)
			}
		}
	case []any:
		for i, child := range val {
			switch childVal := child.(type) {
			case string:
				val[i] = Showcase(childVal, cfg)
			case map[string]any, []any:
				redactJSONValues(childVal, cfg)
			}
		}
	}
}

// Pattern-based redaction

var (
	// Internal URLs and private IPs
	internalURLPattern = regexp.MustCompile(`(?i)\b[a-z0-9-]+\.(internal|local|corp|lan)\b(:[0-9]+)?`)
	privateIPPattern   = regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2[0-9]|3[01])\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3})(?::[0-9]+)?\b`)

	// Cloud ARNs
	// Account ID is optional for some ARN types (e.g., S3: arn:aws:s3:::bucket/key)
	awsARNPattern = regexp.MustCompile(`arn:aws:[a-z0-9-]+:[a-z0-9-]*:(?:[0-9]{12})?:[a-zA-Z0-9/._-]+`)
	// GCP resource paths - require known resource types to avoid matching regular file paths
	gcpPattern = regexp.MustCompile(`projects/[a-z0-9-]+/(?:locations|zones|regions|instances|datasets|buckets|topics|subscriptions)/[a-zA-Z0-9/._-]+`)

	// Database connection strings
	dbConnPattern = regexp.MustCompile(`(?i)(postgres|postgresql|mongodb|mysql|redis)://[^\s\)\"\']+`)

	// Email addresses (basic pattern)
	emailPattern = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b`)

	// AWS account IDs (12 digits in AWS context)
	// Capture separator to preserve it in replacement
	awsAccountPattern = regexp.MustCompile(`(?i)(aws|account|arn)([\s:]\s*)([0-9]{12})\b`)

	// JWT tokens (starts with eyJ)
	jwtPattern = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b`)

	// PEM private keys
	pemKeyPattern = regexp.MustCompile(`-----BEGIN[A-Z ]+PRIVATE KEY-----[\s\S]*?-----END[A-Z ]+PRIVATE KEY-----`)

	// File paths with user directories
	homePathPattern = regexp.MustCompile(`/(?:Users|home)/[^/\s]+`)
)

func redactPatterns(s string) string {
	result := s

	result = internalURLPattern.ReplaceAllString(result, "[INTERNAL_URL]")
	result = privateIPPattern.ReplaceAllString(result, "[PRIVATE_IP]")
	result = awsARNPattern.ReplaceAllString(result, "[AWS_ARN]")
	result = gcpPattern.ReplaceAllString(result, "[GCP_RESOURCE]")
	result = dbConnPattern.ReplaceAllString(result, "[DB_CONNECTION_STRING]")
	// NOTE: Email redaction is handled by redactUsernames() to allow AllowedDomains filtering
	result = jwtPattern.ReplaceAllString(result, "[JWT_TOKEN]")
	result = pemKeyPattern.ReplaceAllString(result, "[PEM_PRIVATE_KEY]")

	// AWS account IDs - preserve context and separator, redact number
	result = awsAccountPattern.ReplaceAllString(result, "${1}${2}[AWS_ACCOUNT_ID]")

	return result
}

func redactBlocklist(s string, blocklist []string) string {
	result := s

	for _, pattern := range blocklist {
		// Convert glob pattern to regex
		regexPattern := globToRegex(pattern)
		re := regexp.MustCompile("(?i)" + regexPattern)
		result = re.ReplaceAllString(result, "[REDACTED]")
	}

	return result
}

func redactFilePaths(s string, allowedPaths []string) string {
	result := s

	// Redact absolute paths with user directories
	result = homePathPattern.ReplaceAllStringFunc(result, func(match string) string {
		// Check if path starts with any allowed prefix
		for _, allowed := range allowedPaths {
			if strings.Contains(match, "/"+allowed) {
				// Extract relative path from allowed prefix
				parts := strings.Split(match, "/"+allowed)
				if len(parts) > 1 {
					return allowed + parts[len(parts)-1]
				}
			}
		}
		// Preserve leading slash when redacting
		return "/[PATH]"
	})

	return result
}

func redactUsernames(s string, allowedDomains []string) string {
	result := s

	// Redact emails unless domain is in allowed list
	result = emailPattern.ReplaceAllStringFunc(result, func(email string) string {
		for _, domain := range allowedDomains {
			if strings.HasSuffix(email, domain) {
				return email // Preserve allowed domains
			}
		}
		return "[EMAIL]"
	})

	return result
}

func redactProjectInfo(s string) string {
	result := s

	// Redact git remote URLs (common patterns)
	// Only match known git hosting domains to avoid over-redacting
	gitRemotePatterns := []*regexp.Regexp{
		regexp.MustCompile(`git@[^:]+:[^/]+/[^\s.]+\.git`),
		regexp.MustCompile(`https://[^/]+/[^/]+/[^\s.]+\.git`),
		// Match GitHub/GitLab/Bitbucket URLs without .git
		regexp.MustCompile(`https://(?:github\.com|gitlab\.com|bitbucket\.org)/[^/]+/[^\s/]+`),
	}

	for _, pattern := range gitRemotePatterns {
		result = pattern.ReplaceAllString(result, "[GIT_REMOTE]")
	}

	return result
}

// globToRegex converts a simple glob pattern to regex.
// Supports * (word chars and hyphens) and ? (single character).
func globToRegex(glob string) string {
	// Escape regex special chars except * and ?
	specialChars := `\.+^$()[]{}|`
	result := glob
	for _, char := range specialChars {
		result = strings.ReplaceAll(result, string(char), `\`+string(char))
	}

	// Convert glob wildcards to regex
	// Use [\w-]* to match word characters and hyphens, staying within token boundaries
	// This prevents greedy matching across dots and other punctuation
	result = strings.ReplaceAll(result, "*", `[\w-]*`)
	result = strings.ReplaceAll(result, "?", `[\w]`)

	// Match word boundaries
	return `\b` + result + `\b`
}

// normalizePath converts an absolute path to a project-relative path.
func normalizePath(absPath string, allowedPaths []string) string {
	// Try to extract relative path based on allowed prefixes
	for _, prefix := range allowedPaths {
		if idx := strings.Index(absPath, "/"+prefix); idx != -1 {
			relPath := absPath[idx+1:]
			return relPath
		}
	}

	// Fallback: just return basename
	return filepath.Base(absPath)
}
