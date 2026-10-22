package sandbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestColdWarmShareCreationReservationsAndFIFO(t *testing.T) {
	for _, mode := range []string{"cold", "warm"} {
		t.Run(mode, func(t *testing.T) {
			c := poolConfig()
			c.Mode = mode
			c.Concurrency = 2
			c.MaxSandboxes = 3
			began, release := make(chan struct{}, 4), make(chan struct{}, 4)
			var inflight, maximum atomic.Int32
			p := newWarmPool(c, func(ctx context.Context) (*executionState, error) {
				n := inflight.Add(1)
				if n > maximum.Load() {
					maximum.Store(n)
				}
				began <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				inflight.Add(-1)
				return &executionState{}, ctx.Err()
			}, func(*executionState) error { return nil })
			p.initialized = true
			t.Cleanup(p.cancel)
			results := make(chan *executionState, 2)
			go func() { instance, _ := p.acquire(context.Background(), 10); results <- instance }()
			<-began
			if s := p.status(); s.Creating != 1 || s.Total != 1 || s.ExecutionInFlight != 1 {
				t.Fatalf("unreserved creation: %+v", s)
			}
			go func() { instance, _ := p.acquire(context.Background(), 10); results <- instance }()
			waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 1 })
			release <- struct{}{}
			first := <-results
			<-began
			if s := p.status(); s.Queued != 0 || s.QueuedBytes != 0 || s.Telemetry.QueueAssigned.Count != 1 || s.Total != 2 || s.ExecutionInFlight != 2 {
				t.Fatalf("FIFO dispatch/capacity: %+v", s)
			}
			release <- struct{}{}
			second := <-results
			p.release(first, true)
			p.release(second, true)
			if maximum.Load() != 1 || p.status().Total != 0 {
				t.Fatal("creation cap bypassed or capacity leaked")
			}
		})
	}
}

func TestCanceledOnDemandCreationRetainsPartialOwnership(t *testing.T) {
	c := poolConfig()
	c.Mode = "cold"
	partial := &executionState{}
	entered := make(chan struct{})
	p := newWarmPool(c, func(ctx context.Context) (*executionState, error) {
		close(entered)
		<-ctx.Done()
		return partial, ctx.Err()
	}, nil)
	defer p.cancel()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		instance, err := p.acquire(ctx, 1)
		if instance != partial {
			done <- errors.New("lost partial ownership")
			return
		}
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := p.status(); s.Total != 1 || s.ExecutionInFlight != 1 || s.Creating != 0 {
		t.Fatalf("released before cleanup: %+v", s)
	}
	p.release(partial, false)
	if p.healthy() || p.status().Total != 1 {
		t.Fatal("uncertain cleanup must retain capacity and stop admission")
	}
}

func TestAdmissionTimeoutDoesNotCancelReservedCreation(t *testing.T) {
	c := poolConfig()
	c.Mode = "cold"
	c.QueueTimeout = time.Millisecond
	p := newWarmPool(c, func(ctx context.Context) (*executionState, error) {
		time.Sleep(10 * time.Millisecond)
		return &executionState{}, ctx.Err()
	}, nil)
	defer p.cancel()
	instance, err := p.acquire(context.Background(), 1)
	if err != nil || instance == nil {
		t.Fatalf("creation was charged to queue timeout: %v", err)
	}
	p.release(instance, true)
}

func TestBackgroundRefillYieldsToForegroundAndResumes(t *testing.T) {
	c := poolConfig()
	c.WarmPoolSize = 1
	c.MaxSandboxes = 3
	var calls atomic.Int32
	p := newWarmPool(c, func(context.Context) (*executionState, error) { calls.Add(1); return &executionState{}, nil }, func(*executionState) error { return nil })
	defer func() {
		p.stop()
		if err := p.drain(); err != nil {
			t.Error(err)
		}
	}()
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if s := p.status(); calls.Load() != 1 || s.Creating != 0 || s.RefillBlockedBy != "execution_saturated" {
		t.Fatalf("refill competed with foreground: %+v", s)
	}
	p.release(first, true)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 1 && s.Creating == 0 })
	if calls.Load() != 2 {
		t.Fatal("refill did not resume after cleanup")
	}
}

func TestConfiguredUpperBoundsRemainWithinInstanceBudget(t *testing.T) {
	c := Config{Mode: "warm", Concurrency: 8, CreateConcurrency: 4, MaxSandboxes: 8, WarmPoolSize: 1}
	if err := normalizePoolConfig(&c); err != nil {
		t.Fatal(err)
	}
	c.CreateConcurrency = 5
	if err := normalizePoolConfig(&c); err == nil {
		t.Fatal("unbounded creation allowed")
	}
	c.CreateConcurrency = 4
	c.MaxSandboxes = 6
	if err := normalizePoolConfig(&c); err == nil {
		t.Fatal("execution budget exceeds instance budget")
	}
}

func TestIdleRefillYieldsWithoutBlockingOnDemandCreation(t *testing.T) {
	c := poolConfig()
	c.Mode, c.WarmRefillPolicy = "warm", "idle"
	c.Concurrency, c.MaxSandboxes, c.WarmPoolSize = 2, 4, 1
	var calls atomic.Int32
	p := newWarmPool(c, func(context.Context) (*executionState, error) {
		calls.Add(1)
		return &executionState{}, nil
	}, func(*executionState) error { return nil })
	defer func() { p.stop(); _ = p.drain() }()
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if s := p.status(); s.RefillBlockedBy != "foreground_active" || s.ExecutionInFlight != 1 || s.Creating != 0 {
		t.Fatalf("refill should yield before execution capacity is full: %+v", s)
	}
	second, err := p.acquire(context.Background(), 1)
	if err != nil || second == first || calls.Load() != 2 {
		t.Fatalf("idle policy blocked or reused demand capacity: %v", err)
	}
	p.release(first, true)
	if s := p.status(); s.RefillBlockedBy != "foreground_active" || s.Idle != 0 || s.Creating != 0 {
		t.Fatalf("refill resumed while another execution still held a slot: %+v", s)
	}
	p.release(second, true)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 1 && s.Creating == 0 })
	if calls.Load() != 3 {
		t.Fatal("idle refill did not resume after foreground cleanup")
	}
}
