package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	sessionprotocol "github.com/X1Kun/AgentBox/internal/session/protocol"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const operationKey = "agentbox.dev/api-operation"
const MaxCodeBytes = sessionprotocol.MaxCodeBytes
const MaxFileBytes = sessionprotocol.MaxFileBytes
const MaxOutputBytes = sessionprotocol.MaxOutputBytes

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
type ExecuteRequest = sessionprotocol.ExecuteRequest
type FileRequest = sessionprotocol.FileRequest
type Execution = sessionprotocol.Execution
type ExecutionView = sessionprotocol.ExecutionView
type FileView = sessionprotocol.FileView

// Configure the adapter once, before serving requests. Lifecycle-only operation
// remains possible, for example on a host without access to the Pod network.
func (s *Service) SetRuntime(r Runtime) { s.runtime = r }
func validPath(p string) bool           { return sessionprotocol.ValidPath(p) }
func (s *Service) checkTarget(ctx context.Context, principal, id, expected string) (*unstructured.Unstructured, *Binding, *unstructured.Unstructured, error) {
	// Reuse only this observation's validated Pod. Each connection boundary
	// makes a new observation, including the full uniqueness/selector scans.
	v, err := s.snapshot(ctx, principal, id)
	if err != nil {
		return nil, nil, nil, err
	}
	if v.State != sessionprotocol.StateReady {
		return nil, nil, nil, fail(sessionprotocol.CodeSessionNotReady)
	}
	if expected == "" || v.IdentityRevision != expected {
		return nil, nil, nil, fail(sessionprotocol.CodeIdentityChanged)
	}
	// Re-read after observing backing objects: Close or another holder may
	// have changed the Claim while those API reads were in flight.
	c, err := s.load(ctx, principal, id)
	if err != nil {
		return nil, nil, nil, err
	}
	b, err := readBinding(c)
	if err != nil {
		return nil, nil, nil, err
	}
	if b == nil || v.pod == nil || ref(v.pod) != b.Pod || revision(b) != expected || c.GetAnnotations()[invalidKey] != "" || c.GetAnnotations()[closeKey] == "true" {
		return nil, nil, nil, fail(sessionprotocol.CodeIdentityChanged)
	}
	return c, b, v.pod, nil
}

