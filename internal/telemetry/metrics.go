// Package telemetry exposes bounded application metrics without querying backends
// during a scrape. Resource identities belong in structured events, not labels.
package telemetry

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const MaxObservationAge = 5 * time.Second

type readiness struct {
	healthy bool
	at      time.Time
}
type Metrics struct {
	Registry          *prometheus.Registry
	mode              string
	requests          *prometheus.CounterVec
	durations         *prometheus.HistogramVec
	inflight          *prometheus.GaugeVec
	operations        *prometheus.CounterVec
	operationDuration *prometheus.HistogramVec
	transitions       *prometheus.CounterVec
	checks            *prometheus.CounterVec
	last              atomic.Pointer[readiness]
	now               func() time.Time
}

func New(mode string) *Metrics {
	if mode != "session" && mode != "batch" {
		panic("unsupported telemetry mode")
	}
	m := &Metrics{Registry: prometheus.NewRegistry(), mode: mode, now: time.Now}
	register := prometheus.WrapRegistererWith(prometheus.Labels{"mode": mode}, m.Registry)
	buckets := []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	m.requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_http_requests_total", Help: "HTTP requests by bounded operation and status; excludes health probes."}, []string{"operation", "status"})
	m.durations = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentbox_http_request_duration_seconds", Help: "API request duration, not workload execution or result correctness.", Buckets: buckets}, []string{"operation"})
	m.inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentbox_http_inflight", Help: "Currently active API HTTP requests."}, []string{"operation"})
	m.operations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_operations_total", Help: "Completed application operation observations in this process."}, []string{"operation", "outcome"})
	m.operationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "agentbox_operation_duration_seconds", Help: "Operation observations, separated from HTTP transport and Kubernetes stage timings.", Buckets: buckets}, []string{"operation", "outcome"})
	m.transitions = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_state_transitions_total", Help: "Committed application state observations; process-local, not unique workload throughput."}, []string{"from", "to"})
	m.checks = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_readiness_checks_total", Help: "Completed backend readiness checks."}, []string{"outcome"})
	register.MustRegister(m.requests, m.durations, m.inflight, m.operations, m.operationDuration, m.transitions, m.checks,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	register.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "agentbox_backend_observation_valid", Help: "One only when the latest completed readiness observation is fresh."}, func() float64 {
		if m.valid() {
			return 1
		}
		return 0
	}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "agentbox_backend_ready", Help: "Fresh backend readiness; inspect observation_valid before interpreting zero."}, func() float64 {
			v := m.last.Load()
			if m.valid() && v.healthy {
				return 1
			}
			return 0
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "agentbox_backend_observation_timestamp_seconds", Help: "Start time of the last completed backend check; zero means no observation."}, func() float64 {
			v := m.last.Load()
			if v == nil {
				return 0
			}
			return float64(v.at.UnixNano()) / 1e9
		}))
	return m
}
func (m *Metrics) valid() bool {
	v := m.last.Load()
	if v == nil {
		return false
	}
	age := m.now().Sub(v.at)
	return age >= 0 && age < MaxObservationAge
}
func (m *Metrics) Backend(healthy bool, started time.Time) {
	if m == nil {
		return
	}
	m.last.Store(&readiness{healthy, started})
	outcome := "error"
	if healthy {
		outcome = "ready"
	}
	m.checks.WithLabelValues(outcome).Inc()
}
func (m *Metrics) Handler() http.Handler {
	handler := promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{MaxRequestsInFlight: 2, Timeout: 2 * time.Second})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/metrics" || r.URL.RawQuery != "" {
			http.NotFound(w, r)
			return
		}
		handler.ServeHTTP(w, r)
	})
}
func operation(mode, path, method string) string {
	if path == "/healthz" || path == "/readyz" {
		return "probe"
	}
	collection := "/v2/sessions"
	if mode == "batch" {
		collection = "/v2/batches"
	}
	if path == collection {
		if mode == "batch" {
			return "submit"
		}
		return "create"
	}
	if !strings.HasPrefix(path, collection+"/") {
		return "other"
	}
	parts := strings.Split(strings.TrimPrefix(path, collection+"/"), "/")
	if len(parts) == 1 {
		if method == "GET" {
			return "get"
		}
		if method == "DELETE" {
			if mode == "batch" {
				return "cancel"
			}
			return "close"
		}
		return "other"
	}
	if mode == "batch" && len(parts) == 3 && parts[1] == "artifacts" {
		return "download"
	}
	if mode == "session" {
		if len(parts) == 2 && parts[1] == "execute" {
			return "execute"
		}
		if len(parts) == 3 && parts[1] == "files" {
			if parts[2] == "read" {
				return "read"
			}
			if parts[2] == "write" {
				return "write"
			}
		}
	}
	return "other"
}

type response struct {
	http.ResponseWriter
	status int
}

func (w *response) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *response) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(data)
}
func (w *response) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (m *Metrics) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := operation(m.mode, r.URL.Path, r.Method)
		if op == "probe" {
			next.ServeHTTP(w, r)
			return
		}
		started := m.now()
		wrapped := &response{ResponseWriter: w}
		completed := false
		m.inflight.WithLabelValues(op).Inc()
		defer func() {
			status := wrapped.status
			if status == 0 {
				status = 200
			}
			m.inflight.WithLabelValues(op).Dec()
			label := strconv.Itoa(status)
			if !completed {
				label = "aborted"
			}
			m.requests.WithLabelValues(op, label).Inc()
			m.durations.WithLabelValues(op).Observe(m.now().Sub(started).Seconds())
		}()
		next.ServeHTTP(wrapped, r)
		completed = true
	})
}
func bounded(value string, allowed ...string) string {
	for _, v := range allowed {
		if value == v {
			return value
		}
	}
	return "other"
}
func (m *Metrics) Operation(stage, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	stage = bounded(stage, "acquire", "execute", "validate", "release", "close", "write", "read", "publication", "artifact_read", "reconcile")
	outcome = bounded(outcome, "complete", "program_failure", "unknown", "error", "invalid", "pending", "canceled", "rejected")
	m.operations.WithLabelValues(stage, outcome).Inc()
	m.operationDuration.WithLabelValues(stage, outcome).Observe(max(0, duration.Seconds()))
}
func state(value string) string {
	return bounded(value, "none", "submitting", "queued", "running", "succeeded", "failed", "cancelling", "cancelled", "unknown", "rejected", "creating", "ready", "closed", "invalidated", "unavailable")
}
func (m *Metrics) Transition(from, to, id string, at time.Time) {
	if m == nil || from == to {
		return
	}
	m.transitions.WithLabelValues(state(from), state(to)).Inc()
	slog.Info("application state observation", "mode", m.mode, "from", state(from), "to", state(to), "object_id", id, "observed_at", at.UTC().Format(time.RFC3339Nano), "time_source", "api_observation")
}
