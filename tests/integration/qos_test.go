//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/cgroup"
	"github.com/X1Kun/AgentBox/internal/qos"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

// Functional controller verification: deterministic pressure samples drive a
// real delegated cpu.max. This is not a latency or performance experiment.
func TestCPUControlCgroup(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated delegated cgroup")
	}
	root := os.Getenv("AGENTBOX_CGROUP_ROOT")
	m, err := cgroup.New(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			initial, err := m.ReadCPUQuota()
			if err != nil {
				t.Fatal(err)
			}
			if initial.Quota != 80000 {
				_, err = m.SetCPUQuota(initial, 80000)
				if err != nil {
					t.Fatal(err)
				}
			}
			c, err := qos.NewCPUController(mode, qos.CPUConfig{}, m, nil)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now()
			var report qos.ControlReport
			for i := 0; i <= 10; i++ {
				now := at.Add(time.Duration(i) * 100 * time.Millisecond)
				s := qos.Snapshot{Sequence: uint64(i + 1), CollectedAt: now, Interval: 100 * time.Millisecond, Sources: []qos.Observation{
					{Source: qos.Source{Name: "protected"}, Available: true, Delta: qos.Delta{Valid: true, CPUSomePercent: 40}},
					{Source: qos.Source{Name: "sandboxes"}, Available: true, Delta: qos.Delta{Valid: true, CPUUsageCores: 1, CPUThrottledPeriods: 1}},
				}}
				report = c.Update(s, now)
			}
			actual, err := m.ReadCPUQuota()
			if err != nil {
				t.Fatal(err)
			}
			want := uint64(80000)
			if mode == "enforce" {
				want = 64000
			}
			if actual.Quota != want || report.SuggestedCPU != 3.2 || report.Applied != (mode == "enforce") {
				t.Fatalf("kernel control: %+v %+v", actual, report)
			}
			t.Logf("mode=%s quota=%d period=%d applied=%v", mode, actual.Quota, actual.Period, report.Applied)
		})
	}
}

func TestCPUQoSRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runsc environment")
	}
	root := os.Getenv("AGENTBOX_CGROUP_ROOT")
	protected := filepath.Join(filepath.Dir(root), "qos-core")
	if err := os.MkdirAll(protected, 0755); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			c := sandbox.Config{Logger: slog.New(slog.NewJSONHandler(os.Stderr, nil)), Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: root, Platform: os.Getenv("AGENTBOX_PLATFORM"), Mode: "cold", Concurrency: 1,
				QoS: qos.Config{Mode: mode, CPUControl: true, ProtectedCgroup: protected, CPU: qos.CPUConfig{MaxCPU: 1}}}
			e, err := sandbox.New(c)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := e.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := e.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitRuntime(t, func() bool { return e.Status().QoS.CPUControl != nil })
			status := e.Status()
			want := 4.0
			if mode == "enforce" {
				want = 1
			}
			if status.QoS.Mode != mode || status.QoS.CPUControl.ActualCPU == nil || *status.QoS.CPUControl.ActualCPU != want {
				t.Fatalf("status wiring: %+v", status.QoS)
			}
			r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "import os\nassert not os.path.exists('/tests/integration.test')\nassert not os.path.exists('/runtime/runsc')\nassert not os.path.exists('/sys/fs/cgroup/cgroup.procs')\nprint('qos-ok')", TimeoutMS: 5000})
			if err != nil || r.Status != "succeeded" || r.Stdout != "qos-ok\n" || !r.CleanupComplete || len(r.CleanupPhases) != 5 {
				t.Fatalf("execution: %+v %v", r, err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			assertRuntimeEmpty(t, c)
		})
	}
}

