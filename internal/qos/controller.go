package qos

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

type CPUActuator interface {
	ReadCPUQuota() (cgroup.CPUBandwidth, error)
	SetCPUQuota(cgroup.CPUBandwidth, uint64) (cgroup.CPUBandwidth, error)
}

type ControlReport struct {
	State, Reason, Action, Error                string
	At                                          time.Time
	Sequence, Decisions, Changes, WriteFailures uint64
	ActualCPU                                   *float64
	SuggestedCPU                                float64
	Applied                                     bool
	Config                                      CPUConfig
}

type CPUController struct {
	mode       string
	policy     *AIMD
	actuator   CPUActuator
	logger     *slog.Logger
	bandwidth  cgroup.CPUBandwidth
	virtualCPU float64
	fault      bool
	report     ControlReport
}

func NewCPUController(mode string, config CPUConfig, actuator CPUActuator, logger *slog.Logger) (*CPUController, error) {
	if mode != "observe" && mode != "enforce" {
		return nil, fmt.Errorf("CPU control requires observe or enforce mode")
	}
	p, err := NewAIMD(config)
	if err != nil {
		return nil, err
	}
	b, err := actuator.ReadCPUQuota()
	if err != nil {
		return nil, fmt.Errorf("initialize CPU control: %w", err)
	}
	c := &CPUController{mode: mode, policy: p, actuator: actuator, logger: logger, bandwidth: b}
	actual := float64(b.Quota) / float64(b.Period)
	minQuota := uint64(math.Ceil(p.config.MinCPU * float64(b.Period)))
	maxQuota := uint64(math.Floor(p.config.MaxCPU * float64(b.Period)))
	if minQuota > maxQuota {
		return nil, fmt.Errorf("CPU bounds have no representable quota")
	}
	if b.Quota < minQuota {
		return nil, fmt.Errorf("initial CPU budget is below configured minimum")
	}
	if actual > p.config.MaxCPU && mode == "enforce" {
		b, err = actuator.SetCPUQuota(b, maxQuota)
		if err != nil {
			return nil, fmt.Errorf("initialize CPU maximum: %w", err)
		}
		c.bandwidth, actual = b, float64(b.Quota)/float64(b.Period)
	}
	c.virtualCPU = math.Min(actual, float64(maxQuota)/float64(b.Period))
	c.report = ControlReport{State: "starting", Reason: "awaiting_cpu_samples", ActualCPU: &actual, SuggestedCPU: c.virtualCPU, Config: p.config}
	return c, nil
}

func (c *CPUController) Update(snapshot Snapshot, now time.Time) ControlReport {
	r := c.report
	if snapshot.Sequence <= r.Sequence {
		return r
	}
	r.At, r.Sequence, r.Applied = now, snapshot.Sequence, false
	r.Action = "hold"
	if c.fault {
		r.ActualCPU = nil
		c.report = r
		return r
	}
	b, err := c.actuator.ReadCPUQuota()
	if err != nil || b != c.bandwidth {
		r.ActualCPU = nil
		if err == nil {
			actual := float64(b.Quota) / float64(b.Period)
			r.ActualCPU = &actual
			err = fmt.Errorf("CPU budget changed outside the controller")
		}
		c.fail(&r, err, false)
		c.report = r
		return r
	}
	actual := float64(b.Quota) / float64(b.Period)
	r.ActualCPU = &actual
	sample := policySampleAt(snapshot, now)
	if snapshot.Pressure != nil && snapshot.Pressure.State == "unknown" {
		sample.Valid = false
	}
	current := actual
	if c.mode == "observe" {
		current = c.virtualCPU
	}
	d := c.policy.Decide(now, current, sample)
	// The shared pressure classification governs all live actuators. AIMD
	// still owns budget demand, bounds and cooldown, not scheduling health.
	if p := snapshot.Pressure; p != nil {
		if d.CPU < current && p.EffectiveState != "pressured" {
			d.CPU, d.Reason = current, "pressure_pending"
		}
		if d.CPU > current && p.EffectiveState == "pressured" {
			d.CPU, d.Reason = current, "recovery_pending"
		}
	}
	r.Decisions++
	r.State, r.Reason, r.SuggestedCPU = d.State, d.Reason, d.CPU
	quota := uint64(math.Round(d.CPU * float64(b.Period)))
	quota = max(uint64(math.Ceil(c.policy.config.MinCPU*float64(b.Period))), min(quota, uint64(math.Floor(c.policy.config.MaxCPU*float64(b.Period)))))
	r.SuggestedCPU = float64(quota) / float64(b.Period)
	currentQuota := uint64(math.Round(current * float64(b.Period)))
	if quota != currentQuota {
		r.Action = "increase"
		if quota < currentQuota {
			r.Action = "decrease"
		}
		if c.mode == "enforce" {
			observed, err := c.actuator.SetCPUQuota(b, quota)
			if err != nil {
				r.ActualCPU = nil // A partial write/readback failure makes the actual value uncertain.
				c.fail(&r, err, true)
				c.report = r
				return r
			}
			c.bandwidth = observed
			value := float64(observed.Quota) / float64(observed.Period)
			r.ActualCPU, r.Applied = &value, true
		}
		c.virtualCPU = float64(quota) / float64(b.Period)
		c.policy.Commit(now)
		r.Changes++
		if c.logger != nil {
			c.logger.Info("CPU budget decision", "mode", c.mode, "action", r.Action, "reason", r.Reason, "suggested_cpu", r.SuggestedCPU, "applied", r.Applied, "sample_sequence", r.Sequence)
		}
	}
	c.report = r
	return r
}

func (c *CPUController) fail(r *ControlReport, err error, write bool) {
	c.fault = true
	r.State, r.Reason, r.Error = "degraded", "cpu_control_failed", err.Error()
	if write {
		r.WriteFailures++
	}
	if c.logger != nil {
		c.logger.Error("CPU control stopped", "error", err)
	}
}

func policySample(s Snapshot) PolicySample {
	var core, sandboxes *Observation
	for i := range s.Sources {
		switch s.Sources[i].Source.Name {
		case "protected":
			core = &s.Sources[i]
		case "sandboxes":
			sandboxes = &s.Sources[i]
		}
	}
	if s.Stale || s.CollectedAt.IsZero() || s.Duration > 3*s.Interval || core == nil || sandboxes == nil || !core.Available || !sandboxes.Available || !core.Delta.Valid || !sandboxes.Delta.Valid {
		return PolicySample{}
	}
	return PolicySample{Valid: true, ProtectedPressure: core.Delta.CPUSomePercent, SandboxCPU: sandboxes.Delta.CPUUsageCores, Throttled: sandboxes.Delta.CPUThrottledPeriods > 0}
}

func policySampleAt(s Snapshot, now time.Time) PolicySample {
	sample := policySample(s)
	if s.Interval <= 0 || now.Sub(s.CollectedAt) > 3*s.Interval || now.Before(s.CollectedAt) || math.IsNaN(sample.ProtectedPressure) || math.IsInf(sample.ProtectedPressure, 0) || sample.ProtectedPressure < 0 || math.IsNaN(sample.SandboxCPU) || math.IsInf(sample.SandboxCPU, 0) || sample.SandboxCPU < 0 {
		sample.Valid = false
	}
	return sample
}
