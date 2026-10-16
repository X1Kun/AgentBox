//go:build integration

package benchmarks

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

const token = "local-benchmark-token-never-donated-to-sandbox"

type sample struct {
	Mode, Scenario                 string
	Sequence, HTTP                 int
	Status, ExecutionID, Error     string
	ScheduledMS, StartedMS, WallMS float64
	Timings                        api.Timings
	Cleanup                        bool
}

type summary struct {
	PoolMinIdle    int     `json:"pool_min_idle"`
	PoolMaxTotal   int     `json:"pool_max_total"`
	PoolMaxQueued  int     `json:"pool_max_queued"`
	Mode           string  `json:"mode"`
	Scenario       string  `json:"scenario"`
	Offered        int     `json:"offered"`
	Accepted       int     `json:"accepted"`
	Completed      int     `json:"completed"`
	Rejected       int     `json:"rejected"`
	Errors         int     `json:"errors"`
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	CompletionRate float64 `json:"completed_per_second"`
	P50MS          float64 `json:"success_wall_p50_ms"`
	P95MS          float64 `json:"success_wall_p95_ms"`
	P99MS          float64 `json:"success_wall_p99_ms"`
	SetupP50MS     float64 `json:"success_setup_p50_ms"`
	ExecuteP50MS   float64 `json:"success_execution_p50_ms"`
}

func number(t *testing.T, key string, defaultValue, minimum, maximum int) int {
	t.Helper()
	if os.Getenv(key) == "" {
		return defaultValue
	}
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < minimum || n > maximum {
		t.Fatalf("%s must be %d..%d", key, minimum, maximum)
	}
	return n
}

func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sort.Float64s(values)
	return values[int(math.Ceil(float64(len(values))*p))-1]
}

func collectSummary(mode, scenario string, rows []sample, elapsed time.Duration) summary {
	s := summary{Mode: mode, Scenario: scenario, Offered: len(rows), ElapsedSeconds: elapsed.Seconds()}
	var wall, setup, execute []float64
	for _, row := range rows {
		if row.HTTP == 200 {
			s.Accepted++
		}
		if row.HTTP == 503 {
			s.Rejected++
			continue
		}
		if row.HTTP != 200 || row.Status != "succeeded" || !row.Cleanup || row.Error != "" {
			s.Errors++
			continue
		}
		s.Completed++
		wall = append(wall, row.WallMS)
		setup = append(setup, row.Timings.SetupMS)
		execute = append(execute, row.Timings.ExecutionMS)
	}
	s.P50MS = percentile(wall, 0.50)
	s.P95MS = percentile(wall, 0.95)
	s.P99MS = percentile(wall, 0.99)
	s.SetupP50MS = percentile(setup, 0.50)
	s.ExecuteP50MS = percentile(execute, 0.50)
	s.CompletionRate = float64(s.Completed) / s.ElapsedSeconds
	return s
}

func runRequest(client *http.Client, url, mode, scenario string, index int, origin, scheduled time.Time) sample {
	start := time.Now()
	row := sample{Mode: mode, Scenario: scenario, Sequence: index, ScheduledMS: float64(scheduled.Sub(origin)) / float64(time.Millisecond), StartedMS: float64(start.Sub(origin)) / float64(time.Millisecond)}
	body := fmt.Sprintf(`{"runtime":"python","code":"print('Hello World')","request_id":"bench-%s-%s-%d"}`, mode, scenario, index)
	request, _ := http.NewRequest("POST", url+"/v1/execute", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		row.Error = err.Error()
	} else {
		row.HTTP = response.StatusCode
		var result api.Result
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if row.HTTP == 200 {
			if readErr != nil {
				row.Error = readErr.Error()
			} else if err := json.Unmarshal(data, &result); err != nil {
				row.Error = err.Error()
			}
			row.Status = result.Status
			row.ExecutionID = result.ExecutionID
			row.Timings = result.Timings
			row.Cleanup = result.CleanupComplete
			if result.Status == "succeeded" && result.Stdout != "Hello World\n" {
				row.Error = "unexpected stdout"
			}
		} else if row.HTTP != 503 {
			row.Error = "unexpected HTTP response"
		}
	}
	row.WallMS = float64(time.Since(start)) / float64(time.Millisecond)
	return row
}

