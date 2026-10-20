package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDynamicPoolRealStallFIFOAndRecovery(t *testing.T) {
	c := dynamicConfig()
	c.WarmMaxSize = 2
	var next atomic.Int64
	began, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var destroyed sync.Map
	p := newWarmPool(c, func(ctx context.Context) (*executionState, error) {
		n := next.Add(1)
		if n == 2 {
			close(began)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &executionState{id: fmt.Sprint(n)}, nil
	}, func(x *executionState) error {
		if _, exists := destroyed.LoadOrStore(x, true); exists {
			return errors.New("duplicate destruction")
		}
		return nil
	})
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		p.stop()
		if err := p.drain(); err != nil {
			t.Error(err)
		}
	})
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-began:
	case <-time.After(time.Second):
		t.Fatal("replacement did not start")
	}
	type lease struct {
		x   *executionState
		err error
	}
	a, b := make(chan lease, 1), make(chan lease, 1)
	go func() { x, err := p.acquire(context.Background(), 10); a <- lease{x, err} }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 1 && s.Inventory.BothWaitRequests == 1 })
	go func() { x, err := p.acquire(context.Background(), 20); b <- lease{x, err} }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 2 })
	if s := p.status(); s.Target != 1 || s.Inventory.InventoryStalls != 0 {
		t.Fatal("expanded while both resources unavailable")
	}
	complete := func(x *executionState) {
		t.Helper()
		p.mark(x, poolDestroying)
		if err := p.destroy(x); err != nil {
			t.Fatal(err)
		}
		p.release(x, true)
	}
	complete(first)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Target == 2 && s.Inventory.InventoryStalls == 1 })
	once.Do(func() { close(release) })
	second := <-a
	if second.err != nil || second.x == first {
		t.Fatalf("first FIFO waiter: %+v", second)
	}
	select {
	case <-b:
		t.Fatal("FIFO bypass")
	default:
	}
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle > 0 && s.Creating == 0 })
	complete(second.x)
	third := <-b
	if third.err != nil || third.x == first || third.x == second.x {
		t.Fatalf("second FIFO waiter: %+v", third)
	}
	complete(third.x)
	s := waitPool(t, p, func(s poolSnapshot) bool {
		return s.Target == 1 && s.Total == 1 && s.Idle == 1 && s.Inventory.ShrinkSucceeded == 1
	})
	if s.QueuedBytes != 0 || s.Telemetry.QueueAssigned.Count != 2 || s.Inventory.ScaleUps != 1 || s.Inventory.ScaleDowns != 1 {
		t.Fatalf("accounting: %+v", s)
	}
	data, err := json.Marshal((&Engine{pool: p}).Status())
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	i := decoded["inventory"].(map[string]any)
	if i["target"] != decoded["target"] || i["inventory_stalled_requests"] != float64(1) || i["shrink_succeeded"] != float64(1) || i["downscale_window_ms"] != float64(300) {
		t.Fatalf("public report: %s", data)
	}
}

func TestInventoryCanceledAndExpiredWaiters(t *testing.T) {
	for _, cancelRequest := range []bool{true, false} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			c := dynamicConfig()
			c.QueueTimeout = 30 * time.Millisecond
			if cancelRequest {
				c.QueueTimeout = time.Second
			}
			p := newWarmPool(c, nil, nil)
			p.initialized = true
			t.Cleanup(p.cancel)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := p.acquire(ctx, 10); done <- err }()
			waitPool(t, p, func(s poolSnapshot) bool { return s.Inventory.InventoryStalls == 1 })
			for n := 0; n < 5; n++ {
				p.mu.Lock()
				p.signalLocked()
				p.mu.Unlock()
			}
			if cancelRequest {
				cancel()
			}
			select {
			case err := <-done:
				if cancelRequest && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if !cancelRequest && !errors.Is(err, ErrBusy) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiter did not leave")
			}
			if s := p.status(); s.Inventory.InventoryStalls != 1 || s.Queued != 0 || s.QueuedBytes != 0 {
				t.Fatalf("duplicate event or retained waiter: %+v", s)
			}
			ctx2, cancel2 := context.WithCancel(context.Background())
			cancel2()
			if _, err := p.acquire(ctx2, 1); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if p.status().Inventory.InventoryStalls != 1 {
				t.Fatal("pre-canceled request counted as inventory demand")
			}
		})
	}
}

func TestDynamicPausePreservesCreationWithoutInventingDemand(t *testing.T) {
	c := dynamicConfig()
	c.WarmMinSize = 2
	var calls atomic.Int32
	began, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	p := newWarmPool(c, func(context.Context) (*executionState, error) {
		if calls.Add(1) == 2 {
			close(began)
			<-release
		}
		return &executionState{}, nil
	}, func(*executionState) error { return nil })
	t.Cleanup(func() { once.Do(func() { close(release) }); p.stop(); _ = p.drain() })
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-began:
	case <-time.After(time.Second):
		t.Fatal("creation did not start")
	}
	p.setPressure(true)
	once.Do(func() { close(release) })
	waitPool(t, p, func(s poolSnapshot) bool { return s.Creating == 0 && s.Idle == 2 })
	if s := p.status(); s.Target != 2 || calls.Load() != 2 {
		t.Fatalf("paused controller acted: %+v", s)
	}
	p.setPressure(false)
	x, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	p.release(x, true)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 2 && s.Creating == 0 })
	if s := p.status(); s.Target != 2 || s.Inventory.InventoryStalls != 0 {
		t.Fatal("normal post-recovery consumption invented demand")
	}
}
