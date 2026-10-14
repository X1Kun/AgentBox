// Package doctor performs read-only capability checks. A passing report is not
// a substitute for executing the real isolation integration suite.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/cgroup"
)

type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
type Report struct {
	Ready    bool    `json:"ready"`
	Platform string  `json:"platform"`
	Checks   []Check `json:"checks"`
}

func Inspect(binary, platform, rootfs, cgroupRoot string) Report {
	r := Report{Ready: true, Platform: platform}
	add := func(name string, ok bool, detail string) {
		r.Checks = append(r.Checks, Check{name, ok, detail})
		r.Ready = r.Ready && ok
	}
	add("platform", platform == "kvm" || platform == "systrap", "platform must explicitly be kvm or systrap")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	b, err := cmd.CombinedOutput()
	add("runsc", err == nil, strings.TrimSpace(string(b))+errString(err))
	if platform == "kvm" {
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			add("kvm", false, err.Error())
		} else {
			version, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0xAE00, 0)
			_ = f.Close()
			add("kvm", errno == 0 && version == 12, fmt.Sprintf("KVM_GET_API_VERSION=%d errno=%d", version, errno))
		}
	}
	add("execution_privileges", os.Geteuid() == 0, "phase 1 requires root in a delegated systemd unit or isolated test container")
	if cgroupRoot == "auto" {
		cgroupRoot, err = cgroup.CurrentPath()
		if err != nil {
			add("cgroup_path", false, err.Error())
		}
	}
	b, err = os.ReadFile(filepath.Join(cgroupRoot, "cgroup.controllers"))
	controllers := " " + strings.Join(strings.Fields(string(b)), " ") + " "
	ok := err == nil && cgroupRoot != "/sys/fs/cgroup"
	for _, name := range []string{"cpu", "memory", "pids"} {
		ok = ok && strings.Contains(controllers, " "+name+" ")
	}
	add("cgroup_controllers", ok, cgroupRoot+": "+strings.TrimSpace(string(b))+errString(err))
	err = syscall.Access(filepath.Join(cgroupRoot, "cgroup.subtree_control"), 2)
	add("cgroup_writable", err == nil, cgroupRoot+errString(err))
	for _, name := range []string{"cpu", "memory", "io"} {
		b, err = os.ReadFile("/proc/pressure/" + name)
		add("psi_"+name, err == nil, strings.TrimSpace(string(b))+errString(err))
	}
	_, err = os.Stat(filepath.Join(rootfs, "usr/local/bin/python3"))
	add("python_rootfs", err == nil && filepath.Clean(rootfs) != "/", rootfs+errString(err))
	return r
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}
