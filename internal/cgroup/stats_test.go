package cgroup

import (
	"errors"
	"testing"
)

func TestObservationStatsRejectMissingCountersAndPreserveLimits(t *testing.T) {
	files := map[string]string{"cpu.stat": "usage_usec 42\nuser_usec 30\nsystem_usec 12\nnr_periods 10\nnr_throttled 2\nthrottled_usec 150\n", "cpu.max": "80000 20000", "memory.current": "4096", "memory.high": "0", "memory.max": "max", "memory.events": "high 3\nmax 2\noom 1\noom_kill 1\n"}
	read := func(name string) (string, error) {
		value, ok := files[name]
		if !ok {
			return "", errors.New("missing file")
		}
		return value, nil
	}
	stats, err := ReadStats(read)
	if err != nil || stats.CPUQuota.Value != 80000 || stats.CPUPeriodUsec != 20000 || stats.MemoryHigh.Unlimited || stats.MemoryHigh.Value != 0 || !stats.MemoryMax.Unlimited || stats.OOMKills != 1 {
		t.Fatalf("stats: %+v %v", stats, err)
	}
	for _, data := range []string{"usage_usec 42\n", "usage_usec nope\nnr_periods 1\nnr_throttled 0\nthrottled_usec 0\n", "usage_usec 1\nusage_usec 2\nnr_periods 1\nnr_throttled 0\nthrottled_usec 0\n"} {
		files["cpu.stat"] = data
		if _, err := ReadStats(read); err == nil {
			t.Fatal("invalid stats accepted", data)
		}
	}
}
