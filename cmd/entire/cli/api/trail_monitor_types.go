package api

import (
	"encoding/json"
	"fmt"
)

// TrailMonitor is one monitor result served on the trail detail resource
// (TrailDetailResponse.monitors). Monitors are the trail's scores: the
// built-in confidence/risk/drift/security runners and any custom runner whose
// config declares an output.trail_monitor. Exactly one of the value fields is
// set, matching ValueType; all are null until the monitor has been evaluated.
//
// The wire object also carries history and last_failed_attempt, which the CLI
// does not expose.
type TrailMonitor struct {
	Key            string   `json:"key"`
	Label          string   `json:"label"`
	ValueType      string   `json:"value_type"`
	PercentValue   *float64 `json:"percent_value"`
	SizeValue      *string  `json:"size_value"`
	BooleanValue   *bool    `json:"boolean_value"`
	Polarity       *string  `json:"polarity"`
	Rationale      *string  `json:"rationale"`
	TrendDelta     *float64 `json:"trend_delta,omitempty"`
	Evaluating     bool     `json:"evaluating,omitempty"`
	State          string   `json:"state,omitempty"`
	Outcome        *string  `json:"outcome,omitempty"`
	EvaluatedAtSHA *string  `json:"evaluated_at_sha,omitempty"`
	StaleReason    *string  `json:"stale_reason,omitempty"`
	RunID          string   `json:"run_id"`
	HeadSHA        *string  `json:"head_sha"`
	UpdatedAt      string   `json:"updated_at"`
}

// TrailRunner is one runner's state for the trail's head, served on the trail
// detail resource (TrailDetailResponse.runners).
type TrailRunner struct {
	RunnerID       string  `json:"runner_id"`
	State          string  `json:"state"`
	Outcome        *string `json:"outcome"`
	RunID          *string `json:"run_id"`
	EvaluatedAtSHA *string `json:"evaluated_at_sha"`
	StartedAt      *string `json:"started_at"`
	StaleReason    *string `json:"stale_reason"`
}

// Automation states shared by monitors, runners, and gates.
const (
	TrailAutomationRunning            = "running"
	TrailAutomationNotRun             = "not_run"
	TrailAutomationStale              = "stale"
	TrailAutomationConfigurationError = "configuration_error"
	TrailAutomationDisabled           = "disabled"
	TrailAutomationNotApplicable      = "not_applicable"
	TrailAutomationEvaluated          = "evaluated"
	TrailAutomationErrored            = "errored"
)

// Monitor quality values, matching the web app's monitor badges.
const (
	TrailMonitorQualitySuccess = "success"
	TrailMonitorQualityWarning = "warning"
	TrailMonitorQualityDanger  = "danger"
	TrailMonitorQualityNeutral = "neutral"
)

// Quality buckets the monitor's value the same way the web app does
// (entire.io frontend .../trails/$number/{-$slug}/-lib/monitorQuality.ts), so
// the CLI and the trail page agree on what is green. Keep the two in sync
// until the server serves quality itself.
func (m TrailMonitor) Quality() string {
	polarity := ""
	if m.Polarity != nil {
		polarity = *m.Polarity
	}
	lowerIsBetter := polarity == "lower_is_better"

	if m.ValueType == "boolean" && m.BooleanValue != nil {
		good := *m.BooleanValue
		if lowerIsBetter {
			good = !good
		}
		if good {
			return TrailMonitorQualitySuccess
		}
		return TrailMonitorQualityDanger
	}

	if polarity == "" || polarity == "neutral" {
		return TrailMonitorQualityNeutral
	}

	switch m.ValueType {
	case "percent":
		if m.PercentValue == nil {
			return TrailMonitorQualityNeutral
		}
		adjusted := *m.PercentValue
		if lowerIsBetter {
			adjusted = 100 - adjusted
		}
		switch {
		case adjusted >= 67:
			return TrailMonitorQualitySuccess
		case adjusted >= 34:
			return TrailMonitorQualityWarning
		default:
			return TrailMonitorQualityDanger
		}
	case "size":
		if m.SizeValue == nil {
			return TrailMonitorQualityNeutral
		}
		rank, ok := map[string]int{"small": 0, "medium": 1, "large": 2}[*m.SizeValue]
		if !ok {
			return TrailMonitorQualityNeutral
		}
		if lowerIsBetter {
			rank = 2 - rank
		}
		switch rank {
		case 2:
			return TrailMonitorQualitySuccess
		case 1:
			return TrailMonitorQualityWarning
		default:
			return TrailMonitorQualityDanger
		}
	}
	return TrailMonitorQualityNeutral
}

// DecodeMonitors decodes the detail resource's monitors. It returns nil, nil
// when the field is absent or null.
func (r *TrailResource) DecodeMonitors() ([]TrailMonitor, error) {
	if len(r.Monitors) == 0 || isJSONNull(r.Monitors) {
		return nil, nil
	}
	var out []TrailMonitor
	if err := json.Unmarshal(r.Monitors, &out); err != nil {
		return nil, fmt.Errorf("decode trail monitors: %w", err)
	}
	return out, nil
}

// DecodeRunners decodes the detail resource's runners. It returns nil, nil
// when the field is absent or null.
func (r *TrailResource) DecodeRunners() ([]TrailRunner, error) {
	if len(r.Runners) == 0 || isJSONNull(r.Runners) {
		return nil, nil
	}
	var out []TrailRunner
	if err := json.Unmarshal(r.Runners, &out); err != nil {
		return nil, fmt.Errorf("decode trail runners: %w", err)
	}
	return out, nil
}

func isJSONNull(raw json.RawMessage) bool { return string(raw) == "null" }
