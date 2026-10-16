package sandbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/cgroup"
)

func TestStatusReportPreservesPublicFieldsAndReadiness(t *testing.T) {
	p := newWarmPool(Config{WarmPoolSize: 3, MaxSandboxes: 8}, nil, nil)
	defer p.cancel()
	p.initialized = true
	p.creating = 2
	p.items[&executionState{id: "idle"}] = poolReady
	p.items[&executionState{id: "leased"}] = poolLeased
	p.items[&executionState{id: "running"}] = poolRunning
	p.items[&executionState{id: "destroying"}] = poolDestroying
	p.waiters = []*poolWaiter{{bytes: 100}, {bytes: 200}}
	p.queuedBytes = 300
	p.creationCount = 17
	p.creationFailures = 2
	p.cleanupFailures = 0
	p.creationTotalMS = 123.5
	e := &Engine{pool: p}
	data, err := json.Marshal(e.Status())
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	expected := `{"mode":"warm","ready":true,"target":3,"max_sandboxes":8,"creating":2,"idle":1,"leased":1,"running":1,"destroying":1,"total":6,"queued":2,"queued_bytes":300,"creation_count":17,"creation_failures":2,"cleanup_failures":0,"creation_total_ms":123.5}`
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
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
