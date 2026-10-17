package qos

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Mode            string
	Interval        time.Duration
	ProtectedCgroup string
}

func (c *Config) Normalize() error {
	if c.Mode == "" {
		c.Mode = "off"
	}
	if c.Mode != "off" && c.Mode != "observe" {
		return fmt.Errorf("qos mode must be off or observe; enforcement is not implemented")
	}
	if c.Interval == 0 {
		c.Interval = 100 * time.Millisecond
	}
	if c.Interval < 50*time.Millisecond || c.Interval > time.Second {
		return fmt.Errorf("qos interval must be 50ms..1s")
	}
	if c.Mode == "off" {
		return nil
	}
	path := filepath.Clean(c.ProtectedCgroup)
	if !filepath.IsAbs(path) || !strings.HasPrefix(path, "/sys/fs/cgroup/") {
		return fmt.Errorf("observe mode requires a protected cgroup below /sys/fs/cgroup")
	}
	c.ProtectedCgroup = path
	return nil
}

type Delta struct {
	Valid               bool
	Interval            time.Duration
	CPUSomePercent      float64
	CPUFullPercent      float64
	MemorySomePercent   float64
	MemoryFullPercent   float64
	CPUUsageCores       float64
	CPUPeriods          uint64
	CPUThrottledPeriods uint64
	CPUThrottledUsec    uint64
	MemoryHighEvents    uint64
	MemoryMaxEvents     uint64
	OOMEvents           uint64
	OOMKills            uint64
}

type Observation struct {
	Source          Source
	Available       bool
	Error, Baseline string
	Reading         Reading
	Delta           Delta
}
type Snapshot struct {
	State         string
	Interval      time.Duration
	CollectedAt   time.Time
	Age, Duration time.Duration
	Stale         bool
	Sources       []Observation
}
type Observer struct {
	config          Config
	reader          Reader
	sources         []Source
	previous        map[string]Reading // owned by the sampling goroutine
	mu              sync.Mutex
	snapshot        Snapshot
	started, closed bool
	cancel          context.CancelFunc
	done            chan struct{}
}

func New(config Config, sandboxParent string) (*Observer, error) {
	if err := config.Normalize(); err != nil {
		return nil, err
	}
	if config.Mode == "off" {
		return nil, nil
	}
	protected := config.ProtectedCgroup
	// Resolve existing paths for overlap checks; a not-yet-started core service
	// is allowed and will be reported as unavailable until its cgroup appears.
	if resolved, err := filepath.EvalSymlinks(protected); err == nil {
		protected = resolved
	}
	sandboxParent = filepath.Clean(sandboxParent)
	if protected == sandboxParent || strings.HasPrefix(protected, sandboxParent+"/") || strings.HasPrefix(sandboxParent, protected+"/") || !strings.HasPrefix(protected, "/sys/fs/cgroup/") {
		return nil, fmt.Errorf("protected cgroup must be separate from sandbox subtree")
	}
	sources := []Source{{Name: "host", Path: "/proc/pressure"}, {Name: "sandboxes", Path: sandboxParent, Cgroup: true}, {Name: "protected", Path: protected, Cgroup: true}}
	return newObserver(config, FileReader{}, sources), nil
}
func newObserver(config Config, reader Reader, sources []Source) *Observer {
	return &Observer{config: config, reader: reader, sources: sources, previous: make(map[string]Reading), snapshot: Snapshot{State: "starting", Interval: config.Interval}}
}

