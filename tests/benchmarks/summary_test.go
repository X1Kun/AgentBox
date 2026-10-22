//go:build integration

package benchmarks

import (
	"reflect"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

func TestBenchmarkSummaryProtectionProfilesSeparateActuators(t *testing.T) {
	p99 := 20.0
	good := coreSummary{Offered: 100, OnTimeFraction: 1, SuccessP99MS: &p99}
	bad := good
	bad.OnTimeFraction = .9
	if !coreTargetPassed(good, good, good) || coreTargetPassed(good, bad, good) {
		t.Fatal("arrival-window failures hidden by full-window success")
	}
	plans := refillProfiles("protection")
	if len(plans) != 6 {
		t.Fatal("missing protection controls")
	}
	for _, p := range plans {
		var c sandbox.Config
		configureRefillProfile(&c, p, "/sys/fs/cgroup/core")
		if err := c.QoS.Normalize(); err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		if p.Control == "refill-only" && (c.WarmRefillPolicy != "slots" || !c.QoS.RefillOnly || c.QoS.CPUControl || c.QoS.Scheduling) {
			t.Fatalf("PSI-only profile changed additional actuators: %+v", c)
		}
		if p.Control == "adaptive" && (!c.QoS.CPUControl || !c.QoS.Scheduling || c.QoS.CPU.MinCPU != 1 || c.QoS.CPU.MaxCPU != 2) {
			t.Fatal("invalid adaptive profile")
		}
		if c.ParentCPUMilli != int64(p.ParentCPU*1000) {
			t.Fatal("test budget not available in server configuration")
		}
	}
}

func TestBenchmarkSummaryKeepsFailuresOutsideSuccessPercentiles(t *testing.T) {
	rows := []sample{
		{HTTP: 200, Status: "succeeded", Cleanup: true, WallMS: 10, Timings: api.Timings{SetupMS: 1, ExecutionMS: 8}},
		{HTTP: 200, Status: "succeeded", Cleanup: true, WallMS: 20, Timings: api.Timings{SetupMS: 2, ExecutionMS: 16}},
		{HTTP: 503, WallMS: 1},
		{HTTP: 500, WallMS: 1000},
		{HTTP: 200, Status: "timeout", Cleanup: true, WallMS: 30000},
	}
	s := collectSummary("warm", "steady", rows, time.Second)
	if s.Offered != 5 || s.Accepted != 3 || s.Completed != 2 || s.Rejected != 1 || s.Errors != 2 || s.P50MS != 10 || s.P99MS != 20 || s.CompletionRate != 2 {
		t.Fatalf("misleading summary: %+v", s)
	}
}

func TestBenchmarkSummaryCoreCountsAllOfferedWorkAndSeparatesWaveWindows(t *testing.T) {
	rows := []coreSample{{HTTP: 200, ScheduledWallMS: 10}, {HTTP: 200, ScheduledWallMS: 100}, {HTTP: 503, ScheduledWallMS: 1}, {Dropped: true}, {HTTP: 0, Error: "timeout"}}
	s := coreStats(rows)
	if s.Offered != 5 || s.Completed != 2 || s.OnTime != 1 || s.OnTimeFraction != .2 || s.Rejected != 1 || s.ClientDropped != 1 || s.Errors != 1 || s.SuccessP99MS == nil || *s.SuccessP99MS != 100 {
		t.Fatalf("Core failures hidden by success latency: %+v", s)
	}
	if s := coreStats(rows[2:]); s.SuccessP99MS != nil || s.OnTime != 0 {
		t.Fatal("empty success population reported zero latency")
	}
	trace := makeWaveTrace(1)
	if inWaveArrivals(1999, trace) || !inWaveArrivals(2000, trace) || inWaveArrivals(3000, trace) || !inWaveArrivals(13000, trace) || inWaveArrivals(18000, trace) {
		t.Fatal("Core traffic leaked between arrival and idle/recovery windows")
	}
}

func TestBenchmarkSummaryWaveTracePreservesArrivalsAndIdleWindows(t *testing.T) {
	trace := makeWaveTrace(2)
	if !reflect.DeepEqual(trace, makeWaveTrace(2)) {
		t.Fatal("mode-dependent random arrival schedule")
	}
	if len(trace.Arrivals) != 50 || len(trace.Waves) != 4 || trace.DurationMS != 54000 {
		t.Fatalf("invalid trace: %+v", trace)
	}
	for i, w := range trace.Waves {
		expected := 5
		if i%2 == 1 {
			expected = 20
		}
		var count int
		for _, a := range trace.Arrivals {
			if a.Wave != w.Index {
				continue
			}
			count++
			ms := int64(a.Offset / time.Millisecond)
			if ms < w.StartMS || ms >= w.ArrivalsUntilMS {
				t.Fatal("arrival leaked into idle period")
			}
		}
		if count != expected || w.IdleUntilMS-w.ArrivalsUntilMS != 10000 {
			t.Fatalf("invalid wave: %+v", w)
		}
		if i+1 < len(trace.Waves) && w.IdleUntilMS != trace.Waves[i+1].StartMS {
			t.Fatal("next wave depends on recovery")
		}
	}
}

func TestBenchmarkSummaryWaveRecoveryRequiresResponsesAndInventory(t *testing.T) {
	plan := wavePlan{Index: 1, StartMS: 2000, ArrivalsUntilMS: 3000, IdleUntilMS: 13000}
	rows := []sample{{HTTP: 200, Status: "succeeded", Cleanup: true, FinishedMS: 3200, ScheduledWallMS: 1200, WallMS: 1000}}
	frames := []poolReading{
		{AtMS: 3100, Ready: true, Idle: 3, Total: 3}, // before response: not recovered
		{AtMS: 3250, Ready: true, Idle: 3, Total: 4, Creating: 1},
		{AtMS: 3300, Ready: true, Idle: 3, Total: 3},
	}
	got := summarizeWave(plan, rows, frames, 3, 3)
	if got.RecoveryMS == nil || *got.RecoveryMS != 300 {
		t.Fatalf("false recovery: %+v", got)
	}
	if got.Requests.ScheduledP50MS != 1200 {
		t.Fatal("client scheduling delay omitted")
	}
	frames = append(frames[:2], poolReading{AtMS: 13000, Ready: true, Idle: 3, Total: 3})
	if r := summarizeWave(plan, rows, frames, 3, 3); r.RecoveryMS != nil {
		t.Fatal("recovery borrowed from next wave")
	}
	rows[0].HTTP = 503
	rows[0].Status = ""
	if r := summarizeWave(plan, rows, nil, 3, 3); r.FirstHTTP != 503 || r.Requests.Completed != 0 || r.Requests.Rejected != 1 {
		t.Fatal("rejected first request hidden")
	}
}

func TestBenchmarkSummaryResourceAccountingIncludesIdleAndRejectsMissingData(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	frames := []poolReading{
		{AtMS: 0, SandboxCPU: u(10), ContainerCPU: u(100), SandboxMemory: u(100), ContainerMemory: u(1000)},
		{AtMS: 2000, SandboxCPU: u(30), ContainerCPU: u(160), SandboxMemory: u(200), ContainerMemory: u(2000)},
	}
	r := summarizeResources(frames)
	if r.SandboxCPU == nil || *r.SandboxCPU != 20 || *r.ContainerCPU != 60 || *r.SandboxMeanMemory != 150 || r.ObservedSeconds != 2 {
		t.Fatalf("invalid full-window accounting: %+v", r)
	}
	frames[1].SandboxMemory = nil
	if r := summarizeResources(frames); r.InvalidSamples == 0 || r.SandboxMeanMemory != nil {
		t.Fatal("missing memory treated as zero")
	}
	frames[1].SandboxMemory = u(200)
	frames[1].SandboxCPU = u(1)
	if r := summarizeResources(frames); r.InvalidSamples == 0 || r.SandboxCPU != nil {
		t.Fatal("counter reset fabricated CPU savings")
	}
	s := collectSummary("warm", "waves", []sample{{HTTP: 503, Error: "executor unavailable"}}, time.Second)
	if s.Errors != 1 || s.Rejected != 0 {
		t.Fatal("unhealthy executor counted as capacity rejection")
	}
}

func TestBenchmarkSummaryCapacityCountsOnlyOnTimeSuccessfulWork(t *testing.T) {
	u := func(n uint64) *uint64 { return &n }
	frames := []poolReading{
		{AtMS: 0, Ready: true, Executing: 8, Creating: 4, Total: 8, SandboxCPU: u(0), ContainerCPU: u(0), SandboxMemory: u(1), ContainerMemory: u(2), SandboxThrottled: u(0), ContainerThrottled: u(0), SandboxStall: u(0)},
		{AtMS: 1000, Ready: true, Idle: 3, Total: 3, SandboxCPU: u(100), ContainerCPU: u(200), SandboxMemory: u(1), ContainerMemory: u(2), SandboxThrottled: u(10), ContainerThrottled: u(0), SandboxStall: u(10)},
	}
	rows := []sample{{HTTP: 200, Status: "succeeded", Cleanup: true, ScheduledWallMS: 100, WallMS: 90}, {HTTP: 200, Status: "succeeded", Cleanup: true, ScheduledWallMS: 1500, WallMS: 1490}, {HTTP: 503, WallMS: 1}}
	s := evaluateCapacity("warm", 4, 3, rows, frames, 800, 1000, .95, 8, 4, "")
	if s.Pass || s.Classification != "slo_boundary" || s.OnTime != 1 || s.OnTimeFraction != 1.0/3 || s.Summary.Rejected != 1 {
		t.Fatalf("fast rejection counted as capacity: %+v", s)
	}
	if s.PeakExecuting != 8 || s.PeakCreating != 4 || s.ExecutionFullFraction != 1 || *s.ThrottledUsec != 10 {
		t.Fatalf("missing limiter evidence: %+v", s)
	}
	if s := evaluateCapacity("warm", 4, 3, rows, frames, 800, 1000, .5, 8, 4, "memory_guard"); s.Classification != "memory_guard" || s.Pass {
		t.Fatal("resource guard misreported as a capacity limit")
	}
	good := []sample{{HTTP: 200, Status: "succeeded", Cleanup: true, ScheduledWallMS: 200, WallMS: 10, StartedMS: 190, ScheduledMS: 0}}
	if s := evaluateCapacity("warm", 64, 1, good, frames, 800, 1000, .95, 8, 4, ""); s.Classification != "load_generator_lag" {
		t.Fatal("load generator limit blamed on server")
	}
	good = append(good, sample{HTTP: 503, StartedMS: 200})
	good[0].StartedMS = 0
	if s := evaluateCapacity("warm", 64, 2, good, frames, 800, 1000, .95, 8, 4, ""); s.Classification != "load_generator_lag" {
		t.Fatal("late rejected requests hid load generator lag")
	}
}
