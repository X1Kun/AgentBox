//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/qos"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

// A trusted test load is confined to 0.05 CPU in the protected cgroup. It
// produces real PSI; no control endpoint or fabricated production metric is used.
func TestPressureSchedulingRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runsc and cgroups")
	}
	root := os.Getenv("AGENTBOX_CGROUP_ROOT")
	core := filepath.Join(filepath.Dir(root), "scheduling-core")
	for _, mode := range []string{"observe", "enforce", "refill-only"} {
		t.Run(mode, func(t *testing.T) {
			if err := os.MkdirAll(core, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(core, "cpu.max"), []byte("1000 20000"), 0644); err != nil {
				t.Fatal(err)
			}
			c := sandbox.Config{Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)), Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: root, Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: "warm", Concurrency: 1, WarmPoolSize: 1, MaxSandboxes: 3, QueueTimeout: 3 * time.Second,
				QoS: qos.Config{Mode: mode, Scheduling: true, ProtectedCgroup: core, Interval: 50 * time.Millisecond, CPU: qos.CPUConfig{MaxCPU: 1, DecisionInterval: 100 * time.Millisecond, StableFor: 200 * time.Millisecond}}}
			if mode == "refill-only" {
				c.QoS.Mode, c.QoS.Scheduling, c.QoS.RefillOnly = "enforce", false, true
			}
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
			waitRuntime(t, func() bool {
				s := e.Status()
				return s.Idle == 1 && s.QoS.Pressure != nil && s.QoS.Pressure.State == "normal"
			})
			type outcome struct {
				result api.Result
				err    error
			}
			active, queued := make(chan outcome, 1), make(chan outcome, 1)
			if mode == "enforce" {
				go func() {
					r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "import time; time.sleep(3); print('inflight-ok')", TimeoutMS: 5000})
					active <- outcome{r, err}
				}()
				waitRuntime(t, func() bool { return e.Status().Running == 1 })
				go func() {
					r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('queued')"})
					queued <- outcome{r, err}
				}()
				waitRuntime(t, func() bool { return e.Status().Queued == 1 })
			}
			// Pass the path as a shell positional parameter; it is not interpolated.
			load := exec.Command("/bin/sh", "-c", `set -e; echo $$ > "$1/cgroup.procs"; while :; do :; done`, "qos-load", core)
			if err := load.Start(); err != nil {
				t.Fatal(err)
			}
			stopped := false
			stopLoad := func() {
				if !stopped {
					_ = load.Process.Kill()
					_ = load.Wait()
					stopped = true
				}
			}
			t.Cleanup(stopLoad)
			waitRuntime(t, func() bool { s := e.Status(); return s.QoS.Pressure.EffectiveState == "pressured" })
			status := e.Status()
			if !status.Ready {
				t.Fatal("pressure was reported as engine failure")
			}
			if mode == "observe" {
				if !status.QoS.Scheduling.AdmissionOpen || status.QoS.Scheduling.RefillPaused {
					t.Fatal("observe enforced scheduling")
				}
				r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('preview-ok')"})
				if err != nil || r.Status != "succeeded" || r.Stdout != "preview-ok\n" || !r.CleanupComplete {
					t.Fatalf("observe execution: %+v %v", r, err)
				}
			} else if mode == "refill-only" {
				if !status.QoS.Scheduling.AdmissionOpen || !status.QoS.Scheduling.RefillPaused || status.QoS.CPUControl != nil {
					t.Fatal("refill-only changed CPU or admission")
				}
				for i := 0; i < 2; i++ {
					r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('refill-only')"})
					if err != nil || r.Status != "succeeded" || !r.CleanupComplete {
						t.Fatalf("ready/demand execution failed: %+v %v", r, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(root, "sandboxes", "cpu.max"))
				if err != nil || string(data) != "80000 20000\n" {
					t.Fatalf("refill-only changed parent CPU: %s %v", data, err)
				}
				if s := e.Status(); s.Idle != 0 || s.Creating != 0 {
					t.Fatal("refilled under pressure")
				}
			} else {
				if status.QoS.Scheduling.AdmissionOpen || !status.QoS.Scheduling.RefillPaused {
					t.Fatal("scheduling gate not enforced")
				}
				_, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('rejected')"})
				if !errors.Is(err, sandbox.ErrPressure) {
					t.Fatalf("new request was not pressure rejected: %v", err)
				}
				if r := <-queued; !errors.Is(r.err, sandbox.ErrPressure) {
					t.Fatalf("queued request: %+v", r)
				}
				if r := <-active; r.err != nil || r.result.Status != "succeeded" || r.result.Stdout != "inflight-ok\n" || !r.result.CleanupComplete {
					t.Fatalf("pressure interrupted inflight task: %+v", r)
				}
				waitRuntime(t, func() bool { return e.Status().Creating == 0 })
				count := e.Status().CreationCount
				time.Sleep(150 * time.Millisecond)
				if e.Status().CreationCount != count {
					t.Fatal("created more instances while paused")
				}
			}
			stopLoad()
			if mode == "enforce" || mode == "refill-only" {
				if err := os.Remove(core); err != nil {
					t.Fatal(err)
				}
				waitRuntime(t, func() bool { s := e.Status(); return s.QoS.Pressure.State == "unknown" })
				if e.Status().QoS.Scheduling.AdmissionOpen {
					if mode != "refill-only" {
						t.Fatal("missing source reopened admission")
					}
				}
				if !e.Status().QoS.Scheduling.RefillPaused {
					t.Fatal("missing source reopened refill")
				}
				if err := os.Mkdir(core, 0755); err != nil {
					t.Fatal(err)
				}
			}
			waitRuntime(t, func() bool {
				s := e.Status()
				return s.QoS.Scheduling.AdmissionOpen && !s.QoS.Scheduling.RefillPaused && s.Idle == 1
			})
			r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print('recovered')"})
			if err != nil || r.Status != "succeeded" || !r.CleanupComplete {
				t.Fatalf("recovered execution: %+v %v", r, err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			assertRuntimeEmpty(t, c)
		})
	}
}
