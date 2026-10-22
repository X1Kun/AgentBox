//go:build integration

package benchmarks

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

const waveSeed int64 = 20261021

type plannedArrival struct {
	Offset time.Duration `json:"offset_ns"`
	Wave   int           `json:"wave"`
}
type wavePlan struct {
	Index           int   `json:"index"`
	Cycle           int   `json:"cycle"`
	Requests        int   `json:"requests"`
	StartMS         int64 `json:"start_ms"`
	ArrivalsUntilMS int64 `json:"arrivals_until_ms"`
	IdleUntilMS     int64 `json:"idle_until_ms"`
}
type waveTrace struct {
	Seed          int64            `json:"seed"`
	InitialIdleMS int64            `json:"initial_idle_ms"`
	DurationMS    int64            `json:"duration_ms"`
	Waves         []wavePlan       `json:"waves"`
	Arrivals      []plannedArrival `json:"arrivals"`
}

// Generate once and replay unchanged in each mode. The first request begins
// each wave; remaining arrivals are irregular, reproducible offsets, not a
// closed-loop load whose rate falls when responses slow down.
func makeWaveTrace(cycles int) waveTrace {
	r := rand.New(rand.NewSource(waveSeed))
	trace := waveTrace{Seed: waveSeed, InitialIdleMS: 2000, DurationMS: int64(2000 + 26000*cycles)}
	for cycle := 0; cycle < cycles; cycle++ {
		for phase, count := range []int{5, 20} {
			start := int64(2000 + cycle*26000)
			duration := int64(1000)
			if phase == 1 {
				start += 11000
				duration = 5000
			}
			index := len(trace.Waves) + 1
			trace.Waves = append(trace.Waves, wavePlan{Index: index, Cycle: cycle + 1, Requests: count, StartMS: start, ArrivalsUntilMS: start + duration, IdleUntilMS: start + duration + 10000})
			for i := 0; i < count; i++ {
				offset := time.Duration(start) * time.Millisecond
				if i > 0 {
					offset += time.Duration(r.Int63n(int64(time.Duration(duration) * time.Millisecond)))
				}
				trace.Arrivals = append(trace.Arrivals, plannedArrival{Offset: offset, Wave: index})
			}
		}
	}
	sort.SliceStable(trace.Arrivals, func(i, j int) bool { return trace.Arrivals[i].Offset < trace.Arrivals[j].Offset })
	return trace
}

type poolReading struct {
	QoS                *qosReading `json:"qos,omitempty"`
	SharedCPU          *uint64     `json:"shared_cpu_usec,omitempty"`
	SharedThrottled    *uint64     `json:"shared_throttled_usec,omitempty"`
	SharedStall        *uint64     `json:"shared_cpu_stall_usec,omitempty"`
	RefillAttempts     uint64      `json:"refill_creation_attempts"`
	RefillForeground   uint64      `json:"refill_started_with_foreground"`
	CoreCPU            *uint64     `json:"core_cpu_usec,omitempty"`
	CoreStall          *uint64     `json:"core_cpu_stall_usec,omitempty"`
	CoreMemory         *uint64     `json:"core_memory_bytes,omitempty"`
	CreationCount      uint64      `json:"creation_count"`
	SandboxThrottled   *uint64     `json:"sandbox_throttled_usec"`
	ContainerThrottled *uint64     `json:"namespace_root_throttled_usec"`
	SandboxStall       *uint64     `json:"sandbox_cpu_stall_usec"`
	SandboxOOM         *uint64     `json:"sandbox_oom_kills"`
	ContainerOOM       *uint64     `json:"namespace_root_oom_kills"`
	AtMS               float64     `json:"at_ms"`
	Ready              bool        `json:"ready"`
	Idle               int         `json:"idle"`
	Creating           int         `json:"creating"`
	Executing          int         `json:"executing"`
	Total              int         `json:"total"`
	Queued             int         `json:"queued"`
	RefillBlockedBy    string      `json:"refill_blocked_by"`
	SandboxCPU         *uint64     `json:"sandbox_cpu_usec"`
	ContainerCPU       *uint64     `json:"namespace_root_cpu_usec"`
	SandboxMemory      *uint64     `json:"sandbox_memory_bytes"`
	ContainerMemory    *uint64     `json:"namespace_root_memory_bytes"`
	Error              string      `json:"error,omitempty"`
}

