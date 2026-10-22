//go:build integration

package benchmarks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

type capacityStep struct {
	CompletedInWindow     int           `json:"completed_in_offer_window"`
	DeliveredRate         float64       `json:"delivered_in_offer_window_per_second"`
	Rate                  int           `json:"offered_per_second"`
	Planned               int           `json:"planned"`
	Unsent                int           `json:"unsent_after_guard"`
	Summary               summary       `json:"requests"`
	OnTime                int           `json:"successful_within_slo"`
	OnTimeFraction        float64       `json:"on_time_fraction_of_offered"`
	Pass                  bool          `json:"meets_target"`
	Classification        string        `json:"classification"`
	StartMS               float64       `json:"start_ms"`
	OfferEndMS            float64       `json:"offer_end_ms"`
	EndMS                 float64       `json:"end_ms"`
	QueueP95MS            float64       `json:"success_queue_p95_ms"`
	PeakExecuting         int           `json:"sampled_peak_executing"`
	PeakCreating          int           `json:"sampled_peak_creating"`
	PeakQueued            int           `json:"sampled_peak_queued"`
	ExecutionFullFraction float64       `json:"execution_full_sample_fraction"`
	CreationFullFraction  float64       `json:"creation_full_sample_fraction"`
	Created               uint64        `json:"created_instances"`
	ThrottledUsec         *uint64       `json:"sandbox_throttled_usec"`
	OuterThrottledUsec    *uint64       `json:"namespace_root_throttled_usec"`
	CPUPressurePercent    *float64      `json:"sandbox_cpu_stall_percent"`
	Resources             waveResources `json:"resources_including_recovery"`
}

type capacityRun struct {
	Mode             string         `json:"mode"`
	StartupMS        float64        `json:"startup_ms"`
	Steps            []capacityStep `json:"steps"`
	LastPassingRate  *int           `json:"last_tested_passing_rate"`
	FirstFailingRate *int           `json:"first_tested_failing_rate"`
	EndReason        string         `json:"end_reason"`
	CleanupComplete  bool           `json:"cleanup_complete"`
}

func successful(row sample) bool {
	return row.HTTP == 200 && row.Status == "succeeded" && row.Cleanup && row.Error == ""
}
func deltaCounter(a, b *uint64) *uint64 {
	if a == nil || b == nil || *b < *a {
		return nil
	}
	n := *b - *a
	return &n
}

// A fast rejection is not useful work. The denominator is every offered call,
// including overload rejections and failed/late executions.
func evaluateCapacity(mode string, rate, planned int, rows []sample, frames []poolReading, offerEndMS, sloMS, target float64, execution, creation int, guard string) capacityStep {
	step := capacityStep{OfferEndMS: offerEndMS, Rate: rate, Planned: planned, Unsent: planned - len(rows), Summary: collectSummary(mode, fmt.Sprintf("capacity-%d", rate), rows, time.Second), Resources: summarizeResources(frames)}
	var queues []float64
	for _, row := range rows {
		if successful(row) {
			queues = append(queues, row.Timings.QueueMS)
			if row.ScheduledWallMS <= sloMS {
				step.OnTime++
			}
		}
	}
	if len(rows) > 0 {
		step.OnTimeFraction = float64(step.OnTime) / float64(len(rows))
	}
	step.QueueP95MS = percentile(queues, .95)
	var activeFrames, executionFull, creationFull int
	for _, f := range frames {
		step.Summary.PoolMaxTotal = max(step.Summary.PoolMaxTotal, f.Total)
		step.PeakExecuting = max(step.PeakExecuting, f.Executing)
		step.PeakCreating = max(step.PeakCreating, f.Creating)
		step.PeakQueued = max(step.PeakQueued, f.Queued)
		if f.AtMS >= step.OfferEndMS {
			continue
		}
		activeFrames++
		if f.Executing >= execution {
			executionFull++
		}
		if f.Creating >= creation {
			creationFull++
		}
	}
	if activeFrames > 0 {
		step.ExecutionFullFraction = float64(executionFull) / float64(activeFrames)
		step.CreationFullFraction = float64(creationFull) / float64(activeFrames)
	}
	step.Summary.PoolMaxQueued = step.PeakQueued
	if len(frames) > 0 {
		step.Summary.PoolMinIdle = frames[0].Idle
		for _, f := range frames {
			step.Summary.PoolMinIdle = min(step.Summary.PoolMinIdle, f.Idle)
		}
	}
	if len(frames) > 1 {
		first, last := frames[0], frames[len(frames)-1]
		step.StartMS = first.AtMS
		step.EndMS = last.AtMS
		step.Summary.ElapsedSeconds = (last.AtMS - first.AtMS) / 1000
		if step.Summary.ElapsedSeconds > 0 {
			step.Summary.CompletionRate = float64(step.Summary.Completed) / step.Summary.ElapsedSeconds
		}
		if last.CreationCount >= first.CreationCount {
			step.Created = last.CreationCount - first.CreationCount
		}
		step.ThrottledUsec = deltaCounter(first.SandboxThrottled, last.SandboxThrottled)
		step.OuterThrottledUsec = deltaCounter(first.ContainerThrottled, last.ContainerThrottled)
		if n := deltaCounter(first.SandboxStall, last.SandboxStall); n != nil && last.AtMS > first.AtMS {
			pressure := 100 * float64(*n) / ((last.AtMS - first.AtMS) * 1000)
			step.CPUPressurePercent = &pressure
		}
	}
	// Separately diagnose a load generator that cannot keep the intended pace.
	lagLimit := max(5.0, 250.0/float64(rate))
	switch {
	case guard != "":
		step.Classification = guard
	case step.Unsent != 0 || len(rows) == 0:
		step.Classification = "incomplete_load"
	case step.Resources.InvalidSamples != 0 || len(frames) < 2 || step.ThrottledUsec == nil || step.OuterThrottledUsec == nil || step.CPUPressurePercent == nil:
		step.Classification = "invalid_observation"
	case step.Summary.Errors > 0:
		step.Classification = "execution_error"
	case step.Summary.SendLagP95MS > lagLimit:
		step.Classification = "load_generator_lag"
	case step.OnTimeFraction < target || step.Summary.ScheduledP95MS > sloMS:
		step.Classification = "slo_boundary"
	default:
		step.Pass = true
		step.Classification = "within_target"
	}
	return step
}

