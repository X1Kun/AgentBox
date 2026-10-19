package sandbox

import (
	"context"
	"errors"
	"time"
)

// Queue membership, instance ownership and execution permits share the pool
// mutex, so a dispatch cannot consume only one of the two required resources.
type poolWaiter struct {
	bytes     int
	enteredAt time.Time
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

// acquire assigns an execution permit and a READY instance atomically. Queued
// callers remain FIFO and are removed promptly on cancellation or expiry.
func (p *warmPool) acquire(ctx context.Context, bytes int) (_ *executionState, retErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.initialized || p.failed || p.stopped {
		return nil, admissionError("executor_unhealthy", ErrUnhealthy)
	}
	if len(p.waiters) == 0 && len(p.ready) > 0 && p.executing < p.config.Concurrency {
		return p.takeLocked(), nil
	}
	if len(p.waiters) >= p.config.QueueSize {
		p.telemetry.QueueCountFull++
		return nil, admissionError("queue_count_full", ErrBusy)
	}
	if bytes > p.config.QueueBytes-p.queuedBytes {
		p.telemetry.QueueBytesFull++
		return nil, admissionError("queue_bytes_full", ErrBusy)
	}
	waiter := &poolWaiter{bytes: bytes, enteredAt: time.Now()}
	p.telemetry.QueueEntered++
	p.waiters = append(p.waiters, waiter)
	p.queuedBytes += bytes
	p.signalLocked()
	defer func() {
		elapsed := time.Since(waiter.enteredAt)
		switch {
		case retErr == nil:
			p.telemetry.QueueAssigned.Record(elapsed)
		case errors.Is(retErr, context.Canceled) || errors.Is(retErr, context.DeadlineExceeded):
			p.telemetry.QueueCanceled.Record(elapsed)
		case errors.Is(retErr, ErrUnhealthy):
			p.telemetry.QueueUnavailable.Record(elapsed)
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
		if !time.Now().Before(deadline) {
			return nil, admissionError("queue_timeout", ErrBusy)
		}
		if p.waiters[0] == waiter && len(p.ready) > 0 && p.executing < p.config.Concurrency {
			return p.takeLocked(), nil
		}
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
