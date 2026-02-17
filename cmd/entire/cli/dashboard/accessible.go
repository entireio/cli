package dashboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// RunAccessible runs a text-based menu for accessibility mode.
func RunAccessible(w io.Writer) error {
	for {
		var choice string

		form := huh.NewForm(
			huh.NewGroup(
				huh.NewSelect[string]().
					Title("Entire Dashboard").
					Options(
						huh.NewOption("View sessions", "sessions"),
						huh.NewOption("View checkpoints", "checkpoints"),
						huh.NewOption("View active sessions", "active"),
						huh.NewOption("View settings", "settings"),
						huh.NewOption("Quit", "quit"),
					).
					Value(&choice),
			),
		).WithAccessible(true)

		if err := form.Run(); err != nil {
			return fmt.Errorf("menu selection failed: %w", err)
		}

		switch choice {
		case "sessions":
			if err := showAccessibleSessions(w); err != nil {
				fmt.Fprintf(w, "Error: %v\n", err)
			}
		case "checkpoints":
			if err := showAccessibleCheckpoints(w); err != nil {
				fmt.Fprintf(w, "Error: %v\n", err)
			}
		case "active":
			if err := showAccessibleActive(w); err != nil {
				fmt.Fprintf(w, "Error: %v\n", err)
			}
		case "settings":
			if err := showAccessibleSettings(w); err != nil {
				fmt.Fprintf(w, "Error: %v\n", err)
			}
		case "quit":
			return nil
		}

		fmt.Fprintln(w)
	}
}

func showAccessibleSessions(w io.Writer) error {
	sessions, err := strategy.ListSessions()
	if err != nil {
		return fmt.Errorf("loading sessions: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Fprintln(w, "\nNo sessions found.")
		return nil
	}

	fmt.Fprintf(w, "\nSessions (%d):\n", len(sessions))
	fmt.Fprintln(w, strings.Repeat("-", 60))

	for _, s := range sessions {
		shortID := s.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		desc := s.Description
		if desc == "" || desc == strategy.NoDescription {
			desc = "(no description)"
		} else {
			desc = truncate(desc, 50)
		}
		fmt.Fprintf(w, "  %-12s  %s  %s  %d checkpoints\n",
			shortID, desc, timeAgo(s.StartTime), len(s.Checkpoints))
	}

	return nil
}

func showAccessibleCheckpoints(w io.Writer) error {
	strat := getConfiguredStrategy()
	if strat == nil {
		return errors.New("no strategy configured")
	}

	points, err := strat.GetRewindPoints(50)
	if err != nil {
		return fmt.Errorf("loading checkpoints: %w", err)
	}

	if len(points) == 0 {
		fmt.Fprintln(w, "\nNo checkpoints found.")
		return nil
	}

	fmt.Fprintf(w, "\nCheckpoints (%d):\n", len(points))
	fmt.Fprintln(w, strings.Repeat("-", 60))

	for _, p := range points {
		cpType := cpTypeSession
		if p.IsTaskCheckpoint {
			cpType = cpTypeTask
		}
		if p.IsLogsOnly {
			cpType = cpTypeCommitted
		}

		cpID := p.CheckpointID.String()
		if cpID == "" {
			cpID = truncate(p.ID, 12)
		}

		prompt := truncate(p.SessionPrompt, 40)
		if prompt == "" {
			prompt = truncate(p.Message, 40)
		}

		fmt.Fprintf(w, "  %-12s  [%-9s]  %-10s  %s\n",
			cpID, cpType, timeAgo(p.Date), prompt)
	}

	return nil
}

func showAccessibleActive(w io.Writer) error {
	store, err := session.NewStateStore()
	if err != nil {
		return fmt.Errorf("creating state store: %w", err)
	}

	states, err := store.List(context.Background())
	if err != nil {
		return fmt.Errorf("listing sessions: %w", err)
	}

	// Filter active
	var active []*session.State
	for _, s := range states {
		if s.EndedAt == nil {
			active = append(active, s)
		}
	}

	if len(active) == 0 {
		fmt.Fprintln(w, "\nNo active sessions.")
		return nil
	}

	fmt.Fprintf(w, "\nActive Sessions (%d):\n", len(active))
	fmt.Fprintln(w, strings.Repeat("-", 60))

	for _, s := range active {
		shortID := s.SessionID
		if len(shortID) > 7 {
			shortID = shortID[:7]
		}

		agentLabel := string(s.AgentType)
		if agentLabel == "" {
			agentLabel = "(unknown)"
		}

		phase := string(s.Phase)
		if phase == "" {
			phase = "idle"
		}

		prompt := truncate(s.FirstPrompt, 40)

		fmt.Fprintf(w, "  %-9s  %-8s  %-14s  %s  %s\n",
			shortID, phase, agentLabel, timeAgo(s.StartedAt), prompt)
	}

	return nil
}

func showAccessibleSettings(w io.Writer) error {
	s, err := settings.Load()
	if err != nil {
		return fmt.Errorf("loading settings: %w", err)
	}

	fmt.Fprintln(w, "\nSettings:")
	fmt.Fprintln(w, strings.Repeat("-", 40))

	enabledStr := "Enabled"
	if !s.Enabled {
		enabledStr = "Disabled"
	}
	fmt.Fprintf(w, "  Status:    %s\n", enabledStr)
	fmt.Fprintf(w, "  Strategy:  %s\n", s.Strategy)

	logLevel := s.LogLevel
	if logLevel == "" {
		logLevel = "info (default)"
	}
	fmt.Fprintf(w, "  Log Level: %s\n", logLevel)

	telemetryStr := "not configured"
	if s.Telemetry != nil {
		if *s.Telemetry {
			telemetryStr = "opted in"
		} else {
			telemetryStr = "opted out"
		}
	}
	fmt.Fprintf(w, "  Telemetry: %s\n", telemetryStr)

	fmt.Fprintln(w, "\nInstalled Agents:")
	agents := agent.List()
	if len(agents) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, name := range agents {
		ag, agErr := agent.Get(name)
		if agErr != nil {
			continue
		}
		hooksStr := "no hooks"
		if hs, ok := ag.(agent.HookSupport); ok {
			if hs.AreHooksInstalled() {
				hooksStr = "hooks installed"
			} else {
				hooksStr = "hooks not installed"
			}
		}
		fmt.Fprintf(w, "  %s (%s) - %s\n", ag.Type(), name, hooksStr)
	}

	return nil
}
