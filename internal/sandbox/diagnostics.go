package sandbox

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Failure-only, bounded snapshots. No cmdline, environment, task files or guest
// output: the retained evidence is safe to inspect without exposing API keys.
func (r runscCLI) captureFailure(operation, id string, pid int, before *storageCounters) {
	sample := map[string]any{"at": time.Now().UTC(), "operation": operation, "sandbox_id": id, "pid": pid}
	read := func(path string) string {
		file, err := os.Open(path)
		if err != nil {
			return "unavailable: " + err.Error()
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 4096))
		if err != nil {
			return "unavailable: " + err.Error()
		}
		return string(data)
	}
	host := map[string]string{}
	for _, name := range []string{"cpu", "io", "memory"} {
		host[name] = read("/proc/pressure/" + name)
	}
	sample["host_pressure"] = host
	if before != nil {
		sample["storage_before"] = before
	}
	mounts := make(map[string]any)
	mountinfo := readBoundedMountInfo()
	for name, path := range map[string]string{"runsc": r.binaryPath, "rootfs": r.rootfs, "state": r.stateDir} {
		if path == "" {
			continue
		}
		mount := storageForPath(path, mountinfo)
		entry := map[string]any{"mount": mount}
		if validDevice(mount.Device) {
			base := filepath.Join("/sys/dev/block", mount.Device)
			entry["device_stat"] = read(filepath.Join(base, "stat"))
			entry["device_inflight"] = read(filepath.Join(base, "inflight"))
		}
		mounts[name] = entry
	}
	sample["storage"] = mounts
	pids := []int{pid}
	if r.sandboxParent != "" && filepath.Base(id) == id && id != "." && id != ".." {
		group := filepath.Join(r.sandboxParent, id)
		values := map[string]string{}
		for _, name := range []string{"cgroup.events", "cgroup.procs", "memory.current", "memory.events", "memory.stat", "cpu.stat", "io.stat", "io.pressure"} {
			values[name] = read(filepath.Join(group, name))
		}
		sample["sandbox"] = values
		sample["parent_cgroup_stat"] = read(filepath.Join(r.sandboxParent, "cgroup.stat"))
		for _, field := range strings.Fields(values["cgroup.procs"]) {
			n, err := strconv.Atoi(field)
			if err == nil && n > 0 && n != pid {
				pids = append(pids, n)
			}
			if len(pids) == 4 {
				break
			}
		}
	}
	processes := map[string]map[string]string{}
	for _, n := range pids {
		if n <= 0 {
			continue
		}
		values := map[string]string{}
		for _, name := range []string{"status", "wchan", "stack", "io"} {
			values[name] = read(filepath.Join("/proc", strconv.Itoa(n), name))
		}
		processes[strconv.Itoa(n)] = values
	}
	sample["processes"] = processes
	log := r.logger
	if log == nil {
		log = slog.Default()
	}
	log.Error("runtime failure snapshot", "snapshot", sample)
}

// Resolve the mount without opening files on a stalled data filesystem. Mount
// and device information comes from procfs; only sysfs counters are read
// for the backing device at failure time. Overlay/virtual filesystems may not expose a block device.
type storageMount struct {
	Path       string `json:"path"`
	MountPoint string `json:"mount_point,omitempty"`
	Device     string `json:"device,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	Source     string `json:"source,omitempty"`
}

func storageForPath(path, mountinfo string) storageMount {
	result := storageMount{Path: path}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`)
	for _, line := range strings.Split(mountinfo, "\n") {
		left, right, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		fields, fs := strings.Fields(left), strings.Fields(right)
		if len(fields) < 5 || len(fs) < 2 {
			continue
		}
		mount := unescape.Replace(fields[4])
		if path != mount && !strings.HasPrefix(path, strings.TrimSuffix(mount, "/")+"/") {
			continue
		}
		if len(mount) < len(result.MountPoint) {
			continue
		}
		result.MountPoint, result.Device, result.Filesystem, result.Source = mount, fields[2], fs[0], unescape.Replace(fs[1])
	}
	return result
}

func readBoundedMountInfo() string {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer file.Close()
	data, _ := io.ReadAll(io.LimitReader(file, 256<<10))
	return string(data)
}

func validDevice(device string) bool {
	major, minor, ok := strings.Cut(device, ":")
	if !ok {
		return false
	}
	_, e1 := strconv.ParseUint(major, 10, 32)
	_, e2 := strconv.ParseUint(minor, 10, 32)
	return e1 == nil && e2 == nil
}

type storageCounters struct {
	At      time.Time         `json:"at"`
	Devices map[string]string `json:"devices"`
}

func (r runscCLI) storageLayout() map[string]storageMount {
	result := map[string]storageMount{}
	info := readBoundedMountInfo()
	for name, path := range map[string]string{"runsc": r.binaryPath, "rootfs": r.rootfs, "state": r.stateDir} {
		if path != "" {
			result[name] = storageForPath(path, info)
		}
	}
	return result
}

// Baselines are sampled only for create/delete, with devices deduplicated.
// Raw counters and timestamps permit offline deltas; no device-wide I/O is
// attributed to this task, since unrelated host users may share the device.
func (r runscCLI) deviceCounters() map[string]string {
	result := map[string]string{}
	for _, mount := range r.storage {
		if !validDevice(mount.Device) {
			continue
		}
		if _, ok := result[mount.Device]; ok {
			continue
		}
		file, err := os.Open(filepath.Join("/sys/dev/block", mount.Device, "stat"))
		if err != nil {
			result[mount.Device] = "unavailable: " + err.Error()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, 4096))
		_ = file.Close()
		if err != nil {
			result[mount.Device] = "unavailable: " + err.Error()
		} else {
			result[mount.Device] = string(data)
		}
	}
	return result
}
