package cgroup

import (
	"fmt"
	"strconv"
	"strings"
)

type PIDStats struct {
	Current   uint64
	Max       Limit
	LimitHits uint64
}

func ReadPIDs(read func(string) (string, error)) (PIDStats, error) {
	var result PIDStats
	for name, destination := range map[string]*uint64{"pids.current": &result.Current} {
		data, err := read(name)
		if err != nil {
			return result, err
		}
		*destination, err = strconv.ParseUint(strings.TrimSpace(data), 10, 64)
		if err != nil {
			return result, fmt.Errorf("%s: %w", name, err)
		}
	}
	data, err := read("pids.max")
	if err != nil {
		return result, err
	}
	if result.Max, err = parseLimit(strings.TrimSpace(data)); err != nil {
		return result, fmt.Errorf("pids.max: %w", err)
	}
	data, err = read("pids.events")
	if err != nil {
		return result, err
	}
	if err = readCounters(data, map[string]*uint64{"max": &result.LimitHits}); err != nil {
		return result, fmt.Errorf("pids.events: %w", err)
	}
	return result, nil
}

type DeviceIO struct {
	Device                                   string
	ReadBytes, WriteBytes, ReadIOs, WriteIOs uint64
}

func ParseIOStats(data string) ([]DeviceIO, error) {
	result := []DeviceIO{}
	seen := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		parts := strings.Split(fields[0], ":")
		if len(parts) != 2 || seen[fields[0]] {
			return nil, fmt.Errorf("invalid or duplicate I/O device")
		}
		for _, part := range parts {
			if _, err := strconv.ParseUint(part, 10, 32); err != nil {
				return nil, fmt.Errorf("invalid I/O device")
			}
		}
		seen[fields[0]] = true
		d := DeviceIO{Device: fields[0]}
		destinations := map[string]*uint64{"rbytes": &d.ReadBytes, "wbytes": &d.WriteBytes, "rios": &d.ReadIOs, "wios": &d.WriteIOs}
		found := map[string]bool{}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				return nil, fmt.Errorf("invalid I/O field")
			}
			dest, required := destinations[key]
			if !required {
				continue
			}
			if found[key] {
				return nil, fmt.Errorf("duplicate I/O counter %s", key)
			}
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid I/O counter %s", key)
			}
			*dest, found[key] = n, true
		}
		if len(found) != len(destinations) {
			return nil, fmt.Errorf("missing I/O counters")
		}
		result = append(result, d)
		if len(result) > 128 {
			return nil, fmt.Errorf("I/O device count exceeds observation limit")
		}
	}
	return result, nil
}
