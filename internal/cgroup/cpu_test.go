package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSandboxBandwidthAndIndependentLimits(t *testing.T) {
	parent, err := ParentBandwidth(0)
	if err != nil || parent.Quota != 80000 || parent.Period != 20000 {
		t.Fatalf("parent default changed: %+v %v", parent, err)
	}
	if b, err := ParentBandwidth(1000); err != nil || b.Quota != 20000 {
		t.Fatalf("parent milli-CPU conversion: %+v %v", b, err)
	}
	if _, err := ParentBandwidth(4001); err == nil {
		t.Fatal("parent budget exceeded hard ceiling")
	}
	for _, milli := range []int64{-1, 1, 49, 4001, 1 << 62} {
		if _, err := SandboxBandwidth(milli); err == nil {
			t.Fatalf("accepted invalid sandbox milli-CPU: %d", milli)
		}
	}
	for _, milli := range []int64{0, 50, 500, 1000, 1500, 4000} {
		t.Run(fmt.Sprint(milli), func(t *testing.T) {
			dir := t.TempDir()
			parent := filepath.Join(dir, "cpu.max")
			if err := os.WriteFile(parent, []byte("80000 20000"), 0600); err != nil {
				t.Fatal(err)
			}
			m := &Manager{SandboxParentPath: dir, sandboxCPUMilli: milli}
			g, err := m.NewGroup("ab-00000000000000000000000000000000")
			if err != nil {
				t.Fatal(err)
			}
			if milli == 0 {
				milli = 1000
			}
			actual, err := os.ReadFile(filepath.Join(g.CgroupPath, "cpu.max"))
			if err != nil || string(actual) != fmt.Sprintf("%d 20000", milli*20) {
				t.Fatalf("child limit: %s %v", actual, err)
			}
			actual, err = os.ReadFile(parent)
			if err != nil || string(actual) != "80000 20000" {
				t.Fatal("child configuration changed the parent budget", err)
			}
		})
	}
}

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
