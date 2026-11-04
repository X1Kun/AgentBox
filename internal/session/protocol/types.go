// Package protocol defines the v2 wire contract without runtime or Kubernetes dependencies.
package protocol

import "errors"

const MaxCodeBytes = 64 << 10
const MaxFileBytes = 256 << 10
const MaxOutputBytes = 64 << 10
const MaxBodyBytes = 512 << 10
const MaxResponseBytes = 1 << 20

type Error struct {
	Code      string
	SessionID string
}

func (e *Error) Error() string { return e.Code }
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeBackendUnavailable
}

type CreateRequest struct {
	RequestID  string `json:"request_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}
type View struct {
	ID               string `json:"session_id"`
	State            State  `json:"state"`
	Reason           string `json:"reason,omitempty"`
	ExpiresAt        string `json:"expires_at"`
	IdentityRevision string `json:"identity_revision,omitempty"`
	CleanupComplete  bool   `json:"cleanup_complete"`
	OperationActive  bool   `json:"operation_active"`
	ClosedAt         string `json:"closed_at,omitempty"`
}
type ExecuteRequest struct {
	IdentityRevision string `json:"identity_revision"`
	Code             string `json:"code"`
	TimeoutSeconds   int    `json:"timeout_seconds,omitempty"`
}
type FileRequest struct {
	IdentityRevision string `json:"identity_revision"`
	Path             string `json:"path"`
	Content          []byte `json:"content_base64,omitempty"`
}
type Execution struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}
type ExecutionView struct {
	Execution
	SessionID        string `json:"session_id"`
	IdentityRevision string `json:"identity_revision"`
}
type FileView struct {
	SessionID        string `json:"session_id"`
	IdentityRevision string `json:"identity_revision"`
	Path             string `json:"path"`
	Content          []byte `json:"content_base64"`
	Size             int    `json:"size_bytes"`
}
