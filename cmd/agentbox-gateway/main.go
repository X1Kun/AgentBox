package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/gateway"
	"github.com/X1Kun/AgentBox/internal/localhttp"
	"github.com/X1Kun/AgentBox/internal/systemd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var c gateway.Config
	var address string
	flag.StringVar(&address, "listen", "127.0.0.1:8080", "explicit loopback HTTP address")
	flag.StringVar(&c.Socket, "daemon-socket", "/run/agentbox/daemon.sock", "daemon Unix socket")
	flag.IntVar(&c.MaxInflight, "max-inflight", 8, "maximum active execution requests, 1..64; no gateway queue")
	flag.DurationVar(&c.RequestTimeout, "request-timeout", 50*time.Second, "end-to-end request budget, at most 1m")
	flag.Parse()
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 50 * time.Second
	}
	if err := localhttp.ValidateLoopback(address); err != nil {
		return err
	}
	c.Token, c.DaemonToken = os.Getenv("AGENTBOX_GATEWAY_TOKEN"), os.Getenv("AGENTBOX_DAEMON_TOKEN")
	c.Logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	handler, err := gateway.New(c)
	if err != nil {
		return err
	}
	defer handler.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: c.RequestTimeout + 5*time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	if err := systemd.Ready(); err != nil {
		_ = server.Close()
		return err
	}
	c.Logger.Info("agentbox gateway listening", "address", listener.Addr().String(), "daemon_socket", c.Socket)
	select {
	case err := <-done:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		_ = systemd.Stopping()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
