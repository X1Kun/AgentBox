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

	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/sessionapi"
	"github.com/X1Kun/AgentBox/internal/sessionruntime"
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
	var listen, kubeconfig, principalsFile, cert, key string
	var timeout time.Duration
	var maxInflight int
	var execution bool
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
	backend, err := session.New(client, cfg)
	if err != nil {
		return err
	}
	if execution {
		backend.SetRuntime(sessionruntime.New())
	}
	handler, err := sessionapi.New(backend, principals, timeout, maxInflight)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 76 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	logger.Info("session lifecycle API listening", "address", listener.Addr().String(), "namespace", cfg.Namespace, "pool", cfg.Pool, "instance", cfg.Instance, "tls", cert != "")
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
