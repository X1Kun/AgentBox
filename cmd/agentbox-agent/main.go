package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/X1Kun/AgentBox/internal/agent"
	"github.com/X1Kun/AgentBox/internal/deepseek"
	"github.com/X1Kun/AgentBox/internal/mcpclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func environment(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func run() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var prompt, command, gatewayURL, base, model string
	var turns, calls int
	var budget time.Duration
	var outputJSON bool
	flag.StringVar(&prompt, "prompt", "", "task to send to DeepSeek (required)")
	flag.StringVar(&command, "mcp-command", filepath.Join(filepath.Dir(self), "agentbox-mcp"), "trusted local MCP executable")
	flag.StringVar(&gatewayURL, "gateway-url", "http://127.0.0.1:8080", "loopback Gateway URL; an SSH tunnel may forward a remote Gateway")
	flag.StringVar(&base, "deepseek-base-url", environment("DEEPSEEK_BASE_URL", "https://api.deepseek.com"), "DeepSeek API base URL")
	flag.StringVar(&model, "model", environment("DEEPSEEK_MODEL", "deepseek-flash"), "DeepSeek tool-calling model")
	flag.IntVar(&turns, "max-turns", 6, "maximum model requests, 1..20")
	flag.IntVar(&calls, "max-tool-calls", 8, "maximum tool calls, 1..32")
	flag.DurationVar(&budget, "timeout", 3*time.Minute, "whole Agent conversation budget, at most 10m")
	flag.BoolVar(&outputJSON, "json", false, "print final answer and call counts as JSON")
	flag.Parse()
	if len(prompt) == 0 || len(prompt) > 32<<10 || turns < 1 || turns > 20 || calls < 1 || calls > 32 || budget <= 0 || budget > 10*time.Minute {
		return fmt.Errorf("provide -prompt and valid Agent limits")
	}
	modelClient, err := deepseek.New(base, os.Getenv("DEEPSEEK_API_KEY"), model)
	if err != nil {
		return err
	}
	defer modelClient.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	session, err := mcpclient.Connect(ctx, command, gatewayURL, os.Getenv("AGENTBOX_GATEWAY_TOKEN"))
	if err != nil {
		return err
	}
	defer session.Close()
	result, err := (agent.Runner{Model: modelClient, Tools: session, MaxTurns: turns, MaxToolCalls: calls}).Run(ctx, prompt)
	if err != nil {
		return err
	}
	if outputJSON {
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	fmt.Fprintln(os.Stdout, result.Answer)
	return nil
}
