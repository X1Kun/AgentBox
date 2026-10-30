package sessionapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readinessBackend struct {
	backend
	ready  func(context.Context) error
	checks atomic.Int32
}

func (b *readinessBackend) Ready(ctx context.Context) error {
	b.checks.Add(1)
	return b.ready(ctx)
}

func TestReadinessSnapshotFreshnessFailureAndRecovery(t *testing.T) {
	b := &backend{}
	h := handler(t, b, 1)
	now := time.Now()
	h.now = func() time.Time { return now }
	status := func(want int) {
		t.Helper()
		if got := call(h, "GET", "/readyz", "", "").Code; got != want {
			t.Fatalf("readiness status = %d, want %d", got, want)
		}
	}
	status(503) // No completed check yet.
	h.checkReadiness(context.Background())
	status(200)
	now = now.Add(readinessMaxAge)
	status(503)
	b.err = errors.New("backend unavailable")
	h.checkReadiness(context.Background())
	status(503)
	b.err = nil
	h.checkReadiness(context.Background())
	status(200)
	// A stalled refresh must not preserve an old success indefinitely.
	now = now.Add(readinessMaxAge)
	status(503)
}

func TestProbeVolumeDoesNotAmplifyBackendTraffic(t *testing.T) {
	b := &readinessBackend{ready: func(context.Context) error { return nil }}
	h := handler(t, b, 1)
	h.checkReadiness(context.Background())
	h.permits <- struct{}{}
	h.operations <- struct{}{}
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Go(func() {
			if got := call(h, "GET", "/readyz", "", "").Code; got != 200 {
				t.Errorf("cached probe failed: %d", got)
			}
		})
	}
	wg.Wait()
	if got := b.checks.Load(); got != 1 {
		t.Fatalf("1000 probes caused %d backend checks, want 1", got)
	}
}

func TestReadinessMonitorSerialChecksAndShutdown(t *testing.T) {
	started := make(chan struct{})
	b := &readinessBackend{ready: func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return nil // An adapter ignoring the error cannot publish late success.
	}}
	h := handler(t, b, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); h.RunReadiness(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("monitor did not start")
	}
	// A second monitor must not start another backend call.
	h.RunReadiness(ctx)
	for i := 0; i < 100; i++ {
		if got := call(h, "GET", "/readyz", "", "").Code; got != 503 {
			t.Fatalf("incomplete check looked ready: %d", got)
		}
	}
	if got := b.checks.Load(); got != 1 {
		t.Fatalf("parallel backend checks: %d", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not cancel its check")
	}
	if got := call(h, "GET", "/readyz", "", "").Code; got != 503 {
		t.Fatal("stopped monitor looked ready")
	}
}
