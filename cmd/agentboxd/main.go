package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/doctor"
	"github.com/X1Kun/AgentBox/internal/httpapi"
	"github.com/X1Kun/AgentBox/internal/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var c sandbox.Config
	var address string
	var inspect bool
	flag.StringVar(&c.Runsc, "runsc", "runsc", "path to runsc")
	flag.StringVar(&c.Rootfs, "rootfs", ".local/rootfs", "trusted Python rootfs")
	flag.StringVar(&c.StateDir, "state-dir", ".state", "private runtime state directory")
	flag.StringVar(&c.CgroupRoot, "cgroup-root", "auto", "delegated cgroup v2 subtree; auto uses current unit and moves daemon to manager leaf")
	flag.StringVar(&c.Platform, "platform", "kvm", "explicit gVisor platform: kvm or systrap; no automatic fallback")
	flag.IntVar(&c.Concurrency, "concurrency", 1, "concurrent cold executions, 1 to 4")
	flag.StringVar(&address, "listen", "127.0.0.1:8080", "loopback HTTP address")
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
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("listen must use an explicit loopback IP address")
	}
	token := os.Getenv("AGENTBOX_API_TOKEN")
	if len(token) < 32 {
		return fmt.Errorf("AGENTBOX_API_TOKEN must contain at least 32 characters")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	engine, err := sandbox.New(c)
	if err != nil {
		return err
	}
	defer engine.Close()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: httpapi.Handler(engine, token, logger), ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 50 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("agentbox daemon listening", "address", listener.Addr().String(), "platform", c.Platform, "mode", "cold-one-shot")
	select {
	case err := <-done:
		cancel()
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
