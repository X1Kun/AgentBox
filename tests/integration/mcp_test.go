//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/agent"
	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/deepseek"
	"github.com/X1Kun/AgentBox/internal/gatewayclient"
	"github.com/X1Kun/AgentBox/internal/mcpclient"
	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type disconnectableMCPTransport struct {
	mcp.Transport
	connection mcp.Connection
}

func (t *disconnectableMCPTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := t.Transport.Connect(ctx)
	t.connection = connection
	return connection, err
}

func TestMCPRuntime(t *testing.T) {
	f := gatewayRuntime(t, "warm", 2)
	command, err := mcpclient.Command(os.Getenv("AGENTBOX_MCP_BINARY"), f.url, runtimeClientToken)
	if err != nil {
		t.Fatal(err)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	transport := &disconnectableMCPTransport{Transport: &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "runtime-test", Version: "1"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	listed, err := cs.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 2 {
		t.Fatalf("stdio discovery: %+v %v", listed, err)
	}
	for _, tc := range []struct {
		name, code, status string
		timeout            int
	}{
		{"success", "import os\nassert not any(k in os.environ for k in ('DEEPSEEK_API_KEY','AGENTBOX_GATEWAY_TOKEN','AGENTBOX_DAEMON_TOKEN','AGENTBOX_API_TOKEN'))\nprint('mcp-ok')", "succeeded", 5000},
		{"python-error", "raise ValueError('expected MCP failure')", "failed", 5000},
		{"timeout", "while True: pass", "timeout", 100},
	} {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"code": tc.code, "timeout_ms": tc.timeout}})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var reply gatewayclient.Reply
		_ = json.Unmarshal(raw, &reply)
		var result api.Result
		_ = json.Unmarshal(reply.Response, &result)
		if reply.HTTPStatus != 200 || result.Status != tc.status || !result.CleanupComplete || res.IsError != (tc.status != "succeeded") {
			t.Fatalf("%s: %+v isError=%v", tc.name, result, res.IsError)
		}
		f.completion(t, result.RequestID)
		f.assertRemoved(t, result.ExecutionID)
		t.Logf("MCP_RUNTIME case=%s status=%s cleanup=%v", tc.name, result.Status, result.CleanupComplete)
	}
	var completions atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []deepseek.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		n := completions.Add(1)
		message := deepseek.Message{Role: "assistant"}
		finish := "tool_calls"
		switch n {
		case 1:
			message.ToolCalls = []deepseek.ToolCall{{ID: "bad-code", Type: "function", Function: deepseek.FunctionCall{Name: mcpserver.ExecuteTool, Arguments: `{"code":"print(1/0)"}`}}}
		case 2:
			last := request.Messages[len(request.Messages)-1]
			if last.ToolCallID != "bad-code" || !strings.Contains(last.Content, "ZeroDivisionError") {
				t.Error("real Python error not returned to model")
			}
			message.ToolCalls = []deepseek.ToolCall{{ID: "fixed-code", Type: "function", Function: deepseek.FunctionCall{Name: mcpserver.ExecuteTool, Arguments: `{"code":"print(6*7)"}`}}}
		case 3:
			last := request.Messages[len(request.Messages)-1]
			if last.ToolCallID != "fixed-code" || !strings.Contains(last.Content, `42\n`) {
				t.Error("real execution result not returned to model")
			}
			message.Content = "Observed Python result: 42"
			finish = "stop"
		default:
			t.Error("unexpected provider request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
	}))
	defer provider.Close()
	model, err := deepseek.New(provider.URL, "mock-provider-key", "mock-model")
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	result, err := (agent.Runner{Model: model, Tools: cs}).Run(ctx, "Use Python to compute 6*7, correcting errors")
	if err != nil || result.Answer != "Observed Python result: 42" || result.ToolCalls != 2 || result.Turns != 3 {
		t.Fatalf("real runtime repair loop: %+v %v", result, err)
	}
	t.Logf("MCP_AGENT turns=%d tool_calls=%d answer=%q (mock DeepSeek API, real runsc)", result.Turns, result.ToolCalls, result.Answer)
	for _, disconnect := range []bool{false, true} {
		callCtx, callCancel := context.WithCancel(ctx)
		pending := make(chan error, 1)
		go func() {
			_, err := cs.CallTool(callCtx, &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"code": "import time; time.sleep(20)", "timeout_ms": 30000}})
			pending <- err
		}()
		id := f.task(t)
		begin := time.Now()
		if disconnect {
			// Session.Close is a graceful drain. Close the actual stdio transport
			// to model loss of the client rather than waiting for task completion.
			_ = transport.connection.Close()
		} else {
			callCancel()
		}
		var completion gatewayCompletion
		deadline := time.NewTimer(7 * time.Second)
		found := false
		for !found {
			select {
			case completion = <-f.completed:
				found = completion.Result.ExecutionID == id
			case <-deadline.C:
				callCancel()
				t.Fatal("MCP cancellation did not finish real cleanup")
			}
		}
		deadline.Stop()
		callCancel()
		if completion.Err != nil || completion.Result.Status != "cancelled" || !completion.Result.CleanupComplete {
			t.Fatalf("cancel/disconnect completion: %+v %v", completion.Result, completion.Err)
		}
		f.assertRemoved(t, id)
		select {
		case err := <-pending:
			if err == nil {
				t.Fatal("cancelled MCP call succeeded")
			}
		case <-time.After(time.Second):
			t.Fatal("MCP request still pending")
		}
		t.Logf("MCP_CANCEL disconnect=%v cleanup_and_refill_ms=%.3f cleanup=true", disconnect, float64(time.Since(begin))/float64(time.Millisecond))
	}
	if err := f.engine.Close(); err != nil {
		t.Fatal(err)
	}
	assertRuntimeEmpty(t, f.config)
}
