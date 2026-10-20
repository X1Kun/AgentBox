//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/agent"
	"github.com/X1Kun/AgentBox/internal/deepseek"
)

// Live API calls are opt-in, separately selected, and never run in make check.
func TestDeepSeekLiveRuntime(t *testing.T) {
	if os.Getenv("AGENTBOX_LIVE_DEEPSEEK") != "1" {
		t.Skip("explicit live API opt-in and private settings are required")
	}
	settings := liveDeepSeekSettings(t)
	model, err := deepseek.New(settings["DEEPSEEK_BASE_URL"], settings["DEEPSEEK_API_KEY"], settings["DEEPSEEK_MODEL"])
	if err != nil {
		t.Fatal("invalid private DeepSeek settings")
	}
	model.Close() // Validate without making a request; only the ordinary Agent calls the API.
	f := gatewayRuntime(t, "warm", 2)
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	prompt := "Perform this exact sandbox tool demonstration. Do not call get_status. First call execute_python once with code that asserts DEEPSEEK_API_KEY, AGENTBOX_GATEWAY_TOKEN, AGENTBOX_DAEMON_TOKEN and AGENTBOX_API_TOKEN are absent from os.environ, then runs print(1/0). Read its error. Then call execute_python once with print(6*7). Read the actual output and give a concise final answer containing the numeric result. Do not request credentials or use any other tools."
	command := exec.CommandContext(ctx, os.Getenv("AGENTBOX_AGENT_BINARY"),
		"-mcp-command", os.Getenv("AGENTBOX_MCP_BINARY"), "-gateway-url", f.url,
		"-prompt", prompt, "-max-turns", "4", "-max-tool-calls", "3", "-timeout", "100s", "-json")
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8",
		"DEEPSEEK_API_KEY=" + settings["DEEPSEEK_API_KEY"],
		"DEEPSEEK_BASE_URL=" + settings["DEEPSEEK_BASE_URL"],
		"DEEPSEEK_MODEL=" + settings["DEEPSEEK_MODEL"],
		"AGENTBOX_GATEWAY_TOKEN=" + runtimeClientToken}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	redact := func(s string) string { return strings.ReplaceAll(s, settings["DEEPSEEK_API_KEY"], "[redacted]") }
	if err := command.Run(); err != nil {
		details := redact(stderr.String())
		if len(details) > 4096 {
			details = details[:4096]
		}
		t.Fatalf("live Agent failed: %v; %s", err, details)
	}
	var result agent.Result
	if json.Unmarshal(stdout.Bytes(), &result) != nil {
		t.Fatal("Agent did not return valid final JSON")
	}
	if result.Turns < 3 || result.Turns > 4 || result.ToolCalls < 2 || result.ToolCalls > 3 || !strings.Contains(result.Answer, "42") {
		t.Fatal("live model did not complete the requested bounded repair demonstration")
	}
	var events []gatewayCompletion
	for {
		select {
		case event := <-f.completed:
			events = append(events, event)
		default:
			goto collected
		}
	}
collected:
	if len(events) != 2 {
		t.Fatalf("expected exactly two real sandbox executions, got %d", len(events))
	}
	if events[0].Err != nil || events[0].Result.Status != "failed" || !strings.Contains(events[0].Result.Stderr, "ZeroDivisionError") ||
		events[1].Err != nil || events[1].Result.Status != "succeeded" || events[1].Result.Stdout != "42\n" {
		t.Fatal("actual sandbox results did not match error followed by correction")
	}
	for _, event := range events {
		if !event.Result.CleanupComplete {
			t.Fatal("real execution cleanup was not confirmed")
		}
		f.assertRemoved(t, event.Result.ExecutionID)
	}
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, f.config)
	summary, _ := json.Marshal(map[string]any{"provider": "DeepSeek live API", "model": redact(settings["DEEPSEEK_MODEL"]),
		"agent_uid": 1000, "turns": result.Turns, "tool_calls": result.ToolCalls,
		"execution_statuses": []string{events[0].Result.Status, events[1].Result.Status},
		"cleanup_complete":   true, "answer": redact(result.Answer)})
	t.Logf("DEEPSEEK_LIVE_RESULT %s", summary)
}

func liveDeepSeekSettings(t *testing.T) map[string]string {
	t.Helper()
	path := os.Getenv("AGENTBOX_DEEPSEEK_CONFIG")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatal("live settings must be a private regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 16<<10 {
		t.Fatal("cannot read bounded private live settings")
	}
	settings := map[string]string{"DEEPSEEK_BASE_URL": "https://api.deepseek.com", "DEEPSEEK_MODEL": "deepseek-flash"}
	allowed := map[string]bool{"DEEPSEEK_API_KEY": true, "DEEPSEEK_BASE_URL": true, "DEEPSEEK_MODEL": true}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] || seen[key] {
			t.Fatal("unsupported or duplicate live configuration key")
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		settings[key] = value
	}
	if settings["DEEPSEEK_API_KEY"] == "" {
		t.Fatal("DeepSeek API key is missing")
	}
	return settings
}