func (o *Observer) Start(parent context.Context) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started || o.closed {
		return
	}
	o.started = true
	ctx, cancel := context.WithCancel(parent)
	o.cancel = cancel
	o.done = make(chan struct{})
	go o.run(ctx)
}
func (o *Observer) run(ctx context.Context) {
	defer close(o.done)
	defer func() { o.mu.Lock(); o.snapshot.State = "stopped"; o.mu.Unlock() }()
	ticker := time.NewTicker(o.config.Interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		begin := time.Now()
		snapshot := o.collect()
		snapshot.CollectedAt = time.Now()
		snapshot.Duration = snapshot.CollectedAt.Sub(begin)
		o.mu.Lock()
		o.snapshot = snapshot
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (o *Observer) collect() Snapshot {
	snapshot := Snapshot{State: "observing", Interval: o.config.Interval}
	for _, source := range o.sources {
		observation := Observation{Source: source}
		reading, err := o.reader.Read(source)
		if err != nil {
			observation.Error = err.Error()
			delete(o.previous, source.Name)
			snapshot.State = "degraded"
		} else {
			observation.Available = true
			observation.Reading = reading
			previous, exists := o.previous[source.Name]
			switch {
			case !exists:
				observation.Baseline = "initial"
			case previous.Identity != reading.Identity:
				observation.Baseline = "source_changed"
			case reading.At.Sub(previous.At) <= 0:
				observation.Baseline = "invalid_interval"
			case reading.At.Sub(previous.At) > 5*o.config.Interval:
				observation.Baseline = "sample_gap"
			case countersReset(previous, reading, source.Cgroup):
				observation.Baseline = "counter_reset"
			default:
				observation.Delta = derive(previous, reading, source.Cgroup)
			}
			o.previous[source.Name] = reading
		}
		snapshot.Sources = append(snapshot.Sources, observation)
	}
	return snapshot
}
func (o *Observer) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	snapshot := o.snapshot
	snapshot.Sources = append([]Observation(nil), snapshot.Sources...)
	if !snapshot.CollectedAt.IsZero() {
		snapshot.Age = time.Since(snapshot.CollectedAt)
	}
	snapshot.Stale = snapshot.CollectedAt.IsZero() || snapshot.Age > 3*o.config.Interval || snapshot.Duration > 3*o.config.Interval || snapshot.State == "stopped"
	if snapshot.Stale {
		for i := range snapshot.Sources {
			snapshot.Sources[i].Delta.Valid = false
			if snapshot.Sources[i].Available {
				snapshot.Sources[i].Baseline = "stale"
			}
		}
	}
	return snapshot
}
func (o *Observer) Close() {
	o.mu.Lock()
	o.closed = true
	if !o.started {
		o.snapshot.State = "stopped"
		o.mu.Unlock()
		return
	}
	o.cancel()
	done := o.done
	o.mu.Unlock()
	<-done
}
func countersReset(a, b Reading, group bool) bool {
	if b.CPU.Some.TotalUsec < a.CPU.Some.TotalUsec || b.Memory.Some.TotalUsec < a.Memory.Some.TotalUsec || b.Memory.Full.TotalUsec < a.Memory.Full.TotalUsec {
		return true
	}
	if a.CPU.HasFull != b.CPU.HasFull || b.CPU.HasFull && b.CPU.Full.TotalUsec < a.CPU.Full.TotalUsec {
		return true
	}
	if !group {
		return false
	}
	x, y := a.Resources, b.Resources
	return y.CPUUsageUsec < x.CPUUsageUsec || y.CPUPeriods < x.CPUPeriods || y.CPUThrottledPeriods < x.CPUThrottledPeriods || y.CPUThrottledUsec < x.CPUThrottledUsec || y.MemoryHighEvents < x.MemoryHighEvents || y.MemoryMaxEvents < x.MemoryMaxEvents || y.OOMEvents < x.OOMEvents || y.OOMKills < x.OOMKills
}
func derive(a, b Reading, group bool) Delta {
	elapsed := b.At.Sub(a.At)
	us := float64(elapsed) / float64(time.Microsecond)
	d := Delta{Valid: true, Interval: elapsed, CPUSomePercent: 100 * float64(b.CPU.Some.TotalUsec-a.CPU.Some.TotalUsec) / us, MemorySomePercent: 100 * float64(b.Memory.Some.TotalUsec-a.Memory.Some.TotalUsec) / us, MemoryFullPercent: 100 * float64(b.Memory.Full.TotalUsec-a.Memory.Full.TotalUsec) / us}
	if b.CPU.HasFull {
		d.CPUFullPercent = 100 * float64(b.CPU.Full.TotalUsec-a.CPU.Full.TotalUsec) / us
	}
	if group {
		x, y := a.Resources, b.Resources
		d.CPUUsageCores = float64(y.CPUUsageUsec-x.CPUUsageUsec) / us
		d.CPUPeriods = y.CPUPeriods - x.CPUPeriods
		d.CPUThrottledPeriods = y.CPUThrottledPeriods - x.CPUThrottledPeriods
		d.CPUThrottledUsec = y.CPUThrottledUsec - x.CPUThrottledUsec
		d.MemoryHighEvents = y.MemoryHighEvents - x.MemoryHighEvents
		d.MemoryMaxEvents = y.MemoryMaxEvents - x.MemoryMaxEvents
		d.OOMEvents = y.OOMEvents - x.OOMEvents
		d.OOMKills = y.OOMKills - x.OOMKills
	}
	return d
}
