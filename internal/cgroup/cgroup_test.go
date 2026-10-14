package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUsageIncludesOOMAndPeak(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{"cpu.stat": "usage_usec 123\nuser_usec 100\n", "memory.events": "oom 2\noom_kill 1\n", "memory.peak": "536870912\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	u, err := (&Group{CgroupPath: dir}).Usage()
	if err != nil || u.CPUUsec != 123 || u.OOMKills != 1 || u.MemoryPeakBytes != 536870912 {
		t.Fatalf("usage: %+v %v", u, err)
	}
}

func TestRejectsUnownedPaths(t *testing.T) {
	m := &Manager{SandboxParentPath: t.TempDir()}
	for _, id := range []string{"../other", "ab-../../x", "sandbox", "ab-" + "0000000000000000000000000000000g"} {
		if _, err := m.Existing(id); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	if _, err := New(t.TempDir(), false); err == nil {
		t.Fatal("accepted non-cgroup filesystem")
	}
}
