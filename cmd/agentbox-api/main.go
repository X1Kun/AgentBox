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
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/batch"
	"github.com/X1Kun/AgentBox/internal/batchapi"
	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/sessionapi"
	"github.com/X1Kun/AgentBox/internal/sessionruntime"
	"github.com/X1Kun/AgentBox/internal/telemetry"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var cfg session.Config
	var batchCfg batch.Config
	var mode string
	var metricsListen string
	var listen, kubeconfig, principalsFile, cert, key string
	var timeout time.Duration
	var maxInflight int
	var execution bool
	flag.StringVar(&mode, "mode", "session", "API mode: session or batch")
	flag.StringVar(&metricsListen, "metrics-listen", "", "optional separate HTTP metrics listener; explicit IP required")
	flag.StringVar(&batchCfg.DataDir, "batch-data-dir", "", "mounted batch artifacts PVC root")
	flag.StringVar(&batchCfg.TemplateFile, "batch-template-file", "", "administrator-controlled image Job YAML")
	flag.StringVar(&batchCfg.ContractFile, "batch-contract-file", "", "fixed dataset/output contract JSON")
	flag.StringVar(&batchCfg.WorkerImageID, "batch-image-id", "", "verified worker image ID")
	flag.StringVar(&batchCfg.WorkerImageDigest, "batch-image-digest", "", "verified worker manifest digest from the same CRI image")
	flag.DurationVar(&batchCfg.Retention, "batch-retention", 24*time.Hour, "artifact download retention, 1m..168h")
	flag.IntVar(&batchCfg.MaxRecords, "batch-max-records", 128, "bounded durable submission records, 1..1024")
	flag.StringVar(&listen, "listen", "127.0.0.1:8090", "HTTP address; non-loopback addresses require TLS")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "explicit scoped kubeconfig; default is in-cluster credentials")
	flag.StringVar(&cfg.Namespace, "namespace", "", "fixed workload namespace")
	flag.StringVar(&cfg.Pool, "pool", "", "fixed administrator-managed SandboxWarmPool")
	flag.StringVar(&cfg.Instance, "instance", "", "stable API installation identity; keep across restarts")
	flag.StringVar(&cfg.RuntimeClass, "runtime-class", "agentbox-gvisor", "required execution RuntimeClass")
	flag.IntVar(&cfg.DefaultTTL, "default-ttl-seconds", 600, "default hard session TTL")
	flag.IntVar(&cfg.MaxTTL, "max-ttl-seconds", 3600, "maximum hard session TTL")
	flag.DurationVar(&cfg.CloseWait, "close-wait", time.Second, "bounded wait for upstream cleanup; max 10s")
	flag.DurationVar(&timeout, "request-timeout", 5*time.Second, "Kubernetes request budget, 1s..30s")
	flag.IntVar(&maxInflight, "max-inflight", 16, "maximum active HTTP lifecycle requests")
	flag.StringVar(&principalsFile, "principals-file", "", "private JSON file containing principal id/token pairs")
	flag.StringVar(&cert, "tls-cert-file", "", "TLS certificate for non-loopback serving")
	flag.StringVar(&key, "tls-key-file", "", "TLS private key")
	flag.BoolVar(&execution, "enable-execution", false, "enable upstream sandboxd calls; requires Pod network access")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	host, _, err := net.SplitHostPort(listen)
	if mode != "session" && mode != "batch" {
		return errors.New("mode must be session or batch")
	}
	if err != nil {
		return errors.New("invalid listen address")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("listen host must be an explicit IP address")
	}
	if (cert == "") != (key == "") {
		return errors.New("both TLS files are required")
	}
	if !ip.IsLoopback() && cert == "" {
		return errors.New("non-loopback serving requires TLS")
	}
	stat, err := os.Stat(principalsFile)
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0037 != 0 || (stat.Mode().Perm()&0040 != 0 && !sameGroup(stat)) || stat.Size() > 300000 {
		return errors.New("principals file must be a private regular file")
	}
	data, err := os.ReadFile(principalsFile)
	if err != nil || len(data) > 300000 {
		return errors.New("unable to read bounded principals configuration")
	}
	var principals []sessionapi.Principal
	if json.Unmarshal(data, &principals) != nil {
		return errors.New("invalid principals configuration")
	}
	var config *rest.Config
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	if err != nil {
		return errors.New("unable to load Kubernetes credentials")
	}
	config.Timeout = timeout
	config.QPS, config.Burst = 20, 40
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return errors.New("unable to configure Kubernetes client")
	}
	var handler interface {
		http.Handler
		RunReadiness(context.Context)
		SetTelemetry(*telemetry.Metrics)
	}
	var metrics *telemetry.Metrics
	if metricsListen != "" {
		host, _, e := net.SplitHostPort(metricsListen)
		if e != nil || net.ParseIP(host) == nil {
			return errors.New("metrics listener requires an explicit IP")
		}
		metrics = telemetry.New(mode)
	}
	if mode == "batch" {
		batchCfg.Namespace, batchCfg.Instance = cfg.Namespace, cfg.Instance
		backend, e := batch.New(client, batchCfg)
		if e != nil {
			return e
		}
		defer backend.Close()
		backend.SetTelemetry(metrics)
		handler, err = batchapi.New(backend, principals, timeout, maxInflight)
	} else {
		backend, e := session.New(client, cfg)
		if e != nil {
			return e
		}
		if execution {
			backend.SetRuntime(sessionruntime.New())
		}
		backend.SetTelemetry(metrics)
		handler, err = sessionapi.New(backend, principals, timeout, maxInflight)
	}
	if err != nil {
		return err
	}
	handler.SetTelemetry(metrics)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	var publicHandler http.Handler = handler
	if metrics != nil {
		publicHandler = metrics.Wrap(handler)
	}
	server := &http.Server{Handler: publicHandler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 76 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if metrics != nil {
		metricListener, e := net.Listen("tcp", metricsListen)
		if e != nil {
			return e
		}
		metricServer := &http.Server{Handler: metrics.Handler(), ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 4096}
		defer metricServer.Close()
		go func() {
			if e := metricServer.Serve(metricListener); e != nil && !errors.Is(e, http.ErrServerClosed) {
				logger.Error("metrics listener stopped", "reason", "serve_error")
			}
		}()
		logger.Info("metrics listening", "mode", mode, "address", metricListener.Addr().String())
	}
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		handler.RunReadiness(monitorCtx)
	}()
	defer func() {
		cancelMonitor()
		<-monitorDone
	}()
	message := "session lifecycle API listening"
	if mode == "batch" {
		message = "batch API listening"
	}
	logger.Info(message, "mode", mode, "address", listener.Addr().String(), "namespace", cfg.Namespace, "pool", cfg.Pool, "instance", cfg.Instance, "tls", cert != "")
	done := make(chan error, 1)
	go func() {
		if cert != "" {
			done <- server.ServeTLS(listener, cert, key)
		} else {
			done <- server.Serve(listener)
		}
	}()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

// Kubernetes Secret volumes may be made group-readable by fsGroup. Accept
// that only for this process's group; other/world access stays forbidden.
func sameGroup(info os.FileInfo) bool {
	value, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(value.Gid) == os.Getegid()
}
