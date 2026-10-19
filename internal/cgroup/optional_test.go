package cgroup

import (
	"fmt"
	"testing"
)

func TestOptionalIOAndPIDCountersKeepZeroAndMissingDistinct(t *testing.T) {
	devices, err := ParseIOStats("8:0 rbytes=0 wbytes=5 rios=0 wios=1 extra=9\n")
	if err != nil || len(devices) != 1 || devices[0].ReadBytes != 0 || devices[0].WriteBytes != 5 {
		t.Fatalf("I/O: %+v %v", devices, err)
	}
	for _, data := range []string{"8:0 rbytes=0", "8:0 rbytes=0 wbytes=0 rios=0 wios=-1", "8:x rbytes=0 wbytes=0 rios=0 wios=0", "8:0 rbytes=0 rbytes=0 wbytes=0 rios=0 wios=0"} {
		if _, err := ParseIOStats(data); err == nil {
			t.Fatal("invalid counters accepted", data)
		}
	}
	files := map[string]string{"pids.current": "0", "pids.max": "max", "pids.events": "max 2\n"}
	read := func(name string) (string, error) {
		if data, ok := files[name]; ok {
			return data, nil
		}
		return "", fmt.Errorf("missing %s", name)
	}
	pids, err := ReadPIDs(read)
	if err != nil || pids.Current != 0 || !pids.Max.Unlimited || pids.LimitHits != 2 {
		t.Fatalf("PIDs: %+v %v", pids, err)
	}
	delete(files, "pids.events")
	if _, err = ReadPIDs(read); err == nil {
		t.Fatal("missing events became zero")
	}
}
