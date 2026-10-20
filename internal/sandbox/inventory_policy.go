package sandbox

import "time"

const inventoryObservationInterval = 100 * time.Millisecond

type inventoryRecommendation struct {
	at     time.Time
	target int
}

// recommendationMax keeps increasing timestamps and strictly decreasing values.
// Targets are integers in 1..8, so at most eight entries can remain.
type recommendationMax struct{ entries []inventoryRecommendation }

func (q *recommendationMax) record(now time.Time, target int, window time.Duration) int {
	cutoff := now.Add(-window)
	for len(q.entries) > 0 && !q.entries[0].at.After(cutoff) {
		q.entries = q.entries[1:]
	}
	for len(q.entries) > 0 && q.entries[len(q.entries)-1].target <= target {
		q.entries = q.entries[:len(q.entries)-1]
	}
	q.entries = append(q.entries, inventoryRecommendation{now, target})
	return q.entries[0].target
}

type inventoryDecision struct {
	Raw, Stabilized, Target int
	Reason, LimitedBy       string
}

// inventoryPolicy owns only bounded policy state. The caller supplies the
// monotonic clock, observed READY counts, and capacity permission under pool.mu.
type inventoryPolicy struct {
	config                    Config
	history                   recommendationMax
	windowStart, lastObserved time.Time
	lastDecision, stallAt     time.Time
	minReady                  int
	pendingStall              bool
	raw                       int
	resetReason               string
}

func (p *inventoryPolicy) reset(now time.Time, ready, target int, reason string) {
	p.history.entries = p.history.entries[:0]
	p.history.record(now, target, p.config.WarmDownscaleWindow)
	p.windowStart, p.lastObserved, p.lastDecision = now, now, now
	p.stallAt, p.pendingStall = time.Time{}, false
	p.minReady, p.raw, p.resetReason = ready, target, reason
}

// Every READY mutation is observed, including brief depletion between ticks.
func (p *inventoryPolicy) observe(now time.Time, ready, target int) {
	if p.lastObserved.IsZero() {
		p.reset(now, ready, target, "window_pending")
	} else if now.Before(p.lastObserved) || now.Sub(p.lastObserved) > 3*inventoryObservationInterval {
		p.reset(now, ready, target, "sample_gap")
	}
	p.minReady = min(p.minReady, ready)
	p.lastObserved = now
}

func (p *inventoryPolicy) recordStall(now time.Time, ready, target int) {
	p.observe(now, ready, target)
	p.pendingStall, p.stallAt = true, now
}

func (p *inventoryPolicy) decide(now time.Time, ready, target int, lastChange time.Time, canGrow bool) inventoryDecision {
	p.observe(now, ready, target)
	// A blocked controller must rebuild history even if other state events
	// continued arriving while it was waiting for a runtime deletion.
	if now.Before(p.lastDecision) || now.Sub(p.lastDecision) > 3*inventoryObservationInterval {
		p.reset(now, ready, target, "sample_gap")
	}
	p.lastDecision = now
	d := inventoryDecision{Raw: p.raw, Target: target, Reason: p.resetReason}
	if p.pendingStall && now.Sub(p.stallAt) > p.config.WarmAdjustInterval+inventoryObservationInterval {
		p.pendingStall = false
	}
	if p.pendingStall {
		p.raw = min(target+1, p.config.WarmMaxSize)
		d.Raw, d.Reason = p.raw, "inventory_stall"
		d.Stabilized = p.history.record(now, d.Raw, p.config.WarmDownscaleWindow)
		switch {
		case target == p.config.WarmMaxSize:
			p.pendingStall = false
			d.LimitedBy = "maximum_inventory"
		case !canGrow:
			// A capacity-constrained observation is not saved for a later
			// speculative scale-up after its queue has already disappeared.
			p.pendingStall = false
			d.LimitedBy = "capacity_limited"
		case now.Sub(lastChange) < p.config.WarmAdjustInterval:
			d.LimitedBy = "action_cooldown"
		default:
			p.pendingStall = false // one coalesced event drives at most one action
			d.Target = d.Raw
		}
		return d
	}
	if now.Sub(p.windowStart) >= inventoryObservationInterval {
		p.raw = target
		if p.minReady > 0 {
			p.raw = max(p.config.WarmMinSize, target-1)
		}
		p.windowStart, p.minReady, p.resetReason = now, ready, "inventory_sufficient"
	}
	d.Raw = p.raw
	// Record the unconstrained recommendation, never the held/applied target.
	d.Stabilized = p.history.record(now, d.Raw, p.config.WarmDownscaleWindow)
	if d.Raw < target {
		d.Reason = "unused_reserve"
		if d.Stabilized >= target {
			d.LimitedBy = "downscale_stabilized"
		} else if now.Sub(lastChange) < p.config.WarmAdjustInterval {
			d.LimitedBy = "action_cooldown"
		} else {
			d.Target = max(d.Stabilized, target-1)
		}
	} else {
		d.Reason = p.resetReason
	}
	return d
}
