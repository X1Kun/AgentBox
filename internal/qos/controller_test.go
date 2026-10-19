package qos

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

type quotaFixture struct {
	value  cgroup.CPUBandwidth
	writes int
	fail   bool
}

func (f *quotaFixture) ReadCPUQuota() (cgroup.CPUBandwidth, error) { return f.value, nil }
func (f *quotaFixture) SetCPUQuota(expected cgroup.CPUBandwidth, quota uint64) (cgroup.CPUBandwidth, error) {
	f.writes++
	if f.fail {
		return f.value, errors.New("injected write failure")
	}
	if expected != f.value {
		return f.value, errors.New("unexpected budget")
	}
	f.value.Quota = quota
	return f.value, nil
}
func controlSnapshot(at time.Time, seq uint64, pressure float64) Snapshot {
	return Snapshot{Sequence: seq, CollectedAt: at, Interval: 100 * time.Millisecond, Sources: []Observation{
		{Source: Source{Name: "protected"}, Available: true, Delta: Delta{Valid: true, CPUSomePercent: pressure}},
		{Source: Source{Name: "sandboxes"}, Available: true, Delta: Delta{Valid: true, CPUUsageCores: 1, CPUThrottledPeriods: 1}},
	}}
}

func TestCPUControllerShadowAndEnforcement(t *testing.T) {
	for _, mode := range []string{"observe", "enforce"} {
		t.Run(mode, func(t *testing.T) {
			f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}}
			c, err := NewCPUController(mode, CPUConfig{}, f, nil)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now()
			var r ControlReport
			for i := 0; i <= 10; i++ {
				now := at.Add(time.Duration(i) * 100 * time.Millisecond)
				r = c.Update(controlSnapshot(now, uint64(i+1), 30), now)
			}
			if math.Abs(r.SuggestedCPU-3.2) > 1e-9 || r.Action != "decrease" || r.Changes != 1 {
				t.Fatalf("suppression: %+v", r)
			}
			if mode == "observe" {
				if f.writes != 0 || r.Applied || *r.ActualCPU != 4 {
					t.Fatalf("shadow wrote quota: %+v", r)
				}
			} else if f.writes != 1 || !r.Applied || *r.ActualCPU != 3.2 {
				t.Fatalf("enforcement: %+v", r)
			}
			// Recovery requires sustained low pressure and observed budget demand.
			for i := 11; i <= 32; i++ {
				now := at.Add(time.Duration(i) * 100 * time.Millisecond)
				r = c.Update(controlSnapshot(now, uint64(i+1), 0), now)
			}
			if r.SuggestedCPU <= 3.2 || r.SuggestedCPU > 4 {
				t.Fatalf("no bounded recovery: %+v", r)
			}
			writes := f.writes
			now := at.Add(3300 * time.Millisecond)
			s := controlSnapshot(now, 34, 30)
			s.Sources[0].Available = false
			r = c.Update(s, now)
			if r.State != "degraded" || r.Applied || f.writes != writes {
				t.Fatalf("invalid sample caused action: %+v", r)
			}
		})
	}
}

func TestCPUControllerWriteFailureAndExternalChangeStopControl(t *testing.T) {
	for _, external := range []bool{false, true} {
		f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}, fail: !external}
		c, err := NewCPUController("enforce", CPUConfig{}, f, nil)
		if err != nil {
			t.Fatal(err)
		}
		at := time.Now()
		if external {
			f.value.Quota = 40000
		}
		var r ControlReport
		for i := 0; i <= 30; i++ {
			now := at.Add(time.Duration(i) * 100 * time.Millisecond)
			r = c.Update(controlSnapshot(now, uint64(i+1), 30), now)
		}
		if r.State != "degraded" || r.Error == "" || r.Applied {
			t.Fatalf("failure hidden: %+v", r)
		}
		if !external && (f.writes != 1 || r.WriteFailures != 1 || r.ActualCPU != nil) {
			t.Fatalf("failed writes retried or actual fabricated: %+v", r)
		}
		if external && f.writes != 0 {
			t.Fatal("overwrote external controller")
		}
	}
}

