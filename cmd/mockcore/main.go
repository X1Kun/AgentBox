package main

import (
	"context"
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

	"github.com/X1Kun/AgentBox/internal/mockcore"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var c mockcore.Config
	var address string
	flag.IntVar(&c.Iterations, "iterations", 50000, "fixed SHA-256 iterations per request, 1..2000000")
	flag.IntVar(&c.Concurrency, "concurrency", 4, "maximum in-flight core requests, 1..16")
	flag.StringVar(&address, "listen", "127.0.0.1:8081", "explicit loopback IP and port")
	flag.Parse()
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("listen must use an explicit loopback IP")
	}
	handler, err := mockcore.Handler(c)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 8 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8192, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("mock core listening", "address", listener.Addr().String(), "iterations", c.Iterations, "concurrency", c.Concurrency)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, stop := context.WithTimeout(context.Background(), 6*time.Second)
		defer stop()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
