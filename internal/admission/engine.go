package admission

import (
	"github.com/X1Kun/AgentBox/internal/admission/health"
	"time"
)

type State string

const (
	Unknown        State = "unknown"
	HealthyPending State = "healthy_pending"
	Healthy        State = "healthy"
	Protect        State = "protect"
	Degraded       State = "degraded"
)

type Decision struct {
	State  State  `json:"state"`
	Reason string `json:"reason"`
	Permit bool   `json:"permit"`
}
type Engine struct {
	cfg       Config
	last      health.Sample
	goodSince time.Time
	badSince  time.Time
	state     State
	allow     bool
}

func NewEngine(c Config) *Engine { return &Engine{cfg: c, state: Unknown} }
func (e *Engine) Reset() {
	e.last = health.Sample{}
	e.goodSince = time.Time{}
	e.badSince = time.Time{}
	e.state = Unknown
}
func (e *Engine) invalidate(state State, reason string) Decision {
	e.goodSince = time.Time{}
	e.badSince = time.Time{}
	e.state = state
	return Decision{State: state, Reason: reason}
}
func (e *Engine) Evaluate(now time.Time, s health.Sample, readErr error) (result Decision) {
	defer func() { e.allow = result.Permit }()
	if readErr != nil {
		return e.invalidate(Degraded, "source_unavailable")
	}
	if reason := s.Reason(e.cfg.Health, now, seconds(e.cfg.MaxAgeSeconds), e.cfg.MinimumSamples); reason != "ok" {
		return e.invalidate(Unknown, reason)
	}
	if !e.last.SourceStarted.IsZero() && !s.SourceStarted.Equal(e.last.SourceStarted) {
		if !s.SourceStarted.After(e.last.SourceStarted) {
			return e.invalidate(Unknown, "source_regressed")
		}
		e.Reset()
	}
	if !e.last.End.IsZero() {
		if s.Sequence < e.last.Sequence || s.End.Before(e.last.End) {
			return e.invalidate(Unknown, "sample_regressed")
		}
		if s.Sequence == e.last.Sequence {
			if s != e.last {
				return e.invalidate(Unknown, "sample_changed")
			}
			// A fresh repeated snapshot can sustain a decided Healthy state,
			// but never complete a pending stability interval.
			return Decision{State: e.state, Reason: "unchanged_sample", Permit: e.allow && e.state == Healthy}
		}
		if !s.End.After(e.last.End) {
			return e.invalidate(Unknown, "sample_regressed")
		}
		if s.End.Sub(e.last.End) > seconds(e.cfg.MaxAgeSeconds) {
			e.goodSince = time.Time{}
			e.badSince = time.Time{}
			e.state = Unknown
		}
	}
	e.last = s
	ratio := s.Ratio()
	if ratio >= e.cfg.HealthyRatio {
		e.badSince = time.Time{}
		if e.goodSince.IsZero() {
			// Old but fresh Prometheus windows cannot reconstruct a health
			// dwell that supposedly happened before this process observed them.
			e.goodSince = now
		}
		if s.End.Sub(e.goodSince) >= seconds(e.cfg.StableSeconds) {
			e.state = Healthy
			return Decision{Healthy, "stable_health", true}
		}
		e.state = HealthyPending
		return Decision{HealthyPending, "health_stabilizing", false}
	}
	if ratio < e.cfg.ProtectRatio {
		e.goodSince = time.Time{}
		if e.badSince.IsZero() {
			e.badSince = now
		}
		if s.End.Sub(e.badSince) >= seconds(e.cfg.ProtectSeconds) {
			e.state = Protect
			return Decision{Protect, "quality_low", false}
		}
		// Stop new permissions during the deterioration dwell as well.
		return Decision{e.state, "quality_deteriorating", false}
	}
	// Hysteresis retains an established Healthy state in the dead band, but
	// a recovering/protecting state cannot become healthy without good evidence.
	e.badSince = time.Time{}
	e.goodSince = time.Time{}
	return Decision{e.state, "hysteresis", e.state == Healthy}
}
