package sandbox

import (
	"sort"
	"sync"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/metrics"
)

const cleanupEventCapacity = 64

type activeCleanup struct {
	phase api.CleanupPhase
	start time.Time
}

// A bounded event journal complements per-task traces. Active spans remain
// visible during a stalled operation; polling may miss older completed spans.
type cleanupTelemetry struct {
	mu      sync.Mutex
	active  map[string]activeCleanup
	recent  []api.CleanupPhase
	dropped uint64
}

func (e *Engine) cleanupPhase(instance *executionState, phase string, action func() error) error {
	start := time.Now()
	event := api.CleanupPhase{SandboxID: instance.id, Phase: phase, StartedAt: start.UTC().Format(time.RFC3339Nano)}
	e.cleanup.mu.Lock()
	if e.cleanup.active == nil {
		e.cleanup.active = make(map[string]activeCleanup)
	}
	e.cleanup.active[instance.id] = activeCleanup{event, start}
	e.cleanup.mu.Unlock()
	err := action()
	event.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	event.DurationMS = ms(time.Since(start))
	if err != nil {
		event.Error = err.Error()
		if len(event.Error) > 4096 {
			event.Error = event.Error[:4096]
		}
	}
	instance.cleanupPhases = append(instance.cleanupPhases, event)
	e.cleanup.mu.Lock()
	delete(e.cleanup.active, instance.id)
	if len(e.cleanup.recent) == cleanupEventCapacity {
		copy(e.cleanup.recent, e.cleanup.recent[1:])
		e.cleanup.recent = e.cleanup.recent[:cleanupEventCapacity-1]
		e.cleanup.dropped++
	}
	e.cleanup.recent = append(e.cleanup.recent, event)
	e.cleanup.mu.Unlock()
	return err
}

func (c *cleanupTelemetry) report() *api.CleanupReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.active) == 0 && len(c.recent) == 0 {
		return nil
	}
	r := &api.CleanupReport{Active: []api.CleanupPhase{}, Recent: append([]api.CleanupPhase(nil), c.recent...), Dropped: c.dropped}
	for _, active := range c.active {
		phase := active.phase
		phase.DurationMS = ms(time.Since(active.start))
		r.Active = append(r.Active, phase)
	}
	sort.Slice(r.Active, func(i, j int) bool { return r.Active[i].SandboxID < r.Active[j].SandboxID })
	return r
}

func durationReport(d metrics.Duration) api.DurationReport {
	r := api.DurationReport{Count: d.Count, TotalMS: ms(d.Total), MaxMS: ms(d.Max)}
	for _, bucket := range d.Buckets() {
		v := api.DurationBucket{Count: bucket.Count}
		if !bucket.Overflow {
			upper := ms(bucket.Upper)
			v.UpperMS = &upper
		}
		r.Buckets = append(r.Buckets, v)
	}
	return r
}

func schedulingReport(s poolSnapshot) *api.SchedulingReport {
	t, c := s.Telemetry, s.Config
	return &api.SchedulingReport{
		ExecutionInFlight: s.ExecutionInFlight, ExecutionLimit: c.Concurrency, CreationLimit: c.CreateConcurrency,
		QueueCountLimit: c.QueueSize, QueueBytesLimit: c.QueueBytes, QueueTimeoutMS: ms(c.QueueTimeout), OldestQueueMS: s.OldestQueueMS,
		CreationAttempts: t.CreationAttempts, CreationSucceeded: durationReport(t.CreationSucceeded),
		CreationFailed: durationReport(t.CreationFailed), CreationAborted: durationReport(t.CreationAborted),
		QueueEntered: t.QueueEntered, QueueAssigned: durationReport(t.QueueAssigned), QueueCanceled: durationReport(t.QueueCanceled),
		QueueTimedOut: durationReport(t.QueueTimedOut), QueueUnavailable: durationReport(t.QueueUnavailable),
		QueueCountFull: t.QueueCountFull, QueueBytesFull: t.QueueBytesFull,
	}
}