// Deterministic feedback, actual cgroup writes: validates mechanism, not SLO gains.
func TestBorrowedCPUControlCgroup(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated cgroup")
	}
	m, err := cgroup.New(os.Getenv("AGENTBOX_CGROUP_ROOT"), false)
	if err != nil {
		t.Fatal(err)
	}
	c, err := qos.NewCPUController("enforce", qos.CPUConfig{MinCPU: .5, BaseCPU: 1, MaxCPU: 2}, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := m.ReadCPUQuota()
	if err != nil || initial.Quota != 20000 {
		t.Fatalf("initial borrowed budget: %+v %v", initial, err)
	}
	at := time.Now()
	var report qos.ControlReport
	for i := 0; i <= 25; i++ {
		now := at.Add(time.Duration(i) * 100 * time.Millisecond)
		snapshot := qos.Snapshot{Sequence: uint64(i + 1), CollectedAt: now, Interval: 100 * time.Millisecond, Sources: []qos.Observation{
			{Source: qos.Source{Name: "protected"}, Available: true, Delta: qos.Delta{Valid: true, CPUSomePercent: 0}},
			{Source: qos.Source{Name: "sandboxes"}, Available: true, Delta: qos.Delta{Valid: true, CPUUsageCores: 1, CPUThrottledPeriods: 1}},
		}, Business: &qos.BusinessReport{BusinessSample: qos.BusinessSample{SampledAt: now, WindowMS: 1000, Requests: 10}, Available: true, Healthy: true}}
		report = c.Update(snapshot, now)
	}
	if report.ActualCPU == nil || *report.ActualCPU <= 1 {
		t.Fatalf("did not borrow: %+v", report)
	}
	now := at.Add(2600 * time.Millisecond)
	snapshot := qos.Snapshot{Sequence: 27, CollectedAt: now, Interval: 100 * time.Millisecond, Sources: []qos.Observation{
		{Source: qos.Source{Name: "protected"}, Available: true, Delta: qos.Delta{Valid: true}},
		{Source: qos.Source{Name: "sandboxes"}, Available: true, Delta: qos.Delta{Valid: true, CPUUsageCores: 1}},
	}, Business: &qos.BusinessReport{BusinessSample: qos.BusinessSample{SampledAt: now, WindowMS: 1000, Requests: 10, Rejected: 1}, Available: true, Healthy: false}}
	report = c.Update(snapshot, now)
	actual, err := m.ReadCPUQuota()
	if err != nil || actual.Quota != 20000 || report.Reason != "business_unhealthy" {
		t.Fatalf("business rejection did not revoke quota: %+v %+v %v", report, actual, err)
	}
}

func TestBusinessFeedbackRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_INTEGRATION") != "1" {
		t.Skip("requires isolated runtime")
	}
	root := os.Getenv("AGENTBOX_CGROUP_ROOT")
	protected := filepath.Join(filepath.Dir(root), "business-core")
	if err := os.MkdirAll(protected, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "health.json")
	publish := func(at time.Time, rejected uint64) {
		data, _ := json.Marshal(qos.BusinessSample{SampledAt: at, WindowMS: 1000, Requests: 10, Rejected: rejected})
		if err := os.WriteFile(path+".new", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}
	c := sandbox.Config{Runsc: os.Getenv("AGENTBOX_RUNSC"), Rootfs: os.Getenv("AGENTBOX_ROOTFS"), StateDir: filepath.Join(t.TempDir(), "state"), CgroupRoot: root, Platform: os.Getenv("AGENTBOX_PLATFORM"), Concurrency: 1,
		QoS: qos.Config{Mode: "enforce", ProtectedCgroup: protected, BusinessFile: path, CPU: qos.CPUConfig{MinCPU: .5, BaseCPU: 1, MaxCPU: 2}}}
	e, err := sandbox.New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	publish(time.Now(), 1)
	waitRuntime(t, func() bool {
		q := e.Status().QoS
		return q.Business != nil && q.Business.Available && !q.Business.Healthy && q.CPUControl.BaseCPU == 1
	})
	publish(time.Now(), 0)
	waitRuntime(t, func() bool { b := e.Status().QoS.Business; return b != nil && b.Available && b.Healthy })
	publish(time.Now().Add(-10*time.Second), 0)
	waitRuntime(t, func() bool {
		q := e.Status().QoS
		return q.Business != nil && !q.Business.Available && q.CPUControl.Reason == "business_or_pressure_unknown"
	})
	r, err := e.Execute(context.Background(), api.Request{Runtime: "python", Code: "print(42)"})
	if err != nil || r.Status != "succeeded" || !r.CleanupComplete {
		t.Fatalf("guarded execution: %+v %v", r, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, c)
}
