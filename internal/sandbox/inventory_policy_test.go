package sandbox

import (
	"context"
	"testing"
	"time"
)

func TestRecommendationMaxMatchesWindowScan(t *testing.T) {
	var q recommendationMax
	var points []inventoryRecommendation
	now := time.Now()
	window := 7 * time.Second
	for n := 0; n < 256; n++ {
		now = now.Add(time.Duration(n%3) * time.Second)
		value := 1 + (n*n+3*n)%8
		points = append(points, inventoryRecommendation{now, value})
		want := 0
		for _, point := range points {
			if point.at.After(now.Add(-window)) {
				want = max(want, point.target)
			}
		}
		if got := q.record(now, value, window); got != want {
			t.Fatalf("step %d: got %d want %d", n, got, want)
		}
		if len(q.entries) > 8 {
			t.Fatal("unbounded recommendation history")
		}
		for i := 1; i < len(q.entries); i++ {
			if q.entries[i].at.Before(q.entries[i-1].at) || q.entries[i].target >= q.entries[i-1].target {
				t.Fatal("invalid monotonic queue")
			}
		}
	}
}

func policyFixture(t *testing.T, target, ready int) (*warmPool, *time.Time) {
	t.Helper()
	c := dynamicConfig()
	clock := time.Now()
	p := newWarmPool(c, nil, nil)
	p.now = func() time.Time { return clock }
	p.initialized, p.inventory.Target, p.inventory.LastChange = true, target, clock
	for n := 0; n < ready; n++ {
		x := &executionState{}
		p.ready = append(p.ready, x)
		p.items[x] = poolReady
	}
	p.resetInventoryLocked("window_pending")
	t.Cleanup(p.cancel)
	return p, &clock
}

func TestInventoryCounterexamplesNoLongerExpand(t *testing.T) {
	t.Run("slot_queue_with_ready_stock", func(t *testing.T) {
		p, clock := policyFixture(t, 2, 2)
		p.executing = 1
		w := &poolWaiter{}
		p.waiters = []*poolWaiter{w}
		for tick := 0; tick < 10; tick++ {
			*clock = clock.Add(inventoryObservationInterval)
			p.observeWaiterLocked(w)
			p.adjustInventoryLocked(*clock)
		}
		if p.inventory.ScaleUps != 0 || p.inventory.InventoryStalls != 0 || p.inventory.SlotWaitRequests != 1 {
			t.Fatalf("slot queue misclassified: %+v", p.inventory)
		}
	})
	for _, initial := range []int{1, 3} {
		t.Run(string(rune('0'+initial))+"_sparse_requests", func(t *testing.T) {
			p, clock := policyFixture(t, initial, initial)
			// Every ten seconds is less than the thirty-second downscale window.
			p.config.WarmDownscaleWindow = 30 * time.Second
			p.policy.config = p.config
			p.resetInventoryLocked("window_pending")
			refill := func() {
				for len(p.ready) < p.inventory.Target {
					x := &executionState{}
					p.items[x] = poolReady
					p.ready = append(p.ready, x)
				}
				p.signalLocked()
			}
			for tick := 1; tick <= 1000; tick++ {
				*clock = clock.Add(inventoryObservationInterval)
				if tick%100 == 0 {
					x, err := p.acquire(context.Background(), 1)
					if err != nil {
						t.Fatal(err)
					}
					p.release(x, true)
					refill()
				}
				p.adjustInventoryLocked(*clock)
				for x := p.reserveSurplusLocked(); x != nil; x = p.reserveSurplusLocked() {
					delete(p.items, x)
				}
				refill()
			}
			if p.inventory.Target != 1 || p.inventory.ScaleUps != 0 || p.inventory.InventoryStalls != 0 || p.telemetry.QueueEntered != 0 {
				t.Fatalf("sparse traffic grew or blocked: %+v", p.inventory)
			}
		})
	}
}

func TestInventoryStallTransitionAndDeduplication(t *testing.T) {
	p, clock := policyFixture(t, 1, 0)
	p.executing = 1
	w, tail := &poolWaiter{}, &poolWaiter{}
	p.waiters = []*poolWaiter{w, tail}
	p.observeWaiterLocked(tail)
	p.observeWaiterLocked(w)
	p.observeWaiterLocked(w)
	if p.inventory.BothWaitRequests != 1 || p.inventory.InventoryStalls != 0 {
		t.Fatal("both wait or FIFO misclassified")
	}
	p.executing = 0
	p.observeWaiterLocked(w)
	for n := 0; n < 10; n++ {
		p.observeWaiterLocked(w)
	}
	*clock = clock.Add(inventoryObservationInterval)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 2 || p.inventory.InventoryStalls != 1 || p.inventory.ScaleUps != 1 {
		t.Fatalf("actual stall not applied once: %+v", p.inventory)
	}
	for n := 0; n < 10; n++ {
		*clock = clock.Add(inventoryObservationInterval)
		p.observeWaiterLocked(w)
		p.adjustInventoryLocked(*clock)
	}
	if p.inventory.Target != 2 || p.inventory.ScaleUps != 1 {
		t.Fatal("same waiter drove repeated scale-up")
	}
	p.waiters = p.waiters[1:]
	p.observeWaiterLocked(tail)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 3 || p.inventory.InventoryStalls != 2 {
		t.Fatal("new head stall was lost")
	}
}

