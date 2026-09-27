package auth

import (
	"context"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/entireclient/contexts"
)

// ActiveContext reads; ActingContext acts. The split is the contract — a caller
// that only describes the login (a printed link, a cache key) must not make the
// CLI say anything.
func TestActiveContextIsSilent_ActingContextAnnounces(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	seedProdAndStagingContexts(t, configDir, stagingFixture.name)
	var notice strings.Builder
	CaptureContextNoticeForTest(t, &notice)

	if _, ok, err := ActiveContext(); err != nil || !ok {
		t.Fatalf("ActiveContext() ok = %v, err = %v", ok, err)
	}
	if notice.Len() != 0 {
		t.Fatalf("ActiveContext announced %q; it is an accessor", notice.String())
	}

	if _, ok, err := ActingContext(); err != nil || !ok {
		t.Fatalf("ActingContext() ok = %v, err = %v", ok, err)
	}
	if got := notice.String(); got != "Using context 'me@partial'.\n" {
		t.Fatalf("notice = %q, want the acting login named", got)
	}
}

// Naming the login the user just named is noise, and it is the same line
// clusterdiscovery.selectLoginContext withholds for an explicit selection.
func TestContextNotice_SkippedForExplicitSelection(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T)
	}{
		{"--context", func(t *testing.T) { contexts.SetFlagOverrideForTest(t, stagingFixture.name) }},
		{"$ENTIRE_CONTEXT", func(t *testing.T) { t.Setenv(contexts.EnvContextVar, stagingFixture.name) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configDir := isolateCellClientEnv(t, "")
			seedProdAndStagingContexts(t, configDir, prodFixture.name)
			tc.apply(t)
			var notice strings.Builder
			CaptureContextNoticeForTest(t, &notice)

			got, err := ResolveDataAPI(context.Background())
			if err != nil {
				t.Fatalf("ResolveDataAPI: %v", err)
			}
			if got.BaseURL != "https://partial.to" {
				t.Fatalf("BaseURL = %q, want the explicitly selected login's site", got.BaseURL)
			}
			if notice.Len() != 0 {
				t.Fatalf("notice = %q, want none for an identity the user named", notice.String())
			}
		})
	}
}

// `entire agent-help` opts out: its output is read by an agent, and the login it
// resolves there authenticates a background probe rather than requested work.
func TestSilenceContextNotice(t *testing.T) {
	configDir := isolateCellClientEnv(t, "")
	seedProdAndStagingContexts(t, configDir, stagingFixture.name)
	var notice strings.Builder
	CaptureContextNoticeForTest(t, &notice)
	SilenceContextNotice()

	if _, ok, err := ActingContext(); err != nil || !ok {
		t.Fatalf("ActingContext() ok = %v, err = %v", ok, err)
	}
	if notice.Len() != 0 {
		t.Fatalf("notice = %q, want none after SilenceContextNotice", notice.String())
	}
}
