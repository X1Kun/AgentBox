package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/X1Kun/AgentBox/internal/gatewayclient"
	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var address string
	flag.StringVar(&address, "gateway-url", "http://127.0.0.1:8080", "loopback Gateway URL; remote access uses an SSH tunnel")
	flag.Parse()
	client, err := gatewayclient.New(address, os.Getenv("AGENTBOX_GATEWAY_TOKEN"))
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	err = mcpserver.New(ctx, client, logger).Run(ctx, &mcp.StdioTransport{})
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
