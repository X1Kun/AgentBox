package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/cgroup"
)

var (
	ErrBusy      = errors.New("execution capacity exhausted")
	ErrUnhealthy = errors.New("executor unavailable; restart after resolving cleanup failure")
)

type Config struct {
	Runsc       string
	Rootfs      string
	StateDir    string
	CgroupRoot  string
	MoveSelf    bool
	Platform    string
	Concurrency int
	OutputLimit int
}

type Engine struct {
	config    Config
	runtime   runscCLI
	groups    *cgroup.Manager
	lock      *os.File
	slots     chan struct{}
	broken    atomic.Bool
	closed    atomic.Bool
	mu        sync.Mutex
	active    sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

type ownership struct {
	CgroupRoot string `json:"cgroup_root"`
	Rootfs     string `json:"rootfs"`
	Platform   string `json:"platform"`
}

type executionState struct {
	id         string
	bundlePath string
	group      *cgroup.Group
	session    *session
	startedAt  time.Time
}

func New(c Config) (_ *Engine, retErr error) {
	if c.Platform != "kvm" && c.Platform != "systrap" {
		return nil, errors.New("platform must explicitly be kvm or systrap")
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("phase 1 requires root in a dedicated systemd unit or isolated test container; rootless resource enforcement is not supported")
	}
	if c.Concurrency < 1 || c.Concurrency > 4 {
		return nil, errors.New("concurrency must be between 1 and 4")
	}
	if c.OutputLimit == 0 {
		c.OutputLimit = 1 << 20
	}
	if c.OutputLimit < 1 || c.OutputLimit > 1<<20 {
		return nil, errors.New("output limit must be between 1 byte and 1 MiB")
	}
	var err error
	c.Runsc, err = exec.LookPath(c.Runsc)
	if err != nil {
		return nil, fmt.Errorf("locate runsc: %w", err)
	}
	c.Runsc, err = filepath.Abs(c.Runsc)
	if err != nil {
		return nil, err
	}
	c.Rootfs, err = filepath.EvalSymlinks(c.Rootfs)
	if err != nil {
		return nil, fmt.Errorf("rootfs: %w", err)
	}
	c.Rootfs, err = filepath.Abs(c.Rootfs)
	if err != nil {
		return nil, err
	}
	if c.Rootfs == "/" {
		return nil, errors.New("host root cannot be a sandbox rootfs")
	}
	if _, err = os.Stat(filepath.Join(c.Rootfs, "usr/local/bin/python3")); err != nil {
		return nil, fmt.Errorf("rootfs must contain /usr/local/bin/python3: %w", err)
	}
	if c.Platform == "kvm" {
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("KVM access: %w", err)
		}
		_ = f.Close()
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return nil, err
	}
	if c.StateDir == "/" || c.StateDir == c.Rootfs || strings.HasPrefix(c.StateDir, c.Rootfs+"/") || strings.HasPrefix(c.Rootfs, c.StateDir+"/") {
		return nil, errors.New("state directory must be separate from rootfs")
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(c.StateDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be a private directory (mode 0700)")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("state directory must be owned by the daemon user")
	}
	fd, err := syscall.Open(filepath.Join(c.StateDir, "daemon.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "daemon.lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("state directory already locked: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = lock.Close()
		}
	}()
	if c.CgroupRoot == "auto" {
		c.CgroupRoot, err = cgroup.CurrentPath()
		if err != nil {
			return nil, err
		}
		c.MoveSelf = true
	}
	c.CgroupRoot, err = filepath.EvalSymlinks(c.CgroupRoot)
	if err != nil {
		return nil, err
	}
	owner := ownership{c.CgroupRoot, c.Rootfs, c.Platform}
	ownerPath := filepath.Join(c.StateDir, "owner.json")
	if b, readErr := os.ReadFile(ownerPath); readErr == nil {
		var previous ownership
		if err = json.Unmarshal(b, &previous); err != nil || previous != owner {
			return nil, errors.New("state directory belongs to a different configuration; reconcile it with its original configuration")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	} else {
		entries, err := os.ReadDir(c.StateDir)
		if err != nil {
			return nil, err
		}
		if len(entries) != 1 || entries[0].Name() != "daemon.lock" {
			return nil, errors.New("unowned state directory is not empty")
		}
		if err = writeJSON(ownerPath, owner, 0600); err != nil {
			return nil, err
		}
	}
	groups, err := cgroup.New(c.CgroupRoot, c.MoveSelf)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"bundles", "runsc"} {
		if err = os.MkdirAll(filepath.Join(c.StateDir, dir), 0700); err != nil {
			return nil, err
		}
	}
	e := &Engine{
		config: c,
		runtime: runscCLI{
			binaryPath: c.Runsc,
			stateDir:   filepath.Join(c.StateDir, "runsc"),
			platform:   c.Platform,
		},
		groups: groups,
		lock:   lock,
		slots:  make(chan struct{}, c.Concurrency),
	}
	if err = e.reconcile(); err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	return e, nil
}

func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), mode)
}

