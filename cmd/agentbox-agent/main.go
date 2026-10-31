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
	var prompt, command, apiURL, caFile, base, model string
	var turns, calls, ttl int
	var budget, readyBudget, cleanupBudget time.Duration
	var outputJSON bool
	flag.StringVar(&prompt, "prompt", "", "task to send to DeepSeek (required)")
	flag.StringVar(&command, "mcp-command", filepath.Join(filepath.Dir(self), "agentbox-mcp"), "trusted local MCP executable")
	flag.StringVar(&apiURL, "api-url", "http://127.0.0.1:8090", "v2 session API URL")
	flag.StringVar(&caFile, "api-ca-file", "", "optional trusted API CA bundle")
	flag.StringVar(&base, "deepseek-base-url", environment("DEEPSEEK_BASE_URL", "https://api.deepseek.com"), "DeepSeek API base URL")
	flag.StringVar(&model, "model", environment("DEEPSEEK_MODEL", "deepseek-flash"), "DeepSeek tool-calling model")
	flag.IntVar(&turns, "max-turns", 6, "maximum model requests, 1..20")
	flag.IntVar(&calls, "max-tool-calls", 8, "maximum tool calls, 1..32")
	flag.DurationVar(&budget, "timeout", 3*time.Minute, "whole Agent conversation budget, at most 10m")
	flag.IntVar(&ttl, "session-ttl-seconds", 600, "hard session TTL, 60..3600 seconds")
	flag.DurationVar(&readyBudget, "ready-timeout", time.Minute, "bounded wait for readiness, at most 3m")
	flag.DurationVar(&cleanupBudget, "cleanup-timeout", 20*time.Second, "detached cleanup budget, at most 2m")
	flag.BoolVar(&outputJSON, "json", false, "print final answer and call counts as JSON")
	flag.Parse()
	if len(prompt) == 0 || len(prompt) > 32<<10 || turns < 1 || turns > 20 || calls < 1 || calls > 32 || budget <= 0 || budget > 10*time.Minute || flag.NArg() != 0 || ttl < 60 || ttl > 3600 || readyBudget < time.Second || readyBudget > 3*time.Minute || cleanupBudget < time.Second || cleanupBudget > 2*time.Minute {
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
	session, err := mcpclient.Connect(ctx, command, apiURL, os.Getenv("AGENTBOX_API_TOKEN"), caFile)
	if err != nil {
		return err
	}
	defer session.Close()
	result, err := (agent.Runner{Model: modelClient, Tools: session, MaxTurns: turns, MaxToolCalls: calls, SessionTTLSeconds: ttl, ReadyTimeout: readyBudget, CleanupTimeout: cleanupBudget}).Run(ctx, prompt)
	if outputJSON {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if err != nil {
		return fmt.Errorf("%w (request_id=%s session_id=%s cleanup_complete=%t)", err, result.RequestID, result.SessionID, result.CleanupComplete)
	}
	fmt.Fprintln(os.Stdout, result.Answer)
	return nil
}
