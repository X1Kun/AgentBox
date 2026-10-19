package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCPUQuotaBoundsPeriodAndExternalWriter(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cpu.max")
	if err := os.WriteFile(file, []byte("80000 20000"), 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{SandboxParentPath: dir}
	b, err := m.ReadCPUQuota()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []uint64{0, 999, 80001} {
		if _, err := m.SetCPUQuota(b, q); err == nil {
			t.Fatal("accepted unsafe quota", q)
		}
	}
	actual, err := m.SetCPUQuota(b, 20000)
	if err != nil || actual.Quota != 20000 || actual.Period != 20000 {
		t.Fatalf("write/readback: %+v %v", actual, err)
	}
	if _, err := m.SetCPUQuota(b, 40000); err == nil {
		t.Fatal("overwrote unexpected current value")
	}
	if err := os.WriteFile(file, []byte("20000 100000"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReadCPUQuota(); err == nil {
		t.Fatal("accepted bandwidth period change")
	}
}
