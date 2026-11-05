package session

import (
	"errors"
	"github.com/X1Kun/AgentBox/internal/admission/health"
	"github.com/prometheus/client_golang/prometheus"
	"sync"
	"time"
)

// Quality collects closed arrival windows, delayed by one deadline so every
// scheduled request has had its full opportunity to produce a timely result.
// Missing completions count as timeout. Dropped/late arrivals invalidate the
// window. Late completions never retroactively turn a timeout into a success.
type Quality struct {
	mu       sync.Mutex
	config   Config
	contract health.Contract
	start    time.Time
	events   map[string]Event
	arrivals map[string]Event
	last     health.Sample
	hasLast  bool
	invalid  bool
	now      func() time.Time
	desc     *prometheus.Desc
}

func NewQuality(c Config, contract health.Contract, start time.Time) (*Quality, error) {
	if Validate(c) != nil || !contract.Valid() || c.DeadlineMS != contract.DeadlineSeconds*1000 {
		return nil, errors.New("invalid quality contract")
	}
	for _, r := range c.Requests {
		if r.Action != "execute" || r.ExpectedExit != 0 {
			return nil, errors.New("quality source requires successful follow-up executions only")
		}
	}
	return &Quality{config: c, contract: contract, start: start, events: map[string]Event{}, arrivals: map[string]Event{}, now: time.Now,
		desc: prometheus.NewDesc(health.Metric, "Trusted closed follow-up windows; validate source time, contract and counts before use.", []string{"field"}, prometheus.Labels{"domain": contract.Domain, "revision": contract.Revision, "profile": contract.Profile})}, nil
}
func (q *Quality) Arrival(e Event) { q.mu.Lock(); q.arrivals[e.ID] = e; q.mu.Unlock() }
func (q *Quality) Record(e Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.events[e.ID]; exists {
		q.invalid = true
		q.last.Valid = false
	}
	q.events[e.ID] = e
}
func (q *Quality) Invalidate() { q.mu.Lock(); q.invalid = true; q.last.Valid = false; q.mu.Unlock() }
func (q *Quality) Snapshot(now time.Time) (health.Sample, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	window, deadline := time.Duration(q.contract.WindowSeconds)*time.Second, time.Duration(q.contract.DeadlineSeconds)*time.Second
	index := int64(now.Sub(q.start.Add(deadline)) / window)
	if index < 1 {
		return health.Sample{}, false
	}
	if q.hasLast && index <= q.last.Sequence {
		return q.last, true
	}
	end := q.start.Add(time.Duration(index) * window)
	begin := end.Add(-window)
	s := health.Sample{Contract: q.contract, End: end, SourceStarted: q.start, Sequence: index, Valid: !q.invalid}
	for _, r := range q.config.Requests {
		planned := q.start.Add(time.Duration(r.AtMS) * time.Millisecond)
		if planned.Before(begin) || !planned.Before(end) {
			continue
		}
		s.Planned++
		a, sent := q.arrivals[r.ID]
		if !sent || a.SentMS < 0 || a.SentMS-float64(r.AtMS) > float64(q.config.MaxLagMS) {
			s.Valid = false
		}
		e, ok := q.events[r.ID]
		if !ok {
			s.Timeouts++
			continue
		}
		if e.SentMS < 0 || e.SentMS-float64(r.AtMS) > float64(q.config.MaxLagMS) {
			s.Valid = false
		}
		switch {
		case e.Outcome == "correct" && e.FinishedMS-float64(r.AtMS) <= float64(q.config.DeadlineMS):
			s.Timely++
		case e.Outcome == "rejected":
			s.Rejected++
		case e.Outcome == "unknown" || e.FinishedMS-float64(r.AtMS) > float64(q.config.DeadlineMS):
			s.Timeouts++
		default:
			s.Errors++
		}
	}
	q.last, q.hasLast = s, true
	return s, true
}
func (q *Quality) Describe(ch chan<- *prometheus.Desc) { ch <- q.desc }
func (q *Quality) Collect(ch chan<- prometheus.Metric) {
	s, ok := q.Snapshot(q.now())
	if !ok {
		return
	}
	for field, value := range health.Fields(s) {
		ch <- prometheus.MustNewConstMetric(q.desc, prometheus.GaugeValue, value, field)
	}
}
