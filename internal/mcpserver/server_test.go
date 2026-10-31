package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/sessionclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const token = "mcp-api-test-token-with-at-least-32-characters"

func connected(t *testing.T, handler http.Handler, lifetime context.Context) *mcp.ClientSession {
	t.Helper()
	front := httptest.NewServer(handler)
	t.Cleanup(front.Close)
	client, err := sessionclient.New(front.URL, token, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	server := mcpserver.New(lifetime, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "2"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
func TestV2DiscoveryMappingAndLegacyToolsAbsent(t *testing.T) {
	var calls atomic.Int32
	cs := connected(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong API credential")
		}
		if r.URL.Path != "/v2/sessions/ses-test.uid/execute" {
			t.Error("wrong API route")
		}
		var in session.ExecuteRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.IdentityRevision != "revision" || in.Code != "print(42)" {
			t.Error("identity/code changed")
		}
		_ = json.NewEncoder(w).Encode(session.ExecutionView{SessionID: "ses-test.uid", IdentityRevision: "revision", Execution: session.Execution{Stdout: "42\n"}})
	}), context.Background())
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 6 {
		t.Fatalf("discovery: %v", err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == "execute_python" || tool.Name == "get_status" {
			t.Fatal("legacy alias present")
		}
	}
	for _, raw := range []string{`{"session_id":"ses-test.uid","identity_revision":"revision","code":"x","env":{}}`, `{"session_id":"ses-test.uid","identity_revision":"revision","code":"a","code":"b"}`, `{"session_id":"ses-test.uid","identity_revision":null,"code":"x"}`, `{"session_id":"../other","identity_revision":"revision","code":"x"}`, `{"session_id":"..","identity_revision":"revision","code":"x"}`, `{"session_id":"ses-test.uid","identity_revision":"revision","code":"x","timeout_seconds":0}`} {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: json.RawMessage(raw)})
		if err == nil && !res.IsError {
			t.Fatal("invalid arguments accepted")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid input reached API")
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"session_id": "ses-test.uid", "identity_revision": "revision", "code": "print(42)"}})
	if err != nil || res.IsError || calls.Load() != 1 {
		t.Fatalf("execute: %+v %v", res, err)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(raw), `42\n`) {
		t.Fatal("result lost")
	}
}
func TestCancellationAndProcessLifetimeReachAPI(t *testing.T) {
	for _, processStop := range []bool{false, true} {
		t.Run(map[bool]string{false: "request", true: "process"}[processStop], func(t *testing.T) {
			lifetime, stop := context.WithCancel(context.Background())
			defer stop()
			entered, cancelled := make(chan struct{}), make(chan struct{})
			cs := connected(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				<-r.Context().Done()
				close(cancelled)
			}), lifetime)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: mcpserver.ExecuteTool, Arguments: map[string]any{"session_id": "ses-test.uid", "identity_revision": "revision", "code": "while True: pass"}})
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("API not reached")
			}
			if processStop {
				stop()
			} else {
				cancel()
			}
			select {
			case <-cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation lost")
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("call stuck")
			}
		})
	}
}