func TestAIMDBoundsInvalidDataCooldownAndDemand(t *testing.T) {
	c := CPUConfig{}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	p, _ := NewAIMD(c)
	at := time.Now()
	current := 4.0
	for i := 0; i < 500; i++ {
		now := at.Add(time.Duration(i) * 100 * time.Millisecond)
		d := p.Decide(now, current, PolicySample{Valid: true, ProtectedPressure: 80, SandboxCPU: 1})
		if d.CPU != current {
			p.Commit(now)
			current = d.CPU
		}
		if current < c.MinCPU || current > c.MaxCPU {
			t.Fatal("budget escaped bounds")
		}
	}
	if current != c.MinCPU {
		t.Fatalf("did not reach floor: %v", current)
	}
	for i := 500; i < 600; i++ {
		d := p.Decide(at.Add(time.Duration(i)*100*time.Millisecond), current, PolicySample{Valid: true})
		if d.CPU != current {
			t.Fatal("idle budget increased")
		}
	}
	d := p.Decide(at.Add(2*time.Minute), current, PolicySample{Valid: true, ProtectedPressure: 0, SandboxCPU: 1, Throttled: true})
	if d.State != "degraded" || d.CPU != current {
		t.Fatal("gap treated as stable")
	}
	d = p.Decide(at.Add(2*time.Minute+100*time.Millisecond), current, PolicySample{})
	if d.State != "degraded" || d.CPU != current {
		t.Fatal("invalid data treated as low pressure")
	}
}

func TestCPUConfigRejectsInvalidParameters(t *testing.T) {
	for _, c := range []CPUConfig{{MinCPU: -1}, {MaxCPU: 5}, {MinCPU: 2, MaxCPU: 1}, {HighPressure: 9, LowPressure: 10}, {DecreaseFactor: 1}, {IncreaseCPUPerSecond: -1}, {DecisionInterval: time.Millisecond}, {MinCPU: math.NaN()}} {
		if _, err := NewAIMD(c); err == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
	c := Config{Mode: "enforce", ProtectedCgroup: "/sys/fs/cgroup/core", Interval: time.Second, CPU: CPUConfig{DecisionInterval: 100 * time.Millisecond}}
	if err := c.Normalize(); err == nil {
		t.Fatal("accepted decision interval shorter than sampling")
	}
}

func TestControlValidityAndSnapshotOwnership(t *testing.T) {
	f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}}
	c, err := NewCPUController("enforce", CPUConfig{}, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	s := controlSnapshot(at, 1, 40)
	r := c.Update(s, at)
	if duplicate := c.Update(s, at.Add(2*time.Second)); duplicate.Decisions != r.Decisions || f.writes != 0 {
		t.Fatal("duplicate sample was acted on")
	}
	s = controlSnapshot(at, 2, 40)
	r = c.Update(s, at.Add(time.Second))
	if r.State != "degraded" || f.writes != 0 {
		t.Fatal("stale sample caused action")
	}
	// An unrelated source may be unavailable without invalidating CPU inputs.
	s = controlSnapshot(at.Add(1100*time.Millisecond), 3, 40)
	s.Sources = append(s.Sources, Observation{Source: Source{Name: "manager"}, Error: "missing"})
	if !policySample(s).Valid {
		t.Fatal("optional scope disabled CPU control")
	}
	o := newObserver(Config{Interval: time.Second}, nil, nil)
	actual := 4.0
	o.snapshot = Snapshot{CollectedAt: at, Control: &ControlReport{ActualCPU: &actual, Reason: "original"}}
	copy := o.Snapshot()
	*copy.Control.ActualCPU = 1
	copy.Control.Reason = "mutated"
	if *o.snapshot.Control.ActualCPU != 4 || o.snapshot.Control.Reason != "original" {
		t.Fatal("control snapshot alias")
	}
}

func TestControllerConfiguredMaximumAndQuotaResolution(t *testing.T) {
	f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}}
	if _, err := NewCPUController("enforce", CPUConfig{MaxCPU: 1}, f, nil); err != nil || f.value.Quota != 20000 || f.writes != 1 {
		t.Fatalf("initial cap: %+v %v", f, err)
	}
	f = &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}}
	if _, err := NewCPUController("observe", CPUConfig{MaxCPU: 1}, f, nil); err != nil || f.writes != 0 {
		t.Fatal("shadow initialization wrote")
	}
	if _, err := NewCPUController("enforce", CPUConfig{MinCPU: .05001, MaxCPU: .05001}, f, nil); err == nil {
		t.Fatal("accepted unrepresentable CPU bounds")
	}
}
