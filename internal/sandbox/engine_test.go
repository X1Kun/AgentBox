package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/qos"
)

func TestCPUControllerCannotRaiseConfiguredParentBudget(t *testing.T) {
	_, err := New(Config{ParentCPUMilli: 1000, QoS: qos.Config{Mode: "enforce", ProtectedCgroup: "/sys/fs/cgroup/core", CPU: qos.CPUConfig{MaxCPU: 2}}})
	if err == nil || !strings.Contains(err.Error(), "qos-max-cpu") {
		t.Fatalf("controller ceiling not checked before initialization: %v", err)
	}
}

func TestStatusReportPreservesPublicFieldsAndReadiness(t *testing.T) {
	p := newWarmPool(Config{WarmPoolSize: 3, MaxSandboxes: 8}, nil, nil)
	defer p.cancel()
	p.initialized = true
	p.creating = 2
	p.items[&executionState{id: "idle"}] = poolReady
	p.items[&executionState{id: "leased"}] = poolLeased
	p.items[&executionState{id: "running"}] = poolRunning
	p.items[&executionState{id: "destroying"}] = poolDestroying
	p.waiters = []*poolWaiter{{bytes: 100, enteredAt: time.Now()}, {bytes: 200, enteredAt: time.Now()}}
	p.queuedBytes = 300
	p.creationCount = 17
	p.creationFailures = 2
	p.cleanupFailures = 0
	p.creationTotalMS = 123.5
	e := &Engine{pool: p, config: Config{SandboxCPUMilli: 500}}
	data, err := json.Marshal(e.Status())
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	expected := `{"sandbox_cpu_milli":500,"mode":"warm","ready":true,"target":3,"max_sandboxes":8,"creating":2,"idle":1,"leased":1,"running":1,"destroying":1,"total":6,"queued":2,"queued_bytes":300,"creation_count":17,"creation_failures":2,"cleanup_failures":0,"creation_total_ms":123.5}`
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if got["scheduling"] == nil {
		t.Fatal("missing scheduling telemetry")
	}
	delete(got, "scheduling")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status contract changed: %s", data)
	}
	e.broken.Store(true)
	if e.Status().Ready {
		t.Fatal("engine failure hidden by pool readiness")
	}
	e.broken.Store(false)
	p.failed = true
	p.cleanupFailures = 1
	if report := e.Status(); report.Ready || report.CleanupFailures != 1 {
		t.Fatalf("pool failure hidden by engine report: %+v", report)
	}
	p.failed = false
	e.closed.Store(true)
	if e.Status().Ready {
		t.Fatal("closed engine reported ready")
	}
	cold := &Engine{}
	if status := cold.Status(); status.Mode != "cold" || !status.Ready || status.Total != 0 {
		t.Fatalf("cold report: %+v", status)
	}
	cold.closed.Store(true)
	if cold.Status().Ready {
		t.Fatal("closed cold engine reported ready")
	}
}

func cleanupFixture(t *testing.T, failDelete, withSession bool, outputLimit int) (*Engine, *executionState) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "runsc")
	script := "#!/bin/sh\nexit 0\n"
	if failDelete {
		script = "#!/bin/sh\nexit 77\n"
	}
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "bundle")
	if err := os.Mkdir(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	e := &Engine{runtime: runscCLI{binaryPath: binary, stateDir: dir, platform: "kvm"}}
	// Missing cgroups are a supported cleanup state: a prior runtime deletion
	// may already have removed the group. Real kernel cleanup is integration tested.
	instance := &executionState{id: "fixture", bundlePath: bundle, group: &cgroup.Group{CgroupPath: filepath.Join(dir, "removed-cgroup")}, startedAt: time.Now().Add(-time.Second)}
	if withSession {
		var err error
		instance.session, err = newSession(e.runtime, instance.id, bundle, outputLimit)
		if err != nil {
			t.Fatal(err)
		}
	}
	return e, instance
}

