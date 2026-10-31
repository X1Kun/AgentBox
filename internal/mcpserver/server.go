// Package mcpserver exposes explicit v2 session tools over stdio MCP.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/X1Kun/AgentBox/internal/session"
	"github.com/X1Kun/AgentBox/internal/sessionclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	CreateTool  = "session_create_v2"
	GetTool     = "session_get_v2"
	ExecuteTool = "session_execute_v2"
	WriteTool   = "session_write_file_v2"
	ReadTool    = "session_read_file_v2"
	CloseTool   = "session_close_v2"
)

var ToolNames = []string{CreateTool, GetTool, ExecuteTool, WriteTool, ReadTool, CloseTool}

type Backend interface {
	Create(context.Context, session.CreateRequest) sessionclient.Reply
	Get(context.Context, string) sessionclient.Reply
	CloseSession(context.Context, string) sessionclient.Reply
	Execute(context.Context, string, session.ExecuteRequest) sessionclient.Reply
	Write(context.Context, string, session.FileRequest) sessionclient.Reply
	Read(context.Context, string, session.FileRequest) sessionclient.Reply
}
type Input struct {
	SessionID        string `json:"session_id,omitempty"`
	IdentityRevision string `json:"identity_revision,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
	TTLSeconds       int    `json:"ttl_seconds,omitempty"`
	Code             string `json:"code,omitempty"`
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
	Path             string `json:"path,omitempty"`
	Content          []byte `json:"content_base64,omitempty"`
}

func fields(name string) (allowed, required []string) {
	switch name {
	case CreateTool:
		return []string{"request_id", "ttl_seconds"}, []string{"request_id"}
	case GetTool, CloseTool:
		return []string{"session_id"}, []string{"session_id"}
	case ExecuteTool:
		return []string{"session_id", "identity_revision", "code", "timeout_seconds"}, []string{"session_id", "identity_revision", "code"}
	case WriteTool:
		return []string{"session_id", "identity_revision", "path", "content_base64"}, []string{"session_id", "identity_revision", "path", "content_base64"}
	case ReadTool:
		return []string{"session_id", "identity_revision", "path"}, []string{"session_id", "identity_revision", "path"}
	}
	return nil, nil
}

// ValidateArguments is shared with the Agent so an entire model batch can be
// rejected before any of its calls is dispatched. Exact keys, no duplicates/nulls.
func ValidateArguments(name string, raw json.RawMessage) (Input, error) {
	var input Input
	allowed, required := fields(name)
	bad := errors.New("invalid tool arguments")
	if allowed == nil || len(raw) > sessionclient.MaxBodyBytes || !utf8.Valid(raw) {
		return input, bad
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return input, bad
	}
	values := map[string]json.RawMessage{}
	for decoder.More() {
		token, e := decoder.Token()
		key, ok := token.(string)
		if e != nil || !ok {
			return input, bad
		}
		found := false
		for _, k := range allowed {
			if key == k {
				found = true
			}
		}
		if !found || values[key] != nil {
			return input, bad
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil || string(value) == "null" {
			return input, bad
		}
		values[key] = value
	}
	if _, err = decoder.Token(); err != nil {
		return input, bad
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return input, bad
	}
	for _, key := range required {
		if values[key] == nil {
			return input, bad
		}
	}
	if json.Unmarshal(raw, &input) != nil {
		return input, bad
	}
	if (values["timeout_seconds"] != nil && input.TimeoutSeconds == 0) || (values["ttl_seconds"] != nil && input.TTLSeconds == 0) {
		return input, bad
	}
	if name == CreateTool {
		if !sessionclient.ValidRequestID(input.RequestID) || (input.TTLSeconds != 0 && (input.TTLSeconds < 60 || input.TTLSeconds > 86400)) {
			return input, bad
		}
		return input, nil
	}
	if !sessionclient.ValidID(input.SessionID) {
		return input, bad
	}
	if name == GetTool || name == CloseTool {
		return input, nil
	}
	if len(input.IdentityRevision) == 0 || len(input.IdentityRevision) > 128 {
		return input, bad
	}
	if name == ExecuteTool {
		if len(input.Code) == 0 || len(input.Code) > session.MaxCodeBytes || strings.ContainsRune(input.Code, 0) || input.TimeoutSeconds < 0 || input.TimeoutSeconds > 60 {
			return input, bad
		}
	} else {
		if !sessionclient.ValidPath(input.Path) {
			return input, bad
		}
		if len(input.Content) > session.MaxFileBytes {
			return input, bad
		}
	}
	return input, nil
}
func New(lifetime context.Context, client Backend, logger *slog.Logger) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "agentbox-mcp", Version: "2.0.0"}, &mcp.ServerOptions{Logger: logger, Capabilities: &mcp.ServerCapabilities{}, Instructions: "Explicit v2 sessions: files persist; each Python command starts a fresh interpreter. Never replay an uncertain execution/write. Creation recovery uses the same request_id and parameters. Close and query until cleanup_complete; a close request or timeout is not cleanup confirmation."})
	controls, operations := make(chan struct{}, 4), make(chan struct{}, 8)
	for _, name := range ToolNames {
		allowed, required := fields(name)
		properties := map[string]any{}
		for _, key := range allowed {
			p := map[string]any{"type": "string"}
			switch key {
			case "ttl_seconds":
				p = map[string]any{"type": "integer", "minimum": 60, "maximum": 86400}
			case "timeout_seconds":
				p = map[string]any{"type": "integer", "minimum": 1, "maximum": 60}
			case "code":
				p["maxLength"] = session.MaxCodeBytes
			case "content_base64":
				p["maxLength"] = 4 * ((session.MaxFileBytes + 2) / 3)
			case "path":
				p["maxLength"] = 512
			case "request_id":
				p["minLength"] = 1
				p["maxLength"] = 128
			default:
				p["minLength"] = 1
				p["maxLength"] = 160
			}
			properties[key] = p
		}
		descriptions := map[string]string{
			CreateTool:  "Create an asynchronous session with a caller-supplied stable request_id; replay only the same creation parameters to recover its identity.",
			GetTool:     "Query one opaque session ID, readiness, identity_revision and confirmed cleanup. Readiness is a snapshot, not a capacity promise.",
			ExecuteTool: "Run this Python command in a new interpreter in the bound session; workspace files persist. Nonzero exit is a program result. Unknown result/timeout/output limit closes the session; never replay code.",
			WriteTool:   "Write at most 256KiB of base64-encoded file bytes within the bound workspace; unknown writes must not be replayed.",
			ReadTool:    "Read at most 256KiB of workspace file bytes as base64; paths are relative, bounded and cannot escape through symlinks.",
			CloseTool:   "Request closure of one session. Query until closed/cleanup_complete; acknowledgement alone does not prove reclamation.",
		}
		s.AddTool(&mcp.Tool{Name: name, Description: descriptions[name], InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