func runCapacityStep(t *testing.T, engine *sandbox.Engine, config sandbox.Config, client *http.Client, url, output string, rate, count int, sloMS, target float64) ([]sample, capacityStep) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(output, fmt.Sprintf("capacity-%s-%d-pool.jsonl", config.Mode, rate)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0644); err != nil {
		file.Close()
		t.Fatal(err)
	}
	defer file.Close()
	origin := time.Now()
	stop := make(chan struct{})
	var stopped atomic.Bool
	var stopOnce sync.Once
	var reason string
	stopLoad := func(value string) { stopOnce.Do(func() { reason = value; stopped.Store(true) }) }
	budget := engine.Status().Memory
	var parentOOM, outerOOM uint64
	initialized := false
	inspect := func(f *poolReading) {
		switch {
		case !f.Ready:
			stopLoad("executor_unhealthy")
		case f.Error != "" || f.SandboxMemory == nil || f.ContainerMemory == nil || f.SandboxOOM == nil || f.ContainerOOM == nil:
			stopLoad("invalid_observation")
		default:
			if !initialized {
				parentOOM, outerOOM = *f.SandboxOOM, *f.ContainerOOM
				initialized = true
			}
			if *f.SandboxOOM > parentOOM || *f.ContainerOOM > outerOOM {
				stopLoad("oom_guard")
			}
			if float64(*f.SandboxMemory) > .85*float64(budget.ParentBytes) || float64(*f.ContainerMemory) > .85*float64(budget.CapacityBytes) {
				stopLoad("memory_guard")
			}
		}
	}
	recorded := recordPool(engine, config.CgroupRoot, origin, file, stop, 100*time.Millisecond, inspect)
	// A common fixed idle period makes the starting condition reproducible,
	// without waiting on the inventory count or changing the offered schedule.
	time.Sleep(2 * time.Second)
	createdBefore := engine.Status().CreationCount
	offerStart := time.Now()
	offerEnd := offerStart.Add(time.Duration(float64(time.Second) * float64(count) / float64(rate)))
	rows := make([]sample, count)
	var workers sync.WaitGroup
	offered := 0
	for i := 0; i < count; i++ {
		scheduled := offerStart.Add(time.Duration(float64(i) * float64(time.Second) / float64(rate)))
		if delay := time.Until(scheduled); delay > 0 {
			time.Sleep(delay)
		}
		if stopped.Load() {
			break
		}
		offered++
		workers.Add(1)
		go func(i int, scheduled time.Time) {
			defer workers.Done()
			row := runRequest(client, url, config.Mode, fmt.Sprintf("capacity-%d", rate), i, origin, scheduled)
			rows[i] = row
			if row.Error != "" || (row.HTTP != 503 && !successful(row)) {
				stopLoad("execution_error")
			}
		}(i, scheduled)
	}
	if !stopped.Load() {
		if delay := time.Until(offerEnd); delay > 0 {
			time.Sleep(delay)
		}
	}
	workers.Wait()
	if !stopped.Load() {
		time.Sleep(3 * time.Second)
	}
	close(stop)
	recording := <-recorded
	if recording.err != nil {
		stopLoad("recording_error")
	}
	frames := recording.frames
	// Use only the arrival interval for software resource saturation fractions;
	// resource cost still covers preparation and recovery for the whole step.
	evaluationFrames := make([]poolReading, 0, len(frames))
	for _, f := range frames {
		if f.AtMS >= float64(offerStart.Sub(origin))/float64(time.Millisecond) {
			evaluationFrames = append(evaluationFrames, f)
		}
	}
	offerEndMS := float64(offerEnd.Sub(origin)) / float64(time.Millisecond)
	step := evaluateCapacity(config.Mode, rate, count, rows[:offered], evaluationFrames, offerEndMS, sloMS, target, config.Concurrency, config.CreateConcurrency, reason)
	createdAfter := engine.Status().CreationCount
	if createdAfter >= createdBefore {
		step.Created = createdAfter - createdBefore
	}
	for _, row := range rows[:offered] {
		if successful(row) && row.FinishedMS < offerEndMS {
			step.CompletedInWindow++
		}
	}
	step.DeliveredRate = float64(step.CompletedInWindow) / (float64(count) / float64(rate))
	step.Resources = summarizeResources(frames)
	return rows[:offered], step
}

