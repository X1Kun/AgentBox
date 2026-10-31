package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/X1Kun/AgentBox/internal/deepseek"
	"github.com/X1Kun/AgentBox/internal/mcpserver"
	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/sessionapi"
	"github.com/X1Kun/AgentBox/internal/sessionclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type modelFunc func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error)

func (f modelFunc) Complete(c context.Context, m []deepseek.Message, t []deepseek.Tool) (deepseek.Message, error) {
	return f(c, m, t)
}

const apiToken = "agent-session-api-token-with-at-least-32-characters"

type backend struct {
	denyWrite          bool
	mu                 sync.Mutex
	executions, closes int
	requestIDs         []string
	files              map[string][]byte
	fail               bool
	block              chan struct{}
}

func (b *backend) view() session.View {
	return session.View{ID: "ses-test.uid", State: "ready", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), IdentityRevision: "revision"}
}
func (b *backend) Create(_ context.Context, _ string, r session.CreateRequest) (session.View, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requestIDs = append(b.requestIDs, r.RequestID)
	return b.view(), len(b.requestIDs) > 1, nil
}
func (b *backend) Get(context.Context, string, string) (session.View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := b.view()
	if b.closes > 0 {
		v.State = "closed"
		v.CleanupComplete = true
	}
	return v, nil
}
func (b *backend) Close(context.Context, string, string) (session.View, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closes++
	v := b.view()
	v.State = "closed"
	v.CleanupComplete = true
	return v, nil
}
func (b *backend) Ready(context.Context) error { return nil }
func (b *backend) Execute(ctx context.Context, _, id string, r session.ExecuteRequest) (session.ExecutionView, error) {
	b.mu.Lock()
	b.executions++
	b.mu.Unlock()
	if b.block != nil {
		close(b.block)
		<-ctx.Done()
		return session.ExecutionView{}, &session.Error{Code: "operation_outcome_unknown"}
	}
	if b.fail {
		return session.ExecutionView{}, &session.Error{Code: "operation_outcome_unknown"}
	}
	v := session.ExecutionView{SessionID: id, IdentityRevision: r.IdentityRevision, Execution: session.Execution{Stdout: "42\n"}}
	if r.Code == "print(1/0)" {
		v.ExitCode = 1
		v.Stdout = ""
		v.Stderr = "ZeroDivisionError"
	}
	return v, nil
}
func (b *backend) Write(_ context.Context, _, id string, r session.FileRequest) (session.FileView, error) {
	if b.denyWrite {
		return session.FileView{}, &session.Error{Code: "file_access_denied"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files == nil {
		b.files = map[string][]byte{}
	}
	b.files[r.Path] = r.Content
	return session.FileView{SessionID: id, IdentityRevision: r.IdentityRevision, Path: r.Path, Size: len(r.Content)}, nil
}
func (b *backend) Read(_ context.Context, _, id string, r session.FileRequest) (session.FileView, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.files[r.Path]
	if !ok {
		return session.FileView{}, &session.Error{Code: "file_not_found"}
	}
	return session.FileView{SessionID: id, IdentityRevision: r.IdentityRevision, Path: r.Path, Content: v, Size: len(v)}, nil
}
func connectedTools(t *testing.T, b *backend, dropCreate bool) *mcp.ClientSession {
	t.Helper()
	handler, err := sessionapi.New(b, []sessionapi.Principal{{ID: "alice", Token: apiToken}}, time.Second, 4)
	if err != nil {
		t.Fatal(err)
	}
	dropped := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dropCreate && !dropped && r.Method == "POST" && r.URL.Path == "/v2/sessions" {
			dropped = true
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			_ = conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)
	client, err := sessionclient.New(api.URL, apiToken, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	server := mcpserver.New(context.Background(), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
func toolCall(id, name, sid, revision string, extra map[string]any) deepseek.ToolCall {
	in := map[string]any{"session_id": sid}
	if name != mcpserver.GetTool {
		in["identity_revision"] = revision
	}
	for k, v := range extra {
		in[k] = v
	}
	raw, _ := json.Marshal(in)
	return deepseek.ToolCall{ID: id, Type: "function", Function: deepseek.FunctionCall{Name: name, Arguments: string(raw)}}
}
func TestModelMCPAPIRepairFilesAndDetachedClosure(t *testing.T) {
	b := &backend{}
	tools := connectedTools(t, b, false)
	turn := 0
	model := modelFunc(func(_ context.Context, messages []deepseek.Message, tools []deepseek.Tool) (deepseek.Message, error) {
		if len(tools) != 4 {
			t.Fatal("lifecycle tools exposed to model")
		}
		sid, rev, e := BindingFromPrompt(messages)
		if e != nil {
			t.Fatal(e)
		}
		turn++
		message := deepseek.Message{Role: "assistant"}
		switch turn {
		case 1:
			message.ToolCalls = []deepseek.ToolCall{toolCall("write", mcpserver.WriteTool, sid, rev, map[string]any{"path": "a.bin", "content_base64": "AP8="}), toolCall("bad", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "print(1/0)"})}
		case 2:
			if !strings.Contains(messages[len(messages)-1].Content, "ZeroDivisionError") {
				t.Fatal("program error lost")
			}
			message.ToolCalls = []deepseek.ToolCall{toolCall("fix", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "print(6*7)"}), toolCall("read", mcpserver.ReadTool, sid, rev, map[string]any{"path": "a.bin"})}
		case 3:
			message.Content = "Observed 42 and downloaded binary file."
		default:
			t.Fatal("extra model request")
		}
		return message, nil
	})
	result, err := (Runner{Model: model, Tools: tools}).Run(context.Background(), "compute and export")
	if err != nil || !result.CleanupComplete || b.executions != 2 || b.closes != 1 || len(result.Artifacts) != 1 || len(result.Artifacts[0].Content) != 2 || result.ToolCalls != 4 {
		t.Fatalf("result: %+v %v", result, err)
	}
}
func TestLostCreateResponseRecoversSameIdentity(t *testing.T) {
	b := &backend{}
	tools := connectedTools(t, b, true)
	result, err := (Runner{Tools: tools, Model: modelFunc(func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error) {
		return deepseek.Message{Role: "assistant", Content: "done"}, nil
	})}).Run(context.Background(), "test")
	if err != nil || !result.CleanupComplete || len(b.requestIDs) != 2 || b.requestIDs[0] != b.requestIDs[1] {
		t.Fatalf("creation replay changed identity: %+v %v", result, err)
	}
}
func TestUnknownExecutionStopsBatchAndCloses(t *testing.T) {
	b := &backend{fail: true}
	tools := connectedTools(t, b, false)
	model := modelFunc(func(_ context.Context, m []deepseek.Message, _ []deepseek.Tool) (deepseek.Message, error) {
		sid, rev, _ := BindingFromPrompt(m)
		return deepseek.Message{Role: "assistant", ToolCalls: []deepseek.ToolCall{toolCall("one", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "side_effect()"}), toolCall("two", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "must_not_run()"})}}, nil
	})
	result, err := (Runner{Tools: tools, Model: model}).Run(context.Background(), "execute")
	if err == nil || !result.OutcomeUnknown || !result.CleanupComplete || b.executions != 1 || b.closes != 1 {
		t.Fatalf("unknown replay: %+v %v", result, err)
	}
}
func TestBatchBudgetAndForeignIdentityRejectBeforeDataDispatch(t *testing.T) {
	for _, kind := range []string{"foreign", "duplicate", "budget", "legacy", "schema"} {
		t.Run(kind, func(t *testing.T) {
			b := &backend{}
			tools := connectedTools(t, b, false)
			model := modelFunc(func(_ context.Context, m []deepseek.Message, _ []deepseek.Tool) (deepseek.Message, error) {
				sid, rev, _ := BindingFromPrompt(m)
				one := toolCall("one", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "print(1)"})
				two := toolCall("two", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "print(2)"})
				if kind == "foreign" {
					two = toolCall("two", mcpserver.ExecuteTool, "another-session", rev, map[string]any{"code": "print(2)"})
				}
				if kind == "duplicate" {
					two.ID = "one"
				}
				if kind == "legacy" {
					two.Function.Name = "execute_python"
				}
				if kind == "schema" {
					two = toolCall("two", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "print(2)", "timeout_seconds": 0})
				}
				return deepseek.Message{Role: "assistant", ToolCalls: []deepseek.ToolCall{one, two}}, nil
			})
			limit := 8
			if kind == "budget" {
				limit = 1
			}
			result, err := (Runner{Tools: tools, Model: model, MaxToolCalls: limit}).Run(context.Background(), "test")
			if err == nil || b.executions != 0 || !result.CleanupComplete {
				t.Fatalf("unsafe batch: %+v %v", result, err)
			}
		})
	}
}
func TestCancelledConversationStillClosesThroughMCP(t *testing.T) {
	b := &backend{block: make(chan struct{})}
	tools := connectedTools(t, b, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	model := modelFunc(func(_ context.Context, m []deepseek.Message, _ []deepseek.Tool) (deepseek.Message, error) {
		sid, rev, _ := BindingFromPrompt(m)
		return deepseek.Message{Role: "assistant", ToolCalls: []deepseek.ToolCall{toolCall("blocked", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "wait()"})}}, nil
	})
	done := make(chan Result, 1)
	go func() {
		r, e := (Runner{Tools: tools, Model: model, CleanupTimeout: 2 * time.Second}).Run(ctx, "test")
		if e == nil {
			t.Error("cancelled execution succeeded")
		}
		done <- r
	}()
	select {
	case <-b.block:
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not start")
	}
	cancel()
	select {
	case r := <-done:
		if !r.CleanupComplete {
			t.Fatalf("cleanup inherited cancellation: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup stuck")
	}
}
func TestModelFailureAndTruncatedOutputPreserveCleanup(t *testing.T) {
	b := &backend{}
	tools := connectedTools(t, b, false)
	result, err := (Runner{Tools: tools, Model: modelFunc(func(context.Context, []deepseek.Message, []deepseek.Tool) (deepseek.Message, error) {
		return deepseek.Message{}, errors.New("model unavailable")
	})}).Run(context.Background(), "test")
	if err == nil || !result.CleanupComplete {
		t.Fatal("model failure leaked session")
	}
	raw, _ := json.Marshal(session.ExecutionView{SessionID: "sid", IdentityRevision: "rev", Execution: session.Execution{Stdout: strings.Repeat("中", 20000)}})
	view, stop, err := modelView(sessionclient.Reply{HTTPStatus: 200, Response: raw}, mcpserver.ExecuteTool, "sid", "rev")
	if err != nil || stop || len(view) > 32<<10 || !strings.Contains(view, `"model_output_truncated":true`) {
		t.Fatalf("output bound: %v %v", stop, err)
	}
}

func TestDefiniteFileRefusalStopsWithoutClaimingUnknownExecution(t *testing.T) {
	b := &backend{denyWrite: true}
	tools := connectedTools(t, b, false)
	model := modelFunc(func(_ context.Context, m []deepseek.Message, _ []deepseek.Tool) (deepseek.Message, error) {
		sid, rev, _ := BindingFromPrompt(m)
		return deepseek.Message{Role: "assistant", ToolCalls: []deepseek.ToolCall{toolCall("write", mcpserver.WriteTool, sid, rev, map[string]any{"path": "blocked", "content_base64": ""}), toolCall("later", mcpserver.ExecuteTool, sid, rev, map[string]any{"code": "must_not_run()"})}}, nil
	})
	result, err := (Runner{Tools: tools, Model: model}).Run(context.Background(), "test")
	if err == nil || result.OutcomeUnknown || !result.CleanupComplete || b.executions != 0 {
		t.Fatalf("definite refusal misclassified: %+v %v", result, err)
	}
}
