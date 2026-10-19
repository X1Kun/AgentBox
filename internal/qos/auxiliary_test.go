package qos

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

func TestOptionalFamiliesDoNotInvalidateCPUAndRefreshIndependently(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"cpu.pressure":    "some avg10=0 avg60=0 avg300=0 total=10\nfull avg10=0 avg60=0 avg300=0 total=0\n",
		"memory.pressure": "some avg10=0 avg60=0 avg300=0 total=0\nfull avg10=0 avg60=0 avg300=0 total=0\n",
		"cpu.stat":        "usage_usec 20\nuser_usec 15\nsystem_usec 5\nnr_periods 1\nnr_throttled 0\nthrottled_usec 0\n",
		"cpu.max":         "10000 10000", "memory.current": "100", "memory.high": "max", "memory.max": "1000", "memory.events": "high 0\nmax 0\noom 0\noom_kill 0\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	source := Source{Name: "fixture", Path: dir, Cgroup: true}
	r := &FileReader{}
	first, err := r.Read(source)
	if err != nil {
		t.Fatal(err)
	}
	if first.Auxiliary.IOAvailable || first.Auxiliary.PIDsAvailable || first.Resources.CPUUsageUsec != 20 || first.Auxiliary.IOError == "" {
		t.Fatalf("optional failures hidden: %+v", first)
	}
	for name, data := range map[string]string{"io.pressure": files["memory.pressure"], "io.stat": "8:0 rbytes=0 wbytes=0 rios=0 wios=0", "pids.current": "3", "pids.max": "64", "pids.events": "max 0"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cached, err := r.Read(source)
	if err != nil {
		t.Fatal(err)
	}
	if cached.Auxiliary.At != first.Auxiliary.At {
		t.Fatal("optional files read at CPU frequency")
	}
	old := r.auxiliary[dir]
	old.reading.At = time.Now().Add(-time.Second)
	r.auxiliary[dir] = old
	current, err := r.Read(source)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Auxiliary.IOAvailable || !current.Auxiliary.PIDsAvailable || current.Auxiliary.PIDs.Current != 3 || len(current.Auxiliary.Devices) != 1 || current.Auxiliary.IOValid {
		t.Fatalf("recovery must establish a baseline: %+v", current.Auxiliary)
	}
	current.Auxiliary.Devices[0].Device = "mutated"
	if r.auxiliary[dir].reading.Devices[0].Device != "8:0" {
		t.Fatal("cached devices aliased")
	}
	if !strings.Contains(first.Auxiliary.PIDsError, "pids.current") {
		t.Fatal("missing source not identified")
	}
}

func TestOptionalRatesResetWithoutLosingOtherFamilies(t *testing.T) {
	id := identity{1, 2}
	previous := cachedAuxiliary{id, Auxiliary{At: time.Now().Add(-time.Second), IOAvailable: true, IO: Pressure{Some: PressureValue{TotalUsec: 10000}, HasFull: true}, PIDsAvailable: true, PIDs: cgroup.PIDStats{LimitHits: 9}}}
	read := func(name string) (string, error) {
		switch name {
		case "io.pressure":
			return "some avg10=0 avg60=0 avg300=0 total=0\nfull avg10=0 avg60=0 avg300=0 total=0", nil
		case "io.stat":
			return "", nil
		case "pids.current":
			return "2", nil
		case "pids.max":
			return "64", nil
		case "pids.events":
			return "max 11", nil
		}
		return "", fmt.Errorf("unexpected %s", name)
	}
	a := readAuxiliary(Source{Cgroup: true}, read, previous, id)
	if a.IOValid || a.IOBaseline != "counter_reset" || !a.PIDDeltaValid || a.PIDLimitHits != 2 {
		t.Fatalf("family baselines coupled: %+v", a)
	}
}

func TestAncestorDiscoveryDistinguishesNamespaceAndKernelRoots(t *testing.T) {
	rootInfo, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, namespaceRoot := range []bool{false, true} {
		stat := func(string) (os.FileInfo, error) {
			if namespaceRoot {
				return rootInfo, nil
			}
			return nil, os.ErrNotExist
		}
		sources, boundary := observationSources("/sys/fs/cgroup/node/agentbox/sandboxes", "/sys/fs/cgroup/core", func() (string, error) { return "/sys/fs/cgroup/node/agentbox/manager", nil }, stat)
		want := 6
		if namespaceRoot {
			want = 7
		}
		if len(sources) != want || sources[3].Name != "manager" || sources[4].Path != "/sys/fs/cgroup/node/agentbox" || boundary.Path != "/sys/fs/cgroup" {
			t.Fatalf("hierarchy: %+v %+v", sources, boundary)
		}
	}
}
