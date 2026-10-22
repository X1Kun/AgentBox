package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/X1Kun/AgentBox/internal/qos"
)

var (
	ErrBusy      = errors.New("execution capacity exhausted")
	ErrUnhealthy = errors.New("executor unavailable; inspect status and restart after resolving failure")
)

type Config struct {
	Memory              cgroup.MemoryBudget
	SandboxCPUMilli     int64
	ParentCPUMilli      int64
	Logger              *slog.Logger
	QoS                 qos.Config
	Runsc               string
	Rootfs              string
	StateDir            string
	CgroupRoot          string
	MoveSelf            bool
	Platform            string
	Concurrency         int
	OutputLimit         int
	Mode                string
	WarmPoolSize        int
	WarmRefillPolicy    string
	DynamicWarmPool     bool
	WarmMinSize         int
	WarmMaxSize         int
	WarmAdjustInterval  time.Duration
	WarmDownscaleWindow time.Duration
	MaxSandboxes        int
	CreateConcurrency   int
	QueueSize           int
	QueueBytes          int
	QueueTimeout        time.Duration
}

type Engine struct {
	refillPaused       bool
	pressurePaused     bool
	pressureRejections uint64
	config             Config
	runtime            runscCLI
	groups             *cgroup.Manager
	lock               *os.File
	broken             atomic.Bool
	closed             atomic.Bool
	startupRequired    bool
	startupReady       atomic.Bool
	startOnce          sync.Once
	startErr           error
	mu                 sync.Mutex
	active             sync.WaitGroup
	closeOnce          sync.Once
	closeErr           error
	observer           *qos.Observer
	cleanup            cleanupTelemetry
	pool               *warmPool
	ctx                context.Context
	cancel             context.CancelFunc
}

type ownership struct {
	CgroupRoot string `json:"cgroup_root"`
	Rootfs     string `json:"rootfs"`
	Platform   string `json:"platform"`
}

type executionState struct {
	creationTime  time.Duration
	cleanupPhases []api.CleanupPhase
	id            string
	bundlePath    string
	group         *cgroup.Group
	session       *session
	startedAt     time.Time
}

