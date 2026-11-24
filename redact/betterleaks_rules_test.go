package redact

import (
	"slices"
	"strings"
	"testing"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/report"
)

// TestBetterleaksCredentialComponents pins which rules attached as components,
// or marked SkipReport, are treated as credentials. Anything not listed in
// betterleaksIdentifierComponents lands here, so a betterleaks upgrade that
// adds or renames such a rule fails this test and is classified on purpose.
func TestBetterleaksCredentialComponents(t *testing.T) {
	t.Parallel()

	cfg, err := config.Default()
	if err != nil {
		t.Fatalf("load betterleaks default config: %v", err)
	}
	componentLike := make(map[string]bool)
	for id, rule := range cfg.Rules {
		if rule.SkipReport {
			componentLike[id] = true
		}
		for _, component := range rule.Components {
			componentLike[component.RuleID] = true
		}
	}
	for id := range betterleaksIdentifierComponents {
		if !componentLike[id] {
			t.Errorf("betterleaksIdentifierComponents lists %q, which is no longer a component or SkipReport rule", id)
		}
	}
	var credentials []string
	for id := range componentLike {
		if !betterleaksIdentifierComponents[id] {
			credentials = append(credentials, id)
		}
	}
	slices.Sort(credentials)
	want := []string{
		"alibaba-access-key-id",
		"alibaba-sts-access-key-id",
		"alibaba-sts-security-token",
		"aws-secret-access-key",
		"exoscale-api-secret",
		"ovh-consumer-key",
		"polymarket-api-secret",
		"polymarket-passphrase",
	}
	if !slices.Equal(credentials, want) {
		t.Errorf("credential component rules = %v, want %v; classify new rules in betterleaksIdentifierComponents or add them here", credentials, want)
	}
}

// TestBetterleaksFindingSecrets_UnlistedComponentRedacted pins the fail-safe
// default: a component rule missing from the identifier list is redacted, since
// betterleaks suppresses that rule's own finding once it is attached.
func TestBetterleaksFindingSecrets_UnlistedComponentRedacted(t *testing.T) {
	t.Parallel()

	f := report.Finding{
		Secret: "primary",
		ComponentSets: []report.ComponentSet{{Components: []*report.ComponentFinding{
			{RuleID: "some-future-credential", Secret: "credential"},
			{RuleID: "azure-tenant-id", Secret: "tenant"},
		}}},
	}
	got := betterleaksFindingSecrets(f)
	if want := []string{"primary", "credential"}; !slices.Equal(got, want) {
		t.Errorf("betterleaksFindingSecrets = %v, want %v", got, want)
	}
}

// awsSecretKeyFixture is a 40-char secret access key containing "/", which
// splits it into chunks the entropy layer misses. Assembled like awsKeyFixture.
const awsSecretKeyFixture = "Qx7/Lm2PvT9kWz+R4n" + "Bc/Hy6JdF8sGa3Ue5oKi1N"

// azureClientSecretFixture has the Azure AD client secret shape: "~" and "."
// split it for the entropy layer, and betterleaks required a tenant ID and a
// client ID within eight lines before reporting it.
const azureClientSecretFixture = "xR48Q~" + "kT2.mW9vLp4nZq7sYb3cJf6hDg1aEo5uX"

// TestString_AWSCredentialsPairRedactsBoth covers #2808's worse half: with the
// key ID present the composite rule fires, and betterleaks suppresses every
// other finding on the secret (generic-api-key included) in favour of the
// component it attaches, so the secret survived unless read from ComponentSets.
func TestString_AWSCredentialsPairRedactsBoth(t *testing.T) {
	t.Parallel()

	input := "[default]\naws_access_key_id = " + awsKeyFixture + "\naws_secret_access_key = " + awsSecretKeyFixture + "\n"
	want := "[default]\naws_access_key_id = REDACTED\naws_secret_access_key = REDACTED\n"
	if got := String(input); got != want {
		t.Errorf("String(%q) = %q, want %q", input, got, want)
	}
}

// TestBetterleaksDetector_ReportsLoneSecrets pins the two detector adjustments
// directly: a component-only credential and a composite primary each produce a
// finding without their partners.
func TestBetterleaksDetector_ReportsLoneSecrets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		ruleID string
		secret string
	}{
		{
			name:   "AWS secret access key without its key ID",
			input:  "aws_secret_access_key = " + awsSecretKeyFixture,
			ruleID: "aws-secret-access-key",
			secret: awsSecretKeyFixture,
		},
		{
			name:   "Azure AD client secret without tenant and client IDs",
			input:  "AZURE_CLIENT_SECRET=" + azureClientSecretFixture,
			ruleID: "azure-ad-client-secret",
			secret: azureClientSecretFixture,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := getDetector().DetectString(tt.input)
			if !slices.ContainsFunc(findings, func(f report.Finding) bool { return f.RuleID == tt.ruleID && f.Secret == tt.secret }) {
				t.Errorf("DetectString(%q) has no %s finding for the secret; got %v", tt.input, tt.ruleID, findings)
			}
			if got := String(tt.input); strings.Contains(got, tt.secret) {
				t.Errorf("String(%q) = %q, secret survived", tt.input, got)
			}
		})
	}
}
