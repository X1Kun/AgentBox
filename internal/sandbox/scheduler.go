package sandbox

import (
	"context"
	"time"
)

// Queue membership, instance ownership and execution permits share the pool
// mutex, so a dispatch cannot consume only one of the two required resources.
type poolWaiter struct{ bytes int }

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
func (p *warmPool) acquire(ctx context.Context, bytes int) (*executionState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.initialized || p.failed || p.stopped {
		return nil, ErrUnhealthy
	}
	if len(p.waiters) == 0 && len(p.ready) > 0 && p.executing < p.config.Concurrency {
		return p.takeLocked(), nil
	}
	if len(p.waiters) >= p.config.QueueSize || bytes > p.config.QueueBytes-p.queuedBytes {
		return nil, ErrBusy
	}
	waiter := &poolWaiter{bytes: bytes}
	p.waiters = append(p.waiters, waiter)
	p.queuedBytes += bytes
	p.signalLocked()
	defer func() {
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
			return nil, ErrUnhealthy
		}
		if !time.Now().Before(deadline) {
			return nil, ErrBusy
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
