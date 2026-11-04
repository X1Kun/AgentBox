// Package toolcontract defines v2 tool names, schemas and argument validation.
// It depends only on the wire protocol and the standard library.
package toolcontract

import (
	"bytes"
	"encoding/json"
	"errors"
	session "github.com/X1Kun/AgentBox/internal/session/protocol"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	CreateTool  = "session_create_v2"
	GetTool     = "session_get_v2"
	ExecuteTool = "session_execute_v2"
	WriteTool   = "session_write_file_v2"
	ReadTool    = "session_read_file_v2"
	CloseTool   = "session_close_v2"
)

func Names() []string {
	return []string{CreateTool, GetTool, ExecuteTool, WriteTool, ReadTool, CloseTool}
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

func Fields(name string) (allowed, required []string) {
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
	allowed, required := Fields(name)
	bad := errors.New("invalid tool arguments")
	if allowed == nil || len(raw) > session.MaxBodyBytes || !utf8.Valid(raw) {
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
		if !session.ValidRequestID(input.RequestID) || (input.TTLSeconds != 0 && (input.TTLSeconds < 60 || input.TTLSeconds > 86400)) {
			return input, bad
		}
		return input, nil
	}
	if !session.ValidID(input.SessionID) {
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
		if !session.ValidPath(input.Path) {
			return input, bad
		}
		if len(input.Content) > session.MaxFileBytes {
			return input, bad
		}
	}
	return input, nil
}

func Schema(name string) map[string]any {
	allowed, required := Fields(name)
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
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
func Description(name string) string {
	descriptions := map[string]string{
		CreateTool:  "Create an asynchronous session with a caller-supplied stable request_id; replay only the same creation parameters to recover its identity.",
		GetTool:     "Query one opaque session ID, readiness, identity_revision and confirmed cleanup. Readiness is a snapshot, not a capacity promise.",
		ExecuteTool: "Run this Python command in a new interpreter in the bound session; workspace files persist. Nonzero exit is a program result. Unknown result/timeout/output limit closes the session; never replay code.",
		WriteTool:   "Write at most 256KiB of base64-encoded file bytes within the bound workspace; unknown writes must not be replayed.",
		ReadTool:    "Read at most 256KiB of workspace file bytes as base64; paths are relative, bounded and cannot escape through symlinks.",
		CloseTool:   "Request closure of one session. Query until closed/cleanup_complete; acknowledgement alone does not prove reclamation.",
	}
	return descriptions[name]
}
