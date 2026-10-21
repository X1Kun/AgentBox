//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

func warmEngine(t *testing.T, concurrency, size int) (*sandbox.Engine, sandbox.Config) {
	t.Helper()
	c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"), Platform: os.Getenv("AGENTBOX_PLATFORM"), Concurrency: concurrency, Mode: "warm", WarmPoolSize: size, MaxSandboxes: 4, QueueTimeout: 5 * time.Second}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return e, c
}

func waitRuntime(t *testing.T, predicate func() bool) {
	t.Helper()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runtime condition did not converge")
}

func assertRuntimeEmpty(t *testing.T, c sandbox.Config) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(c.StateDir, "bundles"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("bundle residue: %v %v", entries, err)
	}
	entries, err = os.ReadDir(filepath.Join(c.CgroupRoot, "sandboxes"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("cgroup residue: %s", entry.Name())
		}
	}
}

func TestDynamicInventoryRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runsc and delegated cgroups")
	}
	// Hold replacement creation, making a real inventory wait rather than
	// expanding merely because a task consumed one normal READY instance.
	dir := t.TempDir()
	marker, release, wrapper := filepath.Join(dir, "first-create"), filepath.Join(dir, "release-create"), filepath.Join(dir, "runsc-wrapper")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = create ] && [ -f " + quote(marker) + " ]; then\n  while [ ! -f " + quote(release) + " ]; do sleep 0.01; done\n fi\ndone\nexec " + quote(os.Getenv("AGENTBOX_RUNSC")) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := sandbox.Config{Runsc: wrapper, Rootfs: os.Getenv("AGENTBOX_ROOTFS"),
		StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"),
		Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: "warm", Concurrency: 1, MaxSandboxes: 4,
		DynamicWarmPool: true, WarmMinSize: 1, WarmMaxSize: 2,
		WarmAdjustInterval: 100 * time.Millisecond, WarmDownscaleWindow: 300 * time.Millisecond, QueueTimeout: 3 * time.Second}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	// Unblock a reserved create before cleanup joins the background workers.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.Status(); s.Target != 1 || s.Inventory == nil || !s.Inventory.Enabled {
		t.Fatalf("dynamic startup: %+v", s)
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	first, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('normal-consumption')", TimeoutMS: 5000})
	if err != nil || first.Status != "succeeded" || !first.CleanupComplete {
		t.Fatalf("initial execution: %+v %v", first, err)
	}
	if s := e.Status(); s.Target != 1 || s.Inventory.InventoryStalls != 0 {
		t.Fatal("normal consumption triggered expansion")
	}
	waitRuntime(t, func() bool { return e.Status().Creating > 0 })
	type outcome struct {
		result api.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('dynamic-ok')", TimeoutMS: 5000})
		done <- outcome{r, err}
	}()
	waitRuntime(t, func() bool { return e.Status().Target == 2 })
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.result.Status != "succeeded" || r.result.Stdout != "dynamic-ok\n" || !r.result.CleanupComplete {
			t.Fatalf("dynamic execution: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dynamic task did not finish")
	}
	waitRuntime(t, func() bool {
		s := e.Status()
		return s.Target == 1 && s.Idle == 1 && s.Total == 1 && s.Inventory.ShrinkSucceeded == 1
	})
	s := e.Status()
	if s.Total > s.MaxSandboxes || s.Inventory.ScaleUps != 1 || s.Inventory.ScaleDowns != 1 || s.Inventory.ShrinkFailures != 0 || s.Inventory.InventoryStalls != 1 {
		t.Fatalf("dynamic accounting: %+v", s)
	}
	t.Logf("inventory: %+v", *s.Inventory)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, c)
}

func TestWarmRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" || os.Getenv("AGENTBOX_EXECUTION_MODE") != "warm" {
		t.Skip("requires isolated warm runtime environment")
	}
	t.Run("close_during_background_creation", func(t *testing.T) {
		for round := 0; round < 3; round++ {
			e, c := warmEngine(t, 1, 4)
			waitRuntime(t, func() bool { return e.Status().Creating > 0 })
			if err := e.Close(); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			assertRuntimeEmpty(t, c)
			if status := e.Status(); status.Total != 0 || status.Creating != 0 {
				t.Fatalf("round %d retained resources: %+v", round, status)
			}
		}
	})
	t.Run("concurrent_unique_fresh_instances", func(t *testing.T) {
		e, c := warmEngine(t, 2, 2)
		var workers sync.WaitGroup
		results := make(chan api.Result, 8)
		stop := make(chan struct{})
		violations := make(chan api.StatusReport, 1)
		go func() {
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					s := e.Status()
					if s.Total > s.MaxSandboxes {
						select {
						case violations <- s:
						default:
						}
					}
				}
			}
		}()
		for i := 0; i < 8; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "import os\nassert not os.path.exists('/workspace/marker')\nopen('/workspace/marker','w').write('private')\nprint('fresh')"})
				if err != nil {
					t.Error(err)
				}
				results <- r
			}()
		}
		workers.Wait()
		close(stop)
		close(results)
		ids := map[string]bool{}
		for r := range results {
			if r.Status != "succeeded" || r.Stdout != "fresh\n" || !r.CleanupComplete || ids[r.ExecutionID] {
				t.Fatalf("invalid/reused execution: %+v", r)
			}
			ids[r.ExecutionID] = true
		}
		select {
		case s := <-violations:
			t.Fatalf("capacity exceeded: %+v", s)
		default:
		}
		waitRuntime(t, func() bool { return e.Status().Idle == 2 })
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		assertRuntimeEmpty(t, c)
	})
	t.Run("queued_cancel_and_close_cancel_active_work", func(t *testing.T) {
		e, c := warmEngine(t, 1, 2)
		type answer struct {
			r   api.Result
			err error
		}
		active := make(chan answer, 1)
		go func() {
			r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "import time; time.sleep(20)", TimeoutMS: 30000})
			active <- answer{r, err}
		}()
		waitRuntime(t, func() bool { return e.Status().Running == 1 })
		ctx, cancel := context.WithCancel(context.Background())
		queued := make(chan answer, 1)
		go func() {
			r, err := e.Execute(ctx, api.Request{Runtime: "python", Code: "raise AssertionError('cancelled task ran')"})
			queued <- answer{r, err}
		}()
		waitRuntime(t, func() bool { return e.Status().Queued == 1 })
		cancel()
		a := <-queued
		if a.err != nil || a.r.Status != "cancelled" || a.r.ExecutionID != "" {
			t.Fatalf("queued cancel: %+v", a)
		}
		waitRuntime(t, func() bool { return e.Status().QueuedBytes == 0 && e.Status().Queued == 0 })
		go func() {
			r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "raise AssertionError('shutdown task ran')"})
			queued <- answer{r, err}
		}()
		waitRuntime(t, func() bool { return e.Status().Queued == 1 })
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		a = <-active
		if a.err != nil || a.r.Status != "cancelled" || !a.r.CleanupComplete {
			t.Fatalf("active close: %+v", a)
		}
		a = <-queued
		if a.err != nil && !errors.Is(a.err, sandbox.ErrUnhealthy) {
			t.Fatalf("queued close: %+v", a)
		}
		assertRuntimeEmpty(t, c)
	})
	t.Run("dead_idle_instance_is_destroyed_and_replenished", func(t *testing.T) {
		e, c := warmEngine(t, 1, 1)
		entries, err := os.ReadDir(filepath.Join(c.StateDir, "bundles"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("inventory: %v %v", entries, err)
		}
		id := entries[0].Name()
		if err := os.WriteFile(filepath.Join(c.CgroupRoot, "sandboxes", id, "cgroup.kill"), []byte("1"), 0644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('must-not-run')"})
		if err == nil || r.ExecutionID != id || r.Status != "infrastructure_error" || !r.CleanupComplete || r.Stdout != "" {
			t.Fatalf("dead lease: %+v %v", r, err)
		}
		waitRuntime(t, func() bool { return e.Status().Idle == 1 })
		r, err = e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('replacement')"})
		if err != nil || r.Status != "succeeded" || r.Stdout != "replacement\n" || r.ExecutionID == id {
			t.Fatalf("replacement: %+v %v", r, err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		assertRuntimeEmpty(t, c)
	})
}

// Same resource policy in both modes, including on-demand misses and teardown.
func TestSharedAdmissionRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runtime")
	}
	for _, mode := range []string{"cold", "warm"} {
		t.Run(mode, func(t *testing.T) {
			c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"), Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: mode, Concurrency: 2, CreateConcurrency: 2, WarmPoolSize: 1, MaxSandboxes: 4, QueueTimeout: 5 * time.Second}
			e, err := sandbox.New(c)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if err := e.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			status := e.Status()
			if status.Mode != mode || status.Scheduling.CreationLimit != 2 || status.Memory == nil || status.Memory.ParentBytes+status.Memory.ReserveBytes > status.Memory.CapacityBytes {
				t.Fatalf("invalid budget: %+v", status)
			}
			var workers sync.WaitGroup
			results := make(chan api.Result, 6)
			for i := 0; i < 6; i++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					result, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "import os\nassert not os.path.exists('/workspace/marker')\nopen('/workspace/marker','w').write('private')\nprint(42)", TimeoutMS: 5000})
					if err != nil {
						t.Error(err)
					}
					results <- result
				}()
			}
			workers.Wait()
			close(results)
			ids := map[string]bool{}
			for result := range results {
				if result.Status != "succeeded" || result.Stdout != "42\n" || !result.CleanupComplete || ids[result.ExecutionID] {
					t.Fatalf("invalid fresh execution: %+v", result)
				}
				ids[result.ExecutionID] = true
			}
			if e.Status().Total > 4 {
				t.Fatal("capacity exceeded")
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			assertRuntimeEmpty(t, c)
		})
	}
}

func TestMemoryBudgetRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runtime")
	}
	root := os.Getenv("AGENTBOX_CGROUP_ROOT")
	limitPath := filepath.Join(root, "memory.max")
	previous, err := os.ReadFile(limitPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limitPath, []byte("2147483648"), 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.WriteFile(limitPath, previous, 0600); err != nil {
			t.Error(err)
		}
	}()
	c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: root, Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: "warm", Concurrency: 2, WarmPoolSize: 1, MaxSandboxes: 4}
	if e, err := sandbox.New(c); err == nil {
		e.Close()
		t.Fatal("overcommitted default memory budget was accepted")
	}
	c.Memory = cgroup.MemoryBudget{ParentBytes: 1 << 30, SandboxBytes: 256 << 20, ReserveBytes: 768 << 20}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if s := e.Status(); s.Memory.CapacityBytes != 2<<30 || s.Memory.ParentBytes != 1<<30 {
		t.Fatalf("ancestor ceiling ignored: %+v", s.Memory)
	}
	entries, err := os.ReadDir(filepath.Join(root, "sandboxes"))
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, "sandboxes", entry.Name(), "memory.max"))
		if err != nil || strings.TrimSpace(string(b)) != "268435456" {
			t.Fatalf("wrong instance memory limit: %s %v", b, err)
		}
		checked = true
	}
	if !checked {
		t.Fatal("no READY sandbox to inspect")
	}
	result, err := e.Execute(ctx, api.Request{Runtime: "python", Code: "print(42)"})
	if err != nil || result.Status != "succeeded" || !result.CleanupComplete {
		t.Fatalf("small-budget execution: %+v %v", result, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, c)
}

func TestStartupProbeRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runtime")
	}
	c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: os.Getenv("AGENTBOX_CGROUP_ROOT"), Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: "cold", Concurrency: 1, OutputLimit: 1}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.Ready() || e.Status().Ready {
		t.Fatal("ready before startup probe")
	}
	if _, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(1)"}); !errors.Is(err, sandbox.ErrUnhealthy) {
		t.Fatalf("admitted before probe: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.Ready() {
		t.Fatal("not ready after successful probe")
	}
	if s := e.Status(); s.Total != 0 || s.CreationCount != 0 {
		t.Fatalf("probe polluted request inventory: %+v", s)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, c)
	// Fail exec while allowing create/delete; startup must remain closed and clean.
	wrapper := filepath.Join(t.TempDir(), "runsc-fail-exec")
	script := "#!/bin/sh\nfor arg do if [ \"$arg\" = exec ]; then exit 77; fi; done\nexec '" + strings.ReplaceAll(c.Runsc, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Runsc = wrapper
	c.StateDir = filepath.Join(t.TempDir(), "state")
	bad, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.Start(context.Background()); err == nil {
		t.Fatal("startup accepted broken exec")
	}
	if bad.Ready() {
		t.Fatal("failed probe opened admission")
	}
	if err := bad.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, c)

	// Shutdown must join an already-started metadata transaction and its cleanup.
	marker := filepath.Join(t.TempDir(), "probe-create-started")
	cancelWrapper := filepath.Join(t.TempDir(), "runsc-slow-create")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	script = "#!/bin/sh\nfor arg do if [ \"$arg\" = create ]; then : > " + quote(marker) + "; sleep 0.2; fi; done\nexec " + quote(os.Getenv("AGENTBOX_RUNSC")) + " \"$@\"\n"
	if err := os.WriteFile(cancelWrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.Runsc = cancelWrapper
	c.StateDir = filepath.Join(t.TempDir(), "state")
	stopping, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer stopping.Close()
	started := make(chan error, 1)
	go func() { started <- stopping.Start(context.Background()) }()
	waitRuntime(t, func() bool { _, err := os.Stat(marker); return err == nil })
	if err := stopping.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-started; err == nil {
		t.Fatal("shutdown did not cancel startup")
	}
	if stopping.Ready() {
		t.Fatal("shutdown probe opened readiness")
	}
	assertRuntimeEmpty(t, c)
}
