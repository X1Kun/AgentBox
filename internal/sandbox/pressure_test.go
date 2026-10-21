package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/qos"
)

func schedulingEngine(t *testing.T, p *warmPool, mode string) *Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	lock, err := os.Create(filepath.Join(t.TempDir(), "lock"))
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{config: Config{QoS: qos.Config{Mode: mode, Scheduling: true}}, ctx: ctx, cancel: cancel, lock: lock, pool: p}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	return e
}

func gateDecision(paused bool) qos.PressureReport {
	state := "normal"
	if paused {
		state = "pressured"
	}
	return qos.PressureReport{State: state, EffectiveState: state, AdmissionOpen: !paused, RefillPaused: paused}
}

func TestColdPressureAdmissionAndObserveHasNoSideEffects(t *testing.T) {
	for _, mode := range []string{"observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			e := schedulingEngine(t, nil, mode)
			e.applyPressure(gateDecision(true))
			if !e.Ready() {
				t.Fatal("pressure marked the engine unhealthy")
			}
			if mode == "enforce" {
				_, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(1)"})
				var admission *AdmissionError
				if !errors.Is(err, ErrPressure) || !errors.Is(err, ErrBusy) || !errors.As(err, &admission) || admission.Reason != "qos_pressure" {
					t.Fatalf("pressure rejection: %v", err)
				}
				if e.qosSchedulingReport().PressureRejections != 1 {
					t.Fatal("rejection leaked a slot or counter")
				}
			} else {
				if err := e.beginAdmission(); err != nil {
					t.Fatal(err)
				}
				e.active.Done()
				if !e.qosSchedulingReport().AdmissionOpen {
					t.Fatal("observe closed actual admission")
				}
			}
			e.applyPressure(gateDecision(false))
			if err := e.beginAdmission(); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			e.active.Done()
		})
	}
}

func TestWarmQueuedRequestRejectedWithoutCancellingInFlight(t *testing.T) {
	p := testPool(t, poolConfig())
	e := schedulingEngine(t, p, "enforce")
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(1)"})
		done <- err
	}()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 1 })
	e.applyPressure(gateDecision(true))
	if err := <-done; !errors.Is(err, ErrPressure) {
		t.Fatalf("queued request: %v", err)
	}
	s := p.status()
	if s.ExecutionInFlight != 1 || s.Queued != 0 || s.QueuedBytes != 0 || s.Telemetry.QueuePressureRejected.Count != 1 || s.Telemetry.QueueTimedOut.Count != 0 || e.ctx.Err() != nil {
		t.Fatalf("inflight or accounting damaged: %+v", s)
	}
	if e.qosSchedulingReport().PressureRejections != 1 || !e.qosSchedulingReport().RefillPaused {
		t.Fatal("actual scheduling report incorrect")
	}
	p.mark(first, poolDestroying)
	p.release(first, true)
	e.applyPressure(gateDecision(false))
	second, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	p.mark(second, poolDestroying)
	p.release(second, true)
}

func TestQueueCannotMissCloseAndImmediateReopen(t *testing.T) {
	p := testPool(t, poolConfig())
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := p.acquire(context.Background(), 3); done <- err }()
	waitPool(t, p, func(s poolSnapshot) bool { return s.Queued == 1 })
	// Prevent the waiter from observing the intermediate boolean. It must
	// still see the pressure generation and reject, even after reopening.
	p.mu.Lock()
	p.setPressureLocked(true)
	p.setPressureLocked(false)
	p.mu.Unlock()
	p.mark(first, poolDestroying)
	p.release(first, true)
	if err := <-done; !errors.Is(err, ErrPressure) {
		t.Fatalf("missed close/reopen: %v", err)
	}
	s := p.status()
	if s.Queued != 0 || s.QueuedBytes != 0 || s.Telemetry.QueuePressureRejected.Count != 1 {
		t.Fatalf("retained waiter: %+v", s)
	}
	instance, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	p.mark(instance, poolDestroying)
	p.release(instance, true)
}

func TestRefillPauseLetsReservedCreationFinishAndResumes(t *testing.T) {
	c := poolConfig()
	c.MaxSandboxes = 4
	var calls atomic.Int32
	began, release := make(chan struct{}), make(chan struct{})
	p := newWarmPool(c, func(ctx context.Context) (*executionState, error) {
		if calls.Add(1) == 2 {
			close(began)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &executionState{}, nil
	}, func(*executionState) error { return nil })
	t.Cleanup(func() {
		p.stop()
		if err := p.drain(); err != nil {
			t.Error(err)
		}
	})
	if err := p.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-began
	first, err := p.acquire(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	p.setPressure(true)
	close(release)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Creating == 0 && s.Idle == 1 })
	if p.ctx.Err() != nil || calls.Load() != 2 {
		t.Fatal("pressure cancelled creation or started extra work")
	}
	p.mark(first, poolDestroying)
	p.release(first, true)
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 2 {
		t.Fatal("refill ignored pressure pause")
	}
	p.setPressure(false)
	waitPool(t, p, func(s poolSnapshot) bool { return s.Idle == 2 && s.Creating == 0 })
	if calls.Load() != 3 {
		t.Fatal("refill did not resume")
	}
}

func TestShutdownWinsOverPressureRecovery(t *testing.T) {
	e := schedulingEngine(t, nil, "enforce")
	if err := e.beginAdmission(); err != nil {
		t.Fatal(err)
	}
	e.applyPressure(gateDecision(true))
	if e.ctx.Err() != nil {
		t.Fatal("pressure cancelled active context")
	}
	done := make(chan error, 1)
	go func() { done <- e.Close() }()
	deadline := time.Now().Add(time.Second)
	for !e.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !e.closed.Load() {
		t.Fatal("shutdown did not close admission")
	}
	e.applyPressure(gateDecision(false))
	if err := e.beginAdmission(); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("shutdown admission: %v", err)
	}
	e.active.Done()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown deadlocked")
	}
}

func TestStoppingPausedRefillCannotBeReopened(t *testing.T) {
	p := testPool(t, poolConfig())
	p.setPressure(true)
	done := make(chan struct{})
	go func() { p.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("paused worker did not stop")
	}
	p.setPressure(false)
	p.mu.Lock()
	paused, stopped := p.pressurePaused, p.stopped
	p.mu.Unlock()
	if !paused || !stopped {
		t.Fatal("recovery changed a stopped pool")
	}
	if _, err := p.acquire(context.Background(), 1); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("stopped admission: %v", err)
	}
}
