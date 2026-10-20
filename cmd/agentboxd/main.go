package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/doctor"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/localhttp"
	"github.com/X1Kun/AgentBox/internal/sandbox"
	"github.com/X1Kun/AgentBox/internal/systemd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (retErr error) {
	var c sandbox.Config
	var address string
	var inspect bool
	flag.StringVar(&c.Runsc, "runsc", "runsc", "path to runsc")
	flag.StringVar(&c.Rootfs, "rootfs", ".local/rootfs", "trusted Python rootfs")
	flag.StringVar(&c.StateDir, "state-dir", ".state", "private runtime state directory")
	flag.StringVar(&c.CgroupRoot, "cgroup-root", "auto", "delegated cgroup v2 subtree; auto uses current unit and moves daemon to manager leaf")
	flag.StringVar(&c.Platform, "platform", "kvm", "explicit gVisor platform: kvm or systrap; no automatic fallback")
	flag.IntVar(&c.Concurrency, "concurrency", 1, "concurrent executions, 1 to 4")
	flag.StringVar(&c.Mode, "mode", "cold", "execution mode: cold or once-only warm pool")
	flag.IntVar(&c.WarmPoolSize, "warm-size", 3, "target READY sandbox inventory")
	flag.BoolVar(&c.DynamicWarmPool, "warm-dynamic", false, "adapt READY inventory within configured bounds (warm mode only)")
	flag.IntVar(&c.WarmMinSize, "warm-min-size", 1, "minimum dynamic READY inventory")
	flag.IntVar(&c.WarmMaxSize, "warm-max-size", 0, "maximum dynamic READY inventory; 0 uses warm-size")
	flag.DurationVar(&c.WarmAdjustInterval, "warm-adjust-interval", time.Second, "minimum inventory action interval, 100ms to 1m")
	flag.DurationVar(&c.WarmDownscaleWindow, "warm-downscale-window", 30*time.Second, "downscale recommendation stabilization window, up to 1h")
	flag.IntVar(&c.MaxSandboxes, "max-sandboxes", 6, "total sandbox cap, including creation and destruction")
	flag.IntVar(&c.CreateConcurrency, "create-concurrency", 1, "background sandbox creation concurrency, 1 or 2")
	flag.IntVar(&c.QueueSize, "queue-size", 16, "maximum waiting warm requests")
	flag.IntVar(&c.QueueBytes, "queue-bytes", 8<<20, "maximum queued request payload bytes")
	flag.DurationVar(&c.QueueTimeout, "queue-timeout", 100*time.Millisecond, "maximum warm queue wait, 1ms to 5s")
	flag.StringVar(&c.QoS.Mode, "qos", "off", "QoS mode: off, observe or enforce")
	flag.BoolVar(&c.QoS.CPUControl, "qos-cpu-control", false, "preview AIMD in observe mode; automatic in enforce mode")
	flag.BoolVar(&c.QoS.Scheduling, "qos-scheduling", false, "preview or enforce pressure admission and refill gates")
	flag.Float64Var(&c.QoS.CPU.MinCPU, "qos-min-cpu", .5, "minimum sandbox CPU bandwidth")
	flag.Float64Var(&c.QoS.CPU.MaxCPU, "qos-max-cpu", 4, "maximum sandbox CPU bandwidth, at most 4")
	flag.Float64Var(&c.QoS.CPU.LowPressure, "qos-cpu-low", 10, "CPU PSI recovery threshold in percent")
	flag.Float64Var(&c.QoS.CPU.HighPressure, "qos-cpu-high", 20, "CPU PSI suppression threshold in percent")
	flag.DurationVar(&c.QoS.CPU.DecisionInterval, "qos-control-interval", time.Second, "minimum CPU action interval and pressure confirmation duration")
	flag.DurationVar(&c.QoS.CPU.StableFor, "qos-stable-for", 2*time.Second, "continuous low pressure duration before recovery")
	flag.Float64Var(&c.QoS.CPU.DecreaseFactor, "qos-decrease-factor", .8, "AIMD multiplier, at least 0.5 and below 1")
	flag.Float64Var(&c.QoS.CPU.IncreaseCPUPerSecond, "qos-increase-cpu", .25, "maximum additive CPU recovery rate per second")
	flag.DurationVar(&c.QoS.Interval, "qos-interval", 100*time.Millisecond, "observation interval, 50ms to 1s")
	flag.StringVar(&c.QoS.ProtectedCgroup, "qos-protected-cgroup", "none", "absolute protected cgroup path; required in observe/enforce mode")
	flag.StringVar(&address, "listen", "127.0.0.1:8080", "loopback HTTP address or unix:/absolute/socket")
	flag.BoolVar(&inspect, "doctor", false, "print read-only environment report and exit")
	flag.Parse()
	if inspect {
		r := doctor.Inspect(c.Runsc, c.Platform, c.Rootfs, c.CgroupRoot)
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		if err := e.Encode(r); err != nil {
			return err
		}
		if !r.Ready {
			return fmt.Errorf("environment checks failed; see report")
		}
		return nil
	}
	if err := localhttp.ValidateListen(address); err != nil {
		return err
	}
	token := os.Getenv("AGENTBOX_API_TOKEN")
	if len(token) < 32 {
		return fmt.Errorf("AGENTBOX_API_TOKEN must contain at least 32 characters")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	c.Logger = logger
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	engine, err := sandbox.New(c)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, engine.Close()) }()
	startupCtx, startupCancel := context.WithTimeout(ctx, 20*time.Second)
	logger.Info("engine startup started", "mode", c.Mode, "platform", c.Platform)
	startupStarted := time.Now()
	err = engine.Start(startupCtx)
	startupCancel()
	if err != nil {
		logger.Error("engine startup failed", "elapsed_ms", float64(time.Since(startupStarted))/float64(time.Millisecond), "error", err)
		return err
	}
	logger.Info("engine startup completed", "elapsed_ms", float64(time.Since(startupStarted))/float64(time.Millisecond))
	listener, err := localhttp.Listen(address)
	if err != nil {
		return err
	}
	defer listener.Close()
	var handler http.Handler = httpapi.Handler(engine, token, logger)
	if strings.HasPrefix(address, "unix:") {
		handler = httpapi.DeadlineHandler(handler, 50*time.Second)
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 50 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	if err := systemd.Ready(); err != nil {
		cancel()
		_ = server.Close()
		return err
	}
	logger.Info("agentbox daemon listening", "address", listener.Addr().String(), "platform", c.Platform, "mode", c.Mode, "qos", c.QoS.Mode, "state_dir", c.StateDir, "rootfs", c.Rootfs)
	select {
	case err := <-done:
		cancel()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		if err := systemd.Stopping(); err != nil {
			logger.Warn("systemd stopping notification failed", "error", err)
		}
		// Stop admission and refill immediately, alongside HTTP draining. The
		// deferred Close joins this call and reports its cleanup error.
		go func() { _ = engine.Close() }()
		// Active setup may need its independent 10s transaction deadline plus
		// resource cleanup before its handler can return.
		shutdownCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
