package qos

import (
	"fmt"
	"math"
	"time"
)

type CPUConfig struct {
	BaseCPU                              float64 // 0 keeps PSI-only AIMD; nonzero enables guarded borrowing.
	MinCPU, MaxCPU                       float64
	LowPressure, HighPressure            float64
	DecreaseFactor, IncreaseCPUPerSecond float64
	DecisionInterval, StableFor          time.Duration
}

func (c *CPUConfig) Normalize() error {
	if c.MinCPU == 0 {
		c.MinCPU = .5
	}
	if c.MaxCPU == 0 {
		c.MaxCPU = 4
	}
	if c.LowPressure == 0 {
		c.LowPressure = 10
	}
	if c.HighPressure == 0 {
		c.HighPressure = 20
	}
	if c.DecreaseFactor == 0 {
		c.DecreaseFactor = .8
	}
	if c.IncreaseCPUPerSecond == 0 {
		c.IncreaseCPUPerSecond = .25
	}
	if c.DecisionInterval == 0 {
		c.DecisionInterval = time.Second
	}
	if c.StableFor == 0 {
		c.StableFor = 2 * time.Second
	}
	for _, v := range []float64{c.BaseCPU, c.MinCPU, c.MaxCPU, c.LowPressure, c.HighPressure, c.DecreaseFactor, c.IncreaseCPUPerSecond} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("CPU policy parameters must be finite")
		}
	}
	if c.MinCPU < .05 || c.MinCPU > c.MaxCPU || c.MaxCPU > 4 {
		return fmt.Errorf("CPU bounds must satisfy 0.05 <= min <= max <= 4")
	}
	if c.BaseCPU < 0 || (c.BaseCPU > 0 && (c.BaseCPU < c.MinCPU || c.BaseCPU > c.MaxCPU)) {
		return fmt.Errorf("base CPU must lie inside CPU bounds")
	}
	if c.LowPressure <= 0 || c.LowPressure >= c.HighPressure || c.HighPressure > 100 {
		return fmt.Errorf("CPU pressure thresholds must satisfy 0 < low < high <= 100")
	}
	if c.DecreaseFactor < .5 || c.DecreaseFactor >= 1 || c.IncreaseCPUPerSecond <= 0 || c.IncreaseCPUPerSecond > 4 {
		return fmt.Errorf("invalid AIMD decrease or recovery rate")
	}
	if c.DecisionInterval < 100*time.Millisecond || c.DecisionInterval > 10*time.Second || c.StableFor < c.DecisionInterval || c.StableFor > time.Minute {
		return fmt.Errorf("invalid CPU decision interval or stable duration")
	}
	return nil
}

type PolicySample struct {
	BusinessValid     bool
	BusinessHealthy   bool
	Valid             bool
	ProtectedPressure float64
	SandboxCPU        float64
	Throttled         bool
}

type Decision struct {
	CPU           float64
	State, Reason string
}

// AIMD owns no goroutine or I/O. The caller supplies an increasing clock and
// commits a proposed action only after a confirmed write (or shadow step).
type AIMD struct {
	config                                      CPUConfig
	highSince, lowSince, lastSample, lastChange time.Time
}

func NewAIMD(c CPUConfig) (*AIMD, error) {
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return &AIMD{config: c}, nil
}

func (p *AIMD) Decide(now time.Time, current float64, s PolicySample) Decision {
	c := p.config
	hold := Decision{CPU: current, State: "normal", Reason: "within_hysteresis"}
	gap := !p.lastSample.IsZero() && (now.Sub(p.lastSample) <= 0 || now.Sub(p.lastSample) > 3*c.DecisionInterval)
	p.lastSample = now
	if c.BaseCPU > 0 {
		valid := s.Valid && s.BusinessValid && !gap && !math.IsNaN(s.ProtectedPressure) && !math.IsInf(s.ProtectedPressure, 0)
		if !valid || !s.BusinessHealthy {
			p.highSince, p.lowSince = time.Time{}, time.Time{}
			hold.State, hold.Reason = "pressured", "business_unhealthy"
			if !valid {
				hold.State, hold.Reason = "degraded", "business_or_pressure_unknown"
			}
			if current > c.BaseCPU {
				hold.CPU = c.BaseCPU
				return hold
			}
			// The configured base is a deployment choice, not proof of the SLO.
			// Persistent observed harm may reduce below it down to the hard floor.
			if valid && s.SandboxCPU > .01 && (p.lastChange.IsZero() || now.Sub(p.lastChange) >= c.DecisionInterval) {
				hold.CPU = math.Max(c.MinCPU, current*c.DecreaseFactor)
			}
			return hold
		}
	}
	if !s.Valid || gap || math.IsNaN(s.ProtectedPressure) || math.IsInf(s.ProtectedPressure, 0) {
		p.highSince, p.lowSince = time.Time{}, time.Time{}
		hold.State, hold.Reason = "degraded", "invalid_cpu_sample"
		return hold
	}
	if s.ProtectedPressure > c.HighPressure && s.SandboxCPU > .01 {
		p.lowSince = time.Time{}
		if p.highSince.IsZero() {
			p.highSince = now
		}
		hold.State, hold.Reason = "pressured", "pressure_pending"
		if now.Sub(p.highSince) < c.DecisionInterval || (!p.lastChange.IsZero() && now.Sub(p.lastChange) < c.DecisionInterval) {
			return hold
		}
		hold.CPU, hold.Reason = math.Max(c.MinCPU, current*c.DecreaseFactor), "protected_cpu_pressure"
		if c.BaseCPU > 0 && current > c.BaseCPU {
			hold.CPU = math.Min(hold.CPU, c.BaseCPU)
		}
		if hold.CPU == current {
			hold.Reason = "minimum_budget"
		}
		return hold
	}
	p.highSince = time.Time{}
	if s.ProtectedPressure < c.LowPressure {
		if p.lowSince.IsZero() {
			p.lowSince = now
		}
		hold.Reason = "stable_pending"
		if now.Sub(p.lowSince) < c.StableFor {
			return hold
		}
		if s.SandboxCPU < .01 || !(s.Throttled || s.SandboxCPU >= .8*current) {
			hold.Reason = "no_budget_demand"
			return hold
		}
		if !p.lastChange.IsZero() && now.Sub(p.lastChange) < c.DecisionInterval {
			hold.Reason = "cooldown"
			return hold
		}
		elapsed := c.DecisionInterval
		if !p.lastChange.IsZero() {
			elapsed = min(now.Sub(p.lastChange), 2*c.DecisionInterval)
		}
		hold.CPU = math.Min(c.MaxCPU, current+c.IncreaseCPUPerSecond*elapsed.Seconds())
		hold.State, hold.Reason = "recovering", "stable_with_demand"
		if hold.CPU == current {
			hold.State, hold.Reason = "normal", "maximum_budget"
		}
		return hold
	}
	p.lowSince = time.Time{}
	return hold
}

func (p *AIMD) Commit(now time.Time) { p.lastChange = now }
