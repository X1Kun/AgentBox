package qos

import "time"

// PressureReport separates scheduling recommendations from CPU actuator health.
// Unknown samples retain the last effective restriction.
type PressureReport struct {
	State, EffectiveState, Reason string
	AdmissionOpen, RefillPaused   bool
	Sequence                      uint64
	At                            time.Time
}

type PressurePolicy struct {
	config                                       CPUConfig
	effective                                    string
	highSince, lowSince, recoveredAt, lastSample time.Time
	report                                       PressureReport
}

func NewPressurePolicy(config CPUConfig) (*PressurePolicy, error) {
	if err := config.Normalize(); err != nil {
		return nil, err
	}
	return &PressurePolicy{config: config, effective: "normal", report: PressureReport{State: "unknown", EffectiveState: "normal", Reason: "awaiting_cpu_samples", AdmissionOpen: true}}, nil
}

func (p *PressurePolicy) Update(snapshot Snapshot, now time.Time) PressureReport {
	if snapshot.Sequence <= p.report.Sequence {
		return p.report
	}
	sample := policySampleAt(snapshot, now)
	gap := !p.lastSample.IsZero() && (now.Sub(p.lastSample) <= 0 || now.Sub(p.lastSample) > 3*snapshot.Interval)
	p.lastSample = now
	state, reason := p.effective, "within_hysteresis"
	if !sample.Valid || gap {
		p.highSince, p.lowSince = time.Time{}, time.Time{}
		if p.effective == "recovering" {
			p.recoveredAt = now
		}
		state, reason = "unknown", "invalid_cpu_sample"
	} else if sample.ProtectedPressure > p.config.HighPressure {
		p.lowSince = time.Time{}
		if p.highSince.IsZero() {
			p.highSince = now
		}
		reason = "pressure_pending"
		if now.Sub(p.highSince) >= p.config.DecisionInterval {
			p.effective = "pressured"
			reason = "sustained_cpu_pressure"
		}
		state = p.effective
	} else if sample.ProtectedPressure < p.config.LowPressure {
		p.highSince = time.Time{}
		if p.lowSince.IsZero() {
			p.lowSince = now
		}
		reason = "stable_pending"
		if p.effective == "pressured" && now.Sub(p.lowSince) >= p.config.StableFor {
			p.effective, p.recoveredAt = "recovering", now
			reason = "sustained_low_pressure"
		} else if p.effective == "recovering" && now.Sub(p.recoveredAt) >= p.config.DecisionInterval {
			p.effective = "normal"
			reason = "recovered"
		} else if p.effective == "normal" {
			reason = "low_pressure"
		}
		state = p.effective
	} else {
		p.highSince, p.lowSince = time.Time{}, time.Time{}
		if p.effective == "recovering" && now.Sub(p.recoveredAt) >= p.config.DecisionInterval {
			p.effective = "normal"
		}
		state = p.effective
	}
	p.report = PressureReport{State: state, EffectiveState: p.effective, Reason: reason, AdmissionOpen: p.effective != "pressured", RefillPaused: p.effective == "pressured", Sequence: snapshot.Sequence, At: now}
	return p.report
}
