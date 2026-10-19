package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func poolConfig() Config {
	return Config{Mode: "warm", WarmPoolSize: 2, MaxSandboxes: 3, CreateConcurrency: 1, Concurrency: 1, QueueSize: 2, QueueBytes: 100, QueueTimeout: time.Second}
}

func waitPool(t *testing.T, p *warmPool, predicate func(poolSnapshot) bool) poolSnapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := p.status()
		if predicate(status) {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pool state did not converge: %+v", p.status())
	return poolSnapshot{}
}

func testPool(t *testing.T, c Config) *warmPool {
	t.Helper()
	var id atomic.Int64
	p := newWarmPool(c, func(context.Context) (*executionState, error) { return &executionState{id: fmt.Sprint(id.Add(1))}, nil }, func(*executionState) error { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.stop()
		knownCleanupFailures := p.status().CleanupFailures
		if err := p.drain(); err != nil && knownCleanupFailures == 0 {
			t.Error(err)
		}
	})
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == c.WarmPoolSize })
	return p
}

func TestWarmPoolFIFOAndSingleUse(t *testing.T) {
	p := testPool(t, poolConfig())
	first, err := p.acquire(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		instance *executionState
		err      error
	}
	a, b := make(chan answer, 1), make(chan answer, 1)
	go func() { x, err := p.acquire(context.Background(), 20); a <- answer{x, err} }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 1 })
	go func() { x, err := p.acquire(context.Background(), 30); b <- answer{x, err} }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 2 && s.QueuedBytes == 50 })
	if _, err := p.acquire(context.Background(), 1); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue capacity: %v", err)
	}
	p.mark(first, poolDestroying)
	// A destroying instance still holds both total capacity and execution permit.
	time.Sleep(10 * time.Millisecond)
	select {
	case <-a:
		t.Fatal("lease released before cleanup")
	default:
	}
	p.release(first, true)
	second := <-a
	if second.err != nil || second.instance == first {
		t.Fatalf("first waiter: %+v", second)
	}
	select {
	case <-b:
		t.Fatal("FIFO execution permit bypassed")
	default:
	}
	p.release(second.instance, true)
	third := <-b
	if third.err != nil || third.instance == first || third.instance == second.instance {
		t.Fatalf("instance reused: %+v", third)
	}
	p.release(third.instance, true)
	status := waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 0 && s.QueuedBytes == 0 && s.Idle == 2 })
	if status.Telemetry.QueueEntered != 2 || status.Telemetry.QueueAssigned.Count != 2 || status.ExecutionInFlight != 0 {
		t.Fatalf("assigned queue accounting: %+v", status)
	}
}

func TestWarmPoolCancellationExpiryAndByteBound(t *testing.T) {
	c := poolConfig()
	c.QueueTimeout = 100 * time.Millisecond
	p := testPool(t, c)
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := p.acquire(ctx, 80); done <- err }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.QueuedBytes == 80 })
	if _, err := p.acquire(context.Background(), 21); !errors.Is(err, ErrBusy) {
		t.Fatalf("byte bound: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 0 && s.QueuedBytes == 0 })
	begin := time.Now()
	if _, err := p.acquire(context.Background(), 99); !errors.Is(err, ErrBusy) {
		t.Fatalf("expiry: %v", err)
	}
	if time.Since(begin) < c.QueueTimeout {
		t.Fatal("queue expired early")
	}
	if s := p.status(); s.Queued != 0 || s.QueuedBytes != 0 {
		t.Fatalf("expired payload retained: %+v", s)
	}
	p.release(first, true)
}

func TestWarmPoolTotalBoundIncludesCreationAndDestruction(t *testing.T) {
	c := poolConfig()
	c.WarmPoolSize = 2
	c.MaxSandboxes = 2
	c.CreateConcurrency = 2
	var actual, peak atomic.Int64
	p := newWarmPool(c, func(context.Context) (*executionState, error) {
		n := actual.Add(1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		return &executionState{}, nil
	}, func(*executionState) error { actual.Add(-1); return nil })
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.stop(); _ = p.drain() })
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 2 })
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	p.mark(first, poolDestroying)
	time.Sleep(25 * time.Millisecond)
	if s := p.status(); s.Total != 2 || s.Destroying != 1 || s.Creating != 0 {
		t.Fatalf("capacity escaped: %+v", s)
	}
	if peak.Load() > 2 {
		t.Fatal("external resources exceeded cap")
	}
	actual.Add(-1) // kernel-confirmed cleanup, then capacity release
	p.release(first, true)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 2 })
	if peak.Load() > 2 {
		t.Fatal("refill exceeded cap")
	}
}

func TestWarmPoolCleanupFailureStopsAdmissionAndRefill(t *testing.T) {
	p := testPool(t, poolConfig())
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	// Wait for refill to finish. Total also includes in-flight creation, which
	// is correctly destroyed if the pool becomes unhealthy before publication.
	waitPool(t, p, func(s poolSnapshot) bool { return s.Total == 3 && s.Idle == 2 && s.Creating == 0 })
	p.mark(first, poolDestroying)
	p.release(first, false)
	if p.healthy() {
		t.Fatal("cleanup failure remained ready")
	}
	if _, err := p.acquire(context.Background(), 1); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("admission: %v", err)
	}
	s := p.status()
	if s.Total != 3 || s.Destroying != 1 || s.CleanupFailures != 1 {
		t.Fatalf("failed destruction lost ownership: %+v", s)
	}
	p.stop()
	if err := p.drain(); err == nil {
		t.Fatal("shutdown hid retained failed cleanup")
	}
}

