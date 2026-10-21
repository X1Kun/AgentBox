package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/X1Kun/AgentBox/internal/metrics"
)

type poolState string

const (
	cleanupConcurrency = 4

	poolReady      poolState = "ready"
	poolLeased     poolState = "leased"
	poolRunning    poolState = "running"
	poolDestroying poolState = "destroying"
)

// warmPool is the shared Cold/Warm scheduler and owns every instance from creation reservation through confirmed
// destruction. A lease is never returned to READY, even if its task is cancelled.
type warmPool struct {
	config  Config
	create  func(context.Context) (*executionState, error)
	destroy func(*executionState) error

	mu      sync.Mutex
	changed chan struct{}
	items   map[*executionState]poolState
	ready   []*executionState
	waiters []*poolWaiter

	queuedBytes    int
	creating       int
	executing      int
	pressurePaused bool
	pressureEpoch  uint64
	inventory      inventoryState
	policy         *inventoryPolicy
	now            func() time.Time

	started             bool
	initialized         bool
	stopped             bool
	failed              bool
	consecutiveFailures int
	lastError           string

	creationCount    uint64
	creationFailures uint64
	cleanupFailures  uint64
	creationTotalMS  float64
	telemetry        poolTelemetry

	ctx     context.Context
	cancel  context.CancelFunc
	workers sync.WaitGroup
}

// poolSnapshot contains internal counters, without transport serialization.
type poolSnapshot struct {
	Inventory         inventoryState
	Telemetry         poolTelemetry
	ExecutionInFlight int
	OldestQueueMS     float64
	RefillBlockedBy   string
	Config            Config
	Ready             bool
	Target            int
	MaxSandboxes      int
	Creating          int
	Idle              int
	Leased            int
	Running           int
	Destroying        int
	Total             int
	Queued            int
	QueuedBytes       int
	CreationCount     uint64
	CreationFailures  uint64
	CleanupFailures   uint64
	CreationTotalMS   float64
}

type poolTelemetry struct {
	CreationAttempts                                              uint64
	CreationSucceeded, CreationFailed, CreationAborted            metrics.Duration
	QueueEntered                                                  uint64
	QueueAssigned, QueueCanceled, QueueTimedOut, QueueUnavailable metrics.Duration
	QueuePressureRejected                                         metrics.Duration
	QueueCountFull, QueueBytesFull                                uint64
}

func newWarmPool(c Config, create func(context.Context) (*executionState, error), destroy func(*executionState) error) *warmPool {
	ctx, cancel := context.WithCancel(context.Background())
	target, reason := c.WarmPoolSize, "fixed_inventory"
	if c.Mode == "cold" {
		target, reason = 0, "on_demand"
	}
	if c.DynamicWarmPool {
		target, reason = c.WarmMinSize, "minimum_inventory"
	}
	p := &warmPool{config: c, create: create, destroy: destroy, changed: make(chan struct{}), items: make(map[*executionState]poolState), ctx: ctx, cancel: cancel, now: time.Now,
		inventory: inventoryState{Target: target, RawTarget: target, StabilizedTarget: target, Reason: reason, LastChange: time.Now()}}
	if c.Mode == "cold" {
		p.initialized = true
	}
	if c.DynamicWarmPool {
		p.policy = &inventoryPolicy{config: c}
	}
	return p
}

// signalLocked broadcasts state changes without holding a lock during waits.
func (p *warmPool) signalLocked() {
	p.observeInventoryLocked(p.now())
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *warmPool) start(ctx context.Context) error {
	p.mu.Lock()
	if !p.started && !p.stopped {
		p.started = true
		if p.config.DynamicWarmPool {
			p.workers.Add(1)
			go p.manageInventory()
		}
		for i := 0; i < p.config.CreateConcurrency; i++ {
			p.workers.Add(1)
			go p.refill()
		}
	}
	for {
		if p.stopped || p.failed {
			err := errors.New("sandbox pool unavailable: " + p.lastError)
			p.mu.Unlock()
			return err
		}
		if p.initialized {
			p.mu.Unlock()
			return nil
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			p.mu.Lock()
			err := fmt.Errorf("warm pool startup: %w (creating=%d, idle=%d, last_error=%q)", ctx.Err(), p.creating, len(p.ready), p.lastError)
			p.mu.Unlock()
			return err
		case <-changed:
		}
		p.mu.Lock()
	}
}

func (p *warmPool) healthy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.initialized && !p.failed && !p.stopped
}

