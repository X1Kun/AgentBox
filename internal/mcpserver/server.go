// Package mcpserver exposes explicit v2 session tools over stdio MCP.
package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"

	sessionclient "github.com/X1Kun/AgentBox/internal/session/client"
	session "github.com/X1Kun/AgentBox/internal/session/protocol"
	"github.com/X1Kun/AgentBox/internal/toolcontract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	CreateTool  = toolcontract.CreateTool
	GetTool     = toolcontract.GetTool
	ExecuteTool = toolcontract.ExecuteTool
	WriteTool   = toolcontract.WriteTool
	ReadTool    = toolcontract.ReadTool
	CloseTool   = toolcontract.CloseTool
)

type Input = toolcontract.Input

func ValidateArguments(name string, raw json.RawMessage) (Input, error) {
	return toolcontract.ValidateArguments(name, raw)
}

type Backend interface {
	Create(context.Context, session.CreateRequest) sessionclient.Reply
	Get(context.Context, string) sessionclient.Reply
	CloseSession(context.Context, string) sessionclient.Reply
	Execute(context.Context, string, session.ExecuteRequest) sessionclient.Reply
	Write(context.Context, string, session.FileRequest) sessionclient.Reply
	Read(context.Context, string, session.FileRequest) sessionclient.Reply
}

func New(lifetime context.Context, client Backend, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agentbox-mcp", Version: "2.0.0"}, &mcp.ServerOptions{Logger: logger, Capabilities: &mcp.ServerCapabilities{}, Instructions: "Explicit v2 sessions: files persist; each Python command starts a fresh interpreter. Never replay an uncertain execution/write. Creation recovery uses the same request_id and parameters. Close and query until cleanup_complete; a close request or timeout is not cleanup confirmation."})
	controls, operations := make(chan struct{}, 4), make(chan struct{}, 8)
	for _, name := range toolcontract.Names() {
		s.AddTool(&mcp.Tool{Name: name, Description: toolcontract.Description(name), InputSchema: toolcontract.Schema(name)}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ctx, cancel := withLifetime(ctx, lifetime)
			defer cancel()
			in, err := ValidateArguments(name, request.Params.Arguments)
			if err != nil {
				return Result(sessionclient.Failure(400, "invalid_arguments", false), true), nil
			}
			slots := controls
			if name == ExecuteTool || name == ReadTool || name == WriteTool {
				slots = operations
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				return Result(sessionclient.Failure(503, "mcp_capacity", false), true), nil
			}
			var reply sessionclient.Reply
			switch name {
			case CreateTool:
				reply = client.Create(ctx, session.CreateRequest{RequestID: in.RequestID, TTLSeconds: in.TTLSeconds})
			case GetTool:
				reply = client.Get(ctx, in.SessionID)
			case CloseTool:
				reply = client.CloseSession(ctx, in.SessionID)
			case ExecuteTool:
				reply = client.Execute(ctx, in.SessionID, session.ExecuteRequest{IdentityRevision: in.IdentityRevision, Code: in.Code, TimeoutSeconds: in.TimeoutSeconds})
			case WriteTool:
				reply = client.Write(ctx, in.SessionID, session.FileRequest{IdentityRevision: in.IdentityRevision, Path: in.Path, Content: in.Content})
			case ReadTool:
				reply = client.Read(ctx, in.SessionID, session.FileRequest{IdentityRevision: in.IdentityRevision, Path: in.Path})
			}
			failed := reply.HTTPStatus < 200 || reply.HTTPStatus >= 300
			if name == ExecuteTool && !failed {
				var view session.Execution
				failed = json.Unmarshal(reply.Response, &view) != nil || view.ExitCode != 0
			}
			return Result(reply, failed), nil
		})
	}
	return s
}
func withLifetime(ctx, lifetime context.Context) (context.Context, context.CancelFunc) {
	joined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return joined, func() { stop(); cancel() }
}
func Result(reply sessionclient.Reply, isError bool) *mcp.CallToolResult {
	raw, _ := json.Marshal(reply)
	return &mcp.CallToolResult{IsError: isError, StructuredContent: json.RawMessage(raw), Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}
