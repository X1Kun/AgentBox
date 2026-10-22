//go:build integration

package benchmarks

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/mockcore"
	"github.com/X1Kun/AgentBox/internal/qos"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

const coreRate = 60
const coreIterations = 50000
const coreDeadlineMS = 50.0

type coreSample struct {
	Sequence, HTTP                             int
	ScheduledMS, StartedMS, FinishedMS, WallMS float64
	ScheduledWallMS, WorkMS                    float64
	Dropped                                    bool
	Checksum, Error                            string
}

type coreSummary struct {
	Offered, Completed, Rejected, Errors, ClientDropped, OnTime int
	OnTimeFraction                                              float64
	SuccessP50MS, SuccessP99MS                                  *float64
	SendLagP99MS                                                float64
}

func coreStats(rows []coreSample) coreSummary {
	s := coreSummary{Offered: len(rows)}
	var latency, lag []float64
	for _, r := range rows {
		lag = append(lag, max(0, r.StartedMS-r.ScheduledMS))
		switch {
		case r.Dropped:
			s.ClientDropped++
		case r.HTTP == 503:
			s.Rejected++
		case r.HTTP != 200 || r.Error != "":
			s.Errors++
		default:
			s.Completed++
			latency = append(latency, r.ScheduledWallMS)
			if r.ScheduledWallMS <= coreDeadlineMS {
				s.OnTime++
			}
		}
	}
	if len(latency) > 0 {
		p50, p99 := percentile(latency, .5), percentile(latency, .99)
		s.SuccessP50MS, s.SuccessP99MS = &p50, &p99
	}
	if len(rows) > 0 {
		s.OnTimeFraction = float64(s.OnTime) / float64(len(rows))
	}
	s.SendLagP99MS = percentile(lag, .99)
	return s
}

func inWaveArrivals(ms float64, trace waveTrace) bool {
	for _, w := range trace.Waves {
		if ms >= float64(w.StartMS) && ms < float64(w.ArrivalsUntilMS) {
			return true
		}
	}
	return false
}

// Fixed open-loop Core traffic is independent of sandbox response completion.
// Bounded client work is reported as drops rather than silently queued/retried.
func replayCore(origin time.Time, trace waveTrace, client *http.Client, checksum string) []coreSample {
	rows := make([]coreSample, int(trace.DurationMS)*coreRate/1000)
	permits := make(chan struct{}, 32)
	var workers sync.WaitGroup
	for i := range rows {
		scheduled := origin.Add(time.Duration(i) * time.Second / coreRate)
		if delay := time.Until(scheduled); delay > 0 {
			time.Sleep(delay)
		}
		row := coreSample{Sequence: i, ScheduledMS: float64(scheduled.Sub(origin)) / float64(time.Millisecond), StartedMS: msSince(origin)}
		select {
		case permits <- struct{}{}:
		default:
			row.Dropped, row.Error = true, "core client capacity exhausted"
			row.FinishedMS = msSince(origin)
			rows[i] = row
			continue
		}
		workers.Add(1)
		go func(i int, row coreSample) {
			defer workers.Done()
			defer func() { <-permits }()
			start := time.Now()
			row.StartedMS = float64(start.Sub(origin)) / float64(time.Millisecond)
			response, err := client.Get("http://127.0.0.1:8081/work")
			if err != nil {
				row.Error = err.Error()
			} else {
				row.HTTP = response.StatusCode
				var body struct {
					Iterations int     `json:"iterations"`
					Checksum   string  `json:"checksum"`
					WorkMS     float64 `json:"work_ms"`
				}
				err := json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&body)
				response.Body.Close()
				if row.HTTP == 200 {
					row.WorkMS, row.Checksum = body.WorkMS, body.Checksum
					if err != nil || body.Iterations != coreIterations || body.Checksum != checksum {
						row.Error = "invalid core fixed-work response"
					}
				}
			}
			row.FinishedMS = msSince(origin)
			row.WallMS = float64(time.Since(start)) / float64(time.Millisecond)
			row.ScheduledWallMS = row.FinishedMS - row.ScheduledMS
			rows[i] = row
		}(i, row)
	}
	workers.Wait()
	return rows
}

