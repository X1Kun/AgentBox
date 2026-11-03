// agentbox-observer is a read-only namespace and kubelet metrics collector.
package main

import (
	"context"
	"crypto/x509"
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

	"github.com/X1Kun/AgentBox/internal/observer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type config struct {
	Namespaces map[string]string `json:"namespaces"`
	Nodes      []struct {
		Role       string `json:"role"`
		URL        string `json:"url"`
		CAFile     string `json:"ca_file"`
		ServerName string `json:"server_name"`
	} `json:"nodes"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var kubeconfig, configFile, listen string
	var interval time.Duration
	flag.StringVar(&kubeconfig, "kubeconfig", "", "explicit read-only kubeconfig; no implicit credentials")
	flag.StringVar(&configFile, "config", "", "private observed namespace/owned node configuration")
	flag.StringVar(&listen, "listen", "127.0.0.1:19101", "loopback-only metrics listener")
	flag.DurationVar(&interval, "interval", 5*time.Second, "bounded poll interval, 2s..30s")
	flag.Parse()
	host, _, err := net.SplitHostPort(listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || kubeconfig == "" || configFile == "" || flag.NArg() != 0 || interval < 2*time.Second || interval > 30*time.Second {
		return errors.New("observer requires explicit read-only configuration and a loopback listener")
	}
	data, err := os.ReadFile(configFile)
	if err != nil || len(data) > 64<<10 {
		return errors.New("unable to read bounded observer configuration")
	}
	var cfg config
	if json.Unmarshal(data, &cfg) != nil {
		return errors.New("invalid observer configuration")
	}
	kube, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return errors.New("unable to load observer credentials")
	}
	kube.Timeout = 4 * time.Second
	kube.QPS = 5
	kube.Burst = 10
	client, err := dynamic.NewForConfig(kube)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	registry := prometheus.NewRegistry()
	inventory, err := observer.New(client, cfg.Namespaces, 3*interval, logger)
	if err != nil {
		return err
	}
	inventory.Register(registry)
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	targets := []observer.RuntimeTarget{}
	for _, node := range cfg.Nodes {
		cert, err := os.ReadFile(node.CAFile)
		if err != nil {
			return errors.New("unable to read owned-node serving certificate")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(cert) {
			return errors.New("invalid owned-node serving certificate")
		}
		transport, err := rest.TransportFor(&rest.Config{TLSClientConfig: rest.TLSClientConfig{CAData: cert, ServerName: node.ServerName}, BearerToken: kube.BearerToken, BearerTokenFile: kube.BearerTokenFile})
		if err != nil {
			return err
		}
		// TransportFor verifies TLS and adds only the observer service-account credential.
		targets = append(targets, observer.RuntimeTarget{Role: node.Role, URL: node.URL, Client: &http.Client{Transport: transport, Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
	}
	var resources *observer.Runtime
	if len(targets) > 0 {
		resources, err = observer.NewRuntime(targets, cfg.Namespaces, 3*interval)
		if err != nil {
			return err
		}
		registry.MustRegister(resources)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{MaxRequestsInFlight: 2, Timeout: 2 * time.Second}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, MaxHeaderBytes: 4096}
	defer server.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for ctx.Err() == nil {
			budget, stop := context.WithTimeout(ctx, 4*time.Second)
			_ = inventory.Poll(budget)
			stop()
			if resources != nil {
				budget, stop = context.WithTimeout(ctx, 4*time.Second)
				_ = resources.Poll(budget)
				stop()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	go func() { <-ctx.Done(); _ = server.Close() }()
	logger.Info("observer listening", "address", listener.Addr().String())
	err = server.Serve(listener)
	cancel()
	<-done
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
