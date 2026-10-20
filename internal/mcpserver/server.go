// Package mcpserver adapts the AgentBox protocol to two MCP tools.
package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"

	"github.com/X1Kun/AgentBox/internal/api"
	"github.com/X1Kun/AgentBox/internal/gatewayclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const ExecuteTool = "execute_python"
const StatusTool = "get_status"

type Input struct {
	Code      string            `json:"code"`
	Stdin     string            `json:"stdin,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

func New(lifetime context.Context, client *gatewayclient.Client, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agentbox-mcp", Version: "0.1.0"}, &mcp.ServerOptions{
		Logger: logger, Capabilities: &mcp.ServerCapabilities{},
		Instructions: "Execute Python in a fresh isolated sandbox. Inspect status, cleanup_complete and outcome_unknown. Never replay execution after an ambiguous transport failure. Host selection and resource policy are operator configured.",
	})
	executions, controls := make(chan struct{}, 8), make(chan struct{}, 4)
	s.AddTool(&mcp.Tool{Name: ExecuteTool, Description: "Execute Python standard-library code in a fresh network-disabled sandbox; return execution status, bounded stdout/stderr, timings and cleanup confirmation. timeout_ms limits execution, not the full request. Never retry outcome_unknown=true.", InputSchema: map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"code"},
		"properties": map[string]any{
			"code":       map[string]any{"type": "string", "description": "Python code, at most 256KiB UTF-8"},
			"stdin":      map[string]any{"type": "string", "description": "Task stdin, at most 256KiB UTF-8"},
			"timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": api.MaxTimeoutMS, "description": "Execution budget; omitted or zero uses 5000ms"},
			"env":        map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "At most 16 AGENT_ variables; values at most 4096 bytes"},
		},
	}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, cancel := withLifetime(ctx, lifetime)
		defer cancel()
		var input Input
		if err := decode(r.Params.Arguments, &input); err != nil {
			return Result(gatewayclient.Failure(400, "invalid_arguments", false, ""), true), nil
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return Result(gatewayclient.Failure(500, "request_id_unavailable", false, ""), true), nil
		}
		request := api.Request{Runtime: "python", Code: input.Code, Stdin: input.Stdin, TimeoutMS: input.TimeoutMS, Env: input.Env, RequestID: "mcp-" + hex.EncodeToString(nonce[:])}
		if _, err := request.Validate(); err != nil {
			return Result(gatewayclient.Failure(400, "invalid_arguments", false, request.RequestID), true), nil
		}
		select {
		case executions <- struct{}{}:
			defer func() { <-executions }()
		default:
			return Result(gatewayclient.Failure(503, "mcp_capacity", false, request.RequestID), true), nil
		}
		reply := client.Execute(ctx, request)
		var result api.Result
		_ = json.Unmarshal(reply.Response, &result)
		return Result(reply, reply.HTTPStatus != 200 || result.Status != "succeeded" || !result.CleanupComplete), nil
	})
	s.AddTool(&mcp.Tool{Name: StatusTool, Description: "Read daemon readiness, capacity, inventory and QoS state. Readiness does not guarantee execution capacity.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ctx, cancel := withLifetime(ctx, lifetime)
		defer cancel()
		var args struct{}
		if err := decode(r.Params.Arguments, &args); err != nil {
			return Result(gatewayclient.Failure(400, "invalid_arguments", false, ""), true), nil
		}
		select {
		case controls <- struct{}{}:
			defer func() { <-controls }()
		default:
			return Result(gatewayclient.Failure(503, "mcp_capacity", false, ""), true), nil
		}
		reply := client.Status(ctx)
		return Result(reply, reply.HTTPStatus != 200), nil
	})
	return s
}

// The SDK deliberately separates request cancellation from connection context.
// Explicitly join process shutdown to each forwarded HTTP request as well.
func withLifetime(ctx, lifetime context.Context) (context.Context, context.CancelFunc) {
	joined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return joined, func() { stop(); cancel() }
}

func decode(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > api.MaxBodyBytes || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return errors.New("invalid arguments")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected one object")
	}
	return nil
}

// Result preserves the complete Gateway JSON, also for infrastructure/admission errors.
func Result(reply gatewayclient.Reply, isError bool) *mcp.CallToolResult {
	raw, _ := json.Marshal(reply)
	return &mcp.CallToolResult{IsError: isError, StructuredContent: json.RawMessage(raw), Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}