type qosReading struct {
	Sequence       uint64   `json:"sequence"`
	Stale          bool     `json:"stale"`
	CollectionMS   float64  `json:"collection_ms"`
	PressureState  string   `json:"pressure_state"`
	PressureReason string   `json:"pressure_reason"`
	ProtectedStall *float64 `json:"protected_stall_percent,omitempty"`
	RefillOnly     bool     `json:"refill_only"`
	RefillPaused   bool     `json:"refill_paused"`
	AdmissionOpen  bool     `json:"admission_open"`
	ActualCPU      *float64 `json:"actual_cpu,omitempty"`
	CPUChanges     uint64   `json:"cpu_changes"`
	CPUState       string   `json:"cpu_state,omitempty"`
	CPUReason      string   `json:"cpu_reason,omitempty"`
	CPUError       string   `json:"cpu_error,omitempty"`
}

func compactQoS(q *api.QoSReport) *qosReading {
	if q == nil {
		return nil
	}
	r := &qosReading{Sequence: q.Sequence, Stale: q.Stale, CollectionMS: q.CollectionMS, AdmissionOpen: true}
	if q.Pressure != nil {
		r.PressureState, r.PressureReason = q.Pressure.State, q.Pressure.Reason
	}
	if q.Scheduling != nil {
		r.RefillOnly, r.RefillPaused, r.AdmissionOpen = q.Scheduling.RefillOnly, q.Scheduling.RefillPaused, q.Scheduling.AdmissionOpen
	}
	if q.CPUControl != nil {
		r.ActualCPU, r.CPUChanges, r.CPUState, r.CPUReason = q.CPUControl.ActualCPU, q.CPUControl.Changes, q.CPUControl.State, q.CPUControl.Reason
		r.CPUError = q.CPUControl.Error
	}
	for _, s := range q.Sources {
		if s.Name == "protected" && s.Available && s.IntervalMS != nil && s.Sample != nil {
			r.ProtectedStall = s.Sample.CPU.Some.StallPercent
		}
	}
	return r
}

type waveResult struct {
	Plan             wavePlan `json:"plan"`
	Requests         summary  `json:"requests"`
	FirstHTTP        int      `json:"first_http_status"`
	FirstScheduledMS float64  `json:"first_scheduled_response_ms"`
	LastResponseMS   float64  `json:"last_response_ms"`
	StartIdle        int      `json:"ready_before_first_request"`
	RecoveryMS       *float64 `json:"recovered_after_arrivals_ms"`
}
type waveResources struct {
	ObservedSeconds     float64  `json:"observed_seconds"`
	SandboxCPU          *uint64  `json:"sandbox_cpu_usec"`
	ContainerCPU        *uint64  `json:"namespace_root_cpu_usec"`
	SandboxMeanMemory   *float64 `json:"sandbox_mean_memory_bytes"`
	ContainerMeanMemory *float64 `json:"namespace_root_mean_memory_bytes"`
	SandboxPeakMemory   uint64   `json:"sandbox_sampled_peak_memory_bytes"`
	ContainerPeakMemory uint64   `json:"namespace_root_sampled_peak_memory_bytes"`
	InvalidSamples      int      `json:"invalid_samples"`
}
type waveRun struct {
	Mode             string        `json:"mode"`
	Summary          summary       `json:"summary"`
	Waves            []waveResult  `json:"waves"`
	Resources        waveResources `json:"resources"`
	UnhealthySamples int           `json:"unhealthy_samples"`
	Error            string        `json:"error,omitempty"`
}

func counter(path, key string) (*uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	value := strings.TrimSpace(string(data))
	if key != "" {
		value = ""
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			if key == "some.total" && len(f) > 1 && f[0] == "some" {
				for _, part := range f[1:] {
					if strings.HasPrefix(part, "total=") {
						value = strings.TrimPrefix(part, "total=")
					}
				}
				break
			}
			if len(f) == 2 && f[0] == key {
				value = f[1]
				break
			}
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid counter %s: %w", path, err)
	}
	return &n, nil
}

