package sandbox

import (
	"context"
	"errors"
	"time"
)

// Queue membership, instance ownership and execution permits share the pool
// mutex, so a dispatch cannot consume only one of the two required resources.
type poolWaiter struct {
	waitReasons   uint8
	pressureEpoch uint64
	bytes         int
	enteredAt     time.Time
	assignedAt    time.Time
}

func (p *warmPool) takeLocked() *executionState {
	instance := p.ready[0]
	p.ready[0] = nil
	p.ready = p.ready[1:]
	p.items[instance] = poolLeased
	p.executing++
	p.signalLocked()
	return instance
}

// acquire atomically assigns execution and either READY or creation capacity.
// Queued callers remain FIFO and leave promptly on cancellation or expiry.
func (p *warmPool) acquire(ctx context.Context, bytes int) (_ *executionState, retErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.initialized || p.failed || p.stopped {
		return nil, admissionError("executor_unhealthy", ErrUnhealthy)
	}
	if p.pressurePaused {
		return nil, admissionError("qos_pressure", ErrPressure)
	}
	if len(p.waiters) == 0 && p.canAssignLocked() {
		return p.assignLocked(ctx)
	}
	if len(p.waiters) >= p.config.QueueSize {
		p.telemetry.QueueCountFull++
		return nil, admissionError("queue_count_full", ErrBusy)
	}
	if bytes > p.config.QueueBytes-p.queuedBytes {
		p.telemetry.QueueBytesFull++
		return nil, admissionError("queue_bytes_full", ErrBusy)
	}
	waiter := &poolWaiter{bytes: bytes, enteredAt: time.Now(), pressureEpoch: p.pressureEpoch}
	p.telemetry.QueueEntered++
	p.waiters = append(p.waiters, waiter)
	p.queuedBytes += bytes
	p.signalLocked()
	defer func() {
		elapsed := time.Since(waiter.enteredAt)
		if !waiter.assignedAt.IsZero() {
			elapsed = waiter.assignedAt.Sub(waiter.enteredAt)
		}
		switch {
		case !waiter.assignedAt.IsZero():
			// Recorded at reservation, before creation releases the mutex.
		case errors.Is(retErr, context.Canceled) || errors.Is(retErr, context.DeadlineExceeded):
			p.telemetry.QueueCanceled.Record(elapsed)
		case errors.Is(retErr, ErrUnhealthy):
			p.telemetry.QueueUnavailable.Record(elapsed)
		case errors.Is(retErr, ErrPressure):
			p.telemetry.QueuePressureRejected.Record(elapsed)
		default:
			p.telemetry.QueueTimedOut.Record(elapsed)
		}
		var admission *AdmissionError
		if errors.As(retErr, &admission) {
			admission.Queued, admission.QueueWait = true, elapsed
		}
		for i, candidate := range p.waiters {
			if candidate == waiter {
				p.waiters = append(p.waiters[:i], p.waiters[i+1:]...)
				p.queuedBytes -= candidate.bytes
				p.signalLocked()
				break
			}
		}
	}()
	deadline := time.Now().Add(p.config.QueueTimeout)
	timer := time.NewTimer(p.config.QueueTimeout)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.failed || p.stopped {
			return nil, admissionError("executor_unhealthy", ErrUnhealthy)
		}
		if p.pressurePaused || waiter.pressureEpoch != p.pressureEpoch {
			return nil, admissionError("qos_pressure", ErrPressure)
		}
		if !time.Now().Before(deadline) {
			return nil, admissionError("queue_timeout", ErrBusy)
		}
		if p.waiters[0] == waiter && p.canAssignLocked() {
			waiter.assignedAt = time.Now()
			p.telemetry.QueueAssigned.Record(waiter.assignedAt.Sub(waiter.enteredAt))
			p.waiters = p.waiters[1:]
			p.queuedBytes -= waiter.bytes
			return p.assignLocked(ctx)
		}
		p.observeWaiterLocked(waiter)
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-changed:
		}
		p.mu.Lock()
	}
}

const (
	waitSlot uint8 = 1 << iota
	waitInventory
	waitBoth
)

// Only the FIFO head can identify its actual resource blocker. Each reason is
// counted once per waiter; a later both/slot -> inventory transition is retained.
func (p *warmPool) observeWaiterLocked(waiter *poolWaiter) {
	if p.policy == nil || len(p.waiters) == 0 || p.waiters[0] != waiter {
		return
	}
	reason := waitSlot
	if len(p.ready) == 0 {
		reason = waitInventory
		if p.executing >= p.config.Concurrency {
			reason = waitBoth
		}
	}
	if waiter.waitReasons&reason != 0 {
		return
	}
	waiter.waitReasons |= reason
	switch reason {
	case waitSlot:
		p.inventory.SlotWaitRequests++
	case waitBoth:
		p.inventory.BothWaitRequests++
	case waitInventory:
		p.inventory.InventoryStalls++
		p.policy.recordStall(p.now(), len(p.ready), p.inventory.Target)
		p.signalLocked()
	}
}

// A miss reserves creation, execution and total capacity under the same lock.
// Background refill uses this same creating counter and cannot jump the FIFO.
func (p *warmPool) canAssignLocked() bool {
	return p.executing < p.config.Concurrency && (len(p.ready) > 0 ||
		(p.create != nil && p.creating < p.config.CreateConcurrency && len(p.items)+p.creating < p.config.MaxSandboxes))
}

// Called with mu held, returns with mu held. The reservation remains owned by
// the caller through cleanup, including a partial creation or cancellation.
func (p *warmPool) assignLocked(ctx context.Context) (*executionState, error) {
	if len(p.ready) > 0 {
		return p.takeLocked(), nil
	}
	p.creating++
	p.executing++
	p.telemetry.CreationAttempts++
	p.signalLocked()
	p.mu.Unlock()
	begin := time.Now()
	instance, err := p.create(ctx)
	elapsed := time.Since(begin)
	p.mu.Lock()
	p.creating--
	p.creationTotalMS += ms(elapsed)
	if err == nil {
		p.creationCount++
		p.consecutiveFailures = 0
		p.telemetry.CreationSucceeded.Record(elapsed)
	} else if ctx.Err() != nil {
		p.telemetry.CreationAborted.Record(elapsed)
	} else {
		p.creationFailures++
		p.consecutiveFailures++
		p.lastError = err.Error()
		p.telemetry.CreationFailed.Record(elapsed)
		if p.consecutiveFailures >= 3 {
			p.failed = true
		}
	}
	if instance != nil {
		instance.creationTime = elapsed
		p.items[instance] = poolLeased
	} else {
		p.executing--
	}
	p.signalLocked()
	return instance, err
}
