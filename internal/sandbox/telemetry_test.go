package sandbox

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestCreationOutcomesHaveMatchingDenominators(t *testing.T) {
	c := poolConfig()
	c.WarmPoolSize = 1
	var calls atomic.Int32
	p := newWarmPool(c, func(context.Context) (*executionState, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("injected creation failure")
		}
		return &executionState{}, nil
	}, func(*executionState) error { return nil })
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.stop(); _ = p.drain() })
	s := p.status()
	m := s.Telemetry
	if m.CreationAttempts != 2 || m.CreationFailed.Count != 1 || m.CreationSucceeded.Count != 1 || m.CreationAborted.Count != 0 || s.CreationCount != 1 {
		t.Fatalf("outcome counts: %+v", s)
	}
	if math.Abs(ms(m.CreationSucceeded.Total+m.CreationFailed.Total)-s.CreationTotalMS) > 1e-6 {
		t.Fatal("creation sum denominator mismatch")
	}
}

func TestQueueReasonsAndAllWaiterOutcomes(t *testing.T) {
	c := poolConfig()
	c.Concurrency = 1
	c.QueueSize = 1
	c.QueueBytes = 4
	c.QueueTimeout = time.Second
	p := testPool(t, c)
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.mark(first, poolDestroying); p.release(first, true) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := p.acquire(ctx, 1); done <- err }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 1 })
	_, err = p.acquire(context.Background(), 1)
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != "queue_count_full" || admission.Queued || !errors.Is(err, ErrBusy) {
		t.Fatalf("count rejection: %v", err)
	}
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("cancel not retained")
	}
	_, err = p.acquire(context.Background(), 5)
	if !errors.As(err, &admission) || admission.Reason != "queue_bytes_full" {
		t.Fatalf("byte rejection: %v", err)
	}
	s := p.status()
	if s.Telemetry.QueueCanceled.Count != 1 || s.Telemetry.QueueEntered != 1 || s.Telemetry.QueueCountFull != 1 || s.Telemetry.QueueBytesFull != 1 || s.ExecutionInFlight != 1 || s.Queued != 0 {
		t.Fatalf("queue accounting: %+v", s)
	}
}

func TestQueueTimeoutRetainsItsResidenceTime(t *testing.T) {
	c := poolConfig()
	c.Concurrency = 1
	c.QueueTimeout = 20 * time.Millisecond
	p := testPool(t, c)
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { p.mark(first, poolDestroying); p.release(first, true) }()
	_, err = p.acquire(context.Background(), 1)
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Reason != "queue_timeout" || !admission.Queued || admission.QueueWait <= 0 {
		t.Fatalf("timeout: %v", err)
	}
	s := p.status()
	if s.Telemetry.QueueTimedOut.Count != 1 || s.Telemetry.QueueEntered != 1 || s.Queued != 0 {
		t.Fatalf("timeout accounting: %+v", s)
	}
}

func TestCleanupJournalShowsStallsAndBoundsHistory(t *testing.T) {
	e := &Engine{}
	instance := &executionState{id: "slow"}
	began, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	cause := errors.New("delete failed")
	go func() {
		done <- e.cleanupPhase(instance, "runtime_delete", func() error { close(began); <-release; return cause })
	}()
	<-began
	r := e.cleanup.report()
	if len(r.Active) != 1 || r.Active[0].Phase != "runtime_delete" || r.Active[0].FinishedAt != "" {
		t.Fatalf("active phase missing: %+v", r)
	}
	r.Active[0].Phase = "caller mutation"
	close(release)
	if !errors.Is(<-done, cause) {
		t.Fatal("cleanup error changed")
	}
	if len(instance.cleanupPhases) != 1 || instance.cleanupPhases[0].Error != "delete failed" {
		t.Fatal("per-instance trace lost")
	}
	for i := 0; i < cleanupEventCapacity+3; i++ {
		_ = e.cleanupPhase(&executionState{id: fmt.Sprint(i)}, "bundle_remove", func() error { return nil })
	}
	r = e.cleanup.report()
	if len(r.Active) != 0 || len(r.Recent) != cleanupEventCapacity || r.Dropped != 4 {
		t.Fatalf("unbounded history: %+v", r)
	}
	r.Recent[0].Phase = "caller mutation"
	if e.cleanup.report().Recent[0].Phase == "caller mutation" {
		t.Fatal("history aliased")
	}
}
