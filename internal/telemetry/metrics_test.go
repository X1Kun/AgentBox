package telemetry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	return w.Body.String()
}
func TestRoutesAndOutcomesCannotCreateIdentifierLabels(t *testing.T) {
	m := New("session")
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(409) }))
	for n := 0; n < 100; n++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v2/sessions/private-session-id/execute", nil))
	}
	m.Operation("user-controlled-phase", "user-controlled-result", time.Second)
	body := scrape(t, m)
	for _, secret := range []string{"private-session-id", "user-controlled-phase", "user-controlled-result"} {
		if strings.Contains(body, secret) {
			t.Fatalf("unbounded label leaked %s", secret)
		}
	}
	if !strings.Contains(body, `agentbox_http_requests_total{mode="session",operation="execute",status="409"} 100`) {
		t.Fatal(body)
	}
	if !strings.Contains(body, `agentbox_http_inflight{mode="session",operation="execute"} 0`) {
		t.Fatal("inflight leaked")
	}
}
func TestFreshReadinessAndUnknownAreSeparate(t *testing.T) {
	m := New("batch")
	now := time.Now()
	m.now = func() time.Time { return now }
	if !strings.Contains(scrape(t, m), `agentbox_backend_observation_valid{mode="batch"} 0`) {
		t.Fatal("unobserved readiness was known")
	}
	m.Backend(true, now)
	if !strings.Contains(scrape(t, m), `agentbox_backend_ready{mode="batch"} 1`) {
		t.Fatal("fresh healthy check missing")
	}
	now = now.Add(6 * time.Second)
	body := scrape(t, m)
	if !strings.Contains(body, `agentbox_backend_ready{mode="batch"} 0`) || !strings.Contains(body, `agentbox_backend_observation_valid{mode="batch"} 0`) {
		t.Fatal("stale readiness was healthy")
	}
}
func TestProbeVolumeAndMetricsScrapesDoNotEnterBusinessCounters(t *testing.T) {
	m := New("session")
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for n := 0; n < 50; n++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/readyz", nil))
		_ = scrape(t, m)
	}
	if strings.Contains(scrape(t, m), "agentbox_http_requests_total{") {
		t.Fatal("health probes inflated business requests")
	}
}
func TestIndependentRegistriesAndProgramFailure(t *testing.T) {
	for n := 0; n < 2; n++ {
		m := New("session")
		m.Operation("execute", "program_failure", time.Millisecond)
		body := scrape(t, m)
		if !strings.Contains(body, `agentbox_operations_total{mode="session",operation="execute",outcome="program_failure"} 1`) {
			t.Fatal(body)
		}
	}
}

func TestAbortedHandlerDoesNotBecomeSuccessfulRequest(t *testing.T) {
	m := New("batch")
	h := m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test failure") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("middleware suppressed the panic")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v2/batches", nil))
	}()
	body := scrape(t, m)
	if !strings.Contains(body, `operation="submit",status="aborted"} 1`) || !strings.Contains(body, `agentbox_http_inflight{mode="batch",operation="submit"} 0`) || strings.Contains(body, `status="200"`) {
		t.Fatal(body)
	}
}
