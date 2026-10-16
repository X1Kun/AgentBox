//go:build integration

package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
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