// The Claim CAS serializes all data-plane calls across API replicas/restarts.
// There is no expiring lease: a crash cannot quietly admit a second execution.
func (s *Service) acquire(ctx context.Context, principal, id, expected string) (Target, string, error) {
	started := time.Now()
	for attempts := 0; attempts < 3; attempts++ {
		c, b, pod, err := s.checkTarget(ctx, principal, id, expected)
		if err != nil {
			return Target{}, "", err
		}
		if c.GetAnnotations()[operationKey] != "" {
			return Target{}, "", fail(sessionprotocol.CodeSessionBusy)
		}
		ip, _, _ := unstructured.NestedString(pod.Object, "status", "podIP")
		if string(pod.GetUID()) != b.Pod.UID || net.ParseIP(ip) == nil {
			return Target{}, "", fail(sessionprotocol.CodeIdentityChanged)
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
			s.operationEvent(ctx, id, token, "acquire", "unknown", err, started)
			return Target{}, "", fail(sessionprotocol.CodeOperationOutcomeUnknown)
		}
		s.operationEvent(ctx, id, token, "acquire", "complete", nil, started)
		target := Target{IP: ip}
		target.Validate = func(callCtx context.Context) error {
			current, _, p, e := s.checkTarget(callCtx, principal, id, expected)
			if e != nil {
				return e
			}
			if current.GetAnnotations()[operationKey] != token {
				return fail(sessionprotocol.CodeStateConflict)
			}
			currentIP, _, _ := unstructured.NestedString(p.Object, "status", "podIP")
			if string(p.GetUID()) != b.Pod.UID || currentIP != ip {
				return fail(sessionprotocol.CodeIdentityChanged)
			}
			return nil
		}
		return target, token, nil
	}
	return Target{}, "", fail(sessionprotocol.CodeStateConflict)
}
func (s *Service) release(ctx context.Context, principal, id, token string) error {
	for attempts := 0; attempts < 3; attempts++ {
		c, err := s.load(ctx, principal, id)
		if err != nil {
			return err
		}
		if c.GetAnnotations()[operationKey] != token {
			return fail(sessionprotocol.CodeStateConflict)
		}
		err = s.patch(ctx, c, map[string]string{operationKey: ""}, "")
		if err == nil {
			return nil
		}
		if !retryPatch(err) {
			// Resolve only by reading the same Claim UID. Never replay a data
			// operation or clear a different holder's marker after a lost reply.
			current, readErr := s.load(ctx, principal, id)
			if readErr == nil && current.GetAnnotations()[operationKey] == "" {
				return nil
			}
			return err
		}
	}
	return fail(sessionprotocol.CodeStateConflict)
}
func (s *Service) finish(ctx context.Context, principal, id, expected, token string, operationErr error, mutating bool) error {
	// Any uncertain mutation keeps its durable lock and requests full-session
	// shutdown with a detached budget. Timeout/cancellation alone is not cleanup.
	uncertain := operationErr != nil && mutating
	if operationErr == nil {
		started := time.Now()
		_, _, _, operationErr = s.checkTarget(ctx, principal, id, expected)
		uncertain = operationErr != nil
		if uncertain {
			s.operationEvent(ctx, id, token, "validate", "unknown", operationErr, started)
		}
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if uncertain {
		s.requestOperationClose(cleanup, principal, id, token)
		switch Code(operationErr) {
		case sessionprotocol.CodeExecutionOutputLimit:
			return fail(sessionprotocol.CodeExecutionOutputLimit)
		}
		if errors.Is(operationErr, context.DeadlineExceeded) {
			return fail(sessionprotocol.CodeExecutionTimeout)
		}
		return fail(sessionprotocol.CodeOperationOutcomeUnknown)
	}
	started := time.Now()
	if err := s.release(cleanup, principal, id, token); err != nil {
		s.operationEvent(cleanup, id, token, "release", "unknown", err, started)
		s.requestOperationClose(cleanup, principal, id, token)
		return fail(sessionprotocol.CodeOperationOutcomeUnknown)
	}
	s.operationEvent(cleanup, id, token, "release", "complete", nil, started)
	return operationErr
}
func (s *Service) Execute(ctx context.Context, principal, id string, r ExecuteRequest) (ExecutionView, error) {
	if len(r.Code) == 0 || len(r.Code) > MaxCodeBytes || !utf8.ValidString(r.Code) || strings.ContainsRune(r.Code, 0) {
		return ExecutionView{}, fail(sessionprotocol.CodeInvalidRequest)
	}
	seconds := r.TimeoutSeconds
	if seconds == 0 {
		seconds = 20
	}
	if seconds < 1 || seconds > 60 {
		return ExecutionView{}, fail(sessionprotocol.CodeInvalidRequest)
	}
	if s.runtime == nil {
		return ExecutionView{}, fail(sessionprotocol.CodeExecutionUnavailable)
	}
	target, token, err := s.acquire(ctx, principal, id, r.IdentityRevision)
	if err != nil {
		return ExecutionView{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	started := time.Now()
	result, err := s.runtime.Execute(runCtx, target, r.Code)
	if runCtx.Err() != nil {
		err = runCtx.Err()
	}
	cancel()
	outcome := "complete"
	if err != nil {
		outcome = "unknown"
	}
	s.operationEvent(ctx, id, token, "execute", outcome, err, started)
	elapsed := time.Since(started)
	if err = s.finish(ctx, principal, id, r.IdentityRevision, token, err, true); err != nil {
		s.metrics.Operation("execute", "unknown", elapsed)
		return ExecutionView{}, err
	}
	if result.ExitCode != 0 {
		outcome = "program_failure"
	} else {
		outcome = "complete"
	}
	s.metrics.Operation("execute", outcome, elapsed)
	return ExecutionView{Execution: result, SessionID: id, IdentityRevision: r.IdentityRevision}, nil
}
func (s *Service) Write(ctx context.Context, principal, id string, r FileRequest) (FileView, error) {
	if !validPath(r.Path) || len(r.Content) > MaxFileBytes {
		return FileView{}, fail(sessionprotocol.CodeInvalidRequest)
	}
	return s.file(ctx, principal, id, r, true)
}
func (s *Service) Read(ctx context.Context, principal, id string, r FileRequest) (FileView, error) {
	if !validPath(r.Path) || r.Content != nil {
		return FileView{}, fail(sessionprotocol.CodeInvalidRequest)
	}
	return s.file(ctx, principal, id, r, false)
}
func (s *Service) file(ctx context.Context, principal, id string, r FileRequest, write bool) (FileView, error) {
	if s.runtime == nil {
		return FileView{}, fail(sessionprotocol.CodeExecutionUnavailable)
	}
	target, token, err := s.acquire(ctx, principal, id, r.IdentityRevision)
	if err != nil {
		return FileView{}, err
	}
	var content []byte
	started := time.Now()
	if write {
		err = s.runtime.Write(ctx, target, r.Path, r.Content)
	} else {
		content, err = s.runtime.Read(ctx, target, r.Path)
	}
	// Definite filesystem rejections are safe to report without closing the session.
	mutating := write && Code(err) != "file_not_found" && Code(err) != "file_access_denied" && Code(err) != "invalid_path"
	phase, outcome := "read", "complete"
	if write {
		phase = "write"
	}
	if err != nil {
		outcome = "failed"
		if mutating {
			outcome = "unknown"
		}
	}
	s.operationEvent(ctx, id, token, phase, outcome, err, started)
	if err = s.finish(ctx, principal, id, r.IdentityRevision, token, err, mutating); err != nil {
		return FileView{}, err
	}
	size := len(content)
	if write {
		size = len(r.Content)
	}
	return FileView{SessionID: id, IdentityRevision: r.IdentityRevision, Path: r.Path, Content: content, Size: size}, nil
}