func (p *warmPool) refill() {
	defer p.workers.Done()
	for {
		p.mu.Lock()
		if p.stopped || p.failed {
			p.mu.Unlock()
			return
		}
		if p.refillBlockerLocked() != "" {
			changed := p.changed
			p.mu.Unlock()
			select {
			case <-p.ctx.Done():
				return
			case <-changed:
			}
			continue
		}
		p.creating++
		p.telemetry.CreationAttempts++
		p.signalLocked()
		p.mu.Unlock()
		begin := time.Now()
		instance, err := p.create(p.ctx)
		p.mu.Lock()
		p.creating--
		elapsed := time.Since(begin)
		p.creationTotalMS += ms(elapsed)
		if err == nil && !p.stopped && !p.failed {
			p.telemetry.CreationSucceeded.Record(elapsed)
			p.items[instance] = poolReady
			p.ready = append(p.ready, instance)
			p.initialized = true
			p.creationCount++
			p.consecutiveFailures = 0
			p.signalLocked()
			p.mu.Unlock()
			continue
		}
		if err != nil && !p.stopped {
			p.telemetry.CreationFailed.Record(elapsed)
			p.creationFailures++
			p.consecutiveFailures++
			p.lastError = err.Error()
			if p.consecutiveFailures >= 3 {
				p.failed = true
			}
		} else {
			p.telemetry.CreationAborted.Record(elapsed)
		}
		if instance != nil {
			p.items[instance] = poolDestroying
		}
		p.signalLocked()
		p.mu.Unlock()
		if instance != nil {
			cleanupErr := p.destroy(instance)
			p.mu.Lock()
			if cleanupErr == nil {
				delete(p.items, instance)
			} else {
				p.failCleanupLocked(errors.Join(err, cleanupErr))
			}
			p.signalLocked()
			p.mu.Unlock()
		}
		// Back off failed creation rather than spinning under resource pressure.
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-p.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *warmPool) mark(instance *executionState, state poolState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.items[instance]; ok {
		p.items[instance] = state
		p.signalLocked()
	}
}

func (p *warmPool) failCleanupLocked(err error) {
	p.failed = true
	p.cleanupFailures++
	p.lastError = err.Error()
}

func (p *warmPool) release(instance *executionState, cleanupComplete bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.executing--
	if cleanupComplete {
		delete(p.items, instance)
	} else {
		p.failCleanupLocked(errors.New("leased sandbox cleanup failed"))
	}
	p.signalLocked()
}

func (p *warmPool) stop() {
	p.mu.Lock()
	p.stopped = true
	p.cancel()
	p.signalLocked()
	p.mu.Unlock()
	p.workers.Wait()
}

// Called after admissions, background creation and leased executions stop.
// Failed destruction remains counted and recorded for restart reconciliation.
func (p *warmPool) drain() error {
	p.mu.Lock()
	var result error
	if p.cleanupFailures > 0 {
		result = errors.New("sandbox pool retains failed cleanup records: " + p.lastError)
	}
	remaining := p.ready
	p.ready = nil
	for _, instance := range remaining {
		p.items[instance] = poolDestroying
	}
	p.signalLocked()
	p.mu.Unlock()
	// Keep the capacity reservation until each independent deletion completes.
	// A failure must not prevent the other instances from being reclaimed.
	jobs := make(chan int, len(remaining))
	for index := range remaining {
		jobs <- index
	}
	close(jobs)
	cleanupErrors := make([]error, len(remaining))
	var workers sync.WaitGroup
	for worker := 0; worker < min(cleanupConcurrency, len(remaining)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				instance := remaining[index]
				err := p.destroy(instance)
				cleanupErrors[index] = err
				p.mu.Lock()
				if err == nil {
					delete(p.items, instance)
				} else {
					p.failCleanupLocked(err)
				}
				p.signalLocked()
				p.mu.Unlock()
			}
		}()
	}
	workers.Wait()
	return errors.Join(result, errors.Join(cleanupErrors...))
}

func (p *warmPool) status() poolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := poolSnapshot{RefillBlockedBy: p.refillBlockerLocked(), Ready: p.initialized && !p.failed && !p.stopped, Telemetry: p.telemetry, ExecutionInFlight: p.executing, Config: p.config, Inventory: p.inventory,
		Target: p.inventory.Target, MaxSandboxes: p.config.MaxSandboxes,
		Creating: p.creating, Total: len(p.items) + p.creating, Queued: len(p.waiters), QueuedBytes: p.queuedBytes,
		CreationCount: p.creationCount, CreationFailures: p.creationFailures, CleanupFailures: p.cleanupFailures, CreationTotalMS: p.creationTotalMS}
	if len(p.waiters) > 0 {
		s.OldestQueueMS = ms(time.Since(p.waiters[0].enteredAt))
		s.Inventory.HeadWaitReason = "dispatch_ready"
		if p.executing >= p.config.Concurrency {
			s.Inventory.HeadWaitReason = "slot_wait"
			if len(p.ready) == 0 {
				s.Inventory.HeadWaitReason = "both_wait"
			}
		} else if len(p.ready) == 0 {
			s.Inventory.HeadWaitReason = "inventory_wait"
		}
	}
	for _, state := range p.items {
		switch state {
		case poolReady:
			s.Idle++
		case poolLeased:
			s.Leased++
		case poolRunning:
			s.Running++
		case poolDestroying:
			s.Destroying++
		}
	}
	return s
}

// Background preparation is opportunistic. Demand creation still uses the
// shared bounded factory, while idle refill yields whenever foreground work
// fills execution slots or queues. Already reserved creation is not cancelled.
func (p *warmPool) refillBlockerLocked() string {
	if p.pressurePaused {
		return "qos_pressure"
	}
	if len(p.waiters) > 0 {
		return "foreground_queued"
	}
	if p.executing >= p.config.Concurrency {
		return "execution_saturated"
	}
	if len(p.items)+p.creating >= p.config.MaxSandboxes {
		return "total_capacity"
	}
	if len(p.ready)+p.creating >= p.inventory.Target {
		return "inventory_satisfied"
	}
	if p.creating >= p.config.CreateConcurrency {
		return "creation_capacity"
	}
	return ""
}
