package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/deepseek"
	"github.com/X1Kun/AgentBox/internal/gatewayclient"
	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type modelFunc func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error)

func (f modelFunc) Complete(ctx context.Context, m []deepseek.Message, t []deepseek.Tool) (deepseek.Message, error) {
	return f(ctx, m, t)
}

type fakeTools struct {
	calls int
	reply *mcp.CallToolResult
}

func (f *fakeTools) ListTools(context.Context, *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: []*mcp.Tool{{Name: mcpserver.ExecuteTool, InputSchema: map[string]any{"type": "object"}}, {Name: mcpserver.StatusTool, InputSchema: map[string]any{"type": "object"}}}}, nil
}
func (f *fakeTools) CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.calls++
	return f.reply, nil
}
func modelCall(id, name string) deepseek.ToolCall {
	return deepseek.ToolCall{ID: id, Type: "function", Function: deepseek.FunctionCall{Name: name, Arguments: `{"code":"print(1)"}`}}
}

func TestDeepSeekHTTPAndMCPRepairLoop(t *testing.T) {
	const gatewayToken = "agent-test-gateway-token-with-at-least-32-characters"
	var executions, completions atomic.Int32
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+gatewayToken {
			t.Error("wrong Gateway credential")
		}
		var request api.Request
		_ = json.NewDecoder(r.Body).Decode(&request)
		n := executions.Add(1)
		result := api.Result{ExecutionID: "exec", RequestID: request.RequestID, CleanupComplete: true}
		if n == 1 {
			result.Status = "failed"
			result.Stderr = "ZeroDivisionError"
			if request.Code != "print(1/0)" {
				t.Error("first code changed")
			}
		} else {
			result.Status = "succeeded"
			result.Stdout = "42\n"
			if request.Code != "print(6*7)" {
				t.Error("corrected code changed")
			}
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer front.Close()
	gw, err := gatewayclient.New(front.URL, gatewayToken)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	server := mcpserver.New(context.Background(), gw, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer provider-key" || bytes.Contains(raw, []byte(gatewayToken)) {
			t.Error("credential boundary violated")
		}
		var request struct {
			Messages []deepseek.Message `json:"messages"`
			Tools    []deepseek.Tool    `json:"tools"`
		}
		_ = json.Unmarshal(raw, &request)
		if len(request.Tools) != 2 {
			t.Error("MCP tools not mapped to provider")
		}
		n := completions.Add(1)
		message := deepseek.Message{Role: "assistant"}
		finish := "tool_calls"
		switch n {
		case 1:
			call := modelCall("first", mcpserver.ExecuteTool)
			call.Function.Arguments = `{"code":"print(1/0)"}`
			message.ToolCalls = []deepseek.ToolCall{call}
		case 2:
			last := request.Messages[len(request.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "first" || !strings.Contains(last.Content, "ZeroDivisionError") || !strings.Contains(last.Content, `"cleanup_complete":true`) {
				t.Error("Python error/cleanup not returned to model")
			}
			call := modelCall("corrected", mcpserver.ExecuteTool)
			call.Function.Arguments = `{"code":"print(6*7)"}`
			message.ToolCalls = []deepseek.ToolCall{call}
		case 3:
			last := request.Messages[len(request.Messages)-1]
			if last.ToolCallID != "corrected" || !strings.Contains(last.Content, `42\n`) {
				t.Error("success result not correlated")
			}
			message.Content = "The observed result is 42."
			finish = "stop"
		default:
			t.Error("unexpected extra model request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
	}))
	defer provider.Close()
	model, err := deepseek.New(provider.URL, "provider-key", "configured-model")
	if err != nil {
		t.Fatal(err)
	}
	defer model.Close()
	result, err := (Runner{Model: model, Tools: cs}).Run(context.Background(), "Calculate using Python and correct errors")
	if err != nil || result.Answer != "The observed result is 42." || result.Turns != 3 || result.ToolCalls != 2 || executions.Load() != 2 {
		t.Fatalf("repair loop: %+v %v", result, err)
	}
}

func TestUnknownOutcomesAndIncompleteCleanupStopFurtherCalls(t *testing.T) {
	for _, tc := range []struct {
		code int
		body string
	}{
		{502, `{"outcome_unknown":true,"reason":"transport_error"}`},
		{200, `{"status":"succeeded","cleanup_complete":false,"execution_id":"e"}`},
		{500, `{"status":"infrastructure_error","cleanup_complete":true}`},
		{200, `{"status":"succeeded"}`},
	} {
		tools := &fakeTools{reply: mcpserver.Result(gatewayclient.Reply{HTTPStatus: tc.code, Response: json.RawMessage(tc.body)}, true)}
		model := modelFunc(func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error) {
			return deepseek.Message{Role: "assistant", ToolCalls: []deepseek.ToolCall{modelCall("first", mcpserver.ExecuteTool), modelCall("second", mcpserver.ExecuteTool)}}, nil
		})
		_, err := (Runner{Model: model, Tools: tools}).Run(context.Background(), "execute")
		if err == nil || tools.calls != 1 {
			t.Fatalf("unsafe continuation: calls=%d err=%v", tools.calls, err)
		}
	}
}

func TestBudgetAndBatchValidationHappenBeforeExecution(t *testing.T) {
	for _, tc := range []struct {
		turns, calls int
		batch        []deepseek.ToolCall
	}{
		{1, 8, []deepseek.ToolCall{modelCall("one", mcpserver.ExecuteTool)}},
		{6, 1, []deepseek.ToolCall{modelCall("one", mcpserver.ExecuteTool), modelCall("two", mcpserver.ExecuteTool)}},
		{6, 8, []deepseek.ToolCall{modelCall("one", mcpserver.ExecuteTool), modelCall("two", "arbitrary_remote_host")}},
		{6, 8, []deepseek.ToolCall{modelCall("one", mcpserver.ExecuteTool), modelCall("one", mcpserver.ExecuteTool)}},
	} {
		tools := &fakeTools{}
		model := modelFunc(func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error) {
			return deepseek.Message{Role: "assistant", ToolCalls: tc.batch}, nil
		})
		_, err := (Runner{Model: model, Tools: tools, MaxTurns: tc.turns, MaxToolCalls: tc.calls}).Run(context.Background(), "execute")
		if err == nil || tools.calls != 0 {
			t.Fatalf("budget/validation caused side effect: %d %v", tools.calls, err)
		}
	}
}

func TestModelOutputBudgetPreservesExecutionFacts(t *testing.T) {
	for _, text := range []string{strings.Repeat("中", 20000), strings.Repeat("\x00", 20000)} {
		response, _ := json.Marshal(map[string]any{"status": "failed", "cleanup_complete": true, "stdout": text, "stderr": "expected failure", "execution_id": "one", "outcome_unknown": false})
		view, stop, err := modelView(mcpserver.Result(gatewayclient.Reply{HTTPStatus: 200, Response: response}, true), true)
		if err != nil || stop || len(view) > 32<<10 || !json.Valid([]byte(view)) || !strings.Contains(view, `"model_output_truncated":true`) || !strings.Contains(view, `"cleanup_complete":true`) {
			t.Fatalf("bounded view lost facts: stop=%v err=%v bytes=%d", stop, err, len(view))
		}
	}
}
