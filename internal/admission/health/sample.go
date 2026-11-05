// Package health defines trusted, deadline-finalized session quality windows.
// It has no Kubernetes dependency. A successful user program is not, by itself,
// evidence of correct output: the trusted arrival driver verifies that output.
package health

import (
	"math"
	"regexp"
	"time"
)

const Metric = "agentbox_session_health"

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

func ValidName(s string) bool { return identifier.MatchString(s) }

type Contract struct {
	Domain          string `json:"domain"`
	Revision        string `json:"revision"`
	Profile         string `json:"profile"`
	WindowSeconds   int    `json:"window_seconds"`
	DeadlineSeconds int    `json:"deadline_seconds"`
}

func (c Contract) Valid() bool {
	return ValidName(c.Domain) && ValidName(c.Revision) && ValidName(c.Profile) && c.WindowSeconds >= 1 && c.WindowSeconds <= 120 && c.DeadlineSeconds >= 1 && c.DeadlineSeconds <= 60
}

type Sample struct {
	Contract
	End           time.Time `json:"end"`
	SourceStarted time.Time `json:"source_started"`
	Sequence      int64     `json:"sequence"`
	Planned       int64     `json:"planned"`
	Timely        int64     `json:"timely_correct"`
	Errors        int64     `json:"errors"`
	Timeouts      int64     `json:"timeouts"`
	Rejected      int64     `json:"rejected"`
	Valid         bool      `json:"valid"`
}

// Reason deliberately uses finite public categories, never backend messages.
func (s Sample) Reason(c Contract, now time.Time, maxAge time.Duration, minimum int64) string {
	if s.Contract != c {
		return "contract_mismatch"
	}
	if !s.Valid {
		return "invalid_window"
	}
	if s.End.IsZero() || s.SourceStarted.IsZero() || s.SourceStarted.After(s.End) || s.End.After(now) {
		return "invalid_time"
	}
	if now.Sub(s.End) > maxAge {
		return "stale"
	}
	if s.Sequence < 1 || s.Planned < 0 || s.Timely < 0 || s.Errors < 0 || s.Timeouts < 0 || s.Rejected < 0 || s.Timely+s.Errors+s.Timeouts+s.Rejected != s.Planned {
		return "invalid_counts"
	}
	if s.Planned < minimum {
		return "insufficient_samples"
	}
	return "ok"
}

func (s Sample) Ratio() float64 {
	if s.Planned == 0 {
		return 0
	}
	return float64(s.Timely) / float64(s.Planned)
}

func Fields(s Sample) map[string]float64 {
	valid := 0.
	if s.Valid {
		valid = 1
	}
	return map[string]float64{
		"window_end_seconds":     float64(s.End.UnixNano()) / 1e9,
		"source_started_seconds": float64(s.SourceStarted.UnixNano()) / 1e9,
		"sequence":               float64(s.Sequence), "window_seconds": float64(s.WindowSeconds), "deadline_seconds": float64(s.DeadlineSeconds),
		"planned": float64(s.Planned), "timely_correct": float64(s.Timely), "errors": float64(s.Errors), "timeouts": float64(s.Timeouts), "rejected": float64(s.Rejected), "valid": valid,
	}
}

func finiteInteger(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < 1<<53 && math.Trunc(v) == v
}