func readPool(engine *sandbox.Engine, root string, origin time.Time) poolReading {
	s := engine.Status()
	f := poolReading{CreationCount: s.CreationCount, AtMS: msSince(origin), Ready: s.Ready, Idle: s.Idle, Creating: s.Creating, Total: s.Total, Queued: s.Queued}
	f.QoS = compactQoS(s.QoS)
	if s.Scheduling != nil {
		f.RefillAttempts = s.Scheduling.RefillCreationAttempts
		f.RefillForeground = s.Scheduling.RefillWithForeground
		f.Executing = s.Scheduling.ExecutionInFlight
		f.RefillBlockedBy = s.Scheduling.RefillBlockedBy
	}
	var errors []string
	for _, entry := range []struct {
		path, key string
		dst       **uint64
	}{
		{filepath.Join(root, "sandboxes", "cpu.stat"), "usage_usec", &f.SandboxCPU},
		{"/sys/fs/cgroup/cpu.stat", "usage_usec", &f.ContainerCPU},
		{filepath.Join(root, "sandboxes", "memory.current"), "", &f.SandboxMemory},
		{"/sys/fs/cgroup/memory.current", "", &f.ContainerMemory},
		{filepath.Join(root, "sandboxes", "cpu.stat"), "throttled_usec", &f.SandboxThrottled},
		{"/sys/fs/cgroup/cpu.stat", "throttled_usec", &f.ContainerThrottled},
		{filepath.Join(root, "sandboxes", "cpu.pressure"), "some.total", &f.SandboxStall},
		{filepath.Join(root, "sandboxes", "memory.events"), "oom_kill", &f.SandboxOOM},
		{"/sys/fs/cgroup/memory.events", "oom_kill", &f.ContainerOOM},
	} {
		value, err := counter(entry.path, entry.key)
		if err != nil {
			errors = append(errors, err.Error())
		} else {
			*entry.dst = value
		}
	}
	f.Error = strings.Join(errors, "; ")
	return f
}
func msSince(origin time.Time) float64 {
	return float64(time.Since(origin)) / float64(time.Millisecond)
}

func summarizeWave(plan wavePlan, rows []sample, frames []poolReading, startIdle, target int) waveResult {
	result := waveResult{Plan: plan, StartIdle: startIdle}
	if len(rows) > 0 {
		result.FirstHTTP = rows[0].HTTP
		result.FirstScheduledMS = rows[0].ScheduledWallMS
	}
	for _, r := range rows {
		result.LastResponseMS = max(result.LastResponseMS, r.FinishedMS)
	}
	elapsed := time.Duration(max(0, result.LastResponseMS-float64(plan.StartMS)) * float64(time.Millisecond))
	result.Requests = collectSummary("", "waves", rows, elapsed)
	// Never report recovery before the last response or across the next wave.
	for _, f := range frames {
		if f.AtMS < max(float64(plan.ArrivalsUntilMS), result.LastResponseMS) || f.AtMS >= float64(plan.IdleUntilMS) {
			continue
		}
		if f.Ready && f.Executing == 0 && f.Queued == 0 && f.Creating == 0 && f.Total == f.Idle && f.Idle >= target {
			delta := f.AtMS - float64(plan.ArrivalsUntilMS)
			result.RecoveryMS = &delta
			break
		}
	}
	return result
}

func summarizeResources(frames []poolReading) waveResources {
	r := waveResources{}
	if len(frames) < 2 {
		return r
	}
	for _, f := range frames {
		if f.Error != "" || f.SandboxCPU == nil || f.ContainerCPU == nil || f.SandboxMemory == nil || f.ContainerMemory == nil {
			r.InvalidSamples++
			continue
		}
		r.SandboxPeakMemory = max(r.SandboxPeakMemory, *f.SandboxMemory)
		r.ContainerPeakMemory = max(r.ContainerPeakMemory, *f.ContainerMemory)
	}
	first, last := frames[0], frames[len(frames)-1]
	r.ObservedSeconds = (last.AtMS - first.AtMS) / 1000
	if r.InvalidSamples > 0 || r.ObservedSeconds <= 0 {
		return r
	}
	if *last.SandboxCPU < *first.SandboxCPU || *last.ContainerCPU < *first.ContainerCPU {
		r.InvalidSamples++
		return r
	}
	cpu, containerCPU := *last.SandboxCPU-*first.SandboxCPU, *last.ContainerCPU-*first.ContainerCPU
	r.SandboxCPU, r.ContainerCPU = &cpu, &containerCPU
	var sandboxArea, containerArea float64
	for i := 1; i < len(frames); i++ {
		a, b := frames[i-1], frames[i]
		dt := (b.AtMS - a.AtMS) / 1000
		sandboxArea += (float64(*a.SandboxMemory) + float64(*b.SandboxMemory)) * .5 * dt
		containerArea += (float64(*a.ContainerMemory) + float64(*b.ContainerMemory)) * .5 * dt
	}
	mean, containerMean := sandboxArea/r.ObservedSeconds, containerArea/r.ObservedSeconds
	r.SandboxMeanMemory, r.ContainerMeanMemory = &mean, &containerMean
	return r
}

func runWaves(t *testing.T, engine *sandbox.Engine, config sandbox.Config, client *http.Client, url, output string, trace waveTrace) ([]sample, waveRun) {
	return runWavesAt(t, engine, config, client, url, output, trace, time.Now(), nil, nil)
}

