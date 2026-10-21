package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MemoryBudget includes a reserve outside sandboxes for the manager, shared
// dependencies and other services. It is a deployment budget, not free RAM.
type MemoryBudget struct {
	ParentBytes   int64 `json:"parent_bytes"`
	SandboxBytes  int64 `json:"sandbox_bytes"`
	ReserveBytes  int64 `json:"reserve_bytes"`
	CapacityBytes int64 `json:"capacity_bytes"`
}

func ResolveMemory(root string, budget MemoryBudget) (MemoryBudget, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return budget, err
	}
	var capacity int64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "MemTotal:" {
			n, e := strconv.ParseInt(f[1], 10, 64)
			if e != nil {
				return budget, e
			}
			capacity = n * 1024
		}
	}
	if capacity <= 0 {
		return budget, fmt.Errorf("cannot determine host memory capacity")
	}
	for path := filepath.Clean(root); ; path = filepath.Dir(path) {
		data, err := os.ReadFile(filepath.Join(path, "memory.max"))
		if err != nil && !(path == "/sys/fs/cgroup" && os.IsNotExist(err)) {
			return budget, fmt.Errorf("read memory ceiling %s: %w", path, err)
		}
		if err == nil && strings.TrimSpace(string(data)) != "max" {
			limit, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
			if err != nil || limit <= 0 {
				return budget, fmt.Errorf("invalid memory.max at %s", path)
			}
			capacity = min(capacity, limit)
		}
		if path == "/sys/fs/cgroup" {
			break
		}
		if path == "/" {
			return budget, fmt.Errorf("memory budget requires a path below /sys/fs/cgroup")
		}
	}
	return fitMemory(budget, capacity)
}

func fitMemory(b MemoryBudget, capacity int64) (MemoryBudget, error) {
	if b.ParentBytes < 0 || b.SandboxBytes < 0 || b.ReserveBytes < 0 {
		return b, fmt.Errorf("memory budgets must be nonnegative")
	}
	if b.SandboxBytes == 0 {
		b.SandboxBytes = MemoryMax
	}
	if b.ReserveBytes == 0 {
		b.ReserveBytes = 512 << 20
	}
	if b.SandboxBytes < 128<<20 || b.ReserveBytes < 128<<20 {
		return b, fmt.Errorf("sandbox memory and reserve must each be at least 128 MiB")
	}
	b.CapacityBytes = capacity
	if b.ReserveBytes >= capacity {
		return b, fmt.Errorf("memory reserve leaves no sandbox capacity")
	}
	if b.ParentBytes == 0 {
		b.ParentBytes = min(4<<30, capacity-b.ReserveBytes)
	}
	if b.ParentBytes > capacity-b.ReserveBytes || b.ParentBytes < b.SandboxBytes {
		return b, fmt.Errorf("sandbox parent budget must fit capacity minus reserve and hold at least one sandbox")
	}
	return b, nil
}
