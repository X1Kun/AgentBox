package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CPU control is confined to the sandbox parent already owned by this manager.
// The static four-CPU ceiling and bandwidth period cannot be raised by policy.
type CPUBandwidth struct{ Quota, Period uint64 }

// SandboxBandwidth is a service-owned limit, never an execution request field.
// Zero selects the existing one-CPU default. Integer milli-CPU avoids float
// rounding and maps exactly to the fixed 20ms bandwidth period.
func SandboxBandwidth(milli int64) (CPUBandwidth, error) {
	return cpuBandwidth(milli, 1000, "sandbox-cpu-milli")
}

func ParentBandwidth(milli int64) (CPUBandwidth, error) {
	return cpuBandwidth(milli, 4000, "parent-cpu-milli")
}

func cpuBandwidth(milli, defaultMilli int64, name string) (CPUBandwidth, error) {
	if milli == 0 {
		milli = defaultMilli
	}
	if milli < 50 || milli > 4000 {
		return CPUBandwidth{}, fmt.Errorf("%s must be 50..4000 (1000 = 1 CPU)", name)
	}
	return CPUBandwidth{Quota: uint64(milli) * 20, Period: 20000}, nil
}

func (m *Manager) ReadCPUQuota() (CPUBandwidth, error) {
	data, err := os.ReadFile(filepath.Join(m.SandboxParentPath, "cpu.max"))
	if err != nil {
		return CPUBandwidth{}, err
	}
	f := strings.Fields(string(data))
	if len(f) != 2 {
		return CPUBandwidth{}, fmt.Errorf("cpu.max: expected quota and period")
	}
	q, err := strconv.ParseUint(f[0], 10, 64)
	if err != nil {
		return CPUBandwidth{}, fmt.Errorf("cpu.max: finite quota required")
	}
	p, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil || p != 20000 || q < 1000 || q > 80000 {
		return CPUBandwidth{}, fmt.Errorf("cpu.max is outside AgentBox CPU control bounds")
	}
	return CPUBandwidth{q, p}, nil
}

func (m *Manager) SetCPUQuota(expected CPUBandwidth, quota uint64) (CPUBandwidth, error) {
	current, err := m.ReadCPUQuota()
	if err != nil {
		return CPUBandwidth{}, err
	}
	if current != expected {
		return current, fmt.Errorf("cpu.max changed outside the controller")
	}
	if quota < 1000 || quota > 80000 {
		return current, fmt.Errorf("CPU quota must be 1000..80000 usec")
	}
	if err := write(m.SandboxParentPath, "cpu.max", fmt.Sprintf("%d %d", quota, current.Period)); err != nil {
		return current, err
	}
	actual, err := m.ReadCPUQuota()
	if err != nil {
		return CPUBandwidth{}, fmt.Errorf("read back cpu.max: %w", err)
	}
	if actual != (CPUBandwidth{quota, current.Period}) {
		return actual, fmt.Errorf("cpu.max readback mismatch")
	}
	return actual, nil
}
