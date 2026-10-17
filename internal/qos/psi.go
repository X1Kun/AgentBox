// Package qos observes resource contention. It does not write resource controls.
package qos

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

type PressureValue struct {
	Avg10, Avg60, Avg300 float64
	TotalUsec            uint64
}

type Pressure struct {
	Some, Full PressureValue
	HasFull    bool
}

// ParsePressure accepts additional future fields but rejects missing, duplicate
// or malformed known fields. Missing data must never look like zero pressure.
func ParsePressure(data string) (Pressure, error) {
	var result Pressure
	seen := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		kind := fields[0]
		if kind != "some" && kind != "full" {
			continue
		}
		if seen[kind] {
			return result, fmt.Errorf("duplicate PSI %s line", kind)
		}
		seen[kind] = true
		values := map[string]string{}
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok || value == "" {
				return result, fmt.Errorf("invalid PSI field %q", field)
			}
			if _, ok := values[key]; ok {
				return result, fmt.Errorf("duplicate PSI field %s", key)
			}
			values[key] = value
		}
		var value PressureValue
		for key, destination := range map[string]*float64{"avg10": &value.Avg10, "avg60": &value.Avg60, "avg300": &value.Avg300} {
			number, err := strconv.ParseFloat(values[key], 64)
			if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 100 {
				return result, fmt.Errorf("invalid PSI %s", key)
			}
			*destination = number
		}
		number, err := strconv.ParseUint(values["total"], 10, 64)
		if err != nil {
			return result, fmt.Errorf("invalid PSI total: %w", err)
		}
		value.TotalUsec = number
		if kind == "some" {
			result.Some = value
		} else {
			result.Full = value
			result.HasFull = true
		}
	}
	if !seen["some"] {
		return result, fmt.Errorf("missing PSI some line")
	}
	return result, nil
}