func writeCoreCSV(t *testing.T, output string, rows []coreSample) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(output, "core.csv"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"sequence", "scheduled_ms", "started_ms", "finished_ms", "http_status", "client_drop", "wall_ms", "scheduled_wall_ms", "work_ms", "checksum", "error"})
	for _, r := range rows {
		_ = w.Write([]string{strconv.Itoa(r.Sequence), fmt.Sprint(r.ScheduledMS), fmt.Sprint(r.StartedMS), fmt.Sprint(r.FinishedMS), strconv.Itoa(r.HTTP), strconv.FormatBool(r.Dropped), fmt.Sprint(r.WallMS), fmt.Sprint(r.ScheduledWallMS), fmt.Sprint(r.WorkMS), r.Checksum, r.Error})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
}

func startBenchmarkCore(t *testing.T, group, output string) {
	t.Helper()
	if err := os.Mkdir(group, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"memory.max": "268435456", "memory.swap.max": "0", "pids.max": "64"} {
		if err := os.WriteFile(filepath.Join(group, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Trusted binary faults/copying occur outside measurement, as with runsc.
	binary, err := os.ReadFile("/tests/mockcore")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mockcore")
	if err := os.WriteFile(path, binary, 0755); err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(group)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	log, err := os.OpenFile(filepath.Join(output, "core.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-iterations=50000", "-concurrency=4", "-listen=127.0.0.1:8081")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd())}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Error("Core process exited abnormally", err)
			}
		case <-time.After(8 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("Core exceeded shutdown budget")
		}
		log.Close()
		if err := os.Remove(group); err != nil {
			t.Error("Core cgroup cleanup", err)
		}
	})
	membership, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", cmd.Process.Pid))
	if err != nil || strings.TrimSpace(string(membership)) != "0::/benchmark-mixed/core" {
		t.Fatal("Core was not placed in the shared competition group", err)
	}
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if response, err := client.Get("http://127.0.0.1:8081/healthz"); err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Core startup failed; see core.log")
}

type refillCoreRun struct {
	Repetition           int
	Profile              string
	Config               sandbox.Config
	StartupMS            float64
	Core                 coreSummary
	CoreDuringArrivals   coreSummary
	CoreOutsideArrivals  coreSummary
	Sandboxes            waveRun
	RefillAttempts       uint64
	RefillWithForeground uint64
	CleanupComplete      bool
	ParentCPUStart       float64
	ParentCPUFinal       float64
	QoSFinal             *qosReading
	CoreTargetPassed     bool
	ObservationFrames    int
	FreshProtectedFrames int
	RefillPausedFrames   int
}

func coreTargetPassed(windows ...coreSummary) bool {
	for _, s := range windows {
		if s.Offered == 0 || s.OnTimeFraction < .99 || s.SuccessP99MS == nil || *s.SuccessP99MS > coreDeadlineMS {
			return false
		}
	}
	return true
}

type refillProfile struct {
	Name, Mode, Policy, Control string
	ParentCPU                   float64
}

func refillProfiles(comparison string) []refillProfile {
	if comparison == "protection" {
		return []refillProfile{{"baseline", "cold", "slots", "observe", 2}, {"idle-2cpu", "warm", "idle", "observe", 2}, {"idle-1cpu", "warm", "idle", "observe", 1}, {"psi-2cpu", "warm", "slots", "refill-only", 2}, {"psi-1cpu", "warm", "slots", "refill-only", 1}, {"adaptive", "warm", "slots", "adaptive", 2}}
	}
	return []refillProfile{{"baseline", "cold", "slots", "off", 2}, {"cold", "cold", "slots", "off", 2}, {"warm-slots", "warm", "slots", "off", 2}, {"warm-idle", "warm", "idle", "off", 2}}
}

func configureRefillProfile(c *sandbox.Config, p refillProfile, coreGroup string) {
	c.Mode, c.WarmRefillPolicy = p.Mode, p.Policy
	c.ParentCPUMilli = int64(p.ParentCPU * 1000)
	if p.Control == "off" {
		return
	}
	c.QoS = qos.Config{Mode: "observe", ProtectedCgroup: coreGroup, Interval: 100 * time.Millisecond, CPU: qos.CPUConfig{MinCPU: 1, MaxCPU: 2, LowPressure: 10, HighPressure: 20, DecisionInterval: time.Second, StableFor: 2 * time.Second, DecreaseFactor: .8, IncreaseCPUPerSecond: .25}}
	if p.Control == "refill-only" {
		c.QoS.Mode, c.QoS.RefillOnly = "enforce", true
	}
	if p.Control == "adaptive" {
		c.QoS.Mode, c.QoS.CPUControl, c.QoS.Scheduling = "enforce", true, true
	}
}