func TestWarmPoolStopsQueuedAndPartialCreation(t *testing.T) {
	c := poolConfig()
	began := make(chan struct{})
	var destroyed atomic.Int64
	p := newWarmPool(c, func(ctx context.Context) (*executionState, error) {
		close(began)
		<-ctx.Done()
		return &executionState{}, ctx.Err()
	}, func(*executionState) error { destroyed.Add(1); return nil })
	done := make(chan error, 1)
	go func() { done <- p.start(context.Background()) }()
	<-began
	p.stop()
	if err := <-done; err == nil {
		t.Fatal("startup survived stop")
	}
	if destroyed.Load() != 1 || p.status().Total != 0 {
		t.Fatalf("partial creation not destroyed: %+v", p.status())
	}
	if telemetry := p.status().Telemetry; telemetry.CreationAttempts != 1 || telemetry.CreationAborted.Count != 1 || telemetry.CreationFailed.Count != 0 {
		t.Fatalf("stopped creation accounting: %+v", telemetry)
	}
	p2 := testPool(t, c)
	first, err := p2.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, err := p2.acquire(context.Background(), 1); done <- err }()
	waitPool(t, p2, func(s poolSnapshot) bool { return s.Queued == 1 })
	p2.stop()
	if err := <-done; !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("queued stop: %v", err)
	}
	if telemetry := p2.status().Telemetry; telemetry.QueueEntered != 1 || telemetry.QueueUnavailable.Count != 1 {
		t.Fatalf("unavailable queue accounting: %+v", telemetry)
	}
	p2.release(first, true)
}

func TestWarmPoolCreationFailuresAreBounded(t *testing.T) {
	c := poolConfig()
	var attempts atomic.Int64
	p := newWarmPool(c, func(context.Context) (*executionState, error) {
		attempts.Add(1)
		return nil, errors.New("injected create failure")
	}, func(*executionState) error { t.Error("unexpected destroy"); return nil })
	defer p.stop()
	if err := p.start(context.Background()); err == nil {
		t.Fatal("failed initialization reported READY")
	}
	if attempts.Load() != 3 || p.status().CreationFailures != 3 || p.status().Total != 0 {
		t.Fatalf("unbounded failure: %+v", p.status())
	}
}

func TestPoolConfiguration(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Mode = "automatic" }, func(c *Config) { c.MaxSandboxes = 9 }, func(c *Config) { c.WarmPoolSize = 7; c.MaxSandboxes = 6 },
		func(c *Config) { c.CreateConcurrency = 3 }, func(c *Config) { c.QueueBytes = -1 }, func(c *Config) { c.QueueSize = 129 }, func(c *Config) { c.QueueTimeout = 6 * time.Second },
	} {
		c := poolConfig()
		mutate(&c)
		if err := normalizePoolConfig(&c); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func TestDrainUsesFourWorkersAndRetainsOnlyFailedInstances(t *testing.T) {
	c := poolConfig()
	c.WarmPoolSize = 8
	c.MaxSandboxes = 8
	c.CreateConcurrency = 2
	var next atomic.Int64
	started := make(chan string, 8)
	release := make(chan struct{}, 8)
	failureOne, failureFive := errors.New("delete 1 failed"), errors.New("delete 5 failed")
	p := newWarmPool(c, func(context.Context) (*executionState, error) {
		return &executionState{id: fmt.Sprint(next.Add(1))}, nil
	}, func(instance *executionState) error {
		started <- instance.id
		<-release
		switch instance.id {
		case "1":
			return failureOne
		case "5":
			return failureFive
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.start(ctx); err != nil {
		t.Fatal(err)
	}
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 8 })
	p.stop()
	done := make(chan error, 1)
	go func() { done <- p.drain() }()
	defer close(release)
	seen := make(map[string]bool)
	for batch := 0; batch < 2; batch++ {
		for i := 0; i < 4; i++ {
			select {
			case id := <-started:
				if seen[id] {
					t.Fatalf("duplicate cleanup: %s", id)
				}
				seen[id] = true
			case <-ctx.Done():
				t.Fatal("cleanup did not fill four independent worker slots")
			}
		}
		if batch == 0 {
			if s := p.status(); s.Total != 8 || s.Destroying != 8 || s.Idle != 0 {
				t.Fatalf("capacity released before destruction: %+v", s)
			}
		}
		select {
		case id := <-started:
			t.Fatalf("cleanup exceeded concurrency cap: %s", id)
		case <-time.After(20 * time.Millisecond):
		}
		for i := 0; i < 4; i++ {
			release <- struct{}{}
		}
	}
	select {
	case err := <-done:
		if !errors.Is(err, failureOne) || !errors.Is(err, failureFive) {
			t.Fatalf("cleanup errors lost: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("cleanup did not finish")
	}
	if s := p.status(); s.Total != 2 || s.Destroying != 2 || s.CleanupFailures != 2 {
		t.Fatalf("failed ownership not retained: %+v", s)
	}
	if next.Load() != 8 {
		t.Fatal("shutdown refilled inventory")
	}
}
