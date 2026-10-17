package cgroup

import (
	"fmt"
	"strconv"
	"strings"
)

// Stats is a transport-independent observation of a cgroup. Unlimited values
// are explicit; a zero limit is not a substitute for missing or unlimited data.
type Limit struct {
	Unlimited bool
	Value     uint64
}
type Stats struct {
	CPUUsageUsec        uint64
	CPUPeriods          uint64
	CPUThrottledPeriods uint64
	CPUThrottledUsec    uint64
	CPUQuota            Limit
	CPUPeriodUsec       uint64
	MemoryCurrentBytes  uint64
	MemoryHigh          Limit
	MemoryMax           Limit
	MemoryHighEvents    uint64
	MemoryMaxEvents     uint64
	OOMEvents           uint64
	OOMKills            uint64
}

// ReadStats uses the observer's bounded reader so each error identifies its
// source file. All counters needed for rates are required, even when zero.
func ReadStats(read func(string) (string, error)) (Stats, error) {
	var s Stats
	for name, destinations := range map[string]map[string]*uint64{
		"cpu.stat":      {"usage_usec": &s.CPUUsageUsec, "nr_periods": &s.CPUPeriods, "nr_throttled": &s.CPUThrottledPeriods, "throttled_usec": &s.CPUThrottledUsec},
		"memory.events": {"high": &s.MemoryHighEvents, "max": &s.MemoryMaxEvents, "oom": &s.OOMEvents, "oom_kill": &s.OOMKills},
	} {
		data, err := read(name)
		if err != nil {
			return s, err
		}
		if err := readCounters(data, destinations); err != nil {
			return s, fmt.Errorf("%s: %w", name, err)
		}
	}
	data, err := read("cpu.max")
	if err != nil {
		return s, err
	}
	fields := strings.Fields(data)
	if len(fields) != 2 {
		return s, fmt.Errorf("cpu.max: expected quota and period")
	}
	if s.CPUQuota, err = parseLimit(fields[0]); err != nil {
		return s, fmt.Errorf("cpu.max: %w", err)
	}
	if s.CPUPeriodUsec, err = strconv.ParseUint(fields[1], 10, 64); err != nil || s.CPUPeriodUsec == 0 {
		return s, fmt.Errorf("cpu.max: invalid period")
	}
	for name, destination := range map[string]*Limit{"memory.high": &s.MemoryHigh, "memory.max": &s.MemoryMax} {
		data, err := read(name)
		if err != nil {
			return s, err
		}
		value, err := parseLimit(strings.TrimSpace(data))
		if err != nil {
			return s, fmt.Errorf("%s: %w", name, err)
		}
		*destination = value
	}
	data, err = read("memory.current")
	if err != nil {
		return s, err
	}
	s.MemoryCurrentBytes, err = strconv.ParseUint(strings.TrimSpace(data), 10, 64)
	if err != nil {
		return s, fmt.Errorf("memory.current: %w", err)
	}
	return s, nil
}

func readCounters(data string, destinations map[string]*uint64) error {
	seen := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		destination, needed := destinations[fields[0]]
		if !needed {
			continue
		}
		if len(fields) != 2 || seen[fields[0]] {
			return fmt.Errorf("malformed or duplicate %s", fields[0])
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s: %w", fields[0], err)
		}
		*destination = value
		seen[fields[0]] = true
	}
	for key := range destinations {
		if !seen[key] {
			return fmt.Errorf("missing %s", key)
		}
	}
	return nil
}
func parseLimit(value string) (Limit, error) {
	if value == "max" {
		return Limit{Unlimited: true}, nil
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return Limit{}, err
	}
	return Limit{Value: n}, nil
}
