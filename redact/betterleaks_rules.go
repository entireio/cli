package redact

import (
	"context"
	"fmt"

	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/detect"
	"github.com/betterleaks/betterleaks/report"
)

// betterleaksIdentifierComponents lists the betterleaks rules used as
// components that match identifiers, hosts, URLs, and names (client IDs, tenant
// IDs, *.jfrog.io, usernames) rather than credentials. They stay out of
// redaction and, where SkipReport, stay skipped.
//
// Every other rule attached as a component, or marked SkipReport, is treated
// as a credential: its matches are redacted from Finding.ComponentSets and it
// reports standalone. The default is fail-safe because two things hide a
// component's secret, and both must be undone (#2808):
//
//   - SkipReport: the rule never reports standalone, so a secret without its
//     partner nearby produced no finding.
//   - Component suppression: when the composite fires, betterleaks drops every
//     other finding on the component's span (generic-api-key included) and
//     reports the secret only inside Finding.ComponentSets. The AWS secret
//     access key survived beside its key ID whenever the entropy layer missed
//     it.
//
// TestBetterleaksCredentialComponents pins the resulting credential set, so a
// betterleaks upgrade that adds a component rule is reviewed, not inherited.
var betterleaksIdentifierComponents = map[string]bool{
	"aikido-client-id":                     true,
	"algolia-application-id":               true,
	"artifactory-jfrog-url":                true,
	"auth0-client-id.1":                    true,
	"auth0-domain.1":                       true,
	"azure-client-id":                      true,
	"azure-storage-account-name":           true,
	"azure-tenant-id":                      true,
	"browserstack-username.1":              true,
	"canva-client-id":                      true,
	"clickhouse-cloud-key-id":              true,
	"cloudinary-api-key":                   true,
	"cloudinary-cloud-name":                true,
	"ebay-client-id":                       true,
	"generic-username":                     true,
	"langfuse-public-key.1":                true,
	"lark-app-id":                          true,
	"miro-client-id":                       true,
	"mongodb-atlas-service-account-id":     true,
	"mux-access-token-id.1":                true,
	"ovh-application-key":                  true,
	"paypal-client-id.1":                   true,
	"planetscale-id":                       true,
	"plivo-auth-id":                        true,
	"polymarket-address":                   true,
	"ramp-client-id":                       true,
	"razorpay-key-id.1":                    true,
	"salesforce-instance-url.1":            true,
	"snowflake-account-host.1":             true,
	"supabase-project-url":                 true,
	"tableau-personal-access-token-name.1": true,
	"tableau-server-host.1":                true,
	"upstash-redis-rest-url.1":             true,
	"wiz-client-id.1":                      true,
	"zoho-client-id.1":                     true,
}

// newBetterleaksDetector builds the betterleaks detector from the default
// ruleset, adjusted for redaction rather than leak reporting.
//
// betterleaks composite rules require their components nearby (an Azure client
// secret needs a tenant ID and client ID within eight lines), which keeps its
// validation and reports precise. Redaction must not depend on what else the
// transcript happens to contain, so every component becomes optional: the
// primary secret is reported alone, and components are still attached when
// found. Filters never read components, only validation expressions do, and
// this package does not validate.
func newBetterleaksDetector() (*detect.Detector, error) {
	cfg, err := config.Default()
	if err != nil {
		return nil, fmt.Errorf("load betterleaks default config: %w", err)
	}
	for id, rule := range cfg.Rules {
		for _, component := range rule.Components {
			component.Optional = true
		}
		if rule.SkipReport && !betterleaksIdentifierComponents[id] {
			rule.SkipReport = false
			cfg.Rules[id] = rule
		}
	}
	return detect.NewDetectorContext(context.Background(), cfg, detect.ValidationOptions{}), nil
}

// betterleaksFindingSecrets returns the values to redact for one finding: its
// own secret plus every attached component that is not an identifier.
func betterleaksFindingSecrets(f report.Finding) []string {
	secrets := []string{f.Secret}
	for _, set := range f.ComponentSets {
		for _, component := range set.Components {
			if !betterleaksIdentifierComponents[component.RuleID] && component.Secret != "" {
				secrets = append(secrets, component.Secret)
			}
		}
	}
	return secrets
}