func runWavesAt(t *testing.T, engine *sandbox.Engine, config sandbox.Config, client *http.Client, url, output string, trace waveTrace, origin time.Time, inspect func(*poolReading), drain func()) ([]sample, waveRun) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(output, config.Mode+"-pool.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0644); err != nil {
		file.Close()
		t.Fatal(err)
	}
	defer file.Close()
	stop := make(chan struct{})
	done := recordPool(engine, config.CgroupRoot, origin, file, stop, 50*time.Millisecond, inspect)
	rows := make([]sample, len(trace.Arrivals))
	starts := map[int]int{}
	var workers sync.WaitGroup
	for i, arrival := range trace.Arrivals {
		scheduled := origin.Add(arrival.Offset)
		if delay := time.Until(scheduled); delay > 0 {
			time.Sleep(delay)
		}
		if _, ok := starts[arrival.Wave]; !ok {
			starts[arrival.Wave] = engine.Status().Idle
		}
		workers.Add(1)
		go func(i int, a plannedArrival, scheduled time.Time) {
			defer workers.Done()
			row := runRequest(client, url, config.Mode, "waves", i, origin, scheduled)
			row.Wave = a.Wave
			rows[i] = row
		}(i, arrival, scheduled)
	}
	// Include every idle interval and finish all offered calls. Neither response
	// completion nor READY recovery controls when the next request is offered.
	if delay := time.Until(origin.Add(time.Duration(trace.DurationMS) * time.Millisecond)); delay > 0 {
		time.Sleep(delay)
	}
	workers.Wait()
	if drain != nil {
		drain()
	}
	close(stop)
	sampledResult := <-done
	result := waveRun{Mode: config.Mode, Summary: collectSummary(config.Mode, "waves", rows, time.Since(origin)), Resources: summarizeResources(sampledResult.frames)}
	if sampledResult.err != nil {
		result.Error = sampledResult.err.Error()
	}
	for _, f := range sampledResult.frames {
		if !f.Ready {
			result.UnhealthySamples++
		}
		if f.Total > config.MaxSandboxes {
			result.Error = "sampled capacity violation"
		}
		result.Summary.PoolMaxTotal = max(result.Summary.PoolMaxTotal, f.Total)
		result.Summary.PoolMaxQueued = max(result.Summary.PoolMaxQueued, f.Queued)
	}
	result.Summary.PoolMinIdle = engine.Status().Idle
	for _, f := range sampledResult.frames {
		result.Summary.PoolMinIdle = min(result.Summary.PoolMinIdle, f.Idle)
	}
	target := 0
	if config.Mode == "warm" {
		target = config.WarmPoolSize
	}
	for _, plan := range trace.Waves {
		var selected []sample
		for _, r := range rows {
			if r.Wave == plan.Index {
				selected = append(selected, r)
			}
		}
		wave := summarizeWave(plan, selected, sampledResult.frames, starts[plan.Index], target)
		wave.Requests.Mode = config.Mode
		result.Waves = append(result.Waves, wave)
		t.Logf("%s/wave-%d completed=%d/%d scheduled p50=%.2fms p95=%.2fms first=%.2fms HTTP=%d ready-before=%d recovery=%v", config.Mode, plan.Index, wave.Requests.Completed, len(selected), wave.Requests.ScheduledP50MS, wave.Requests.ScheduledP95MS, wave.FirstScheduledMS, wave.FirstHTTP, wave.StartIdle, recoveryText(wave.RecoveryMS))
	}
	return rows, result
}

func recoveryText(value *float64) string {
	if value == nil {
		return "not_observed_in_idle_interval"
	}
	return fmt.Sprintf("%.2fms", *value)
}

// The same recorder is used for sparse waves and capacity steps. The inspector
// can stop a capacity ramp without introducing response-dependent pacing.
type poolRecording struct {
	frames []poolReading
	err    error
}

func recordPool(engine *sandbox.Engine, root string, origin time.Time, file *os.File, stop <-chan struct{}, interval time.Duration, inspect func(*poolReading)) <-chan poolRecording {
	done := make(chan poolRecording, 1)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		encoder := json.NewEncoder(file)
		var frames []poolReading
		take := func() error {
			f := readPool(engine, root, origin)
			if inspect != nil {
				inspect(&f)
			}
			frames = append(frames, f)
			err := encoder.Encode(f)
			if err != nil && inspect != nil {
				f.Error = "recording write failed"
				inspect(&f)
			}
			return err
		}
		if err := take(); err != nil {
			done <- poolRecording{frames, err}
			return
		}
		for {
			select {
			case <-stop:
				err := take()
				done <- poolRecording{frames, err}
				return
			case <-ticker.C:
				if err := take(); err != nil {
					done <- poolRecording{frames, err}
					return
				}
			}
		}
	}()
	return done
}
