package sandbox

import (
	"path/filepath"
)

// OCI structs intentionally cover only the immutable policy used by AgentBox.
type spec struct {
	Version  string  `json:"ociVersion"`
	Process  process `json:"process"`
	Root     root    `json:"root"`
	Hostname string  `json:"hostname"`
	Mounts   []mount `json:"mounts"`
	Linux    linux   `json:"linux"`
}
type process struct {
	Terminal        bool                `json:"terminal"`
	User            user                `json:"user"`
	Args            []string            `json:"args"`
	Env             []string            `json:"env"`
	Cwd             string              `json:"cwd"`
	Capabilities    map[string][]string `json:"capabilities"`
	NoNewPrivileges bool                `json:"noNewPrivileges"`
	Rlimits         []rlimit            `json:"rlimits"`
}
type user struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}
type root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}
type mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options"`
}
type namespace struct {
	Type string `json:"type"`
}
type rlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}
type linux struct {
	Namespaces    []namespace `json:"namespaces"`
	CgroupsPath   string      `json:"cgroupsPath"`
	MaskedPaths   []string    `json:"maskedPaths"`
	ReadonlyPaths []string    `json:"readonlyPaths"`
}

const bootstrap = `import json, os, sys, types
with open('/agentbox/task.json', encoding='utf-8') as f:
    task = json.load(f)
os.environ.update(task.get('env') or {})
code = task['code']
with open('/workspace/main.py', 'w', encoding='utf-8') as f:
    f.write(code)
main = types.ModuleType('__main__')
main.__file__ = '/workspace/main.py'
sys.modules['__main__'] = main
sys.argv = [main.__file__]
exec(compile(code, main.__file__, 'exec'), main.__dict__)
`

const idleBootstrap = `import signal
while True:
    signal.pause()
`

func makeTaskProcess() process {
	return process{
		User: user{1000, 1000}, Args: []string{"/usr/local/bin/python3", "-I", "-B", "-u", "-c", bootstrap},
		Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/workspace", "LANG=C.UTF-8", "TMPDIR=/tmp"},
		Cwd: "/workspace", NoNewPrivileges: true,
		Capabilities: map[string][]string{"bounding": {}, "effective": {}, "inheritable": {}, "permitted": {}, "ambient": {}},
		Rlimits:      []rlimit{{"RLIMIT_NOFILE", 128, 128}, {"RLIMIT_NPROC", 64, 64}, {"RLIMIT_FSIZE", 16 << 20, 16 << 20}, {"RLIMIT_CORE", 0, 0}},
	}
}

func makeSpec(rootfs, bundle, cgroupPath string) spec {
	initProcess := makeTaskProcess()
	// Keep Sentry alive while the attached exec command collects the task's
	// exit status. This container is still destroyed after exactly one task.
	initProcess.Args = []string{"/usr/local/bin/python3", "-I", "-B", "-u", "-c", idleBootstrap}
	return spec{
		Version: "1.0.2", Hostname: "agentbox",
		Root:    root{Path: rootfs, Readonly: true},
		Process: initProcess,
		Mounts: []mount{
			{"/proc", "proc", "proc", []string{"nosuid", "noexec", "nodev"}},
			{"/dev", "tmpfs", "tmpfs", []string{"nosuid", "noexec", "mode=755", "size=1048576"}},
			{"/tmp", "tmpfs", "tmpfs", []string{"nosuid", "nodev", "noexec", "mode=1777", "size=67108864"}},
			{"/workspace", "tmpfs", "tmpfs", []string{"nosuid", "nodev", "noexec", "mode=1777", "size=67108864"}},
			{"/agentbox", "bind", filepath.Join(bundle, "input"), []string{"rbind", "ro", "nosuid", "nodev", "noexec"}},
		},
		Linux: linux{
			Namespaces:    []namespace{{"pid"}, {"ipc"}, {"uts"}, {"mount"}, {"network"}},
			CgroupsPath:   cgroupPath,
			MaskedPaths:   []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/proc/sched_debug", "/sys/firmware"},
			ReadonlyPaths: []string{"/proc/sys", "/proc/sysrq-trigger", "/proc/irq", "/proc/bus"},
		},
	}
}
