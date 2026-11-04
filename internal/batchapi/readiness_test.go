package batchapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/readiness"
	"github.com/X1Kun/AgentBox/internal/telemetry"
)

type healthBackend struct {
	backend
	check      func(context.Context) error
	checks     atomic.Int32
	reconciles atomic.Int32
}

func (b *healthBackend) Ready(ctx context.Context) error { b.checks.Add(1); return b.check(ctx) }
func (b *healthBackend) Reconcile(context.Context) error { b.reconciles.Add(1); return nil }

func TestBatchReadinessExpiresAndNeverSchedulesMaintenance(t *testing.T) {
	s, _ := setup(t)
	b := &healthBackend{check: func(context.Context) error { return nil }}
	s.backend = b
	now := time.Now()
	s.now = func() time.Time { return now }
	s.checkReadiness(context.Background())
	if got := call(s, "GET", "/readyz", "", false).Code; got != 200 {
		t.Fatal(got)
	}
	for range 100 {
		call(s, "GET", "/readyz", "", false)
	}
	if b.checks.Load() != 1 || b.reconciles.Load() != 0 {
		t.Fatal("probes queried the backend or scheduled maintenance")
	}
	now = now.Add(readiness.MaxAge)
	if got := call(s, "GET", "/readyz", "", false).Code; got != 503 {
		t.Fatal("stale success", got)
	}
	b.check = func(context.Context) error { return errors.New("unavailable") }
	s.checkReadiness(context.Background())
	if got := call(s, "GET", "/readyz", "", false).Code; got != 503 {
		t.Fatal(got)
	}
	b.check = func(context.Context) error { return nil }
	s.checkReadiness(context.Background())
	if got := call(s, "GET", "/readyz", "", false).Code; got != 200 {
		t.Fatal(got)
	}
}
func TestBatchMonitorExitClearsSuccessAndRejectsLateSuccess(t *testing.T) {
	s, _ := setup(t)
	s.SetTelemetry(telemetry.New("batch"))
	s.metrics.Backend(true, s.now())
	started := make(chan struct{})
	b := &healthBackend{check: func(ctx context.Context) error { close(started); <-ctx.Done(); return nil }}
	s.backend = b
	s.readiness.Store(true, s.now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.RunReadiness(ctx) }()
	<-started
	s.RunReadiness(ctx) // Duplicate startup must not schedule another check.
	w := httptest.NewRecorder()
	s.metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `agentbox_backend_observation_valid{mode="batch"} 1`) {
		t.Fatal("duplicate monitor invalidated active observations")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("monitor did not stop")
	}
	if b.checks.Load() != 1 || b.reconciles.Load() != 0 {
		t.Fatal("unexpected backend calls")
	}
	if got := call(s, "GET", "/readyz", "", false).Code; got != 503 {
		t.Fatal("stopped monitor remained ready", got)
	}
	w = httptest.NewRecorder()
	s.metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `agentbox_backend_observation_valid{mode="batch"} 0`) {
		t.Fatal("metrics disagreed with stopped HTTP readiness")
	}
}
