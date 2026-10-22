package sandbox

import (
	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/qos"
)

// Lock order: Engine then pool, matching admission. Shutdown never waits for
// workers with Engine.mu held. CPU actuator failures do not bypass this gate.
func (e *Engine) applyPressure(report qos.PressureReport) {
	if (!e.config.QoS.Scheduling && !e.config.QoS.RefillOnly) || e.config.QoS.Mode != "enforce" {
		return
	}
	e.mu.Lock()
	if e.closed.Load() || e.broken.Load() {
		e.mu.Unlock()
		return
	}
	if e.config.QoS.RefillOnly {
		e.refillPaused = report.RefillPaused
		if e.pool != nil {
			e.pool.mu.Lock()
			if !e.pool.stopped && !e.pool.failed && e.pool.refillPressurePaused != e.refillPaused {
				e.pool.refillPressurePaused = e.refillPaused
				e.pool.signalLocked()
			}
			e.pool.mu.Unlock()
		}
		e.mu.Unlock()
		return
	}
	paused := !report.AdmissionOpen
	changed := paused != e.pressurePaused
	e.pressurePaused = paused
	if e.pool != nil {
		e.pool.setPressure(paused)
	}
	e.mu.Unlock()
	if changed && e.config.Logger != nil {
		e.config.Logger.Info("QoS scheduling changed", "pressure_state", report.State, "admission_open", !paused, "refill_paused", paused && e.pool != nil, "reason", report.Reason, "sample_sequence", report.Sequence)
	}
}

func (p *warmPool) setPressure(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setPressureLocked(paused)
}

func (p *warmPool) setPressureLocked(paused bool) {
	if p.stopped || p.failed || p.pressurePaused == paused {
		return
	}
	p.pressurePaused = paused
	// Break recommendation continuity; recovery needs fresh valid history.
	p.resetInventoryLocked("pressure_recovery")
	if paused {
		p.inventory.Reason, p.inventory.LimitedBy = "qos_pressure", "qos_pressure"
	}
	if paused {
		p.pressureEpoch++
	}
	p.signalLocked()
}

func (e *Engine) qosSchedulingReport() *api.QoSSchedulingReport {
	if !e.config.QoS.Scheduling && !e.config.QoS.RefillOnly {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return &api.QoSSchedulingReport{Enabled: true, Enforced: e.config.QoS.Mode == "enforce", RefillOnly: e.config.QoS.RefillOnly, AdmissionOpen: e.Ready() && !e.pressurePaused, RefillPaused: e.pool != nil && (e.pressurePaused || e.refillPaused), PressureRejections: e.pressureRejections}
}
