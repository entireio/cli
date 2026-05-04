package cli

import (
	"bytes"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/recap"
)

const recapTestAgentCodex = "codex"

func TestRecapFlags_RangeKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flags recapFlags
		want  recap.RangeKey
	}{
		{"default_day", recapFlags{}, recap.RangeDay},
		{"day", recapFlags{day: true}, recap.RangeDay},
		{"week", recapFlags{week: true}, recap.RangeWeek},
		{"month", recapFlags{month: true}, recap.RangeMonth},
		{"90d", recapFlags{d90: true}, recap.Range90d},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := c.flags.rangeKey(); got != c.want {
				t.Errorf("rangeKey() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRecapFlags_Mode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		view string
		want recap.ViewMode
	}{
		{"", recap.ViewBoth},
		{"both", recap.ViewBoth},
		{"you", recap.ViewYou},
		{"me", recap.ViewYou},
		{"team", recap.ViewTeam},
		{"contributors", recap.ViewTeam},
	}
	for _, c := range cases {
		t.Run(c.view, func(t *testing.T) {
			t.Parallel()
			if got := (&recapFlags{view: c.view}).mode(); got != c.want {
				t.Errorf("mode() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestRecapCmd_RegistersStaticFlags(t *testing.T) {
	t.Parallel()
	cmd := newRecapCmd()
	for _, name := range []string{"day", "week", "month", "90", "agent", "view", "color", "insecure-http-auth"} {
		if flag := cmd.Flag(name); flag == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestRecapFlags_AgentName(t *testing.T) {
	t.Parallel()
	if got := (&recapFlags{}).agentName(); got != recap.AgentAll {
		t.Errorf("default agent = %q, want all", got)
	}
	if got := (&recapFlags{agent: " Codex "}).agentName(); got != recapTestAgentCodex {
		t.Errorf("agent = %q, want %s", got, recapTestAgentCodex)
	}
}

func TestRecapFlags_ColorEnabled(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	got, err := (&recapFlags{color: "always"}).colorEnabled(&out)
	if err != nil {
		t.Fatalf("colorEnabled(always) error = %v", err)
	}
	if !got {
		t.Fatal("colorEnabled(always) = false, want true")
	}

	got, err = (&recapFlags{color: "never"}).colorEnabled(&out)
	if err != nil {
		t.Fatalf("colorEnabled(never) error = %v", err)
	}
	if got {
		t.Fatal("colorEnabled(never) = true, want false")
	}

	got, err = (&recapFlags{color: "auto"}).colorEnabled(&out)
	if err != nil {
		t.Fatalf("colorEnabled(auto) error = %v", err)
	}
	if got {
		t.Fatal("colorEnabled(auto non-tty) = true, want false")
	}

	if _, err := (&recapFlags{color: "rainbow"}).colorEnabled(&out); err == nil {
		t.Fatal("colorEnabled(invalid) error = nil, want error")
	}
}