func (e *Engine) Ready() bool { return !e.broken.Load() && !e.closed.Load() }

// Close is called after request contexts have been cancelled. It waits for all
// cleanup before releasing ownership of the runtime state.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed.Store(true)
		e.mu.Unlock()
		e.active.Wait()
		e.closeErr = e.lock.Close()
	})
	return e.closeErr
}

func (e *Engine) acquireExecutionSlot() error {
	// Keep admission and active.Add coordinated with Close's shutdown gate.
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.Ready() {
		return ErrUnhealthy
	}
	select {
	case e.slots <- struct{}{}:
	default:
		return ErrBusy
	}
	e.active.Add(1)
	return nil
}

func (e *Engine) releaseExecutionSlot() {
	<-e.slots
	e.active.Done()
}

func (e *Engine) Execute(parent context.Context, request api.Request) (result api.Result, retErr error) {
	r, err := request.Validate()
	if err != nil {
		return result, err
	}
	if err := e.acquireExecutionSlot(); err != nil {
		return result, err
	}
	defer e.releaseExecutionSlot()
	if err := parent.Err(); err != nil {
		return result, err
	}
	begin := time.Now()
	var bytes [16]byte
	if _, err = rand.Read(bytes[:]); err != nil {
		return result, err
	}
	id := "ab-" + hex.EncodeToString(bytes[:])
	result = api.Result{ExecutionID: id, RequestID: r.RequestID, Status: "infrastructure_error"}
	bundle := filepath.Join(e.config.StateDir, "bundles", id)
	if err = os.Mkdir(bundle, 0700); err != nil {
		return result, err
	}
	// The owned bundle directory is the durable intent record, created before
	// runtime/cgroup side effects. Restart reconciliation uses only these IDs.
	group, groupErr := e.groups.NewGroup(id)
	if group == nil {
		if removeErr := os.Remove(bundle); removeErr != nil {
			e.broken.Store(true)
			return result, errors.Join(groupErr, removeErr)
		}
		return result, groupErr
	}
	execution := &executionState{
		id:         id,
		bundlePath: bundle,
		group:      group,
		startedAt:  begin,
	}
	defer func() {
		if cleanupErr := e.finalizeExecution(execution, &result); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	if groupErr != nil {
		return result, groupErr
	}
	if err = e.prepareTaskFiles(bundle, group.CgroupPath, r); err != nil {
		return result, err
	}
	s, err := newSession(e.runtime, id, bundle, e.config.OutputLimit)
	execution.session = s
	if err != nil {
		return result, err
	}
	setupCtx, setupCancel := context.WithTimeout(parent, 10*time.Second)
	err = s.create(setupCtx, bundle)
	setupCancel()
	result.Timings.SetupMS = ms(time.Since(begin))
	if err != nil {
		if parent.Err() != nil {
			result.Status = "cancelled"
			return result, nil
		}
		return result, fmt.Errorf("create sandbox: %w", err)
	}
	if err = group.VerifyPopulated(); err != nil {
		return result, err
	}
	retErr = e.runAndCollectResult(parent, r.TimeoutMS, execution, &result)
	return result, retErr
}

func (e *Engine) prepareTaskFiles(bundlePath, hostCgroupPath string, request api.Request) error {
	if err := prepareInput(bundlePath, request); err != nil {
		return fmt.Errorf("prepare sandbox input: %w", err)
	}
	relativeCgroupPath, err := filepath.Rel("/sys/fs/cgroup", hostCgroupPath)
	if err != nil {
		return fmt.Errorf("resolve OCI cgroup path: %w", err)
	}
	config := makeSpec(e.config.Rootfs, bundlePath, "/"+relativeCgroupPath)
	if err := writeJSON(filepath.Join(bundlePath, "config.json"), config, 0600); err != nil {
		return fmt.Errorf("write OCI config: %w", err)
	}
	return nil
}

func (e *Engine) finalizeExecution(execution *executionState, result *api.Result) error {
	cleanupStart := time.Now()
	defer func() {
		result.Timings.CleanupMS = ms(time.Since(cleanupStart))
		result.Timings.TotalMS = ms(time.Since(execution.startedAt))
	}()

	cleanupErr := e.destroyContainerAndCgroup(execution.id, execution.group)
	if execution.session != nil {
		execution.session.finishStreams()
		result.Stdout, result.Stderr, result.OutputTruncated = execution.session.outputCollector.result()
		if result.OutputTruncated && (result.Status == "succeeded" || result.Status == "failed") {
			result.Status = "output_limit"
		}
	}
	if cleanupErr != nil {
		e.broken.Store(true)
		result.Status = "infrastructure_error"
		return fmt.Errorf("cleanup %s: %w", execution.id, cleanupErr)
	}
	if err := os.RemoveAll(execution.bundlePath); err != nil {
		e.broken.Store(true)
		result.Status = "infrastructure_error"
		return err
	}
	result.CleanupComplete = true
	return nil
}

func (e *Engine) runAndCollectResult(parent context.Context, timeoutMS int, execution *executionState, result *api.Result) error {
	s := execution.session
	group := execution.group
	// The execution timeout covers start and wait; cold setup is separately
	// bounded and reported, rather than advertised as warm execution latency.
	execStart := time.Now()
	execCtx, cancel := context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	// Overflow can occur while the start command is still returning.
	monitorDone := make(chan struct{})
	go func() {
		select {
		case <-s.outputCollector.limitExceeded:
			cancel()
		case <-monitorDone:
		}
	}()
	defer close(monitorDone)
	var exit int
	err := s.start(execCtx)
	if err == nil {
		exit, err = s.wait(execCtx)
	}
	result.Timings.ExecutionMS = ms(time.Since(execStart))
	if execCtx.Err() != nil {
		select {
		case <-s.outputCollector.limitExceeded:
			result.Status = "output_limit"
		default:
			if parent.Err() != nil {
				result.Status = "cancelled"
			} else {
				result.Status = "timeout"
			}
		}
		// Kill before collecting usage, while the cgroup and OOM counters still
		// exist. Runtime metadata deletion follows in the deferred cleanup.
		if killErr := group.Kill(); killErr != nil {
			return killErr
		}
	} else if err != nil {
		// A Sentry killed by memcg may make the wait RPC fail. OOM counters below
		// distinguish that from a generic infrastructure failure.
		result.Status = "infrastructure_error"
	} else {
		result.ExitCode = &exit
		if exit == 0 {
			result.Status = "succeeded"
		} else {
			result.Status = "failed"
		}
	}
	usage, usageErr := group.Usage()
	result.Usage = usage
	if usageErr != nil {
		return usageErr
	}
	if usage.OOMKills > 0 {
		result.Status = "oom"
		return nil
	}
	if result.Status == "infrastructure_error" {
		return err
	}
	return nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (e *Engine) destroyContainerAndCgroup(id string, g *cgroup.Group) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, deleteErr := e.runtime.control(ctx, "delete", "--force", id)
	killErr := g.Kill()
	removeErr := g.Remove(ctx)
	// Even after cgroup.kill succeeds, a failed runtime deletion retains the
	// intent record and stops new admissions until reconciliation succeeds.
	return errors.Join(deleteErr, killErr, removeErr)
}

func (e *Engine) reconcile() error {
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "bundles"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unexpected state entry %q", entry.Name())
		}
		g, err := e.groups.Existing(entry.Name())
		if err != nil {
			return err
		}
		if err = e.destroyContainerAndCgroup(entry.Name(), g); err != nil {
			return err
		}
		if err = os.RemoveAll(filepath.Join(e.config.StateDir, "bundles", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