func TestDestroyPartialInstanceWithoutTaskResult(t *testing.T) {
	e, instance := cleanupFixture(t, false, false, 0)
	if err := e.destroyInstanceResources(instance); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(instance.bundlePath); !os.IsNotExist(err) {
		t.Fatalf("cleaned intent remains: %v", err)
	}
	if !e.Ready() {
		t.Fatal("successful resource cleanup degraded the engine")
	}
}

func TestFinalizeRetainsOutputAndIntentWhenDeletionFails(t *testing.T) {
	e, instance := cleanupFixture(t, true, true, 32)
	if _, err := instance.session.writers[0].Write([]byte("stdout before failure")); err != nil {
		t.Fatal(err)
	}
	result := api.Result{Status: "succeeded"}
	if err := e.finalizeExecution(instance, &result); err == nil {
		t.Fatal("deletion failure hidden")
	}
	if result.Status != "infrastructure_error" || result.CleanupComplete || result.Stdout != "stdout before failure" || e.Ready() {
		t.Fatalf("failure semantics changed: %+v", result)
	}
	if _, err := os.Stat(instance.bundlePath); err != nil {
		t.Fatal("failed cleanup lost its intent", err)
	}
	for _, writer := range instance.session.writers {
		if _, err := writer.Write([]byte("late")); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("output writer remains open: %v", err)
		}
	}
}

func TestFinalizePreservesOutputLimitAfterStreamsFinish(t *testing.T) {
	e, instance := cleanupFixture(t, false, true, 4)
	if _, err := instance.session.writers[0].Write([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	result := api.Result{Status: "succeeded"}
	if err := e.finalizeExecution(instance, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "output_limit" || !result.OutputTruncated || result.Stdout != "1234" || !result.CleanupComplete {
		t.Fatalf("output finalization changed: %+v", result)
	}
	if result.Timings.TotalMS < 1000 {
		t.Fatal("request start time was lost")
	}
	if _, err := os.Stat(instance.bundlePath); !os.IsNotExist(err) {
		t.Fatalf("cleaned intent remains: %v", err)
	}
	select {
	case <-instance.session.outputLimitExceeded():
	default:
		t.Fatal("output limit notification missing")
	}
}

func TestDeleteTimeoutLeavesFallbackTimeAndFailureEvidence(t *testing.T) {
	e, instance := cleanupFixture(t, false, false, 0)
	var log bytes.Buffer
	e.runtime.logger = slog.New(slog.NewJSONHandler(&log, nil))
	// The CLI is still blocked when its deadline snapshot is captured.
	if err := os.WriteFile(e.runtime.binaryPath, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	group := instance.group.CgroupPath
	if err := os.Mkdir(group, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(group, "cgroup.events"), []byte("populated 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Wait until fallback kill starts, then emulate asynchronous kernel exit.
		for i := 0; i < 700; i++ {
			if _, err := os.Stat(filepath.Join(group, "cgroup.kill")); err == nil {
				time.Sleep(50 * time.Millisecond)
				_ = os.RemoveAll(group)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	err := e.destroyInstanceResources(instance)
	<-done
	if err == nil || !e.broken.Load() {
		t.Fatal("delete timeout must remain a cleanup failure")
	}
	var fallbackOK bool
	for _, phase := range instance.cleanupPhases {
		if phase.Phase == "cgroup_empty_and_remove" {
			fallbackOK = phase.Error == ""
		}
	}
	if !fallbackOK {
		t.Fatalf("fallback had no independent wait budget: %+v", instance.cleanupPhases)
	}
	var snapshot map[string]any
	for _, line := range bytes.Split(log.Bytes(), []byte("\n")) {
		var event map[string]any
		if json.Unmarshal(line, &event) == nil && event["msg"] == "runtime failure snapshot" {
			snapshot, _ = event["snapshot"].(map[string]any)
		}
	}
	if snapshot["operation"] != "delete" || snapshot["processes"] == nil {
		t.Fatalf("missing timeout evidence: %s", log.String())
	}
	if _, err := os.Stat(instance.bundlePath); err != nil {
		t.Fatal("timeout discarded intent", err)
	}
}