func TestCapacityScan(t *testing.T) {
	if os.Getenv("AGENTBOX_BENCHMARK") != "1" || os.Getenv("AGENTBOX_BENCH_SCENARIO") != "capacity" {
		t.Skip("requires explicit capacity scenario")
	}
	membership, err := os.ReadFile("/proc/self/cgroup")
	// The launcher verifies a private namespace at / before moving the test
	// process into test-manager so controllers can be enabled on its sibling.
	if err != nil || strings.TrimSpace(string(membership)) != "0::/test-manager" || os.Getenv("AGENTBOX_CGROUP_ROOT") != "/sys/fs/cgroup/agentbox" || os.Getenv("AGENTBOX_BENCH_ENVIRONMENT") != "isolated-docker" {
		t.Fatal("capacity scans require the disposable private-cgroup Docker launcher")
	}
	var rates []int
	for _, part := range strings.Split(os.Getenv("AGENTBOX_CAPACITY_RATES"), ",") {
		r, err := strconv.Atoi(part)
		if err != nil || r < 1 || r > 64 || (len(rates) > 0 && r <= rates[len(rates)-1]) {
			t.Fatal("invalid capacity rates")
		}
		rates = append(rates, r)
	}
	if len(rates) == 0 || len(rates) > 6 {
		t.Fatal("capacity scan requires 1..6 rates")
	}
	count := number(t, "AGENTBOX_CAPACITY_SAMPLES", 200, 100, 1000)
	parentCPU := number(t, "AGENTBOX_CAPACITY_PARENT_CPU", 4, 2, 4)
	sloMS := float64(number(t, "AGENTBOX_CAPACITY_SLO_MS", 1000, 50, 30000))
	target, err := strconv.ParseFloat(os.Getenv("AGENTBOX_CAPACITY_TARGET"), 64)
	if err != nil || !(target >= .5 && target <= 1) {
		t.Fatal("invalid on-time target")
	}
	output := os.Getenv("AGENTBOX_BENCH_OUTPUT")
	if output == "" {
		t.Fatal("new output directory required")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.OpenFile(filepath.Join(output, "requests.csv"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	writer := newRequestWriter(raw)
	var runs []capacityRun
	var configs []sandbox.Config
	for _, mode := range []string{"cold", "warm"} {
		config := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"), Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: mode,
			SandboxCPUMilli: int64(number(t, "AGENTBOX_BENCH_SANDBOX_CPU_MILLI", 1000, 50, 4000)),
			Concurrency:     number(t, "AGENTBOX_BENCH_EXECUTION_CONCURRENCY", 4, 1, 8), CreateConcurrency: number(t, "AGENTBOX_BENCH_CREATE_CONCURRENCY", 1, 1, 4), MaxSandboxes: number(t, "AGENTBOX_BENCH_MAX_SANDBOXES", 6, 3, 8), WarmPoolSize: 3, QueueSize: 16, QueueBytes: 8 << 20, QueueTimeout: 100 * time.Millisecond}
		if mode == "warm" {
			config.WarmRefillPolicy = os.Getenv("AGENTBOX_BENCH_REFILL_POLICY")
		}
		configs = append(configs, config)
		engine, err := sandbox.New(config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Close() })
		// This isolated test may only tighten the existing parent policy. Use
		// the same compare/write/readback path as the production controller.
		manager := &cgroup.Manager{SandboxParentPath: filepath.Join(config.CgroupRoot, "sandboxes")}
		before, err := manager.ReadCPUQuota()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.SetCPUQuota(before, uint64(parentCPU)*20000); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		begin := time.Now()
		err = engine.Start(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		run := capacityRun{Mode: mode, StartupMS: msSince(begin), EndReason: "tested_upper_bound_passed"}
		server := httptest.NewServer(httpapi.Handler(engine, token, slog.New(slog.NewTextHandler(io.Discard, nil))))
		transport := &http.Transport{MaxIdleConnsPerHost: 128, MaxConnsPerHost: 128}
		client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
		for _, rate := range rates {
			t.Logf("capacity %s offered=%d/s planned=%d execution=%d creation=%d parent-cpu=%d sandbox-cpu-milli=%d", mode, rate, count, config.Concurrency, config.CreateConcurrency, parentCPU, config.SandboxCPUMilli)
			rows, step := runCapacityStep(t, engine, config, client, server.URL, output, rate, count, sloMS, target)
			writeRequests(t, writer, rows)
			run.Steps = append(run.Steps, step)
			t.Logf("capacity %s %d/s completed=%d/%d on-time=%.1f%% p95=%.2fms queue-p95=%.2fms peak-executing=%d peak-creating=%d result=%s", mode, rate, step.Summary.Completed, step.Summary.Offered, 100*step.OnTimeFraction, step.Summary.ScheduledP95MS, step.QueueP95MS, step.PeakExecuting, step.PeakCreating, step.Classification)
			if step.Pass {
				v := rate
				run.LastPassingRate = &v
				continue
			}
			run.EndReason = step.Classification
			if step.Classification == "slo_boundary" {
				v := rate
				run.FirstFailingRate = &v
			}
			break
		}
		server.Close()
		transport.CloseIdleConnections()
		if err := engine.Close(); err != nil {
			t.Error(err)
		} else {
			run.CleanupComplete = true
		}
		entries, err := os.ReadDir(filepath.Join(config.StateDir, "bundles"))
		if err != nil || len(entries) != 0 {
			run.CleanupComplete = false
			t.Error("capacity run retained bundles", err)
		}
		entries, err = os.ReadDir(filepath.Join(config.CgroupRoot, "sandboxes"))
		if err != nil {
			run.CleanupComplete = false
			t.Error("cannot verify capacity cgroup cleanup", err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				run.CleanupComplete = false
				t.Error("capacity run retained cgroup", entry.Name())
			}
		}
		runs = append(runs, run)
	}
	executable, _ := os.Executable()
	limits := map[string]map[string]string{}
	for scope, path := range map[string]string{"outer": "/sys/fs/cgroup", "sandboxes": filepath.Join(os.Getenv("AGENTBOX_CGROUP_ROOT"), "sandboxes")} {
		values := map[string]string{}
		for _, name := range []string{"cpu.max", "memory.max", "pids.max"} {
			b, err := os.ReadFile(filepath.Join(path, name))
			if err != nil {
				t.Fatal(err)
			}
			values[name] = strings.TrimSpace(string(b))
		}
		limits[scope] = values
	}
	report := map[string]any{"recorded_at": time.Now().UTC().Format(time.RFC3339), "scenario": "capacity", "rates": rates, "samples_per_step": count, "latency_slo_ms": sloMS, "on_time_target": target, "parent_cpu": parentCPU, "configurations": configs, "limits": limits, "runs": runs, "benchmark_binary_sha256": hashFile(executable), "runsc_sha256": hashFile(os.Getenv("AGENTBOX_RUNSC")), "rootfs_image": os.Getenv("AGENTBOX_ROOTFS_IMAGE"), "platform": os.Getenv("AGENTBOX_PLATFORM"),
		"scope":  "bounded exploratory staircase; fixed workload and CPU/memory budgets; stop at first SLO boundary or infrastructure/resource guard; separate offered, delivered and on-time work; not a physical-host maximum or a statistically calibrated operating limit",
		"guards": "85% sandbox/outer memory budget, any new OOM kill, executor unhealthy, missing metrics, infrastructure errors; resource costs include initial/recovery idle and test driver at namespace root; startup/final shutdown excluded"}
	file, err := os.OpenFile(filepath.Join(output, "capacity.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0644); err != nil {
		file.Close()
		t.Fatal(err)
	}
	encodeErr := json.NewEncoder(file).Encode(report)
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatal(encodeErr, closeErr)
	}
	for _, run := range runs {
		if run.EndReason != "slo_boundary" && run.EndReason != "tested_upper_bound_passed" {
			t.Errorf("%s scan invalid: %s; raw results retained", run.Mode, run.EndReason)
		}
	}
}
