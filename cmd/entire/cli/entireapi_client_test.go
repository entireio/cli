package cli

import (
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestForgeToMirrorProvider(t *testing.T) {
	t.Parallel()

	for _, forge := range []string{"gh", "github", "GitHub", " gh "} {
		if p, ok := forgeToMirrorProvider(forge); !ok || p != mirrorCloneProviderGitHub {
			t.Errorf("forgeToMirrorProvider(%q) = (%q, %v), want (%q, true)", forge, p, ok, mirrorCloneProviderGitHub)
		}
	}
	if _, ok := forgeToMirrorProvider("gitlab"); ok {
		t.Error("forgeToMirrorProvider(gitlab) = ok, want not ok")
	}
}

func TestFirstActiveRepoID(t *testing.T) {
	t.Parallel()

	archived := coreapi.Mirror{MirrorId: "archived", IsArchived: coreapi.NewOptBool(true)}
	failed := coreapi.Mirror{MirrorId: "failed", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusFailed)}
	suspended := coreapi.Mirror{MirrorId: "suspended", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusSuspended)}
	ready := coreapi.Mirror{MirrorId: "ready-ulid", Status: coreapi.NewOptMirrorStatus(coreapi.MirrorStatusReady)}
	unset := coreapi.Mirror{MirrorId: "unset-status-ulid"} // no status → treated as active

	tests := []struct {
		name    string
		mirrors []coreapi.Mirror
		want    string
	}{
		{"none", nil, ""},
		{"single ready", []coreapi.Mirror{ready}, "ready-ulid"},
		{"unset status counts as active", []coreapi.Mirror{unset}, "unset-status-ulid"},
		{"skips archived and unhealthy", []coreapi.Mirror{archived, failed, suspended, ready}, "ready-ulid"},
		{"all inactive", []coreapi.Mirror{archived, failed, suspended}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := firstActiveRepoID(tt.mirrors); got != tt.want {
				t.Fatalf("firstActiveRepoID = %q, want %q", got, tt.want)
			}
		})
	}
}
