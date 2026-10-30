package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const operationKey = "agentbox.dev/api-operation"
const MaxCodeBytes = 64 << 10
const MaxFileBytes = 256 << 10
const MaxOutputBytes = 64 << 10

// Target holds a snapshot, never a discoverable Service address. Validate must
// succeed before and after opening a connection; that connection must not redial.
type Target struct {
	IP       string
	Validate func(context.Context) error
}
type Runtime interface {
	Execute(context.Context, Target, string) (Execution, error)
	Write(context.Context, Target, string, []byte) error
	Read(context.Context, Target, string) ([]byte, error)
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

// Configure the adapter once, before serving requests. Lifecycle-only operation
// remains possible, for example on a host without access to the Pod network.
func (s *Service) SetRuntime(r Runtime) { s.runtime = r }
func validPath(p string) bool {
	if p == "" || len(p) > 512 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}
func (s *Service) checkTarget(ctx context.Context, principal, id, expected string) (*unstructured.Unstructured, *Binding, error) {
	v, err := s.Get(ctx, principal, id)
	if err != nil {
		return nil, nil, err
	}
	if v.State != "ready" {
		return nil, nil, fail("session_not_ready")
	}
	if expected == "" || v.IdentityRevision != expected {
		return nil, nil, fail("identity_changed")
	}
	c, err := s.load(ctx, principal, id)
	if err != nil {
		return nil, nil, err
	}
	b, err := readBinding(c)
	if err != nil {
		return nil, nil, err
	}
	if b == nil || revision(b) != expected || c.GetAnnotations()[invalidKey] != "" || c.GetAnnotations()[closeKey] == "true" {
		return nil, nil, fail("identity_changed")
	}
	return c, b, nil
}

// The Claim CAS serializes all data-plane calls across API replicas/restarts.
// There is no expiring lease: a crash cannot quietly admit a second execution.
func (s *Service) acquire(ctx context.Context, principal, id, expected string) (Target, string, error) {
	for attempts := 0; attempts < 3; attempts++ {
		c, b, err := s.checkTarget(ctx, principal, id, expected)
		if err != nil {
			return Target{}, "", err
		}
		if c.GetAnnotations()[operationKey] != "" {
			return Target{}, "", fail("session_busy")
		}
		pod, err := s.resource(Pods).Get(ctx, b.Pod.Name, metav1.GetOptions{})
		if err != nil {
			return Target{}, "", err
		}
		ip, _, _ := unstructured.NestedString(pod.Object, "status", "podIP")
		if string(pod.GetUID()) != b.Pod.UID || net.ParseIP(ip) == nil {
			return Target{}, "", fail("identity_changed")
		}
		var nonce [16]byte
		if _, err = rand.Read(nonce[:]); err != nil {
			return Target{}, "", err
		}
		token := hex.EncodeToString(nonce[:])
		err = s.patch(ctx, c, map[string]string{operationKey: token}, "")
		if err != nil {
			if retryPatch(err) {
				continue
			}
			// A lost CAS reply may have installed the lock. Do not dispatch or replay.
			return Target{}, "", fail("operation_outcome_unknown")
		}
		target := Target{IP: ip}
		target.Validate = func(callCtx context.Context) error {
			current, binding, e := s.checkTarget(callCtx, principal, id, expected)
			if e != nil {
				return e
			}
			if current.GetAnnotations()[operationKey] != token {
				return fail("state_conflict")
			}
			p, e := s.resource(Pods).Get(callCtx, binding.Pod.Name, metav1.GetOptions{})
			if e != nil {
				return e
			}
			currentIP, _, _ := unstructured.NestedString(p.Object, "status", "podIP")
			if string(p.GetUID()) != b.Pod.UID || currentIP != ip {
				return fail("identity_changed")
			}
			return nil
		}
		return target, token, nil
	}
	return Target{}, "", fail("state_conflict")
}
func (s *Service) release(ctx context.Context, principal, id, token string) error {
	for attempts := 0; attempts < 3; attempts++ {
		c, err := s.load(ctx, principal, id)
		if err != nil {
			return err
		}
		if c.GetAnnotations()[operationKey] != token {
			return fail("state_conflict")
		}
		err = s.patch(ctx, c, map[string]string{operationKey: ""}, "")
		if err == nil {
			return nil
		}
		if !retryPatch(err) {
			return err
		}
	}
	return fail("state_conflict")
}
func (s *Service) finish(ctx context.Context, principal, id, expected, token string, operationErr error, mutating bool) error {
	// Any uncertain mutation keeps its durable lock and requests full-session
	// shutdown with a detached budget. Timeout/cancellation alone is not cleanup.
	uncertain := operationErr != nil && mutating
	if operationErr == nil {
		_, _, operationErr = s.checkTarget(ctx, principal, id, expected)
		uncertain = operationErr != nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if uncertain {
		_, _ = s.Close(cleanup, principal, id)
		switch Code(operationErr) {
		case "execution_output_limit":
			return fail("execution_output_limit")
		}
		if errors.Is(operationErr, context.DeadlineExceeded) {
			return fail("execution_timeout")
		}
		return fail("operation_outcome_unknown")
	}
	if err := s.release(cleanup, principal, id, token); err != nil {
		return fail("operation_outcome_unknown")
	}
	return operationErr
}
func (s *Service) Execute(ctx context.Context, principal, id string, r ExecuteRequest) (ExecutionView, error) {
	if len(r.Code) == 0 || len(r.Code) > MaxCodeBytes || !utf8.ValidString(r.Code) || strings.ContainsRune(r.Code, 0) {
		return ExecutionView{}, fail("invalid_request")
	}
	seconds := r.TimeoutSeconds
	if seconds == 0 {
		seconds = 20
	}
	if seconds < 1 || seconds > 60 {
		return ExecutionView{}, fail("invalid_request")
	}
	if s.runtime == nil {
		return ExecutionView{}, fail("execution_unavailable")
	}
	target, token, err := s.acquire(ctx, principal, id, r.IdentityRevision)
	if err != nil {
		return ExecutionView{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	result, err := s.runtime.Execute(runCtx, target, r.Code)
	if runCtx.Err() != nil {
		err = runCtx.Err()
	}
	cancel()
	if err = s.finish(ctx, principal, id, r.IdentityRevision, token, err, true); err != nil {
		return ExecutionView{}, err
	}
	return ExecutionView{Execution: result, SessionID: id, IdentityRevision: r.IdentityRevision}, nil
}
func (s *Service) Write(ctx context.Context, principal, id string, r FileRequest) (FileView, error) {
	if !validPath(r.Path) || len(r.Content) > MaxFileBytes {
		return FileView{}, fail("invalid_request")
	}
	return s.file(ctx, principal, id, r, true)
}
func (s *Service) Read(ctx context.Context, principal, id string, r FileRequest) (FileView, error) {
	if !validPath(r.Path) || r.Content != nil {
		return FileView{}, fail("invalid_request")
	}
	return s.file(ctx, principal, id, r, false)
}
func (s *Service) file(ctx context.Context, principal, id string, r FileRequest, write bool) (FileView, error) {
	if s.runtime == nil {
		return FileView{}, fail("execution_unavailable")
	}
	target, token, err := s.acquire(ctx, principal, id, r.IdentityRevision)
	if err != nil {
		return FileView{}, err
	}
	var content []byte
	if write {
		err = s.runtime.Write(ctx, target, r.Path, r.Content)
	} else {
		content, err = s.runtime.Read(ctx, target, r.Path)
	}
	// Definite filesystem rejections are safe to report without closing the session.
	mutating := write && Code(err) != "file_not_found" && Code(err) != "file_access_denied" && Code(err) != "invalid_path"
	if err = s.finish(ctx, principal, id, r.IdentityRevision, token, err, mutating); err != nil {
		return FileView{}, err
	}
	size := len(content)
	if write {
		size = len(r.Content)
	}
	return FileView{SessionID: id, IdentityRevision: r.IdentityRevision, Path: r.Path, Content: content, Size: size}, nil
}
