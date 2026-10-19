//go:build integration

package qos

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This test only reads an explicitly selected live cgroup. It never creates
// groups, moves processes, changes limits or launches sandbox work.
func TestLiveKernelObservation(t *testing.T) {
	if os.Getenv("AGENTBOX_LIVE_OBSERVATION") != "1" {
		t.Skip("requires AGENTBOX_LIVE_OBSERVATION=1 and AGENTBOX_LIVE_CGROUP")
	}
	path := filepath.Clean(os.Getenv("AGENTBOX_LIVE_CGROUP"))
	if !strings.HasPrefix(path, "/sys/fs/cgroup/") {
		t.Fatal("select a readable cgroup below /sys/fs/cgroup")
	}
	reader := &FileReader{}
	sources := []Source{{Name: "host", Path: "/proc/pressure"}, {Name: "live", Path: path, Cgroup: true}}
	first := make([]Reading, len(sources))
	for i, source := range sources {
		var err error
		first[i], err = reader.Read(source)
		if err != nil {
			t.Fatalf("%s initial read: %v", source.Name, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	for i, source := range sources {
		current, err := reader.Read(source)
		if err != nil {
			t.Fatalf("%s second read: %v", source.Name, err)
		}
		if current.Identity != first[i].Identity || !current.At.After(first[i].At) || countersReset(first[i], current, source.Cgroup) {
			t.Fatalf("%s live counters changed identity or regressed", source.Name)
		}
		delta := derive(first[i], current, source.Cgroup)
		if !delta.Valid || delta.Interval <= 0 || delta.CPUUserCores < 0 || delta.CPUSystemCores < 0 {
			t.Fatalf("%s invalid delta: %+v", source.Name, delta)
		}
		aux := current.Auxiliary
		if !aux.At.After(first[i].Auxiliary.At) {
			t.Fatalf("%s optional sample did not refresh", source.Name)
		}
		if aux.IOAvailable && first[i].Auxiliary.IOAvailable && !aux.IOValid {
			t.Fatalf("%s I/O baseline did not advance: %s", source.Name, aux.IOBaseline)
		}
		if source.Cgroup && aux.PIDsAvailable && first[i].Auxiliary.PIDsAvailable && !aux.PIDDeltaValid {
			t.Fatalf("PID baseline did not advance: %s", aux.PIDBaseline)
		}
		t.Logf("%s path=%s CPU/memory=valid read_ms=%.3f io_pressure=%t io_devices=%t pids=%t", source.Name, source.Path, float64(current.Duration)/float64(time.Millisecond), aux.IOAvailable, aux.DevicesAvailable, aux.PIDsAvailable)
	}
}