func New(c Config) (_ *Engine, retErr error) {
	cpu, err := cgroup.SandboxBandwidth(c.SandboxCPUMilli)
	if err != nil {
		return nil, err
	}
	c.SandboxCPUMilli = int64(cpu.Quota / 20)
	parentCPU, err := cgroup.ParentBandwidth(c.ParentCPUMilli)
	if err != nil {
		return nil, err
	}
	c.ParentCPUMilli = int64(parentCPU.Quota / 20)
	c.QoS.Logger = c.Logger
	if err := c.QoS.Normalize(); err != nil {
		return nil, err
	}
	if c.QoS.CPUControl && c.QoS.CPU.MaxCPU > float64(c.ParentCPUMilli)/1000 {
		return nil, errors.New("qos-max-cpu must not exceed parent-cpu-milli budget")
	}
	if err := normalizePoolConfig(&c); err != nil {
		return nil, err
	}
	if c.QoS.RefillOnly && c.Mode != "warm" {
		return nil, errors.New("qos-refill-only requires warm mode")
	}
	if c.Platform != "kvm" && c.Platform != "systrap" {
		return nil, errors.New("platform must explicitly be kvm or systrap")
	}
	if os.Geteuid() != 0 {
		return nil, errors.New("phase 1 requires root in a dedicated systemd unit or isolated test container; rootless resource enforcement is not supported")
	}
	if c.Concurrency < 1 || c.Concurrency > MaxExecutionConcurrency {
		return nil, errors.New("concurrency must be between 1 and 8")
	}
	if c.OutputLimit == 0 {
		c.OutputLimit = 1 << 20
	}
	if c.OutputLimit < 1 || c.OutputLimit > 1<<20 {
		return nil, errors.New("output limit must be between 1 byte and 1 MiB")
	}
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
	c.Memory, err = cgroup.ResolveMemory(c.CgroupRoot, c.Memory)
	if err != nil {
		return nil, err
	}
	memoryInstances := c.MaxSandboxes
	if c.Mode == "cold" {
		memoryInstances = c.Concurrency
	}
	if int64(memoryInstances) > c.Memory.ParentBytes/c.Memory.SandboxBytes {
		return nil, fmt.Errorf("mode requires capacity for %d instances, memory budget fits at most %d; reduce max-sandboxes/concurrency/warm-size or configure memory budgets", memoryInstances, c.Memory.ParentBytes/c.Memory.SandboxBytes)
	}
	groups, err := cgroup.NewWithLimits(c.CgroupRoot, c.MoveSelf, c.Memory, c.SandboxCPUMilli, c.ParentCPUMilli)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{"bundles", "runsc"} {
		if err = os.MkdirAll(filepath.Join(c.StateDir, dir), 0700); err != nil {
			return nil, err
		}
	}
	e := &Engine{
		config:          c,
		startupRequired: true,
		runtime: runscCLI{
			binaryPath:    c.Runsc,
			stateDir:      filepath.Join(c.StateDir, "runsc"),
			platform:      c.Platform,
			sandboxParent: groups.SandboxParentPath,
			rootfs:        c.Rootfs,
			logger:        c.Logger,
		},
		groups: groups,
		lock:   lock,
	}
	e.runtime.storage = e.runtime.storageLayout()
	if err = e.reconcile(); err != nil {
		return nil, fmt.Errorf("reconcile: %w", err)
	}
	e.observer, err = qos.New(c.QoS, groups.SandboxParentPath)
	if err != nil {
		return nil, err
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.pool = newWarmPool(c, e.createSandbox, e.destroyInstanceResources)
	if e.observer != nil && (c.QoS.Scheduling || c.QoS.RefillOnly) {
		if err := e.observer.SetPressureHandler(e.applyPressure); err != nil {
			e.cancel()
			if e.pool != nil {
				e.pool.cancel()
			}
			return nil, err
		}
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

// Start verifies the real create/exec/delete path before exposing readiness.
// A failed probe is not retried implicitly; restart after resolving its cause.
func (e *Engine) Start(parent context.Context) error {
	if e.closed.Load() {
		return ErrUnhealthy
	}
	e.startOnce.Do(func() {
		e.mu.Lock()
		if e.closed.Load() {
			e.mu.Unlock()
			e.startErr = ErrUnhealthy
			return
		}
		e.active.Add(1)
		e.mu.Unlock()
		defer e.active.Done()
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		stop := context.AfterFunc(e.ctx, cancel)
		defer stop()
		if err := ctx.Err(); err != nil {
			e.startErr = err
			return
		}
		if e.startupRequired {
			begin := time.Now()
			if e.config.Logger != nil {
				e.config.Logger.Info("startup probe started")
			}
			e.startErr = e.probeRuntime(ctx)
			if e.config.Logger != nil {
				e.config.Logger.Info("startup probe finished", "elapsed_ms", ms(time.Since(begin)), "error", e.startErr)
			}
			if e.startErr != nil {
				e.broken.Store(true)
				return
			}
		}
		if e.pool != nil {
			if e.startErr = e.pool.start(ctx); e.startErr != nil {
				return
			}
		}
		if e.startErr = ctx.Err(); e.startErr != nil {
			return
		}
		if e.observer != nil {
			e.observer.Start(e.ctx)
		}
		e.startupReady.Store(true)
	})
	if e.closed.Load() && e.startErr == nil {
		return ErrUnhealthy
	}
	return e.startErr
}

func (e *Engine) probeRuntime(ctx context.Context) (retErr error) {
	instance, err := e.createSandbox(ctx)
	if instance == nil {
		return fmt.Errorf("startup probe create: %w", err)
	}
	result := api.Result{Status: "infrastructure_error"}
	defer func() {
		retErr = errors.Join(retErr, e.finalizeExecution(instance, &result))
		if retErr == nil && (result.Status != "succeeded" || result.Stdout != "1" || !result.CleanupComplete) {
			retErr = fmt.Errorf("startup probe failed: status=%s, cleanup=%t", result.Status, result.CleanupComplete)
		}
	}()
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = prepareInput(instance.bundlePath, api.Request{Runtime: "python", Code: "print(1, end='')"}); err != nil {
		return err
	}
	return e.runAndCollectResult(ctx, 2000, instance, &result)
}

func (e *Engine) Ready() bool {
	return (!e.startupRequired || e.startupReady.Load()) && !e.broken.Load() && !e.closed.Load() && (e.pool == nil || e.pool.healthy())
}

func (e *Engine) Status() api.StatusReport {
	if e.pool == nil {
		return api.StatusReport{Mode: "cold", Ready: e.Ready(), QoS: e.observationReport(), Cleanup: e.cleanup.report()}
	}
	snapshot := e.pool.status()
	var memory *api.MemoryBudgetReport
	if e.config.Memory.CapacityBytes > 0 {
		b := e.config.Memory
		memory = &api.MemoryBudgetReport{ParentBytes: b.ParentBytes, SandboxBytes: b.SandboxBytes, ReserveBytes: b.ReserveBytes, CapacityBytes: b.CapacityBytes}
	}
	return api.StatusReport{Memory: memory, SandboxCPUMilli: e.config.SandboxCPUMilli, ParentCPUMilli: e.config.ParentCPUMilli,
		Scheduling: schedulingReport(snapshot), Cleanup: e.cleanup.report(), Inventory: inventoryReport(snapshot),
		QoS:  e.observationReport(),
		Mode: poolMode(e.config.Mode), Ready: snapshot.Ready && (!e.startupRequired || e.startupReady.Load()) && !e.broken.Load() && !e.closed.Load(),
		Target: snapshot.Target, MaxSandboxes: snapshot.MaxSandboxes,
		Creating: snapshot.Creating, Idle: snapshot.Idle, Leased: snapshot.Leased,
		Running: snapshot.Running, Destroying: snapshot.Destroying, Total: snapshot.Total,
		Queued: snapshot.Queued, QueuedBytes: snapshot.QueuedBytes,
		CreationCount: snapshot.CreationCount, CreationFailures: snapshot.CreationFailures,
		CleanupFailures: snapshot.CleanupFailures, CreationTotalMS: snapshot.CreationTotalMS,
	}
}

// Close stops admission, cancels all admitted work and background creation,
// then waits for cleanup before releasing ownership of the runtime state.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed.Store(true)
		e.cancel()
		e.mu.Unlock()
		if e.pool != nil {
			e.pool.stop()
		}
		e.active.Wait()
		if e.pool != nil {
			e.closeErr = e.pool.drain()
		}
		if e.observer != nil {
			e.observer.Close()
		}
		e.closeErr = errors.Join(e.closeErr, e.lock.Close())
	})
	return e.closeErr
}

