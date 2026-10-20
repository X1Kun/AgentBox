package sandbox

import "time"

// Reporting values are separate from policy history so status copies are immutable.
type inventoryState struct {
	Target, RawTarget, StabilizedTarget                 int
	Reason, LimitedBy, HeadWaitReason                   string
	LastChange                                          time.Time
	ScaleUps, ScaleDowns                                uint64
	InventoryStalls, SlotWaitRequests, BothWaitRequests uint64
	ShrinkAttempts, ShrinkSucceeded, ShrinkFailures     uint64
}

func (p *warmPool) observeInventoryLocked(now time.Time) {
	if p.policy != nil && p.initialized && !p.stopped && !p.failed && !p.pressurePaused {
		p.policy.observe(now, len(p.ready), p.inventory.Target)
	}
}

func (p *warmPool) resetInventoryLocked(reason string) {
	if p.policy == nil {
		return
	}
	p.policy.reset(p.now(), len(p.ready), p.inventory.Target, reason)
	p.inventory.RawTarget, p.inventory.StabilizedTarget = p.inventory.Target, p.inventory.Target
	p.inventory.Reason, p.inventory.LimitedBy = reason, ""
}

func (p *warmPool) adjustInventoryLocked(now time.Time) {
	if p.policy == nil || !p.initialized || p.stopped || p.failed || p.pressurePaused {
		return
	}
	i := &p.inventory
	d := p.policy.decide(now, len(p.ready), i.Target, i.LastChange, len(p.items)+p.creating < p.config.MaxSandboxes)
	i.RawTarget, i.StabilizedTarget, i.Reason, i.LimitedBy = d.Raw, d.Stabilized, d.Reason, d.LimitedBy
	if d.Target == i.Target {
		return
	}
	if d.Target > i.Target {
		i.ScaleUps++
	} else {
		i.ScaleDowns++
	}
	i.Target, i.LastChange = d.Target, now
	// Seed the accepted scale once; do not feed a held target back each tick.
	p.policy.reset(now, len(p.ready), i.Target, "window_pending")
	p.signalLocked()
}

// Reserve only excess READY under the assignment mutex. Runnable FIFO work has
// priority; an occupied execution slot alone need not prevent idle retirement.
func (p *warmPool) reserveSurplusLocked() *executionState {
	if p.stopped || p.failed || p.pressurePaused || len(p.ready) <= p.inventory.Target ||
		(len(p.waiters) > 0 && p.executing < p.config.Concurrency) {
		return nil
	}
	last := len(p.ready) - 1
	instance := p.ready[last]
	p.ready[last] = nil
	p.ready = p.ready[:last]
	p.items[instance] = poolDestroying
	p.inventory.ShrinkAttempts++
	p.signalLocked()
	return instance
}

// This worker also performs shrink cleanup, so stop joins all reserved
// retirements before drain starts. No untracked cleanup goroutine is created.
func (p *warmPool) manageInventory() {
	defer p.workers.Done()
	ticker := time.NewTicker(inventoryObservationInterval)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		if p.stopped || p.failed {
			p.mu.Unlock()
			return
		}
		p.adjustInventoryLocked(p.now())
		if instance := p.reserveSurplusLocked(); instance != nil {
			p.mu.Unlock()
			err := p.destroy(instance)
			p.mu.Lock()
			if err == nil {
				delete(p.items, instance)
				p.inventory.ShrinkSucceeded++
			} else {
				p.inventory.ShrinkFailures++
				p.failCleanupLocked(err)
			}
			p.signalLocked()
			p.mu.Unlock()
			continue
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-p.ctx.Done():
			return
		case <-changed:
		case <-ticker.C:
		}
	}
}
