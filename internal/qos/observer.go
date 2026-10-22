package qos

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

type Config struct {
	RefillOnly      bool
	Scheduling      bool
	CPUControl      bool
	CPU             CPUConfig
	Logger          *slog.Logger
	Mode            string
	Interval        time.Duration
	ProtectedCgroup string
	BusinessFile    string
}

func (c *Config) Normalize() error {
	if c.Mode == "" {
		c.Mode = "off"
	}
	if c.Mode != "off" && c.Mode != "observe" && c.Mode != "enforce" {
		return fmt.Errorf("qos mode must be off, observe or enforce")
	}
	if c.Interval == 0 {
		c.Interval = 100 * time.Millisecond
	}
	if c.Interval < 50*time.Millisecond || c.Interval > time.Second {
		return fmt.Errorf("qos interval must be 50ms..1s")
	}
	if c.Mode == "off" {
		if c.CPUControl || c.Scheduling || c.RefillOnly || c.CPU.BaseCPU != 0 || c.BusinessFile != "" {
			return fmt.Errorf("QoS policies require observe or enforce mode")
		}
		return nil
	}
	if c.RefillOnly && (c.CPUControl || c.Scheduling || c.CPU.BaseCPU != 0 || c.BusinessFile != "") {
		return fmt.Errorf("qos-refill-only cannot combine with CPU control, admission scheduling or business borrowing")
	}
	if c.Mode == "enforce" && !c.RefillOnly {
		c.CPUControl = true
	}
	if c.CPU.BaseCPU != 0 && !c.CPUControl {
		return fmt.Errorf("base CPU requires CPU control")
	}
	if c.CPUControl || c.Scheduling || c.RefillOnly {
		if err := c.CPU.Normalize(); err != nil {
			return err
		}
		if c.CPU.DecisionInterval < c.Interval {
			return fmt.Errorf("CPU decision interval must not be shorter than observation interval")
		}
	}
	if (c.CPU.BaseCPU > 0) != (c.BusinessFile != "") {
		return fmt.Errorf("qos-base-cpu and qos-business-file must be configured together")
	}
	if c.BusinessFile != "" {
		if !c.CPUControl || !filepath.IsAbs(c.BusinessFile) || filepath.Clean(c.BusinessFile) != c.BusinessFile {
			return fmt.Errorf("business guard requires CPU control and a clean absolute sample path")
		}
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
	CPUUserCores        float64
	CPUSystemCores      float64
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
	Business      *BusinessReport
	Pressure      *PressureReport
	Control       *ControlReport
	Sequence      uint64
	Boundary      HierarchyBoundary
	State         string
	Interval      time.Duration
	CollectedAt   time.Time
	Age, Duration time.Duration
	Stale         bool
	Sources       []Observation
}
type Observer struct {
	pressure        *PressurePolicy
	pressureHandler func(PressureReport)
	controller      *CPUController
	sequence        uint64
	boundary        HierarchyBoundary
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
	sources, boundary := discoverSources(sandboxParent, protected)
	o := newObserver(config, &FileReader{}, sources)
	o.boundary = boundary
	if config.CPUControl || config.Scheduling || config.RefillOnly {
		pressure, err := NewPressurePolicy(config.CPU)
		if err != nil {
			return nil, err
		}
		o.pressure = pressure
	}
	if config.CPUControl {
		controller, err := NewCPUController(config.Mode, config.CPU, &cgroup.Manager{SandboxParentPath: sandboxParent}, config.Logger)
		if err != nil {
			return nil, err
		}
		o.controller = controller
	}
	return o, nil
}
func newObserver(config Config, reader Reader, sources []Source) *Observer {
	return &Observer{config: config, reader: reader, sources: sources, previous: make(map[string]Reading), snapshot: Snapshot{State: "starting", Interval: config.Interval}}
}

// Register before Start. The callback runs without the observer mutex held.
func (o *Observer) SetPressureHandler(handler func(PressureReport)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started || o.closed {
		return fmt.Errorf("pressure handler must be registered before observer start")
	}
	o.pressureHandler = handler
	return nil
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
		if ctx.Err() != nil {
			return
		}
		o.updatePolicies(&snapshot, time.Now())
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
	o.sequence++
	snapshot := Snapshot{State: "observing", Interval: o.config.Interval, Sequence: o.sequence, Boundary: o.boundary}
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
	if o.config.BusinessFile != "" {
		business := readBusiness(o.config.BusinessFile, time.Now())
		snapshot.Business = &business
	}
	return snapshot
}

func (o *Observer) updatePolicies(snapshot *Snapshot, now time.Time) {
	if o.pressure != nil {
		report := o.pressure.Update(*snapshot, now)
		snapshot.Pressure = &report
		if o.pressureHandler != nil {
			o.pressureHandler(report)
		}
	}
	if o.controller != nil {
		report := o.controller.Update(*snapshot, now)
		snapshot.Control = &report
	}
}
func (o *Observer) Snapshot() Snapshot {
	o.mu.Lock()
	defer o.mu.Unlock()
	snapshot := o.snapshot
	if snapshot.Business != nil {
		business := *snapshot.Business
		snapshot.Business = &business
		if !businessFresh(business, time.Now()) {
			snapshot.Business.Available = false
			snapshot.Business.Healthy = false
			snapshot.Business.Error = "stale business window"
		}
	}
	if snapshot.Pressure != nil {
		pressure := *snapshot.Pressure
		snapshot.Pressure = &pressure
	}
	if snapshot.Control != nil {
		control := *snapshot.Control
		if control.ActualCPU != nil {
			actual := *control.ActualCPU
			control.ActualCPU = &actual
		}
		snapshot.Control = &control
	}
	snapshot.Sources = append([]Observation(nil), snapshot.Sources...)
	for i := range snapshot.Sources {
		snapshot.Sources[i].Reading.Auxiliary = cloneAuxiliary(snapshot.Sources[i].Reading.Auxiliary)
	}
	if !snapshot.CollectedAt.IsZero() {
		snapshot.Age = time.Since(snapshot.CollectedAt)
	}
	snapshot.Stale = snapshot.CollectedAt.IsZero() || snapshot.Age > 3*o.config.Interval || snapshot.Duration > 3*o.config.Interval || snapshot.State == "stopped"
	if snapshot.Stale {
		if snapshot.Pressure != nil {
			snapshot.Pressure.State = "unknown"
			snapshot.Pressure.Reason = "stale_snapshot"
		}
		if snapshot.Control != nil {
			snapshot.Control.State = "degraded"
			snapshot.Control.Reason = "stale_snapshot"
			snapshot.Control.Applied = false
		}
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
	return y.CPUUsageUsec < x.CPUUsageUsec || y.CPUUserUsec < x.CPUUserUsec || y.CPUSystemUsec < x.CPUSystemUsec || y.CPUPeriods < x.CPUPeriods || y.CPUThrottledPeriods < x.CPUThrottledPeriods || y.CPUThrottledUsec < x.CPUThrottledUsec || y.MemoryHighEvents < x.MemoryHighEvents || y.MemoryMaxEvents < x.MemoryMaxEvents || y.OOMEvents < x.OOMEvents || y.OOMKills < x.OOMKills
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
		d.CPUUserCores = float64(y.CPUUserUsec-x.CPUUserUsec) / us
		d.CPUSystemCores = float64(y.CPUSystemUsec-x.CPUSystemUsec) / us
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
