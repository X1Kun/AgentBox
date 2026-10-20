package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func dynamicConfig() Config {
	c := poolConfig()
	c.DynamicWarmPool = true
	c.WarmMinSize, c.WarmMaxSize, c.MaxSandboxes = 1, 3, 4
	c.WarmAdjustInterval, c.WarmDownscaleWindow = 100*time.Millisecond, 300*time.Millisecond
	c.QueueTimeout = 5 * time.Second
	return c
}

func TestDynamicInventoryConfiguration(t *testing.T) {
	c := Config{Mode: "warm", Concurrency: 1, DynamicWarmPool: true}
	if err := normalizePoolConfig(&c); err != nil {
		t.Fatal(err)
	}
	if c.WarmMinSize != 1 || c.WarmMaxSize != c.WarmPoolSize || c.WarmAdjustInterval != time.Second || c.WarmDownscaleWindow != 30*time.Second {
		t.Fatalf("defaults: %+v", c)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Mode = "cold" },
		func(c *Config) { c.WarmMinSize = -1 },
		func(c *Config) { c.WarmMinSize = 4 },
		func(c *Config) { c.WarmMaxSize = 5 },
		func(c *Config) { c.WarmMaxSize = -1 },
		func(c *Config) { c.WarmAdjustInterval = time.Millisecond },
		func(c *Config) { c.WarmAdjustInterval = 2 * time.Minute },
		func(c *Config) { c.WarmDownscaleWindow = time.Millisecond },
		func(c *Config) { c.WarmDownscaleWindow = 2 * time.Hour },
	} {
		c := dynamicConfig()
		change(&c)
		if err := normalizePoolConfig(&c); err == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
	// The dynamic maximum, rather than the ignored static target, bounds workers.
	c = dynamicConfig()
	c.WarmPoolSize, c.CreateConcurrency = 1, 2
	if err := normalizePoolConfig(&c); err != nil {
		t.Fatal(err)
	}
}

func TestShrinkReservationShutdownAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			c := dynamicConfig()
			began, release := make(chan *executionState, 1), make(chan struct{})
			var once sync.Once
			p := newWarmPool(c, func(context.Context) (*executionState, error) {
				t.Error("unexpected refill")
				return nil, errors.New("unexpected refill")
			}, func(x *executionState) error {
				began <- x
				<-release
				if fail {
					return errors.New("shrink delete failed")
				}
				return nil
			})
			t.Cleanup(func() { once.Do(func() { close(release) }); p.stop() })
			// Begin with a settled surplus, then wait for downscale stabilization.
			one, two := &executionState{id: "one"}, &executionState{id: "two"}
			p.ready = []*executionState{one, two}
			p.items[one], p.items[two], p.initialized = poolReady, poolReady, true
			p.inventory.Target = 2
			p.resetInventoryLocked("window_pending")
			if err := p.start(context.Background()); err != nil {
				t.Fatal(err)
			}
			var victim *executionState
			select {
			case victim = <-began:
			case <-time.After(3 * time.Second):
				t.Fatal("shrink did not start")
			}
			if victim != two {
				t.Fatal("unexpected shrink victim")
			}
			s := p.status()
			if s.Total != 2 || s.Destroying != 1 || s.Idle != 1 {
				t.Fatalf("lost capacity before cleanup: %+v", s)
			}
			// A leased instance can never be the already reserved shrink victim.
			p.mu.Lock()
			x := p.takeLocked()
			p.setPressureLocked(true) // pause before a replacement can reserve capacity
			p.mu.Unlock()
			if x != one {
				t.Fatal("shrink victim was assigned")
			}
			if fail {
				once.Do(func() { close(release) })
				waitPool(t, p, func(s poolSnapshot) bool { return s.CleanupFailures == 1 })
				p.setPressure(false) // failure, rather than a pressure gate, must win
				if _, err := p.acquire(context.Background(), 1); !errors.Is(err, ErrUnhealthy) {
					t.Fatalf("live pool admitted after shrink failure: %v", err)
				}
				p.stop()
			} else {
				done := make(chan struct{})
				go func() { p.stop(); close(done) }()
				select {
				case <-done:
					t.Fatal("stop failed to join shrink cleanup")
				case <-time.After(10 * time.Millisecond):
				}
				once.Do(func() { close(release) })
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("stop did not finish")
				}
			}
			p.release(x, true) // simulate the caller's separately confirmed cleanup
			s = p.status()
			if fail {
				if s.Ready || s.Total != 1 || s.Destroying != 1 || s.CleanupFailures != 1 || s.Inventory.ShrinkFailures != 1 {
					t.Fatalf("failed shrink lost ownership: %+v", s)
				}
				if _, err := p.acquire(context.Background(), 1); !errors.Is(err, ErrUnhealthy) {
					t.Fatalf("admitted after failure: %v", err)
				}
				if err := p.drain(); err == nil {
					t.Fatal("drain hid shrink failure")
				}
			} else if s.Total != 0 || s.Inventory.ShrinkSucceeded != 1 {
				t.Fatalf("successful cleanup: %+v", s)
			}
		})
	}
}
