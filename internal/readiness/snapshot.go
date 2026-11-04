// Package readiness holds fresh backend observations; probes never call a backend.
package readiness

import (
	"context"
	"sync/atomic"
	"time"
)

const Interval = 2 * time.Second
const MaxAge = 5 * time.Second

type result struct {
	healthy bool
	at      time.Time
}
type Snapshot struct {
	last    atomic.Pointer[result]
	started atomic.Bool
}

func (s *Snapshot) Store(healthy bool, at time.Time) { s.last.Store(&result{healthy, at}) }
func (s *Snapshot) Ready(now time.Time) bool {
	r := s.last.Load()
	return r != nil && r.healthy && now.Sub(r.at) >= 0 && now.Sub(r.at) < MaxAge
}

// Run permits one serial monitor. Shutdown invalidates the snapshot even if the
// HTTP listener remains alive, and a late backend success cannot revive it.
func (s *Snapshot) Run(ctx context.Context, check func(context.Context), stopped func()) {
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		s.last.Store(nil)
		if stopped != nil {
			stopped()
		}
	}()
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