func poolMode(mode string) string {
	if mode == "cold" {
		return "cold"
	}
	return "warm"
}

func (e *Engine) beginAdmission() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.Ready() {
		return admissionError("executor_unhealthy", ErrUnhealthy)
	}
	if e.pressurePaused {
		e.pressureRejections++
		return admissionError("qos_pressure", ErrPressure)
	}
	e.active.Add(1)
	return nil
}

func (e *Engine) Execute(parent context.Context, request api.Request) (api.Result, error) {
	r, err := request.Validate()
	if err != nil {
		return api.Result{}, err
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(e.ctx, cancel)
	defer stop()
	defer cancel()
	return e.executeLeased(ctx, r)
}

func requestBytes(request api.Request) int {
	bytes := len(request.Code) + len(request.Stdin) + len(request.RequestID) + len(request.Runtime)
	for key, value := range request.Env {
		bytes += len(key) + len(value)
	}
	return bytes
}

func (e *Engine) executeLeased(ctx context.Context, request api.Request) (result api.Result, retErr error) {
	if err := e.beginAdmission(); err != nil {
		return result, err
	}
	defer e.active.Done()
	begin := time.Now()
	execution, err := e.pool.acquire(ctx, requestBytes(request))
	queueMS := ms(time.Since(begin))
	if execution != nil {
		queueMS = max(0, queueMS-ms(execution.creationTime))
	}
	if execution == nil {
		if errors.Is(err, ErrPressure) {
			e.mu.Lock()
			e.pressureRejections++
			e.mu.Unlock()
			return result, err
		}
		if ctx.Err() != nil {
			return api.Result{RequestID: request.RequestID, Status: "cancelled", Timings: api.Timings{QueueMS: queueMS, TotalMS: queueMS}}, nil
		}
		return result, err
	}
	execution.startedAt = begin
	result = api.Result{ExecutionID: execution.id, RequestID: request.RequestID, Status: "infrastructure_error", Timings: api.Timings{QueueMS: queueMS}}
	defer func() { e.pool.release(execution, result.CleanupComplete) }()
	defer func() {
		e.pool.mark(execution, poolDestroying)
		retErr = errors.Join(retErr, e.finalizeExecution(execution, &result))
	}()
	if err != nil {
		result.Timings.SetupMS = ms(execution.creationTime)
		if ctx.Err() != nil {
			result.Status = "cancelled"
			return result, nil
		}
		return result, err
	}
	setupStart := time.Now()
	// A READY instance may have died while idle. Never inject or execute a task
	// until both runtime state and cgroup membership have been checked.
	if execution.creationTime == 0 {
		checkCtx, cancel := context.WithTimeout(ctx, time.Second)
		err = execution.session.checkRunning(checkCtx)
		cancel()
		if err == nil {
			err = execution.group.VerifyPopulated()
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = prepareInput(execution.bundlePath, request)
	}
	result.Timings.SetupMS = ms(time.Since(setupStart) + execution.creationTime)
	if err != nil {
		if ctx.Err() != nil {
			result.Status = "cancelled"
			return result, nil
		}
		return result, fmt.Errorf("prepare leased sandbox: %w", err)
	}
	e.pool.mark(execution, poolRunning)
	retErr = e.runAndCollectResult(ctx, request.TimeoutMS, execution, &result)
	return result, retErr
}

// createSandbox records intent before external side effects and returns partial
// ownership on failure so the caller can always use the same cleanup rules.
func (e *Engine) createSandbox(parent context.Context) (*executionState, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return nil, err
	}
	id := "ab-" + hex.EncodeToString(bytes[:])
	bundle := filepath.Join(e.config.StateDir, "bundles", id)
	if err := os.Mkdir(bundle, 0700); err != nil {
		return nil, err
	}
	group, groupErr := e.groups.NewGroup(id)
	if group == nil {
		if err := os.Remove(bundle); err != nil {
			e.broken.Store(true)
			return nil, errors.Join(groupErr, err)
		}
		return nil, groupErr
	}
	instance := &executionState{id: id, bundlePath: bundle, group: group, startedAt: time.Now()}
	if groupErr != nil {
		return instance, groupErr
	}
	if err := e.prepareSandboxFiles(bundle, group.CgroupPath); err != nil {
		return instance, err
	}
	session, err := newSession(e.runtime, id, bundle, e.config.OutputLimit)
	instance.session = session
	if err != nil {
		return instance, err
	}
	if err := session.create(parent, bundle); err != nil {
		return instance, fmt.Errorf("create sandbox: %w", err)
	}
	if err := group.VerifyPopulated(); err != nil {
		return instance, err
	}
	return instance, nil
}

func (e *Engine) prepareSandboxFiles(bundlePath, hostCgroupPath string) error {
	if err := prepareInputDirectory(bundlePath); err != nil {
		return fmt.Errorf("prepare sandbox input directory: %w", err)
	}
	relativeCgroupPath, err := filepath.Rel("/sys/fs/cgroup", hostCgroupPath)
	if err != nil {
		return fmt.Errorf("resolve OCI cgroup path: %w", err)
	}
	config := makeSpec(e.config.Rootfs, bundlePath, "/"+relativeCgroupPath)
	if err := writeJSON(filepath.Join(bundlePath, "config.json"), config, 0600); err != nil {
		return fmt.Errorf("write OCI config: %w", err)
	}
	if err := writeJSON(filepath.Join(bundlePath, "process.json"), makeTaskProcess(), 0600); err != nil {
		return fmt.Errorf("write OCI task process: %w", err)
	}
	return nil
}

// destroyInstanceResources reclaims an idle, used, or partially created
// instance. It does not change pool state or construct a task result.
func (e *Engine) destroyInstanceResources(instance *executionState) (retErr error) {
	defer func() {
		if retErr != nil {
			e.broken.Store(true)
		}
	}()
	cleanupErr := e.destroyContainerAndCgroup(instance)
	// Finish local streams even when runtime deletion fails. The collector
	// remains readable afterwards, so execution finalization can report output.
	if instance.session != nil {
		_ = e.cleanupPhase(instance, "output_readers", func() error { instance.session.finishStreams(); return nil })
	}
	if cleanupErr != nil {
		return fmt.Errorf("cleanup %s: %w", instance.id, cleanupErr)
	}
	// Keep the durable intent whenever external resource cleanup is uncertain.
	return e.cleanupPhase(instance, "bundle_remove", func() error { return os.RemoveAll(instance.bundlePath) })
}

func (e *Engine) finalizeExecution(execution *executionState, result *api.Result) error {
	cleanupStart := time.Now()
	defer func() {
		result.Timings.CleanupMS = ms(time.Since(cleanupStart))
		result.Timings.TotalMS = ms(time.Since(execution.startedAt))
	}()
	cleanupErr := e.destroyInstanceResources(execution)
	result.CleanupPhases = append([]api.CleanupPhase(nil), execution.cleanupPhases...)
	if execution.session != nil {
		result.Stdout, result.Stderr, result.OutputTruncated = execution.session.outputResult()
		if result.OutputTruncated && (result.Status == "succeeded" || result.Status == "failed") {
			result.Status = "output_limit"
		}
	}
	if cleanupErr != nil {
		result.Status = "infrastructure_error"
		return cleanupErr
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
		case <-s.outputLimitExceeded():
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
		case <-s.outputLimitExceeded():
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
	// Sample before runtime deletion, which may remove the cgroup. This
	// includes startup and warm idle, but is not final lifetime accounting:
	// background tasks or a pending kill may still consume resources afterwards.
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

func (e *Engine) destroyContainerAndCgroup(instance *executionState) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deleteErr := e.cleanupPhase(instance, "runtime_delete", func() error {
		_, err := e.runtime.control(ctx, "delete", "--force", instance.id)
		return err
	})
	// A stalled delete must not consume the fallback's wait budget. The
	// runtime command has 5s (+1s pipe drain); fallback has its own bounded 2s.
	fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer fallbackCancel()
	killErr := e.cleanupPhase(instance, "cgroup_kill", instance.group.Kill)
	removeErr := e.cleanupPhase(instance, "cgroup_empty_and_remove", func() error { return instance.group.Remove(fallbackCtx) })
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
		instance := &executionState{
			id: entry.Name(), bundlePath: filepath.Join(e.config.StateDir, "bundles", entry.Name()), group: g,
		}
		if err = e.destroyInstanceResources(instance); err != nil {
			return err
		}
	}
	return nil
}