func waitInventory(t *testing.T, engine *sandbox.Engine, target int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !engine.Ready() {
			t.Fatal("executor became unhealthy")
		}
		if engine.Status().Idle >= target {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("inventory did not refill: %+v", engine.Status())
}

func hashFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unavailable"
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestColdWarmHTTPBenchmark(t *testing.T) {
	if os.Getenv("AGENTBOX_BENCHMARK") != "1" {
		t.Skip("requires explicitly authorized delegated runtime benchmark environment")
	}
	count := number(t, "AGENTBOX_BENCH_SAMPLES", 60, 10, 1000)
	rate := number(t, "AGENTBOX_BENCH_RATE", 10, 1, 100)
	output := os.Getenv("AGENTBOX_BENCH_OUTPUT")
	if output == "" {
		t.Fatal("AGENTBOX_BENCH_OUTPUT must name a new output directory")
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
	writer := csv.NewWriter(raw)
	_ = writer.Write([]string{"mode", "scenario", "sequence", "http_status", "status", "execution_id", "scheduled_ms", "started_ms", "wall_ms", "queue_ms", "setup_ms", "execution_ms", "cleanup_ms", "total_ms", "cleanup_complete", "error"})
	var summaries []summary
	var poolStatuses []api.StatusReport
	var configurations []sandbox.Config
	idleMemory := map[string]string{}
	for _, mode := range []string{"cold", "warm"} {
		config := sandbox.Config{
			Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"),
			StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"),
			Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: mode,
			Concurrency: 4, WarmPoolSize: 3, MaxSandboxes: 6, CreateConcurrency: 1,
			QueueSize: 16, QueueBytes: 8 << 20, QueueTimeout: 100 * time.Millisecond, OutputLimit: 1 << 20,
		}
		configurations = append(configurations, config)
		engine, err := sandbox.New(config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = engine.Close() })
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err = engine.Start(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if mode == "warm" {
			waitInventory(t, engine, 3)
		}
		memory, err := os.ReadFile(filepath.Join(config.CgroupRoot, "sandboxes", "memory.current"))
		if err != nil {
			t.Fatal(err)
		}
		idleMemory[mode] = strings.TrimSpace(string(memory))
		server := httptest.NewServer(httpapi.Handler(engine, token, slog.New(slog.NewTextHandler(io.Discard, nil))))
		t.Cleanup(server.Close)
		transport := &http.Transport{MaxIdleConnsPerHost: 128, MaxConnsPerHost: 128}
		client := &http.Client{Transport: transport, Timeout: 45 * time.Second}
		// Exclude two first-use probes from measured populations. Their failures are
		// still fatal; subsequent CSV contains every offered measured request.
		for i := 0; i < 2; i++ {
			if mode == "warm" {
				waitInventory(t, engine, 3)
			}
			row := runRequest(client, server.URL, mode, "probe", i, time.Now(), time.Now())
			if row.HTTP != 200 || row.Error != "" || row.Status != "succeeded" {
				t.Fatalf("probe failed: %+v", row)
			}
		}
		for _, scenario := range []string{"inventory", "burst", "steady"} {
			if mode == "warm" {
				waitInventory(t, engine, 3)
			}
			minimumIdle, maximumTotal, maximumQueued := engine.Status().Idle, 0, 0
			sampleStop, sampleDone := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(sampleDone)
				ticker := time.NewTicker(time.Millisecond)
				defer ticker.Stop()
				for {
					status := engine.Status()
					if status.Idle < minimumIdle {
						minimumIdle = status.Idle
					}
					if status.Total > maximumTotal {
						maximumTotal = status.Total
					}
					if status.Queued > maximumQueued {
						maximumQueued = status.Queued
					}
					select {
					case <-sampleStop:
						return
					case <-ticker.C:
					}
				}
			}()
			origin := time.Now()
			n := count
			if scenario == "burst" {
				n = 32
			}
			rows := make([]sample, n)
			if scenario == "inventory" {
				for i := 0; i < n; i++ {
					if mode == "warm" {
						waitInventory(t, engine, 3)
					}
					rows[i] = runRequest(client, server.URL, mode, scenario, i, origin, time.Now())
				}
			} else {
				var workers sync.WaitGroup
				for i := 0; i < n; i++ {
					scheduled := origin
					if scenario == "steady" {
						scheduled = origin.Add(time.Duration(float64(i) / float64(rate) * float64(time.Second)))
						if delay := time.Until(scheduled); delay > 0 {
							time.Sleep(delay)
						}
					}
					workers.Add(1)
					go func(i int, scheduled time.Time) {
						defer workers.Done()
						rows[i] = runRequest(client, server.URL, mode, scenario, i, origin, scheduled)
					}(i, scheduled)
				}
				workers.Wait()
			}
			elapsed := time.Since(origin)
			close(sampleStop)
			<-sampleDone
			for _, row := range rows {
				fields := []string{row.Mode, row.Scenario, strconv.Itoa(row.Sequence), strconv.Itoa(row.HTTP), row.Status, row.ExecutionID}
				for _, v := range []float64{row.ScheduledMS, row.StartedMS, row.WallMS, row.Timings.QueueMS, row.Timings.SetupMS, row.Timings.ExecutionMS, row.Timings.CleanupMS, row.Timings.TotalMS} {
					fields = append(fields, strconv.FormatFloat(v, 'f', 3, 64))
				}
				fields = append(fields, strconv.FormatBool(row.Cleanup), row.Error)
				if err := writer.Write(fields); err != nil {
					t.Fatal(err)
				}
			}
			writer.Flush()
			if err := writer.Error(); err != nil {
				t.Fatal(err)
			}
			s := collectSummary(mode, scenario, rows, elapsed)
			s.PoolMinIdle, s.PoolMaxTotal, s.PoolMaxQueued = minimumIdle, maximumTotal, maximumQueued
			if mode == "warm" && maximumTotal > config.MaxSandboxes {
				t.Fatal("sampled pool exceeded total cap")
			}
			summaries = append(summaries, s)
			t.Logf("%s/%s offered=%d completed=%d rejected=%d errors=%d success wall p50=%.2fms p99=%.2fms", mode, scenario, s.Offered, s.Completed, s.Rejected, s.Errors, s.P50MS, s.P99MS)
		}
		poolStatuses = append(poolStatuses, engine.Status())
		server.Close()
		transport.CloseIdleConnections()
		if err := engine.Close(); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Join(config.StateDir, "bundles"))
		if err != nil || len(entries) != 0 {
			t.Fatal("benchmark left execution records", err)
		}
	}
	kernel, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	executable, _ := os.Executable()
	versionCommand := exec.Command(os.Getenv("AGENTBOX_RUNSC"), "--version")
	versionCommand.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	version, versionErr := versionCommand.Output()
	if versionErr != nil {
		t.Fatal(versionErr)
	}
	parentLimits := map[string]string{}
	for _, name := range []string{"cpu.max", "memory.high", "memory.max", "memory.swap.max", "pids.max"} {
		value, err := os.ReadFile(filepath.Join(os.Getenv("AGENTBOX_CGROUP_ROOT"), "sandboxes", name))
		if err != nil {
			t.Fatal(err)
		}
		parentLimits[name] = strings.TrimSpace(string(value))
	}
	report := map[string]any{
		"recorded_at": time.Now().UTC().Format(time.RFC3339),
		"environment": os.Getenv("AGENTBOX_BENCH_ENVIRONMENT"),
		"kernel":      strings.TrimSpace(string(kernel)), "logical_cpus": runtime.NumCPU(), "go_version": runtime.Version(),
		"benchmark_binary_sha256": hashFile(executable),
		"runsc_version":           strings.TrimSpace(string(version)), "runsc_sha256": hashFile(os.Getenv("AGENTBOX_RUNSC")),
		"rootfs_image": os.Getenv("AGENTBOX_ROOTFS_IMAGE"), "platform": os.Getenv("AGENTBOX_PLATFORM"),
		"parent_limits": parentLimits, "configurations": configurations,
		"samples_per_inventory_and_steady": count, "burst_requests": 32, "steady_offered_per_second": rate,
		"pool_status_before_close": poolStatuses, "idle_parent_memory_current_bytes": idleMemory,
		"summaries":        summaries,
		"usage_scope":      "whole sandbox lifetime including runtime startup and warm idle; CLI manager excluded",
		"percentile_scope": "successful HTTP requests with confirmed cleanup; rejected and failed counts reported separately",
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(output, "summary.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Chmod(0644); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for _, s := range summaries {
		if s.Errors != 0 {
			t.Errorf("%s/%s had %d errors; raw evidence retained", s.Mode, s.Scenario, s.Errors)
		}
	}
}
