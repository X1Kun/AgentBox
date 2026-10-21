package cgroup

import "testing"

func TestMemoryBudgetFitsSmallHostAndExplicitLimits(t *testing.T) {
	for _, capacity := range []int64{2 << 30, 4 << 30, 5 << 30, 64 << 30} {
		b, err := fitMemory(MemoryBudget{}, capacity)
		if err != nil || b.ParentBytes+b.ReserveBytes > capacity || b.ParentBytes > 4<<30 || b.SandboxBytes != 512<<20 {
			t.Fatalf("capacity %d: %+v %v", capacity, b, err)
		}
	}
	for _, b := range []MemoryBudget{{ParentBytes: 4 << 30}, {ReserveBytes: 2 << 30}, {SandboxBytes: -1}, {SandboxBytes: 64 << 20}} {
		if _, err := fitMemory(b, 2<<30); err == nil {
			t.Fatalf("unsafe budget accepted: %+v", b)
		}
	}
	b, err := fitMemory(MemoryBudget{ParentBytes: 1 << 30, SandboxBytes: 256 << 20, ReserveBytes: 768 << 20}, 2<<30)
	if err != nil || b.ParentBytes/b.SandboxBytes != 4 {
		t.Fatalf("explicit deployment: %+v %v", b, err)
	}
}
