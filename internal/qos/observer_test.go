package qos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

func TestPressureParsing(t *testing.T) {
	valid := "some total=123 avg300=0.02 avg10=1.25 avg60=0.50 extra=7\nfull avg10=0 avg60=0 avg300=0 total=5\n"
	p, err := ParsePressure(valid)
	if err != nil || p.Some.TotalUsec != 123 || p.Some.Avg10 != 1.25 || !p.HasFull || p.Full.TotalUsec != 5 {
		t.Fatalf("parse: %+v %v", p, err)
	}
	for _, bad := range []string{"", strings.Replace(valid, "avg10=1.25", "avg10=NaN", 1), strings.Replace(valid, "avg10=1.25", "avg10=101", 1), strings.Replace(valid, "total=123", "total=-1", 1), strings.Replace(valid, "avg60=0.50 ", "", 1), valid + "some avg10=0 avg60=0 avg300=0 total=0", strings.Replace(valid, "total=123", "total=123 total=4", 1)} {
		if _, err := ParsePressure(bad); err == nil {
			t.Fatalf("accepted invalid pressure %q", bad)
		}
	}
}

type fixtureReader struct {
	mu       sync.Mutex
	readings map[string]Reading
	failures map[string]error
}

func (f *fixtureReader) Read(s Source) (Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readings[s.Name], f.failures[s.Name]
}
func reading(at time.Time, total uint64) Reading {
	return Reading{At: at, Identity: identity{Device: 1, Inode: 1}, CPU: Pressure{Some: PressureValue{TotalUsec: total}}, Memory: Pressure{Some: PressureValue{TotalUsec: total}, Full: PressureValue{TotalUsec: total / 2}, HasFull: true}, Resources: cgroup.Stats{CPUUsageUsec: total * 2, CPUPeriods: total / 1000, CPUThrottledPeriods: total / 2000, CPUThrottledUsec: total, MemoryHighEvents: total / 1000}}
}
func TestObserverRatesResetAndFailures(t *testing.T) {
	at := time.Now()
	reader := &fixtureReader{readings: map[string]Reading{"core": reading(at, 1000)}, failures: map[string]error{}}
	o := newObserver(Config{Interval: 100 * time.Millisecond}, reader, []Source{{Name: "core", Cgroup: true}})
	first := o.collect().Sources[0]
	if !first.Available || first.Delta.Valid || first.Baseline != "initial" {
		t.Fatalf("first sample: %+v", first)
	}
	// Actual interval is 200ms, not the configured 100ms.
	reader.readings["core"] = reading(at.Add(200*time.Millisecond), 41000)
	sample := o.collect().Sources[0]
	if !sample.Delta.Valid || sample.Delta.CPUSomePercent != 20 || sample.Delta.CPUUsageCores != 0.4 || sample.Delta.CPUPeriods != 40 {
		t.Fatalf("wrong time/counter units: %+v", sample.Delta)
	}
	reader.readings["core"] = reading(at.Add(300*time.Millisecond), 100)
	if s := o.collect().Sources[0]; s.Delta.Valid || s.Baseline != "counter_reset" {
		t.Fatalf("counter reset: %+v", s)
	}
	fresh := reading(at.Add(400*time.Millisecond), 10000)
	fresh.Identity.Inode = 2
	reader.readings["core"] = fresh
	if s := o.collect().Sources[0]; s.Delta.Valid || s.Baseline != "source_changed" {
		t.Fatalf("replacement: %+v", s)
	}
	reader.failures["core"] = errors.New("source missing")
	failed := o.collect()
	if failed.State != "degraded" || failed.Sources[0].Available || failed.Sources[0].Error == "" {
		t.Fatalf("missing source hidden: %+v", failed)
	}
	delete(reader.failures, "core")
	if s := o.collect().Sources[0]; s.Delta.Valid || s.Baseline != "initial" {
		t.Fatalf("read recovery reused stale baseline: %+v", s)
	}
	later := fresh
	later.At = at.Add(2 * time.Second)
	reader.readings["core"] = later
	if s := o.collect().Sources[0]; s.Delta.Valid || s.Baseline != "sample_gap" {
		t.Fatalf("sampling gap hidden: %+v", s)
	}
	if s := o.collect().Sources[0]; s.Delta.Valid || s.Baseline != "invalid_interval" {
		t.Fatalf("zero interval accepted: %+v", s)
	}
}
func TestSnapshotStalenessAndOwnership(t *testing.T) {
	o := newObserver(Config{Interval: 100 * time.Millisecond}, nil, nil)
	o.snapshot = Snapshot{State: "observing", CollectedAt: time.Now().Add(-time.Second), Sources: []Observation{{Available: true, Delta: Delta{Valid: true}}}}
	s := o.Snapshot()
	if !s.Stale || s.Sources[0].Delta.Valid || s.Sources[0].Baseline != "stale" {
		t.Fatalf("old rates remained valid: %+v", s)
	}
	s.Sources[0].Error = "caller mutation"
	if o.snapshot.Sources[0].Error != "" || !o.snapshot.Sources[0].Delta.Valid {
		t.Fatal("snapshot mutated observer state")
	}
}
func TestObserverLifecycleAndConcurrentReads(t *testing.T) {
	r := &fixtureReader{readings: map[string]Reading{"host": reading(time.Now(), 0)}, failures: map[string]error{}}
	o := newObserver(Config{Interval: 50 * time.Millisecond}, r, []Source{{Name: "host"}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o.Start(ctx)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = o.Snapshot()
		}
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for o.Snapshot().CollectedAt.IsZero() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	<-done
	if o.Snapshot().CollectedAt.IsZero() {
		t.Fatal("observer did not sample")
	}
	o.Close()
	o.Close()
	if s := o.Snapshot(); s.State != "stopped" || !s.Stale {
		t.Fatalf("shutdown: %+v", s)
	}
	o.Start(context.Background())
	if o.Snapshot().State != "stopped" {
		t.Fatal("closed observer restarted")
	}
}
func TestObserveConfigurationAndOffMode(t *testing.T) {
	o, err := New(Config{}, "/unused")
	if o != nil || err != nil {
		t.Fatalf("off mode performed initialization: %v %v", o, err)
	}
	for _, c := range []Config{{Mode: "enforce"}, {Mode: "observe"}, {Mode: "observe", ProtectedCgroup: "/etc"}, {Mode: "observe", ProtectedCgroup: "/sys/fs/cgroup/a", Interval: time.Millisecond}} {
		if err := c.Normalize(); err == nil {
			t.Fatalf("invalid config: %+v", c)
		}
	}
	for _, path := range []string{"/sys/fs/cgroup/a", "/sys/fs/cgroup/a/sandboxes", "/sys/fs/cgroup/a/sandboxes/child"} {
		if _, err := New(Config{Mode: "observe", ProtectedCgroup: path}, "/sys/fs/cgroup/a/sandboxes"); err == nil {
			t.Fatal("overlapping protected source", path)
		}
	}
}
func TestFileReaderIsReadOnlyAndDistinguishesMissingData(t *testing.T) {
	dir := t.TempDir()
	data := map[string]string{
		"cpu.pressure":    "some avg10=1 avg60=2 avg300=3 total=200\nfull avg10=0 avg60=0 avg300=0 total=20\n",
		"memory.pressure": "some avg10=0 avg60=0 avg300=0 total=0\nfull avg10=0 avg60=0 avg300=0 total=0\n",
		"cpu.stat":        "usage_usec 10\nnr_periods 1\nnr_throttled 0\nthrottled_usec 0\n",
		"cpu.max":         "max 20000\n", "memory.current": "1024\n", "memory.high": "2048\n", "memory.max": "max\n",
		"memory.events": "high 0\nmax 0\noom 0\noom_kill 0\n",
	}
	for name, value := range data {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0400); err != nil {
			t.Fatal(err)
		}
	}
	r, err := (FileReader{}).Read(Source{Name: "fixture", Path: dir, Cgroup: true})
	if err != nil || r.Resources.MemoryCurrentBytes != 1024 || !r.Resources.CPUQuota.Unlimited || !r.CPU.HasFull {
		t.Fatalf("read: %+v %v", r, err)
	}
	for name, value := range data {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !reflect.DeepEqual(b, []byte(value)) {
			t.Fatal("observer changed source", name, err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "cpu.stat")); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileReader{}).Read(Source{Path: dir, Cgroup: true}); err == nil {
		t.Fatal("missing CPU counters accepted as zero")
	}
	host := t.TempDir()
	for _, name := range []string{"cpu", "memory"} {
		if err := os.WriteFile(filepath.Join(host, name), []byte(data["memory.pressure"]), 0400); err != nil {
			t.Fatal(err)
		}
	}
	h, err := (FileReader{}).Read(Source{Path: host})
	if err != nil || h.CPU.HasFull || !h.Memory.HasFull {
		t.Fatalf("host CPU full is not meaningful: %+v %v", h, err)
	}
}
