package admission

import (
	"github.com/X1Kun/AgentBox/internal/admission/health"
	"github.com/prometheus/client_golang/prometheus"
	"sync"
	"time"
)

type Metrics struct {
	mu        sync.Mutex
	state     *prometheus.GaugeVec
	valid     prometheus.Gauge
	age       prometheus.Gauge
	planned   prometheus.Gauge
	ratio     prometheus.Gauge
	actions   *prometheus.CounterVec
	wait      prometheus.Histogram
	passes    *prometheus.CounterVec
	last      prometheus.Gauge
	decisions *prometheus.CounterVec
}

func NewMetrics(register prometheus.Registerer) *Metrics {
	m := &Metrics{
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_admission_decisions_total", Help: "Health decisions by finite reason."}, []string{"reason"}),
		state:     prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "agentbox_admission_state", Help: "One current controller decision state; bounded state labels."}, []string{"state"}),
		valid:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentbox_admission_sample_valid", Help: "Latest policy sample validity; insufficient samples are invalid."}),
		age:       prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentbox_admission_sample_age_seconds", Help: "Sample age at decision; inspect last reconcile timestamp."}),
		planned:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentbox_admission_window_requests", Help: "All planned requests in the latest window."}),
		ratio:     prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentbox_admission_timely_ratio", Help: "Correct timely ratio at decision; meaningful only with sample_valid."}),
		actions:   prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_admission_actions_total", Help: "Status write attempts; unknown is not a confirmed grant."}, []string{"action", "outcome"}),
		wait:      prometheus.NewHistogram(prometheus.HistogramOpts{Name: "agentbox_admission_check_wait_seconds", Help: "Reserved check wait at status decisions.", Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600, 3600}}),
		passes:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "agentbox_admission_reconciles_total", Help: "Domain reconciliation outcomes."}, []string{"outcome"}),
		last:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "agentbox_admission_reconcile_timestamp_seconds", Help: "Last completed pass; controller death/stall must not look healthy."}),
	}
	register.MustRegister(m.state, m.valid, m.age, m.planned, m.ratio, m.actions, m.wait, m.passes, m.last, m.decisions)
	return m
}
func (m *Metrics) Observe(d Decision, s health.Sample, now time.Time) {
	m.decisions.WithLabelValues(d.Reason).Inc()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, state := range []State{Unknown, HealthyPending, Healthy, Protect, Degraded} {
		v := 0.
		if state == d.State {
			v = 1
		}
		m.state.WithLabelValues(string(state)).Set(v)
	}
	v := 0.
	if d.State != Unknown && d.State != Degraded {
		v = 1
	}
	m.valid.Set(v)
	m.age.Set(max(0, now.Sub(s.End).Seconds()))
	m.planned.Set(float64(s.Planned))
	m.ratio.Set(s.Ratio())
}
func (m *Metrics) Action(action, outcome string, wait time.Duration) {
	m.actions.WithLabelValues(action, outcome).Inc()
	m.wait.Observe(max(0, wait.Seconds()))
}
func (m *Metrics) Reconcile(at time.Time, err error) {
	outcome := "complete"
	if err != nil {
		outcome = "error"
	}
	m.passes.WithLabelValues(outcome).Inc()
	m.last.Set(float64(at.UnixNano()) / 1e9)
	if err != nil {
		m.valid.Set(0)
	}
}
