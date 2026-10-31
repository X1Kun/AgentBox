//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/agent"
	"github.com/X1Kun/AgentBox/internal/deepseek"
	"github.com/X1Kun/AgentBox/internal/mcpclient"
	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/X1Kun/AgentBox/internal/session"
)

type modelFunc func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error)

func (f modelFunc) Complete(c context.Context, m []deepseek.Message, t []deepseek.Tool) (deepseek.Message, error) {
	return f(c, m, t)
}
func TestStdioCancellationKeepsDetachedCleanupAvailable(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "agentbox-mcp")
	build := exec.Command("go", "build", "-o", binary, "./cmd/agentbox-mcp")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build MCP: %v %s", err, output)
	}
	const token = "stdio-integration-api-token-at-least-32-characters"
	entered, cancelled := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	closed := false
	executions := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong credential")
		}
		if r.URL.Path == "/v2/sessions/ses-test.uid/execute" {
			_, _ = io.Copy(io.Discard, r.Body)
			mu.Lock()
			executions++
			mu.Unlock()
			close(entered)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		mu.Lock()
		if r.Method == "DELETE" {
			closed = true
		}
		done := closed
		mu.Unlock()
		view := session.View{ID: "ses-test.uid", State: "ready", IdentityRevision: "revision", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		if done {
			view.State = "closed"
			view.CleanupComplete = true
		}
		_ = json.NewEncoder(w).Encode(view)
	}))
	defer api.Close()
	defer api.CloseClientConnections()
	connect, cancelConnect := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelConnect()
	tools, err := mcpclient.Connect(connect, binary, api.URL, token, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tools.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := modelFunc(func(_ context.Context, m []deepseek.Message, _ []deepseek.Tool) (deepseek.Message, error) {
		id, rev, err := agent.BindingFromPrompt(m)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"session_id": id, "identity_revision": rev, "code": "wait()"})
		return deepseek.Message{Role: "assistant", ToolCalls: []deepseek.ToolCall{{ID: "one", Type: "function", Function: deepseek.FunctionCall{Name: mcpserver.ExecuteTool, Arguments: string(raw)}}}}, nil
	})
	result := make(chan agent.Result, 1)
	go func() {
		r, e := (agent.Runner{Model: model, Tools: tools, CleanupTimeout: 5 * time.Second}).Run(ctx, "cancel the active request")
		if e == nil {
			t.Error("cancelled call succeeded")
		}
		result <- r
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stdio command did not reach API")
	}
	cancel()
	select {
	case r := <-result:
		if !r.CleanupComplete {
			t.Fatalf("cleanup lost with conversation context: %+v", r)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("cleanup stuck")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("API call not cancelled")
	}
	mu.Lock()
	defer mu.Unlock()
	if executions != 1 || !closed {
		t.Fatalf("execution/closure count: %d %v", executions, closed)
	}
}
