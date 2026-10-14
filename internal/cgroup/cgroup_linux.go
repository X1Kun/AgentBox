// Package cgroup manages only a caller-delegated cgroup v2 subtree.
package cgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
)

const MemoryMax = 512 << 20

type Manager struct {
	SandboxParentPath string
}

type Group struct {
	CgroupPath string
}

func CurrentPath() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", p), nil
		}
	}
	return "", errors.New("unified cgroup v2 membership not found")
}

// New requires a private, delegated subtree. It never edits the host root.
// When moveSelf is true, only the daemon process is moved to a manager leaf.
func New(root string, moveSelf bool) (*Manager, error) {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if root == "/sys/fs/cgroup" || !strings.HasPrefix(root, "/sys/fs/cgroup/") {
		return nil, errors.New("cgroup root must be a delegated subtree below /sys/fs/cgroup")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(root, &fs); err != nil {
		return nil, err
	}
	if fs.Type != 0x63677270 {
		return nil, errors.New("cgroup root is not cgroup v2")
	}
	b, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	available := " " + strings.Join(strings.Fields(string(b)), " ") + " "
	for _, controller := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(available, " "+controller+" ") {
			return nil, fmt.Errorf("%s controller is not delegated to %s", controller, root)
		}
	}
	if moveSelf {
		leaf := filepath.Join(root, "manager")
		if err := os.MkdirAll(leaf, 0755); err != nil {
			return nil, err
		}
		if err := write(leaf, "cgroup.procs", strconv.Itoa(os.Getpid())); err != nil {
			return nil, err
		}
	}
	if err := write(root, "cgroup.subtree_control", "+cpu +memory +pids"); err != nil {
		return nil, err
	}
	parent := filepath.Join(root, "sandboxes")
	if err := os.MkdirAll(parent, 0755); err != nil {
		return nil, err
	}
	for _, setting := range [][2]string{
		{"cpu.max", "80000 20000"}, {"memory.high", "3221225472"},
		{"memory.max", "4294967296"}, {"memory.swap.max", "0"},
		{"pids.max", "2048"}, {"cgroup.subtree_control", "+cpu +memory +pids"},
	} {
		if err := write(parent, setting[0], setting[1]); err != nil {
			return nil, err
		}
	}
	return &Manager{SandboxParentPath: parent}, nil
}

func (m *Manager) NewGroup(id string) (*Group, error) {
	if !validID(id) {
		return nil, errors.New("invalid sandbox id")
	}
	g := &Group{CgroupPath: filepath.Join(m.SandboxParentPath, id)}
	if err := os.Mkdir(g.CgroupPath, 0755); err != nil {
		return nil, err
	}
	for _, setting := range [][2]string{
		{"cpu.max", "20000 20000"}, {"memory.high", "402653184"},
		{"memory.max", "536870912"}, {"memory.swap.max", "0"},
		{"memory.oom.group", "1"}, {"pids.max", "256"},
	} {
		if err := write(g.CgroupPath, setting[0], setting[1]); err != nil {
			// Retain ownership of the partial group until cleanup is confirmed.
			return g, err
		}
	}
	return g, nil
}

func validID(id string) bool {
	if len(id) != 35 || !strings.HasPrefix(id, "ab-") {
		return false
	}
	for _, c := range id[3:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (m *Manager) Existing(id string) (*Group, error) {
	if !validID(id) {
		return nil, errors.New("invalid sandbox id")
	}
	return &Group{CgroupPath: filepath.Join(m.SandboxParentPath, id)}, nil
}

func write(path, name, value string) error {
	if err := os.WriteFile(filepath.Join(path, name), []byte(value), 0644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func (g *Group) Kill() error {
	err := write(g.CgroupPath, "cgroup.kill", "1")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Remove waits for kernel-confirmed emptiness; failure retains the directory.
func (g *Group) Remove(ctx context.Context) error {
	for {
		b, err := os.ReadFile(filepath.Join(g.CgroupPath, "cgroup.events"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fields(string(b))["populated"] == 0 {
			err := os.Remove(g.CgroupPath)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if !errors.Is(err, syscall.EBUSY) {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func fields(s string) map[string]uint64 {
	m := make(map[string]uint64)
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 {
			if n, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				m[f[0]] = n
			}
		}
	}
	return m
}

func (g *Group) Usage() (api.Usage, error) {
	var u api.Usage
	for _, file := range []string{"cpu.stat", "memory.events", "memory.peak"} {
		b, err := os.ReadFile(filepath.Join(g.CgroupPath, file))
		if err != nil {
			return u, fmt.Errorf("read %s: %w", file, err)
		}
		switch file {
		case "cpu.stat":
			u.CPUUsec = fields(string(b))["usage_usec"]
		case "memory.events":
			u.OOMKills = fields(string(b))["oom_kill"]
		case "memory.peak":
			u.MemoryPeakBytes, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
			if err != nil {
				return u, err
			}
		}
	}
	return u, nil
}

func (g *Group) VerifyPopulated() error {
	b, err := os.ReadFile(filepath.Join(g.CgroupPath, "cgroup.events"))
	if err != nil {
		return err
	}
	if fields(string(b))["populated"] != 1 {
		return errors.New("sandbox cgroup has no processes; refusing unaccounted execution")
	}
	return nil
}