func TestInventoryCapacityAndPressureDoNotReplayOldStalls(t *testing.T) {
	p, clock := policyFixture(t, 1, 0)
	for n := 0; n < p.config.MaxSandboxes; n++ {
		p.items[&executionState{}] = poolDestroying
	}
	w := &poolWaiter{}
	p.waiters = []*poolWaiter{w}
	*clock = clock.Add(inventoryObservationInterval)
	p.observeWaiterLocked(w)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 1 || p.inventory.LimitedBy != "capacity_limited" {
		t.Fatalf("ignored total capacity: %+v", p.inventory)
	}
	clear(p.items)
	*clock = clock.Add(inventoryObservationInterval)
	p.observeWaiterLocked(w)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 1 {
		t.Fatal("replayed capacity-blocked stall")
	}
	p.setPressure(true)
	p.policy.recordStall(*clock, 0, 1)
	*clock = clock.Add(time.Second)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 1 {
		t.Fatal("acted during pressure")
	}
	p.setPressure(false)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 1 || p.policy.pendingStall {
		t.Fatal("replayed pre-recovery demand")
	}
}

func TestInventoryCooldownCoalescesEventsAndHonorsMaximum(t *testing.T) {
	p, clock := policyFixture(t, 1, 0)
	for n := 0; n < 2; n++ {
		w := &poolWaiter{}
		p.waiters = []*poolWaiter{w}
		p.observeWaiterLocked(w)
		p.adjustInventoryLocked(*clock)
	}
	if p.inventory.Target != 1 || p.inventory.LimitedBy != "action_cooldown" {
		t.Fatal("cooldown bypassed")
	}
	*clock = clock.Add(inventoryObservationInterval)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 2 || p.inventory.ScaleUps != 1 || p.inventory.InventoryStalls != 2 {
		t.Fatalf("events did not coalesce: %+v", p.inventory)
	}
	*clock = clock.Add(inventoryObservationInterval)
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 2 {
		t.Fatal("coalesced event replayed")
	}
	for n := 0; n < 4; n++ {
		p.waiters = []*poolWaiter{{}}
		p.observeWaiterLocked(p.waiters[0])
		p.adjustInventoryLocked(*clock)
		*clock = clock.Add(inventoryObservationInterval)
	}
	if p.inventory.Target != p.config.WarmMaxSize || p.inventory.ScaleUps != 2 || p.inventory.LimitedBy != "maximum_inventory" {
		t.Fatalf("maximum escaped: %+v", p.inventory)
	}
}

func TestInventoryStabilizationGapAndBusyRetirement(t *testing.T) {
	p, clock := policyFixture(t, 3, 3)
	p.executing = 1 // in-flight work does not make unused READY valuable
	for tick := 1; tick <= 2; tick++ {
		*clock = clock.Add(inventoryObservationInterval)
		p.adjustInventoryLocked(*clock)
	}
	if p.inventory.Target != 3 || p.inventory.RawTarget != 2 || p.inventory.LimitedBy != "downscale_stabilized" {
		t.Fatalf("missing downscale protection: %+v", p.inventory)
	}
	*clock = clock.Add(time.Second) // no valid observations during this gap
	p.adjustInventoryLocked(*clock)
	if p.inventory.Target != 3 || p.inventory.RawTarget != 3 {
		t.Fatal("sample gap treated as idle")
	}
	for tick := 1; tick <= 3; tick++ {
		*clock = clock.Add(inventoryObservationInterval)
		p.adjustInventoryLocked(*clock)
	}
	if p.inventory.Target != 2 {
		t.Fatalf("held target fed back into history: %+v", p.inventory)
	}
	if x := p.reserveSurplusLocked(); x == nil || p.items[x] != poolDestroying {
		t.Fatal("unused READY could not retire during active execution")
	}
	// But runnable FIFO work gets the remaining READY before retirement.
	p.executing = 0
	p.waiters = []*poolWaiter{{}}
	p.inventory.Target = 1
	if p.reserveSurplusLocked() != nil {
		t.Fatal("retirement bypassed runnable FIFO")
	}
}
