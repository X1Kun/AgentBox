package qos

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

func TestBusinessWindowValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	now := time.Now()
	sample := BusinessSample{SampledAt: now, WindowMS: 1000, Requests: 10}
	write := func(v any) {
		t.Helper()
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(sample)
	if r := readBusiness(path, now); !r.Available || !r.Healthy {
		t.Fatal(r)
	}
	sample.Rejected = 1
	write(sample)
	if r := readBusiness(path, now); !r.Available || r.Healthy {
		t.Fatal(r)
	}
	if r := readBusiness(path, now.Add(3*time.Second)); r.Available {
		t.Fatal("stale success accepted")
	}
	if r := readBusiness(path, now.Add(-time.Second)); r.Available {
		t.Fatal("future timestamp accepted")
	}
	sample.Rejected = 11
	write(sample)
	if r := readBusiness(path, now); r.Available {
		t.Fatal("impossible counters accepted")
	}
	write(map[string]any{"sampled_at": now, "window_ms": 1000})
	if r := readBusiness(path, now); r.Available {
		t.Fatal("missing counters treated as zero")
	}
	write(map[string]string{"token": "must-not-leak"})
	if r := readBusiness(path, now); r.Available || r.Error == "must-not-leak" {
		t.Fatal(r)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if r := readBusiness(link, now); r.Available {
		t.Fatal("symlink accepted")
	}
}

func TestBorrowingStartsConservativeAndRevokesOnBusinessHarm(t *testing.T) {
	for _, mode := range []string{"observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}}
			c, err := NewCPUController(mode, CPUConfig{MinCPU: .5, BaseCPU: 1, MaxCPU: 2}, f, nil)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "enforce" && f.value.Quota != 20000 {
				t.Fatal("borrowed CPU before healthy observations")
			}
			if mode == "observe" && f.writes != 0 {
				t.Fatal("observe wrote initial budget")
			}
			at := time.Now()
			var r ControlReport
			for i := 0; i <= 25; i++ {
				now := at.Add(time.Duration(i) * 100 * time.Millisecond)
				s := controlSnapshot(now, uint64(i+1), 0)
				s.Business = &BusinessReport{BusinessSample: BusinessSample{SampledAt: now, WindowMS: 1000, Requests: 1}, Available: true, Healthy: true}
				r = c.Update(s, now)
			}
			if r.SuggestedCPU <= 1 {
				t.Fatal("healthy demand did not borrow")
			}
			now := at.Add(2600 * time.Millisecond)
			s := controlSnapshot(now, 27, 0)
			s.Business = &BusinessReport{BusinessSample: BusinessSample{SampledAt: now, WindowMS: 1000, Requests: 10, Rejected: 1}, Available: true, Healthy: false}
			r = c.Update(s, now)
			if r.SuggestedCPU != 1 || r.Reason != "business_unhealthy" {
				t.Fatalf("low PSI hid rejection: %+v", r)
			}
			for i := 27; i < 45; i++ {
				now = at.Add(time.Duration(i) * 100 * time.Millisecond)
				s = controlSnapshot(now, uint64(i+1), 0) // producer missing: no recovery
				r = c.Update(s, now)
				if r.SuggestedCPU > 1 {
					t.Fatal("missing telemetry permitted borrowing")
				}
			}
			if mode == "observe" && f.writes != 0 {
				t.Fatal("shadow guard wrote quota")
			}
		})
	}
}

func TestBorrowingRevokesOnInvalidPSIAndRequiresPairedConfig(t *testing.T) {
	p, err := NewAIMD(CPUConfig{MinCPU: .5, BaseCPU: 1, MaxCPU: 2})
	if err != nil {
		t.Fatal(err)
	}
	d := p.Decide(time.Now(), 2, PolicySample{})
	if d.CPU != 1 || d.State != "degraded" {
		t.Fatalf("invalid inputs retained loan: %+v", d)
	}
	for _, c := range []Config{
		{Mode: "enforce", ProtectedCgroup: "/sys/fs/cgroup/core", CPU: CPUConfig{BaseCPU: 1}},
		{Mode: "enforce", ProtectedCgroup: "/sys/fs/cgroup/core", BusinessFile: "/tmp/health"},
		{Mode: "off", BusinessFile: "/tmp/health"},
	} {
		if err := c.Normalize(); err == nil {
			t.Fatal("unpaired guard configuration accepted")
		}
	}
}

func TestBusinessSnapshotOwnershipAndStaleness(t *testing.T) {
	o := newObserver(Config{Interval: time.Second}, nil, nil)
	now := time.Now()
	o.snapshot = Snapshot{CollectedAt: now, Business: &BusinessReport{BusinessSample: BusinessSample{SampledAt: now, WindowMS: 1000}, Available: true, Healthy: true}}
	copy := o.Snapshot()
	copy.Business.Healthy = false
	if !o.snapshot.Business.Healthy {
		t.Fatal("business report alias")
	}
	o.snapshot.Business.SampledAt = now.Add(-3 * time.Second)
	copy = o.Snapshot()
	if copy.Business.Available || copy.Business.Healthy {
		t.Fatal("stale business report stayed healthy")
	}
}

func TestPersistentBusinessHarmReducesBelowBaseWithoutCrossingFloor(t *testing.T) {
	p, err := NewAIMD(CPUConfig{MinCPU: .5, BaseCPU: 1, MaxCPU: 2})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	current := 1.0
	for i := 0; i <= 40; i++ {
		now := at.Add(time.Duration(i) * 100 * time.Millisecond)
		d := p.Decide(now, current, PolicySample{Valid: true, BusinessValid: true, BusinessHealthy: false, SandboxCPU: 1})
		if d.CPU != current {
			p.Commit(now)
			current = d.CPU
		}
		if current < .5 {
			t.Fatal("business guard crossed hard floor")
		}
	}
	if current != .5 {
		t.Fatalf("treated base as a guaranteed SLO: %v", current)
	}
	for i := 41; i < 60; i++ {
		d := p.Decide(at.Add(time.Duration(i)*100*time.Millisecond), current, PolicySample{Valid: true, SandboxCPU: 1, Throttled: true})
		if d.CPU != current {
			t.Fatal("missing business signal recovered CPU")
		}
	}
}
