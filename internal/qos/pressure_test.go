package qos

import (
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

func TestPressureConfirmationHysteresisUnknownAndRecovery(t *testing.T) {
	p, err := NewPressurePolicy(CPUConfig{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	sequence := uint64(0)
	step := func(tick int, pressure float64, valid bool) PressureReport {
		now := at.Add(time.Duration(tick) * 100 * time.Millisecond)
		sequence++
		s := controlSnapshot(now, sequence, pressure)
		s.Sources[0].Delta.Valid = valid
		// The gate must remain effective after sandbox work drains.
		s.Sources[1].Delta.CPUUsageCores = 0
		return p.Update(s, now)
	}
	if r := step(0, 0, false); r.State != "unknown" || !r.AdmissionOpen {
		t.Fatalf("initial unknown: %+v", r)
	}
	for i := 1; i <= 10; i++ {
		if r := step(i, 40, true); !r.AdmissionOpen {
			t.Fatal("closed before pressure confirmation")
		}
	}
	if r := step(11, 40, true); r.State != "pressured" || r.AdmissionOpen || !r.RefillPaused {
		t.Fatalf("not closed: %+v", r)
	}
	if r := step(12, 15, true); r.AdmissionOpen {
		t.Fatal("hysteresis reopened")
	}
	if r := step(13, 0, false); r.State != "unknown" || r.AdmissionOpen || r.EffectiveState != "pressured" {
		t.Fatalf("unknown lost restriction: %+v", r)
	}
	for i := 14; i < 34; i++ {
		if r := step(i, 0, true); r.AdmissionOpen {
			t.Fatal("reopened before stable duration")
		}
	}
	if r := step(34, 0, true); r.State != "recovering" || !r.AdmissionOpen || r.RefillPaused {
		t.Fatalf("not recovering: %+v", r)
	}
	for i := 35; i <= 44; i++ {
		step(i, 0, true)
	}
	if p.report.State != "normal" {
		t.Fatalf("not normal: %+v", p.report)
	}
}

func TestPressureAndCPUActuatorFailureAreIndependent(t *testing.T) {
	at := time.Now()
	f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}, fail: true}
	c, err := NewCPUController("enforce", CPUConfig{}, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := NewPressurePolicy(CPUConfig{})
	o := newObserver(Config{Interval: 100 * time.Millisecond}, nil, nil)
	o.controller, o.pressure = c, p
	var reports []PressureReport
	if err := o.SetPressureHandler(func(r PressureReport) { reports = append(reports, r); _ = o.Snapshot() }); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= 20; i++ {
		now := at.Add(time.Duration(i) * 100 * time.Millisecond)
		s := controlSnapshot(now, uint64(i+1), 40)
		o.updatePolicies(&s, now)
	}
	if f.writes != 1 || c.report.State != "degraded" || reports[len(reports)-1].AdmissionOpen {
		t.Fatalf("actuator fault disabled gate: %+v", c.report)
	}
	o.snapshot = Snapshot{CollectedAt: at, Pressure: &reports[len(reports)-1]}
	r := o.Snapshot()
	r.Pressure.AdmissionOpen = true
	if o.snapshot.Pressure.AdmissionOpen {
		t.Fatal("pressure snapshot aliased")
	}
	o.Close()
	if err := o.SetPressureHandler(nil); err == nil {
		t.Fatal("registered handler after close")
	}
}

func TestSchedulingPreviewDoesNotInitializeCPUActuator(t *testing.T) {
	o, err := New(Config{Mode: "observe", Scheduling: true, ProtectedCgroup: "/sys/fs/cgroup/core"}, "/sys/fs/cgroup/agentbox-test/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.pressure == nil || o.controller != nil {
		t.Fatal("pressure preview requires no CPU actuator")
	}
	c := Config{Mode: "off", Scheduling: true}
	if err := c.Normalize(); err == nil {
		t.Fatal("accepted scheduling in off mode")
	}
}

func TestRefillOnlyEnforcementDoesNotInitializeCPUActuator(t *testing.T) {
	o, err := New(Config{Mode: "enforce", RefillOnly: true, ProtectedCgroup: "/sys/fs/cgroup/core"}, "/sys/fs/cgroup/agentbox-test/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if o.pressure == nil || o.controller != nil || o.config.CPUControl {
		t.Fatal("refill-only initialized CPU enforcement")
	}
	for _, c := range []Config{{Mode: "off", RefillOnly: true}, {Mode: "enforce", RefillOnly: true, CPUControl: true}, {Mode: "enforce", RefillOnly: true, Scheduling: true}} {
		if err := c.Normalize(); err == nil {
			t.Fatalf("ambiguous scope accepted: %+v", c)
		}
	}
	legacy := Config{Mode: "enforce", ProtectedCgroup: "/sys/fs/cgroup/core"}
	if err := legacy.Normalize(); err != nil || !legacy.CPUControl {
		t.Fatal("legacy enforce no longer enables CPU control", err)
	}
}

func TestSharedPressureStateGovernsCPUBudgetActions(t *testing.T) {
	f := &quotaFixture{value: cgroup.CPUBandwidth{Quota: 80000, Period: 20000}}
	c, err := NewCPUController("enforce", CPUConfig{}, f, nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	step := func(i int, pressure float64, state string) ControlReport {
		now := at.Add(time.Duration(i) * 100 * time.Millisecond)
		s := controlSnapshot(now, uint64(i+1), pressure)
		s.Pressure = &PressureReport{State: state, EffectiveState: state}
		return c.Update(s, now)
	}
	for i := 0; i <= 20; i++ {
		step(i, 40, "normal")
	}
	if f.writes != 0 {
		t.Fatal("CPU action bypassed shared pressure confirmation")
	}
	if r := step(21, 40, "pressured"); !r.Applied || f.writes != 1 {
		t.Fatalf("confirmed pressure did not apply: %+v", r)
	}
	for i := 22; i <= 50; i++ {
		step(i, 0, "pressured")
	}
	if f.writes != 1 {
		t.Fatal("CPU recovered before scheduling pressure cleared")
	}
	if r := step(51, 0, "recovering"); !r.Applied || f.writes != 2 {
		t.Fatalf("confirmed recovery did not apply: %+v", r)
	}
}