func TestRefillCoreComparison(t *testing.T) {
	if os.Getenv("AGENTBOX_BENCHMARK") != "1" || os.Getenv("AGENTBOX_BENCH_SCENARIO") != "refill-core" {
		t.Skip("requires explicit refill-core comparison")
	}
	membership, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || strings.TrimSpace(string(membership)) != "0::/test-manager" || os.Getenv("AGENTBOX_CGROUP_ROOT") != "/sys/fs/cgroup/agentbox" || os.Getenv("AGENTBOX_BENCH_ENVIRONMENT") != "isolated-docker" {
		t.Fatal("requires disposable private-cgroup launcher")
	}
	output := os.Getenv("AGENTBOX_BENCH_OUTPUT")
	if output == "" {
		t.Fatal("new output directory required")
	}
	rootCPU, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil || strings.TrimSpace(string(rootCPU)) != "600000 100000" {
		t.Fatal("refill-core requires its fixed six-CPU outer budget")
	}
	shared := "/sys/fs/cgroup/benchmark-mixed"
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"cpu.max": "200000 100000", "memory.max": "5368709120", "memory.swap.max": "0", "pids.max": "2112", "cgroup.subtree_control": "+cpu +memory +pids"} {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	engineRoot := filepath.Join(shared, "agentbox")
	if err := os.Mkdir(engineRoot, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, path := range []string{filepath.Join(engineRoot, "sandboxes"), engineRoot, shared} {
			if err := os.Remove(path); err != nil {
				t.Error("shared cgroup cleanup", path, err)
			}
		}
	})
	coreGroup := filepath.Join(shared, "core")
	startBenchmarkCore(t, coreGroup, output)
	checksum, err := mockcore.Work(context.Background(), coreIterations)
	if err != nil {
		t.Fatal(err)
	}
	trace := makeWaveTrace(number(t, "AGENTBOX_BENCH_WAVE_CYCLES", 2, 1, 5))
	repetitions := number(t, "AGENTBOX_REFILL_REPETITIONS", 2, 1, 3)
	comparison := os.Getenv("AGENTBOX_REFILL_COMPARISON")
	if comparison == "" {
		comparison = "timing"
	}
	if comparison != "timing" && comparison != "protection" {
		t.Fatal("invalid refill comparison")
	}
	transport := &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64}
	coreClient := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	var runs []refillCoreRun
	executable, _ := os.Executable()
	report := map[string]any{"recorded_at": time.Now().UTC().Format(time.RFC3339), "scenario": "refill-core", "trace": trace, "core_rate": coreRate, "core_iterations": coreIterations, "core_checksum": checksum, "core_deadline_ms": coreDeadlineMS, "outer_cpu_max": strings.TrimSpace(string(rootCPU)), "shared_cpu_max": "200000 100000", "parent_cpu_max": "40000 20000", "qos": "off", "order": "baseline/cold/warm-slots/warm-idle; reversed on alternating repetitions", "scope": "Core and sandbox Sentry/Gofer share a 2-CPU group; driver and runtime control CLIs outside it, within outer 6 CPU/5GiB; same open-loop arrivals, no retries/READY barriers; Core failures/drops counted; startup/final shutdown outside measurement; not a physical-host capacity or production SLO claim", "benchmark_sha256": hashFile(executable), "mockcore_sha256": hashFile("/tests/mockcore"), "runsc_sha256": hashFile(os.Getenv("AGENTBOX_RUNSC")), "rootfs_image": os.Getenv("AGENTBOX_ROOTFS_IMAGE"), "platform": os.Getenv("AGENTBOX_PLATFORM")}
	report["comparison"], report["profiles"] = comparison, refillProfiles(comparison)
	report["acceptance"] = "all Core windows (whole, sandbox arrivals, outside arrivals): >=99% on-time and success P99 <=50ms; preserve rejects and sandbox cost; no actuator faults"
	if comparison == "protection" {
		report["qos"], report["parent_cpu_max"], report["order"] = "observe on static controls; enforce on PSI/adaptive profiles", "per-profile; adaptive bounded 1..2 CPU", "listed profiles then reverse order on alternating repetitions"
	}
	limits := map[string]map[string]string{}
	for scope, path := range map[string]string{"outer": "/sys/fs/cgroup", "shared": shared, "core": coreGroup} {
		limits[scope] = map[string]string{}
		for _, name := range []string{"cpu.max", "memory.max", "pids.max"} {
			b, err := os.ReadFile(filepath.Join(path, name))
			if err != nil {
				t.Fatal(err)
			}
			limits[scope][name] = strings.TrimSpace(string(b))
		}
	}
	report["limits"] = limits
	writeReport := func() {
		report["runs"] = runs
		data, err := json.MarshalIndent(report, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(output, "refill-core.json"), append(data, '\n'), 0644)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(output, "refill-core.json"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for rep := 0; rep < repetitions; rep++ {
		order := refillProfiles(comparison)
		if rep%2 == 1 {
			for a, b := 0, len(order)-1; a < b; a, b = a+1, b-1 {
				order[a], order[b] = order[b], order[a]
			}
		}
		for _, plan := range order {
			profile := plan.Name
			dir := filepath.Join(output, fmt.Sprintf("r%d-%s", rep+1, profile))
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatal(err)
			}
			config := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: engineRoot, Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: "cold", WarmRefillPolicy: "slots", WarmPoolSize: 3, Concurrency: number(t, "AGENTBOX_BENCH_EXECUTION_CONCURRENCY", 4, 1, 8), CreateConcurrency: number(t, "AGENTBOX_BENCH_CREATE_CONCURRENCY", 1, 1, 4), MaxSandboxes: number(t, "AGENTBOX_BENCH_MAX_SANDBOXES", 6, 3, 8), SandboxCPUMilli: int64(number(t, "AGENTBOX_BENCH_SANDBOX_CPU_MILLI", 1000, 50, 4000)), QueueTimeout: 100 * time.Millisecond}
			config.QueueSize, config.QueueBytes = 16, 8<<20
			configureRefillProfile(&config, plan, coreGroup)
			engine, err := sandbox.New(config)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = engine.Close() })
			manager := &cgroup.Manager{SandboxParentPath: filepath.Join(config.CgroupRoot, "sandboxes")}
			cpu, err := manager.ReadCPUQuota()
			if err == nil {
				cpu, err = manager.SetCPUQuota(cpu, uint64(plan.ParentCPU*20000))
			}
			if err != nil {
				t.Fatal(err)
			}
			begin := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			err = engine.Start(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			result := refillCoreRun{Repetition: rep + 1, Profile: profile, Config: config, StartupMS: msSince(begin), ParentCPUStart: float64(cpu.Quota) / float64(cpu.Period)}
			server := httptest.NewServer(httpapi.Handler(engine, token, slog.New(slog.NewTextHandler(io.Discard, nil))))
			client := &http.Client{Timeout: 30 * time.Second}
			before := engine.Status().Scheduling
			origin := time.Now()
			coreDone := make(chan []coreSample, 1)
			go func() { coreDone <- replayCore(origin, trace, coreClient, checksum) }()
			actuatorFault := false
			inspect := func(f *poolReading) {
				if f.QoS != nil {
					result.ObservationFrames++
					if !f.QoS.Stale && f.QoS.ProtectedStall != nil {
						result.FreshProtectedFrames++
					}
					if f.QoS.RefillPaused {
						result.RefillPausedFrames++
					}
					actuatorFault = actuatorFault || f.QoS.CPUError != ""
				}
				for _, c := range []struct {
					name, key string
					dst       **uint64
				}{{"cpu.stat", "usage_usec", &f.CoreCPU}, {"cpu.pressure", "some.total", &f.CoreStall}, {"memory.current", "", &f.CoreMemory}} {
					n, err := counter(filepath.Join(coreGroup, c.name), c.key)
					if err != nil {
						f.Error += "; core observation: " + err.Error()
					} else {
						*c.dst = n
					}
				}
				for _, c := range []struct {
					name, key string
					dst       **uint64
				}{{"cpu.stat", "usage_usec", &f.SharedCPU}, {"cpu.stat", "throttled_usec", &f.SharedThrottled}, {"cpu.pressure", "some.total", &f.SharedStall}} {
					n, err := counter(filepath.Join(shared, c.name), c.key)
					if err != nil {
						f.Error += "; shared observation: " + err.Error()
					} else {
						*c.dst = n
					}
				}
			}
			offeredTrace := trace
			if profile == "baseline" {
				offeredTrace.Arrivals = nil
			}
			t.Logf("refill-core repetition=%d profile=%s duration=%dms", rep+1, profile, trace.DurationMS)
			var coreRows []coreSample
			rows, waves := runWavesAt(t, engine, config, client, server.URL, dir, offeredTrace, origin, inspect, func() { coreRows = <-coreDone })
			writeCoreCSV(t, dir, coreRows)
			raw, err := os.OpenFile(filepath.Join(dir, "requests.csv"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				t.Fatal(err)
			}
			if err := raw.Chmod(0644); err != nil {
				t.Fatal(err)
			}
			writeRequests(t, newRequestWriter(raw), rows)
			raw.Close()
			var arrivals, idle []coreSample
			for _, r := range coreRows {
				if inWaveArrivals(r.ScheduledMS, trace) {
					arrivals = append(arrivals, r)
				} else {
					idle = append(idle, r)
				}
			}
			result.Core, result.CoreDuringArrivals, result.CoreOutsideArrivals = coreStats(coreRows), coreStats(arrivals), coreStats(idle)
			result.CoreTargetPassed = coreTargetPassed(result.Core, result.CoreDuringArrivals, result.CoreOutsideArrivals)
			malformed := false
			for _, row := range coreRows {
				malformed = malformed || row.Error == "invalid core fixed-work response"
			}
			result.Sandboxes = waves
			after := engine.Status().Scheduling
			result.QoSFinal = compactQoS(engine.Status().QoS)
			finalCPU, err := manager.ReadCPUQuota()
			if err != nil {
				t.Fatal(err)
			}
			result.ParentCPUFinal = float64(finalCPU.Quota) / float64(finalCPU.Period)
			result.RefillAttempts = after.RefillCreationAttempts - before.RefillCreationAttempts
			result.RefillWithForeground = after.RefillWithForeground - before.RefillWithForeground
			server.Close()
			client.CloseIdleConnections()
			closeErr := engine.Close()
			entries, readErr := os.ReadDir(filepath.Join(config.StateDir, "bundles"))
			result.CleanupComplete = closeErr == nil && readErr == nil && len(entries) == 0
			groups, groupErr := os.ReadDir(manager.SandboxParentPath)
			for _, entry := range groups {
				if entry.IsDir() {
					result.CleanupComplete = false
				}
			}
			result.CleanupComplete = result.CleanupComplete && groupErr == nil
			runs = append(runs, result)
			writeReport()
			t.Logf("refill-core %s Core=%d/%d on-time=%.1f%% sandbox=%d/%d refill=%d foreground-starts=%d", profile, result.Core.Completed, result.Core.Offered, 100*result.Core.OnTimeFraction, waves.Summary.Completed, waves.Summary.Offered, result.RefillAttempts, result.RefillWithForeground)
			if actuatorFault || (comparison == "protection" && result.FreshProtectedFrames == 0) || malformed || !result.CleanupComplete || waves.Error != "" || waves.UnhealthySamples != 0 || waves.Resources.InvalidSamples != 0 || waves.Summary.Errors != 0 || result.Core.ClientDropped != 0 || result.Core.SendLagP99MS > 5 {
				t.Fatal("invalid comparison or cleanup; raw data/report retained")
			}
			if plan.Policy == "idle" && result.RefillWithForeground != 0 {
				t.Fatal("idle policy started background creation during foreground work")
			}
			if plan.Control != "adaptive" && result.ParentCPUFinal != plan.ParentCPU {
				t.Fatal("fixed budget changed during comparison")
			}
			if profile == "baseline" && !result.CoreTargetPassed {
				t.Fatal("Core-only baseline did not meet experimental target; comparison not calibrated")
			}
		}
	}
}
