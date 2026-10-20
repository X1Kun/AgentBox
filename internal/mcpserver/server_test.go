package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/gatewayclient"
	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const token = "mcp-public-test-token-with-at-least-32-characters"

func session(t *testing.T, handler http.Handler) *mcp.ClientSession {
	return sessionWithLifetime(t, handler, context.Background())
}

func sessionWithLifetime(t *testing.T, handler http.Handler, lifetime context.Context) *mcp.ClientSession {
	t.Helper()
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)
	gw, err := gatewayclient.New(front.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	s := mcpserver.New(lifetime, gw, slog.New(slog.NewTextHandler(io.Discard, nil)))
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := s.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := c.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestDiscoveryValidationAndMapping(t *testing.T) {
	var calls atomic.Int32
	cs := session(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token || r.URL.Path != "/v1/execute" {
			t.Error("wrong Gateway boundary")
		}
		var request api.Request
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid request")
		}
		if request.Runtime != "python" || request.TimeoutMS != 5000 || request.Env["AGENT_TEST"] != "value" || !strings.HasPrefix(request.RequestID, "mcp-") {
			t.Errorf("mapping: %+v", request)
		}
		_ = json.NewEncoder(w).Encode(api.Result{ExecutionID: "test-exec", RequestID: request.RequestID, Status: "succeeded", Stdout: "42\n", CleanupComplete: true})
	}))
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 2 {
		t.Fatalf("discovery: %+v %v", listed, err)
	}
	for _, args := range []any{
		map[string]any{"code": "print(1)", "rootfs": "/"}, map[string]any{"code": "print(1)", "runtime": "bash"},
		map[string]any{"code": "print(1)", "timeout_ms": 30001}, map[string]any{"code": "print(1)", "env": map[string]string{"PATH": "bad"}},
		map[string]any{"code": strings.Repeat("x", api.MaxCodeBytes+1)},
	} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: args})
		if err != nil || !res.IsError {
			t.Fatalf("validation: %+v %v", res, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid request reached Gateway")
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"code": "print(42)", "env": map[string]string{"AGENT_TEST": "value"}}})
	if err != nil || res.IsError {
		t.Fatalf("execute: %+v %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var reply gatewayclient.Reply
	if json.Unmarshal(raw, &reply) != nil || reply.HTTPStatus != 200 || !strings.Contains(string(reply.Response), `"stdout":"42\n"`) {
		t.Fatalf("structured output: %s", raw)
	}
}

func TestErrorsRetainStructuredResults(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{200, `{"status":"failed","stderr":"ValueError","cleanup_complete":true}`},
		{200, `{"status":"timeout","cleanup_complete":true}`},
		{500, `{"status":"infrastructure_error","cleanup_complete":false}`},
		{503, `{"reason":"qos_pressure","queue_ms":12}`},
		{502, `{"reason":"daemon_response_failed","outcome_unknown":true}`},
	} {
		t.Run(tc.body, func(t *testing.T) {
			cs := session(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"code": "print(1)"}})
			if err != nil || !res.IsError {
				t.Fatalf("error result: %+v %v", res, err)
			}
			raw, _ := json.Marshal(res.StructuredContent)
			var reply gatewayclient.Reply
			_ = json.Unmarshal(raw, &reply)
			var got, want any
			_ = json.Unmarshal(reply.Response, &got)
			_ = json.Unmarshal([]byte(tc.body), &want)
			if reply.HTTPStatus != tc.status || !reflect.DeepEqual(got, want) {
				t.Fatalf("result changed: %s", raw)
			}
		})
	}
}

func TestCancellationReachesGateway(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	cs := session(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"code": "while True: pass"}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway not reached")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled call succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MCP call did not cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("MCP cancellation did not reach Gateway")
	}
}

func TestProcessShutdownCancelsForwardedRequest(t *testing.T) {
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	entered, cancelled := make(chan struct{}), make(chan struct{})
	cs := sessionWithLifetime(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}), lifetime)
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"code": "while True: pass"}})
		done <- res
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway not reached")
	}
	stop()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("process shutdown did not cancel Gateway HTTP")
	}
	select {
	case res := <-done:
		if res == nil || !res.IsError {
			t.Fatal("shutdown returned success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("MCP call did not unwind")
	}
}
